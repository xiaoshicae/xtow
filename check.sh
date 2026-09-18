#!/bin/sh
# 把设计约束编译成检查。趁还没东西可违反的时候加上——
# 晚了就得豁免一堆存量，那等于没加。
set -e
fail() { echo "✗ $1"; exit 1; }

# ---- 1. 核心的依赖足迹 ----
# 使用者 import 任何一个 xtow 的包，都会背上核心的全部依赖约束，
# 所以核心必须极瘦。每加一个都要先问：能不能不加。
n=$(go list -m all | grep -vc '^github.com/xiaoshicae/xtow')
[ "$n" -le 3 ] || fail "核心模块图 $n 个模块，超过上限 3（当前应为 yaml 及其测试依赖）"
echo "✓ 核心模块图 $n 个（上限 3）"

# ---- 2. registry 必须零第三方依赖 ----
# 它是集成包唯一认识的东西，一旦它有依赖，所有集成都被迫背上
d=$(go list -deps ./registry | grep -E '^[^/]*\.' | grep -vc xiaoshicae || true)
[ "$d" -eq 0 ] || fail "registry 混进了 $d 个第三方包"
echo "✓ registry 零第三方依赖"

# ---- 3. 基础包零第三方依赖 ----
d=$(go list -deps ./xerror ./xutil | grep -E '^[^/]*\.' | grep -vc xiaoshicae || true)
[ "$d" -eq 0 ] || fail "xerror/xutil 混进了第三方包"
echo "✓ xerror / xutil 零第三方依赖"

# ---- 4. 只有集成包可以有 init() ----
# 集成包的 init 只登记不初始化；核心自己则连登记都不该有。
# 判据是「这个目录调没调 registry.Register」，不是目录名也不是有没有 go.mod：
# xlog 零依赖留在核心模块里，同样是集成包。
integrations=$(grep -rl 'registry\.Register(' --include='*.go' . | grep -v '_test.go' | xargs -r -n1 dirname | sort -u)
for f in $(grep -rl '^func init()' --include='*.go' . | grep -v '_test.go'); do
  d=$(dirname "$f")
  echo "$integrations" | grep -qx "$d" || fail "$f 有 init()，但它不是集成包（没有 registry.Register）"
done
echo "✓ init() 只出现在集成包里"

# ---- 5. 核心公开 API 数量上限 ----
# 框架的 API 是永久的。让「加一个」有代价，超了就得先砍再加。
a=$(go doc -all . | grep -cE '^(func|type) ')
[ "$a" -le 15 ] || fail "根包公开 API $a 个，超过上限 15"
echo "✓ 根包公开 API $a 个（上限 15）"

# ---- 6. 每个集成包必须导出纯构造器 New，且不得引用根包 ----
# New 保证「零装配」永远只是默认路径，不是唯一路径：
# 测试和特殊场景始终可以绕开框架直接造实例。
# 不引用根包保证依赖是单向的——集成认识 registry，框架认识 registry，彼此不认识。
[ -n "$integrations" ] || fail "一个集成包都没找到，第 4 步的判据失效了"
for d in $integrations; do
  grep -rq '^func New(' "$d"/*.go || fail "$d 缺少纯构造器 New"
  ! grep -rq '"github.com/xiaoshicae/xtow"' "$d"/*.go || fail "$d 引用了根包（只能 import registry）"
done
echo "✓ 集成包检查通过（$(echo "$integrations" | wc -w) 个）"

# ---- 7. 基本卫生 ----
[ -z "$(gofmt -l .)" ] || fail "有文件未格式化：$(gofmt -l .)"
go vet ./... >/dev/null 2>&1 || fail "go vet 未通过"
echo "✓ gofmt / go vet"
