#!/bin/sh
# 打 tag 发布。多模块仓库的每个 module 有自己的 tag，且必须按依赖顺序发。
#
#   ./release.sh v0.1.0          # 只打印要做什么，不改任何东西
#   ./release.sh v0.1.0 --apply  # 真的改 go.mod、提交、打 tag（仍不推送）
#
# 推送是单独一步，由人来做：Go 的 module proxy 会永久缓存 tag，
# 推错了删不掉，只能再发一个版本盖过去。
set -e

VERSION="$1"
APPLY="$2"
[ -n "$VERSION" ] || { echo "用法：./release.sh v0.1.0 [--apply]"; exit 1; }
case "$VERSION" in
  v*.*.*) ;;
  *) echo "版本号格式应为 vX.Y.Z，got=$VERSION"; exit 1;;
esac

MOD=github.com/xiaoshicae/xtow

# 发布顺序 = 依赖顺序。被依赖的先发，否则后面的模块 require 的版本还不存在。
#
# example 不在列表里，也不打 tag：它是示例不是库，replace 要一直留着，
# 这样它永远编译的是仓库当前的代码，而不是某个已发布版本。
ORDER="xtrace xmetric xcache xgorm xredis xhttp xgin xginswagger"

run() {
  echo "  \$ $*"
  if [ "$APPLY" = "--apply" ]; then
    "$@"
  fi
}

echo "== 1. 确认工作区干净 =="
[ -z "$(git status --porcelain)" ] || { echo "✗ 工作区有未提交的改动，先提交或暂存"; exit 1; }

echo "== 2. 跑一遍检查和测试 =="
./check.sh >/dev/null
./test.sh -count=1 >/dev/null
echo "  ✓ 通过"

echo "== 3. 把各子模块开发用的 replace 换成真实版本号 =="
# replace 指向仓库内的相对路径，只在本地开发有意义。
# 消费者的构建会忽略依赖里的 replace，所以留着不会出错，
# 但那意味着子模块 require 的是 v0.0.0，谁都拉不到。
#
# 用 go mod edit 而不是 sed：require 块的缩进、子模块路径后缀这些细节
# 手写正则很容易弄错，而弄错的后果是发出去一个装不上的版本。
for m in $ORDER; do
  [ -f "$m/go.mod" ] || continue
  echo "  $m/go.mod"
  # 这个模块 require 了哪些仓库内的模块，就把哪些的版本号钉上。
  # 要跳过它自己：go mod edit -json 里也有 module 自身的路径，
  # 不跳的话会给它加一条「自己 require 自己」
  for dep in $(GOWORK=off go mod edit -json "$m/go.mod" | grep -oE "\"$MOD(/[a-z]+)?\"" | tr -d '"' | sort -u); do
    [ "$dep" = "$MOD/$m" ] && continue
    run env GOWORK=off go mod edit -dropreplace="$dep" -require="$dep@$VERSION" "$m/go.mod"
  done
done

echo "== 4. 确认去掉 replace 之后还能编译 =="
if [ "$APPLY" = "--apply" ]; then
  echo "  （tag 还没打，此时依赖拉不到，跳过；推送 tag 之后再验证）"
fi

echo "== 5. 提交并打 tag =="
run git add -A
run git commit -m "release: $VERSION"
run git tag "$VERSION"
for m in $ORDER; do
  [ -f "$m/go.mod" ] || continue
  run git tag "$m/$VERSION"
done

echo
if [ "$APPLY" = "--apply" ]; then
  cat <<TIP

已在本地打好 tag。确认无误后推送：

  git push origin main --tags

推送之后 tag 就被 module proxy 永久缓存了，删不掉，只能再发一版盖过去。
推完验证一下别人装不装得上：

  cd \$(mktemp -d) && go mod init probe && go get $MOD/xgorm@$VERSION
TIP
else
  echo "以上是 --apply 时会执行的命令，当前什么都没改。"
fi
