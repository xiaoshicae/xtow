package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/xiaoshicae/xtow"
	"github.com/xiaoshicae/xtow/registry"
	"github.com/xiaoshicae/xtow/xgin"
	"github.com/xiaoshicae/xtow/xmetric"
)

// 这是唯一一处把各模块放在一起跑的测试：单元测试证明不了
// 阶段顺序、跨模块的链路关联、以及关闭是不是真的逆序。

// oneShot 干完活就返回的 Runnable，让 Run 不等信号就走完整的关闭流程
type oneShot struct{ work func(context.Context) error }

func (o *oneShot) Start(ctx context.Context) error { return o.work(ctx) }
func (o *oneShot) Stop(context.Context) error      { return nil }

// serveOnce 起真正的 xgin 服务，打一次请求，然后停掉。
//
// 与 oneShot 那条路径互补：那条验证阶段顺序和跨模块关联，
// 这条验证「main 里那几行真的能起一个服务」。
type serveOnce struct {
	g    *xgin.XGin
	port int
	hit  func(base string)
}

func (s *serveOnce) Start(ctx context.Context) error {
	go func() {
		base := fmt.Sprintf("http://127.0.0.1:%d", s.port)
		for i := 0; i < 100; i++ {
			if resp, err := http.Get(base + "/metrics"); err == nil {
				resp.Body.Close()
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		s.hit(base)
		s.g.Stop(context.Background())
	}()
	return s.g.Start(ctx)
}

func (s *serveOnce) Stop(ctx context.Context) error { return s.g.Stop(ctx) }

// isolateConfigs 存一份各组件的配置，测试结束后还原。
//
// 组件的配置是包级变量，一个用例灌进去的值会留给下一个用例——
// map 类型的字段尤其麻烦，配置加载是「合并」而不是「替换」，
// 上一个用例设的 ConstLabels 会一直跟着走，让后面的断言莫名其妙地对不上。
func isolateConfigs(t *testing.T) {
	t.Helper()
	list := registry.Snapshot()
	saved := make([]reflect.Value, len(list))
	for i, c := range list {
		if c.Config == nil {
			continue
		}
		cur := reflect.ValueOf(c.Config).Elem()
		saved[i] = reflect.New(cur.Type()).Elem()
		saved[i].Set(cur)
	}
	t.Cleanup(func() {
		for i, c := range list {
			if c.Config == nil || !saved[i].IsValid() {
				continue
			}
			reflect.ValueOf(c.Config).Elem().Set(saved[i])
		}
	})
}

func TestEndToEnd(t *testing.T) {
	isolateConfigs(t)
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
		// 日志必须最先起：否则后面组件的初始化日志没地方写。
		// 链路和指标必须早于客户端：否则客户端发的 Span 挂不上、打的点收不到。
		// 同一档内的相对顺序是登记顺序（即 import 路径字典序），不是框架的承诺
		// 同一档内部不保证顺序，所以这里按档分组比
		wantInit := []string{"XLog", "XMetric", "XTrace", "XCache", "XHttp"}
		if got := extract(framework.String(), "初始化"); !equal(got, wantInit) {
			t.Errorf("初始化顺序=%v want=%v", got, wantInit)
		}
	})

	t.Run("关闭严格逆序", func(t *testing.T) {
		// 日志最后关，所以每个组件的关闭日志都写得出去
		wantStop := []string{"XHttp", "XCache", "XTrace", "XMetric", "XLog"}
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

// TestServe 把 main 里那几行原样跑一遍：起服务、打请求、优雅关闭
func TestServe(t *testing.T) {
	isolateConfigs(t)
	dir := t.TempDir()
	port := freePort(t)
	cfg := filepath.Join(dir, "application.yml")
	os.WriteFile(cfg, []byte(fmt.Sprintf(`
App:
  Name: xone.demo.app
XLog:
  Console: false
  File: {Enable: true, Path: %s, Name: app.log}
XMetric:
  Namespace: demo
  GoMetrics: false
  ProcessMetrics: false
XGin:
  Host: 127.0.0.1
  Port: %d
`, dir, port)), 0o644)

	var body, metrics string
	var traceID string
	srv := &serveOnce{
		g:    xgin.New().WithRoutes(routes),
		port: port,
		hit: func(base string) {
			resp, err := http.Get(base + "/hello")
			if err != nil {
				t.Errorf("请求失败：%v", err)
				return
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			body = string(b)
			traceID = resp.Header.Get("X-Trace-Id")

			// 故意打一个 panic 的接口，确认 recover 中间件兜住了
			if r, err := http.Get(base + "/boom"); err == nil {
				if r.StatusCode != 500 {
					t.Errorf("panic 应被兜成 500，got=%d", r.StatusCode)
				}
				r.Body.Close()
			}

			if r, err := http.Get(base + "/metrics"); err == nil {
				m, _ := io.ReadAll(r.Body)
				r.Body.Close()
				metrics = string(m)
			}
		},
	}

	if err := xtow.Run(srv, xtow.WithConfigPath(cfg), xtow.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))); err != nil {
		t.Fatalf("Run 失败：%v", err)
	}

	t.Run("接口通", func(t *testing.T) {
		if !strings.Contains(body, "hello") {
			t.Errorf("响应不对，got=%q", body)
		}
	})

	t.Run("响应里回带TraceID", func(t *testing.T) {
		// 从一次调用直接跳到链路，靠的就是这个头
		if traceID == "" {
			t.Error("应在响应头里回带 X-Trace-Id")
		}
	})

	t.Run("日志带上链路标识", func(t *testing.T) {
		line := findLog(t, dir, "收到请求")
		if line["trace_id"] != traceID {
			t.Errorf("业务日志的 trace_id 应与响应头一致，日志=%v 响应头=%s", line["trace_id"], traceID)
		}
	})

	t.Run("访问日志自动记下来", func(t *testing.T) {
		// 业务代码一行都没写，访问日志是内置中间件记的
		line := findLog(t, dir, "请求完成")
		if line["路由"] != "/hello" || line["状态"] != float64(200) {
			t.Errorf("访问日志不对，got=%v", line)
		}
	})

	t.Run("请求指标自动采集", func(t *testing.T) {
		if !strings.Contains(metrics, `demo_http_requests_total{method="GET",route="/hello",status="200"} 1`) {
			t.Errorf("应自动采集请求指标\n实际=\n%s", metrics)
		}
		// 路由用模板而不是真实路径，否则时间序列会随 URL 里的 id 无限增长
		if !strings.Contains(metrics, `route="/boom",status="500"`) {
			t.Errorf("panic 的请求也该按 500 计入\n实际=\n%s", metrics)
		}
	})

	t.Run("panic计入错误日志指标", func(t *testing.T) {
		// 跨了三个模块：xgin 的 recover → xlog 的观察者 → xmetric 的计数器
		if !strings.Contains(metrics, "demo_log_errors_total") {
			t.Errorf("panic 应当被计入错误日志指标\n实际=\n%s", metrics)
		}
	})
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
