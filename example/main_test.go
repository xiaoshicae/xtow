package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"

	"github.com/xiaoshicae/xtow"
	"github.com/xiaoshicae/xtow/xmetric"
)

// 这是唯一一处把各模块放在一起跑的测试：单元测试证明不了
// 阶段顺序、跨模块的链路关联、以及关闭是不是真的逆序。

// oneShot 干完活就返回的 Runnable，让 Run 不等信号就走完整的关闭流程
type oneShot struct{ work func(context.Context) error }

func (o *oneShot) Start(ctx context.Context) error { return o.work(ctx) }
func (o *oneShot) Stop(context.Context) error      { return nil }

func TestEndToEnd(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "application.yml")
	os.WriteFile(cfg, []byte(`
App:
  Name: xone.demo.app
  Version: v0.1.0
XLog:
  Console: false
  File:
    Enable: true
    Path: `+dir+`
    Name: app.log
XTrace:
  SampleRatio: 1
XMetric:
  Namespace: demo
  ConstLabels:
    env: "${DEMO_ENV:dev}"
  GoMetrics: false
  ProcessMetrics: false
`), 0o644)

	// 框架自己的日志单独收一份，用来断言初始化和关闭的顺序
	var framework strings.Builder
	fwLogger := slog.New(slog.NewTextHandler(&framework, nil))

	err := xtow.Run(&oneShot{work: func(ctx context.Context) error {
		// 业务代码只认识原生的 OpenTelemetry / slog / prometheus
		ctx, span := otel.Tracer("demo").Start(ctx, "干活")
		slog.InfoContext(ctx, "处理中")
		span.End()

		xmetric.CounterInc("jobs", xmetric.T("result", "ok"))

		// 一行埋点都不写，这条错误日志自己会变成 log_errors_total
		slog.ErrorContext(ctx, "出事了")
		return nil
	}}, xtow.WithConfigPath(cfg), xtow.WithLogger(fwLogger))
	if err != nil {
		t.Fatalf("Run 失败：%v", err)
	}

	t.Run("阶段顺序", func(t *testing.T) {
		// 日志必须最先起：否则后面组件的初始化日志没地方写
		// 链路和指标必须早于客户端：否则它们发的 Span 挂不上、打的点收不到
		wantInit := []string{"XLog", "XMetric", "XTrace"}
		if got := extract(framework.String(), "初始化"); !equal(got, wantInit) {
			t.Errorf("初始化顺序=%v want=%v", got, wantInit)
		}
	})

	t.Run("关闭严格逆序", func(t *testing.T) {
		// 日志最后关，所以每个组件的关闭日志都写得出去
		wantStop := []string{"XTrace", "XMetric", "XLog"}
		if got := extract(framework.String(), "关闭"); !equal(got, wantStop) {
			t.Errorf("关闭顺序=%v want=%v", got, wantStop)
		}
	})

	t.Run("日志自动带上链路标识", func(t *testing.T) {
		// xlog 不依赖 OpenTelemetry，这条全靠 xtrace 注入的那个扩展点
		line := findLog(t, dir, "处理中")
		if line["trace_id"] == nil || line["span_id"] == nil {
			t.Errorf("业务日志应带上 trace_id / span_id，got=%v", line)
		}
	})

	t.Run("指标带上前缀和常量标签", func(t *testing.T) {
		out := scrape(t)
		if !strings.Contains(out, `demo_jobs{env="dev",result="ok"} 1`) {
			t.Errorf("前缀、常量标签、环境变量默认值都该生效\n实际=\n%s", out)
		}
	})

	t.Run("错误日志自动计数", func(t *testing.T) {
		// 这条跨了三个模块：xlog 的观察者 → xmetric 的计数器 → /metrics
		out := scrape(t)
		if !strings.Contains(out, `demo_log_errors_total{`) {
			t.Fatalf("应有 log_errors_total（业务一行埋点都没写）\n实际=\n%s", out)
		}
		if !strings.Contains(out, `level="ERROR"} 1`) {
			t.Errorf("应数到那一条 Error\n实际=\n%s", out)
		}
		// 链路标识做 exemplar，面板上能从这个点跳回链路
		if !strings.Contains(out, "env=\"dev\"") {
			t.Errorf("常量标签也该带上\n实际=\n%s", out)
		}
	})
}

// extract 按前后顺序取出框架日志里某个动作涉及的组件名
func extract(log, action string) []string {
	var out []string
	for _, line := range strings.Split(log, "\n") {
		if !strings.Contains(line, "msg="+action) {
			continue
		}
		if _, after, ok := strings.Cut(line, "组件="); ok {
			out = append(out, strings.Fields(after)[0])
		}
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// findLog 在日志文件里找到指定 msg 的那一行
func findLog(t *testing.T, dir, msg string) map[string]any {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "app.log.*"))
	if len(files) == 0 {
		t.Fatal("没写出日志文件")
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		m := map[string]any{}
		if json.Unmarshal([]byte(line), &m) == nil && m["msg"] == msg {
			return m
		}
	}
	t.Fatalf("日志里没有 msg=%q，内容=\n%s", msg, b)
	return nil
}

// scrape 抓一次 /metrics
func scrape(t *testing.T) string {
	t.Helper()
	w := newRecorder()
	xmetric.Handler().ServeHTTP(w, newRequest())
	return w.body()
}

func newRecorder() *recorder { return &recorder{} }

type recorder struct {
	http.ResponseWriter
	buf    strings.Builder
	header http.Header
}

func (r *recorder) Header() http.Header {
	if r.header == nil {
		r.header = http.Header{}
	}
	return r.header
}
func (r *recorder) Write(p []byte) (int, error) { return r.buf.Write(p) }
func (r *recorder) WriteHeader(int)             {}
func (r *recorder) body() string                { return r.buf.String() }

func newRequest() *http.Request {
	req, _ := http.NewRequest("GET", "/metrics", nil)
	return req
}
