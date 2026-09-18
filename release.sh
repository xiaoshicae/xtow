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
  v*) ;;
  *) echo "版本号要以 v 开头，got=$VERSION"; exit 1;;
esac

# 发布顺序 = 依赖顺序。被依赖的先发，否则后面的模块 require 的版本还不存在。
ORDER=". xtrace xmetric xcache xgorm xredis xhttp xgin xginswagger"

run() {
  echo "  \$ $*"
  [ "$APPLY" = "--apply" ] && eval "$@"
  return 0
}

echo "== 1. 确认工作区干净 =="
if [ -n "$(git status --porcelain)" ]; then
  echo "✗ 工作区有未提交的改动，先提交或暂存"
  exit 1
fi

echo "== 2. 跑一遍检查和测试 =="
./check.sh >/dev/null
./test.sh -count=1 >/dev/null
echo "  ✓ 通过"

echo "== 3. 把各子模块的 replace 换成真实版本号 =="
# replace 指向仓库内的相对路径，只在本地开发有意义。
# 消费者的构建会忽略依赖里的 replace，所以留着不会出错，
# 但那意味着子模块 require 的是 v0.0.0，谁都拉不到。
for m in $ORDER; do
  [ "$m" = "." ] && continue
  [ -f "$m/go.mod" ] || continue
  echo "  $m/go.mod"
  run "sed -i.bak -E 's#^(\\trequire )?github.com/xiaoshicae/xtow(/[a-z]+)? v0.0.0#github.com/xiaoshicae/xtow\\2 $VERSION#' $m/go.mod"
  run "sed -i.bak '/^replace /,/^)/d; /^replace github.com\\/xiaoshicae/d' $m/go.mod"
  run "rm -f $m/go.mod.bak"
done

echo "== 4. 提交并打 tag =="
run "git add -A"
run "git commit -m 'release: $VERSION'"
for m in $ORDER; do
  if [ "$m" = "." ]; then
    run "git tag $VERSION"
  else
    [ -f "$m/go.mod" ] || continue
    run "git tag $m/$VERSION"
  fi
done

echo
if [ "$APPLY" = "--apply" ]; then
  echo "已在本地打好 tag。确认无误后推送："
  echo "  git push origin main --tags"
  echo
  echo "推送之后 tag 就被 module proxy 永久缓存了，删不掉，只能再发一版盖过去。"
else
  echo "以上是 --apply 时会执行的命令，当前什么都没改。"
fi
