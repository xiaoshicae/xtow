#!/bin/sh
# 把设计约束编译成检查。趁还没东西可违反的时候加上——
# 晚了就得豁免一堆存量，那等于没加。
set -e
fail() { echo "✗ $1"; exit 1; }

# 仓库里的模块：根 + 每个带 go.mod 的子目录
modules=". $(find . -mindepth 2 -name go.mod -not -path './.git/*' | xargs -r -n1 dirname | sort)"

# ---- 1. 核心的依赖足迹 ----
# 使用者 import 任何一个核心里的包，都会背上核心的全部依赖约束，所以核心必须极瘦。
# 每加一个都要先问：能不能不加。
# GOWORK=off 是必须的：工作区里 go list -m all 会把所有模块的依赖并在一起。
n=$(GOWORK=off go list -m all | grep -vc '^github.com/xiaoshicae/xtow')
[ "$n" -le 3 ] || fail "核心模块图 $n 个模块，超过上限 3（当前应为 yaml 及其测试依赖）"
echo "✓ 核心模块图 $n 个（上限 3）"

# ---- 2. 核心不得依赖任何集成模块 ----
# 反过来就把集成的依赖又带回给所有人了，分模块也就白分了
! GOWORK=off go list -m all | grep -q '^github.com/xiaoshicae/xtow/' \
  || fail "核心 require 了集成模块，分模块的意义没了"
echo "✓ 核心不依赖任何集成模块"

# ---- 3. registry 与基础包必须零第三方依赖 ----
# registry 是集成包唯一认识的东西，一旦它有依赖，所有集成都被迫背上
for pkg in ./registry ./xerror ./xutil; do
  d=$(GOWORK=off go list -deps "$pkg" | grep -E '^[^/]*\.' | grep -vc xiaoshicae || true)
  [ "$d" -eq 0 ] || fail "$pkg 混进了 $d 个第三方包"
done
echo "✓ registry / xerror / xutil 零第三方依赖"

# ---- 4. 只有集成包可以有 init() ----
# 集成包的 init 只登记不初始化；核心自己则连登记都不该有。
# 判据是「这个目录调没调 registry.Register」，不是目录名也不是有没有 go.mod：
# xlog 零依赖留在核心模块里，同样是集成包。
# 两个登记入口：组件登记进框架，方言登记进 xgorm。两者都只是「记下来」，
# 不初始化任何东西，所以放在 init() 里是对的
integrations=$(grep -rlE 'registry\.Register\(|xgorm\.RegisterDialect\(' --include='*.go' . | grep -v '_test.go' | xargs -r -n1 dirname | sort -u)
for f in $(grep -rl '^func init()' --include='*.go' . | grep -v '_test.go'); do
  d=$(dirname "$f")
  echo "$integrations" | grep -qx "$d" || fail "$f 有 init()，但它不是集成包（没有 registry.Register）"
done
echo "✓ init() 只出现在集成包里"

# ---- 5. 核心公开 API 数量上限 ----
# 框架的 API 是永久的。让「加一个」有代价，超了就得先砍再加。
a=$(go doc -all . | grep -cE '^(func|type) ')
[ "$a" -le 15 ] || fail "根包公开 API $a 个，超过上限 15"

# xconfig / xclient 是核心里给集成包用的两个辅助包，它们越小越好：
# 每加一个导出就是一条所有第三方集成都得跟着理解的规矩
x=$(go doc -all ./xconfig | grep -cE '^(func|type) ')
[ "$x" -le 4 ] || fail "xconfig 公开 API $x 个，超过上限 4"
cl=$(go doc -all ./xclient | grep -cE '^(func|type) ')
[ "$cl" -le 8 ] || fail "xclient 公开 API $cl 个，超过上限 8"
echo "✓ 公开 API：根包 $a（上限 15）、xconfig $x（上限 4）、xclient $cl（上限 8）"

# ---- 6. 集成包必须导出纯构造器 New，且不得引用根包 ----
# New 保证「零装配」永远只是默认路径，不是唯一路径：
# 测试和特殊场景始终可以绕开框架直接造实例。
# 不引用根包保证依赖是单向的——集成认识 registry，框架认识 registry，彼此不认识。
[ -n "$integrations" ] || fail "一个集成包都没找到，第 4 步的判据失效了"
for d in $integrations; do
  # 驱动包 import 的是 xgorm 而不是框架，这条对它不适用
  grep -rq 'registry\.Register(' "$d"/*.go || continue
  ! grep -rq '"github.com/xiaoshicae/xtow"' "$d"/*.go || fail "$d 引用了根包（只能 import registry）"
  # 只认领配置、不造任何东西的包（如 xapp）没有构造器可言，这条对它是空的
  grep -rq 'Init:' "$d"/*.go || continue
  # New[T any]( 也算：泛型构造器同样是「绕开框架直接造一个」的入口
  grep -rqE '^func New[(\[]' "$d"/*.go || fail "$d 登记了 Init，却没有纯构造器 New"
done
echo "✓ 集成包检查通过（$(echo "$integrations" | wc -w) 个）"

# ---- 7. 配置字段都写进文档 ----
# 配置是使用者唯一的操作界面，加了字段却没写文档，等于没加。
# 反过来文档里多写一个不存在的 key，会让人配了半天发现不生效。
missing=""
for f in $(grep -rhoE 'yaml:"[A-Za-z]+"' --include='*.go' --exclude='*_test.go' . | sed 's/yaml:"//; s/"//' | sort -u); do
  grep -q "\b$f\b" docs/config.md || missing="$missing $f"
done
[ -z "$missing" ] || fail "这些配置字段没写进 docs/config.md：$missing"
echo "✓ 配置字段都写进文档了"

# ---- 8. 运行期字符串必须是英文 ----
# 注释写给读这份代码的人，用中文；但错误和日志会落进使用者的系统——
# 进他们的告警、他们的日志检索、他们的 issue。中文的日志字段名还会变成
# JSON 的 key，让日志平台的索引和看板直接对不上。
zh=$(python3 - <<'PY_EOF'
import re, glob
zh = re.compile(r'[\u4e00-\u9fff]')
strlit = re.compile(r'"(?:[^"\\]|\\.)*"')
for f in sorted(glob.glob('**/*.go', recursive=True)):
    if f.endswith('_test.go'):
        continue
    for i, line in enumerate(open(f, encoding='utf-8'), 1):
        if line.lstrip().startswith('//'):
            continue
        for m in strlit.finditer(line.split('//')[0]):
            if zh.search(m.group()):
                print(f"{f}:{i}")
                break
PY_EOF
)
[ -z "$zh" ] || fail "这些地方的运行期字符串还是中文（错误和日志要用英文）：
$zh"
echo "✓ 运行期字符串都是英文"

# ---- 9. 基本卫生 ----
[ -z "$(gofmt -l .)" ] || fail "有文件未格式化：$(gofmt -l .)"
for m in $modules; do
  (cd "$m" && GOWORK=off go vet ./... >/dev/null 2>&1) || fail "$m go vet 未通过"
done
echo "✓ gofmt / go vet（$(echo "$modules" | wc -w) 个模块）"
