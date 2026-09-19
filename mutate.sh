#!/bin/sh
# 变异测试：把每一条承诺对应的代码改坏，看有没有测试会失败。
#
#   ./mutate.sh
#
# 活下来的变异 = 一条没有牙齿的承诺：代码写着、文档写着，但改坏了没人知道。
# 这个仓库前后被外部 review 挑出过二十多个问题，事后归类，绝大多数都是
# 「承诺有、测试没有」。与其每次等人来挑，不如让它自己说出来。
#
# 这不是 CI 的一部分（跑一轮要几分钟，而且要改工作区），是改完一批
# 安全或生命周期相关的代码之后手动跑一次的东西。
#
# 只有「我们自己有代码在守」的承诺才适合放进来。像「ctx 能给整次逻辑请求
# 封顶」那种由依赖库保证的性质，这里没有哪一行可以改坏，硬写一条变异
# 只会让「活下来 = 缺测试」这个信号失真——那种承诺靠测试守着就行，
# 它防的是升级依赖时的回归，不是防我们自己改错。
set -e

[ -z "$(git status --porcelain)" ] || { echo "✗ 工作区不干净，先提交或暂存"; exit 1; }

HELPERS="$(CDPATH= cd "$(dirname "$0")" && pwd)/mutation_helpers.py"
[ -f "$HELPERS" ] || { echo "✗ 找不到 $HELPERS"; exit 1; }

TMP=$(mktemp -d)

# 只还原自己动过的那几个文件。原来这里写的是 git checkout -- .，
# 那等于「脚本被打断时把整个工作区清空」——前置检查保证了开跑时是干净的，
# 但脚本自己不该持有这种权力，何况被 kill 时 trap 根本不一定跑得到
restore() {
  [ -f "$TMP/touched" ] || return 0
  while IFS= read -r f; do
    [ -f "$TMP/bak/$(echo "$f" | tr / _)" ] && cp "$TMP/bak/$(echo "$f" | tr / _)" "$f"
  done < "$TMP/touched"
}
trap 'restore; rm -rf "$TMP"' EXIT INT TERM
mkdir -p "$TMP/bak"

total=0
survived=0
stale=0

# mutate 名字 文件 模块 测试过滤  （python 改写代码从 stdin 读）
mutate() {
  name="$1"; file="$2"; module="$3"; filter="$4"
  # 变异只写 swap() / cut()，读文件、写回、以及「模式还匹配得上吗」的断言
  # 都由 mutation_helpers.py 提供，不用每条自己记得写
  cat "$HELPERS" > "$TMP/m.py"
  cat >> "$TMP/m.py"
  printf '\nopen(_p, "w", encoding="utf-8").write(_s)\n' >> "$TMP/m.py"
  total=$((total + 1))

  cp "$file" "$TMP/orig"
  cp "$file" "$TMP/bak/$(echo "$file" | tr / _)"
  echo "$file" >> "$TMP/touched"
  python3 "$TMP/m.py" "$file" || { echo "  ? $name（变异没应用上，改坏的位置可能已经不在了）"; stale=$((stale + 1)); cp "$TMP/orig" "$file"; return; }

  # 改不动和「改坏了没人发现」一样严重：这条承诺这一轮根本没被检查，
  # 而脚本从前照样报绿。重构挪走了一段代码，对应的变异就这样悄悄失效了
  if cmp -s "$TMP/orig" "$file"; then
    echo "  ? $name（变异没改动任何东西，模式失效了）"
    stale=$((stale + 1))
    cp "$TMP/orig" "$file"
    return
  fi

  # -timeout 是必须的：有些变异会让测试挂住而不是失败（比如把 ctx 换成
  # Background，等信号的那一步就永远等不到）。没有上限的话一个这样的变异
  # 就能把整轮跑死在那里。挂住同样说明测试察觉到了，算作被杀掉
  if (cd "$module" && GOWORK=off go test -count=1 -timeout 90s -run "$filter" ./... >/dev/null 2>&1); then
    echo "  ✗ $name —— 改坏了但测试全过"
    survived=$((survived + 1))
  else
    echo "  ✓ $name"
  fi
  cp "$TMP/orig" "$file"
}

echo "== 配置 =="
mutate "字段拼错要启动失败" xconfig/xconfig.go . 'TestDecode' <<'PY'
swap('dec.KnownFields(true)','dec.KnownFields(false)')
PY
mutate "占位符按替换后的内容判定类型" internal/config/config.go . 'TestLoad' <<'PY'
swap('\t\tif n.Value != before {\n\t\t\tretag(n)\n\t\t}','\t\t_ = before')
PY
mutate "重复的顶层 key 要报错" internal/config/config.go . 'TestLoad' <<'PY'
cut('\t\tif prev, dup := lines[key.Value]; dup {', 'key.Value, key.Line, prev)\n\t\t}\n')
PY

echo "== 启动与退出 =="
mutate "第二个信号能终止卡住的进程" xtow.go . 'TestRun' <<'PY'
swap('\t\t\tsignal.Stop(ch)\n\t\t\to.log().Info(','\t\t\to.log().Info(',1)
PY
mutate "组件 Close 受停止预算约束" xtow.go . 'TestShutdown' <<'PY'
swap('if err := closeWithin(ctx, n); err != nil {','if err := safe(n.key, n.c.Close); err != nil {')
PY
mutate "初始化期间收到信号就不启动服务" xtow.go . 'TestRun' <<'PY'
swap('closers, err := initAll(ctx, list, o)','closers, err := initAll(context.Background(), list, o)')
PY
mutate "出错时问得出是谁报的" xtow.go . 'TestRun' <<'PY'
old = 'xerror.Newf("xtow", "init", "component %s failed: %w", c.Key, err)'
swap(old, old.replace('%w', '%v'))
PY
mutate "建连重试可以被取消" xutil/convert.go . 'TestRetry' <<'PY'
swap('\tif err := parent.Err(); err != nil {\n\t\treturn err\n\t}\n\n','')
PY
mutate "建实例 panic 不漏掉已建好的" xclient/xclient.go . 'TestBuild' <<'PY'
swap('safeNew(ctx, r.module, name, cfgs[name], new)', 'new(ctx, cfgs[name])')
PY

echo "== 流程编排 =="
mutate "被取消的流程不能报成功" xflow/xflow.go . 'TestExecute' <<'PY'
swap('\t\t\tif ctx.Err() == nil {\n\t\t\t\tcontinue\n\t\t\t}','\t\t\tcontinue')
PY
mutate "监控实现 panic 被隔离" xflow/monitor.go . 'TestMonitor' <<'PY'
# 两处：notifyStep 和 notifyFlow 各有一个，都去掉才算关掉隔离
swap('\tdefer recoverNotify()\n', '', count=2)
PY

echo "== HTTP 服务 =="
mutate "服务不超过调用方给的截止时间" xgin/xgin.go ./xgin 'TestStop' <<'PY'
swap('context.WithTimeout(ctx, g.conf().ShutdownTimeout)','context.WithTimeout(context.WithoutCancel(ctx), g.conf().ShutdownTimeout)')
PY
mutate "超时后强制断掉在途连接" xgin/xgin.go ./xgin 'TestStop' <<'PY'
swap('\t\tif cerr := srv.Close(); cerr != nil {\n\t\t\tslog.Warn("xgin force close failed", "error", cerr)\n\t\t}\n','')
PY
mutate "内置路由也走用户中间件" xgin/xgin.go ./xgin 'TestBuild' <<'PY'
swap('\t\te.Use(middleware.Recover(g.recover))\n\t\te.Use(g.extra...)\n', '\t\te.Use(middleware.Recover(g.recover))\n')
swap('\t\tfor _, f := range g.routes {', '\t\te.Use(g.extra...)\n\t\tfor _, f := range g.routes {')
PY
mutate "默认不信任 X-Forwarded-For" xgin/xgin.go ./xgin 'TestBuild|TestLog' <<'PY'
cut('\tif err := e.SetTrustedProxies(c.TrustedProxies); err != nil {',
    '_ = e.SetTrustedProxies([]string{})\n\t}\n')
PY

echo "== 中间件 =="
mutate "代理网段写错要启动失败" xgin/config.go ./xgin 'TestValidate' <<'PY'
swap('if !isIPOrCIDR(p) {','if false {')
PY
mutate "指标的 method 标签收敛" xgin/middleware/metric.go ./xgin 'TestMetric' <<'PY'
swap('normalizeMethod(c.Request.Method)', 'c.Request.Method')
PY
mutate "请求头里的凭证被遮掉" xgin/middleware/redact.go ./xgin 'TestRedact' <<'PY'
swap('\t\tif set[strings.ToLower(k)] {\n\t\t\tattrs = append(attrs, slog.String(k, Redacted))\n\t\t\tcontinue\n\t\t}\n','')
PY
mutate "关独立实例不影响全局链路" xtrace/xtrace.go ./xtrace 'TestClose' <<'PY'
swap('\tif live == tp {\n\t\tlive = nil\n\t}','\tlive = nil')
PY
mutate "配置一律在 Start 生效" xgin/xgin.go ./xgin 'TestStart' <<'PY'
swap('\tapplyConfig(g.engine, c)\n','')
PY
mutate "密码里的参数名骗不过注入" xgorm/dsn.go ./xgorm 'TestInjectPostgresKV' <<'PY'
swap('for _, tok := range splitKV(dsn) {\n\t\tif k, _, ok := strings.Cut(tok, \"=\"); ok {','for _, tok := range strings.Fields(dsn) {\n\t\tif k, _, ok := strings.Cut(tok, \"=\"); ok {')
PY
mutate "查询串不进访问日志" xgin/middleware/log.go ./xgin 'TestLog' <<'PY'
swap('"path", c.Request.URL.Path,','"path", c.Request.URL.RequestURI(),')
PY
mutate "请求体只缓存前缀" xgin/middleware/log.go ./xgin 'TestSnapshotBody' <<'PY'
swap('io.ReadAll(io.LimitReader(req.Body, maxRequestBody))','io.ReadAll(req.Body)',1)
PY
mutate "预读时的错误接回下游" xgin/middleware/log.go ./xgin 'TestSnapshotBody' <<'PY'
swap('\tif b.preErr != nil {\n\t\treturn 0, b.preErr\n\t}\n','')
PY

echo "== 客户端 =="
mutate "关闭时清掉空闲连接" xhttp/xhttp.go ./xhttp 'TestNew' <<'PY'
swap('\treturn client, &clientCloser{pool: pool}, nil','\treturn client, &clientCloser{pool: traced(cfg, pool)}, nil')
PY
mutate "重试耗时算整次逻辑请求" xhttp/metric.go ./xhttp 'TestMetric' <<'PY'
swap('elapsed(resp.Request, resp.Time())','resp.Time()')
PY
mutate "DSN 里的密码不进日志" xgorm/dsn.go ./xgorm 'TestParseKV|TestPostgresConnInfo|TestLogConn' <<'PY'
# splitKV 现在有两处调用（注入参数、提取连接信息共用一套解析）。
# 这条承诺针对的是提取那一处，带上下文精确定位，别把另一处也改了
swap('''func parseKV(dsn string) map[string]string {
\tout := map[string]string{}
\tfor _, tok := range splitKV(dsn) {''',
     '''func parseKV(dsn string) map[string]string {
\tout := map[string]string{}
\tfor _, tok := range strings.Fields(dsn) {''')
PY
mutate "首次建连受 ctx 管" xgorm/xgorm.go ./xgorm 'TestNew' <<'PY'
swap('gorm.Config{DisableAutomaticPing: true}','gorm.Config{}')
PY
mutate "Redis 命令遵守请求 deadline" xredis/xredis.go ./xredis 'TestNew' <<'PY'
swap('\t\tContextTimeoutEnabled: true,\n','')
PY
mutate "MaxCost 就是能存多少条" xcache/xcache.go ./xcache 'TestNew' <<'PY'
swap('IgnoreInternalCost: true,','IgnoreInternalCost: false,')
PY
mutate "Log 关掉时 GORM 不自己往标准输出写" xgorm/xgorm.go ./xgorm 'TestNew' <<'PY'
swap('gormCfg.Logger = logger.Discard','gormCfg.Logger = logger.Default')
PY

echo
bad=$((survived + stale))
if [ "$bad" -eq 0 ]; then
  echo "✓ $total 条承诺全部有测试盯着"
  exit 0
fi
[ "$survived" -eq 0 ] || echo "✗ $survived 条改坏了也没人发现"
[ "$stale" -eq 0 ] || echo "✗ $stale 条的变异模式已经失效，这一轮根本没检查到（多半是重构挪走了那段代码）"
echo "  —— 共 $total 条"
exit 1
