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
set -e

[ -z "$(git status --porcelain)" ] || { echo "✗ 工作区不干净，先提交或暂存"; exit 1; }

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"; git checkout -- . 2>/dev/null || true' EXIT

total=0
survived=0

# mutate 名字 文件 模块 测试过滤  （python 改写代码从 stdin 读）
mutate() {
  name="$1"; file="$2"; module="$3"; filter="$4"
  cat > "$TMP/m.py"
  total=$((total + 1))

  cp "$file" "$TMP/orig"
  python3 "$TMP/m.py" "$file" || { echo "  ? $name（变异没应用上，改坏的位置可能已经不在了）"; cp "$TMP/orig" "$file"; return; }

  if cmp -s "$TMP/orig" "$file"; then
    echo "  ? $name（变异没改动任何东西，模式失效了）"
    cp "$TMP/orig" "$file"
    return
  fi

  if (cd "$module" && GOWORK=off go test -count=1 -run "$filter" ./... >/dev/null 2>&1); then
    echo "  ✗ $name —— 改坏了但测试全过"
    survived=$((survived + 1))
  else
    echo "  ✓ $name"
  fi
  cp "$TMP/orig" "$file"
}

echo "== 配置 =="
mutate "字段拼错要启动失败" xconfig/xconfig.go . 'TestDecode' <<'PY'
import sys; p=sys.argv[1]; s=open(p,encoding='utf-8').read()
open(p,'w',encoding='utf-8').write(s.replace('dec.KnownFields(true)','dec.KnownFields(false)'))
PY
mutate "占位符按替换后的内容判定类型" internal/config/config.go . 'TestLoad' <<'PY'
import sys; p=sys.argv[1]; s=open(p,encoding='utf-8').read()
open(p,'w',encoding='utf-8').write(s.replace('\t\tif n.Value != before {\n\t\t\tretag(n)\n\t\t}','\t\t_ = before'))
PY
mutate "重复的顶层 key 要报错" internal/config/config.go . 'TestLoad' <<'PY'
import sys, re; p=sys.argv[1]; s=open(p,encoding='utf-8').read()
s=re.sub(r'\t\tif prev, dup := lines\[key\.Value\]; dup \{\n(.*\n)*?\t\t\}\n', '', s, count=1)
open(p,'w',encoding='utf-8').write(s)
PY

echo "== 启动与退出 =="
mutate "第二个信号能终止卡住的进程" xtow.go . 'TestRun' <<'PY'
import sys; p=sys.argv[1]; s=open(p,encoding='utf-8').read()
open(p,'w',encoding='utf-8').write(s.replace('\t\t\tsignal.Stop(ch)\n\t\t\to.log().Info(','\t\t\to.log().Info(',1))
PY
mutate "组件 Close 受停止预算约束" xtow.go . 'TestShutdown' <<'PY'
import sys; p=sys.argv[1]; s=open(p,encoding='utf-8').read()
open(p,'w',encoding='utf-8').write(s.replace('if err := closeWithin(ctx, n); err != nil {','if err := safe(n.key, n.c.Close); err != nil {'))
PY
mutate "初始化期间收到信号就不启动服务" xtow.go . 'TestRun' <<'PY'
import sys; p=sys.argv[1]; s=open(p,encoding='utf-8').read()
open(p,'w',encoding='utf-8').write(s.replace('closers, err := initAll(ctx, list, o)','closers, err := initAll(context.Background(), list, o)'))
PY
mutate "建连重试可以被取消" xutil/convert.go . 'TestRetry' <<'PY'
import sys; p=sys.argv[1]; s=open(p,encoding='utf-8').read()
open(p,'w',encoding='utf-8').write(s.replace('\tif err := parent.Err(); err != nil {\n\t\treturn err\n\t}\n\n',''))
PY
mutate "建实例 panic 不漏掉已建好的" xclient/xclient.go . 'TestBuild' <<'PY'
import sys; p=sys.argv[1]; s=open(p,encoding='utf-8').read()
open(p,'w',encoding='utf-8').write(s.replace('safeNew(ctx, name, cfgs[name], new)','new(ctx, cfgs[name])'))
PY

echo "== 流程编排 =="
mutate "被取消的流程不能报成功" xflow/xflow.go . 'TestExecute' <<'PY'
import sys; p=sys.argv[1]; s=open(p,encoding='utf-8').read()
open(p,'w',encoding='utf-8').write(s.replace('\t\t\tif ctx.Err() == nil {\n\t\t\t\tcontinue\n\t\t\t}','\t\t\tcontinue'))
PY
mutate "监控实现 panic 被隔离" xflow/monitor.go . 'TestMonitor' <<'PY'
import sys; p=sys.argv[1]; s=open(p,encoding='utf-8').read()
open(p,'w',encoding='utf-8').write(s.replace('\tdefer recoverNotify()\n','',1))
PY

echo "== HTTP 服务 =="
mutate "服务不超过调用方给的截止时间" xgin/xgin.go ./xgin 'TestStop' <<'PY'
import sys; p=sys.argv[1]; s=open(p,encoding='utf-8').read()
open(p,'w',encoding='utf-8').write(s.replace('context.WithTimeout(ctx, g.conf().ShutdownTimeout)','context.WithTimeout(context.WithoutCancel(ctx), g.conf().ShutdownTimeout)'))
PY
mutate "超时后强制断掉在途连接" xgin/xgin.go ./xgin 'TestStop' <<'PY'
import sys; p=sys.argv[1]; s=open(p,encoding='utf-8').read()
open(p,'w',encoding='utf-8').write(s.replace('\t\tif cerr := srv.Close(); cerr != nil {\n\t\t\tslog.Warn("xgin force close failed", "error", cerr)\n\t\t}\n',''))
PY
mutate "内置路由也走用户中间件" xgin/xgin.go ./xgin 'TestBuild' <<'PY'
import sys; p=sys.argv[1]; s=open(p,encoding='utf-8').read()
s=s.replace('\t\te.Use(middleware.Recover(g.recover))\n\t\te.Use(g.extra...)\n','\t\te.Use(middleware.Recover(g.recover))\n')
s=s.replace('\t\tfor _, f := range g.routes {','\t\te.Use(g.extra...)\n\t\tfor _, f := range g.routes {')
open(p,'w',encoding='utf-8').write(s)
PY
mutate "默认不信任 X-Forwarded-For" xgin/xgin.go ./xgin 'TestClientIP|TestLog' <<'PY'
import sys, re; p=sys.argv[1]; s=open(p,encoding='utf-8').read()
s=re.sub(r'\t\tif err := e\.SetTrustedProxies\(g\.conf\(\)\.TrustedProxies\); err != nil \{\n(.*\n)*?\t\t\}\n', '', s, count=1)
open(p,'w',encoding='utf-8').write(s)
PY

echo "== 中间件 =="
mutate "指标的 method 标签收敛" xgin/middleware/metric.go ./xgin 'TestMetric' <<'PY'
import sys; p=sys.argv[1]; s=open(p,encoding='utf-8').read()
open(p,'w',encoding='utf-8').write(s.replace('normalizeMethod(c.Request.Method)','c.Request.Method').replace('\tif _, ok := knownMethods[m]; ok {\n\t\treturn m\n\t}\n\treturn methodOther','\treturn m'))
PY
mutate "请求头里的凭证被遮掉" xgin/middleware/redact.go ./xgin 'TestRedact' <<'PY'
import sys; p=sys.argv[1]; s=open(p,encoding='utf-8').read()
open(p,'w',encoding='utf-8').write(s.replace('\t\tif set[strings.ToLower(k)] {\n\t\t\tattrs = append(attrs, slog.String(k, Redacted))\n\t\t\tcontinue\n\t\t}\n',''))
PY
mutate "请求体只缓存前缀" xgin/middleware/log.go ./xgin 'TestSnapshotBody' <<'PY'
import sys; p=sys.argv[1]; s=open(p,encoding='utf-8').read()
open(p,'w',encoding='utf-8').write(s.replace('io.ReadAll(io.LimitReader(req.Body, maxRequestBody))','io.ReadAll(req.Body)',1))
PY
mutate "预读时的错误接回下游" xgin/middleware/log.go ./xgin 'TestSnapshotBody' <<'PY'
import sys; p=sys.argv[1]; s=open(p,encoding='utf-8').read()
open(p,'w',encoding='utf-8').write(s.replace('\tif b.preErr != nil {\n\t\treturn 0, b.preErr\n\t}\n',''))
PY

echo "== 客户端 =="
mutate "关闭时清掉空闲连接" xhttp/xhttp.go ./xhttp 'TestNew' <<'PY'
import sys; p=sys.argv[1]; s=open(p,encoding='utf-8').read()
open(p,'w',encoding='utf-8').write(s.replace('\treturn client, &clientCloser{pool: pool}, nil','\treturn client, &clientCloser{pool: traced(cfg, pool)}, nil'))
PY
mutate "重试耗时算整次逻辑请求" xhttp/metric.go ./xhttp 'TestMetric' <<'PY'
import sys; p=sys.argv[1]; s=open(p,encoding='utf-8').read()
open(p,'w',encoding='utf-8').write(s.replace('elapsed(resp.Request, resp.Time())','resp.Time()'))
PY
mutate "DSN 里的密码不进日志" xgorm/dsn.go ./xgorm 'TestParseKV|TestPostgresConnInfo|TestLogConn' <<'PY'
import sys; p=sys.argv[1]; s=open(p,encoding='utf-8').read()
open(p,'w',encoding='utf-8').write(s.replace('for _, tok := range splitKV(dsn) {','for _, tok := range strings.Fields(dsn) {'))
PY
mutate "首次建连受 ctx 管" xgorm/xgorm.go ./xgorm 'TestNew' <<'PY'
import sys; p=sys.argv[1]; s=open(p,encoding='utf-8').read()
open(p,'w',encoding='utf-8').write(s.replace('gorm.Config{DisableAutomaticPing: true}','gorm.Config{}'))
PY
mutate "Redis 命令遵守请求 deadline" xredis/xredis.go ./xredis 'Test' <<'PY'
import sys; p=sys.argv[1]; s=open(p,encoding='utf-8').read()
open(p,'w',encoding='utf-8').write(s.replace('\t\tContextTimeoutEnabled: true,\n',''))
PY

echo
if [ "$survived" -eq 0 ]; then
  echo "✓ $total 条承诺全部有测试盯着"
else
  echo "✗ $total 条里有 $survived 条改坏了也没人发现"
  exit 1
fi
