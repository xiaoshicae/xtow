package xtrace

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/xiaoshicae/xtow/internal/config"
	"github.com/xiaoshicae/xtow/registry"
	"github.com/xiaoshicae/xtow/xapp"
	"github.com/xiaoshicae/xtow/xlog"
)

// recorder 收集 Span 的 SpanProcessor，用来断言「Span 真的到了处理器手里」
type recorder struct {
	mu    sync.Mutex
	spans []string
	shut  bool
}

func (r *recorder) OnStart(context.Context, sdktrace.ReadWriteSpan) {}
func (r *recorder) OnEnd(s sdktrace.ReadOnlySpan) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spans = append(r.spans, s.Name())
}
func (r *recorder) Shutdown(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.shut = true
	return nil
}
func (r *recorder) ForceFlush(context.Context) error { return nil }
func (r *recorder) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.spans...)
}
func (r *recorder) isShut() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.shut
}

func TestNew_默认配置产出真provider(t *testing.T) {
	rec := &recorder{}
	tr, closer, err := New(DefaultConfig(), rec)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tr.TracerProvider.(*sdktrace.TracerProvider); !ok {
		t.Fatalf("开启时应是 SDK 实现，got=%T", tr.TracerProvider)
	}

	_, span := tr.TracerProvider.Tracer("t").Start(context.Background(), "干活")
	if !span.SpanContext().IsValid() {
		t.Error("默认全采样，SpanContext 应有效")
	}
	span.End()

	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	if got := rec.names(); len(got) != 1 || got[0] != "干活" {
		t.Errorf("Span 应送到注册的处理器，got=%v", got)
	}
	if !rec.isShut() {
		t.Error("关闭 provider 应一并关掉挂在它上面的处理器")
	}
}

func TestNew_关闭链路时是noop(t *testing.T) {
	c := DefaultConfig()
	c.Enable = false
	rec := &recorder{}
	tr, closer, err := New(c, rec)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	if _, ok := tr.TracerProvider.(*sdktrace.TracerProvider); ok {
		t.Error("关闭时不该建出 SDK provider")
	}
	_, span := tr.TracerProvider.Tracer("t").Start(context.Background(), "干活")
	span.End()
	if len(rec.names()) != 0 {
		t.Errorf("关闭时处理器不该收到 Span，got=%v", rec.names())
	}
}

func TestNew_关闭链路仍保留Header透传(t *testing.T) {
	// X-Request-Id 该不该带给下游，跟要不要采样 Span 是两个问题
	c := DefaultConfig()
	c.Enable = false
	c.ForwardHeaders = []string{"X-Request-Id"}
	tr, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	out := http.Header{}
	ctx := tr.Propagator.Extract(context.Background(), headerCarrier(http.Header{"X-Request-Id": {"r1"}}))
	tr.Propagator.Inject(ctx, headerCarrier(out))
	if out.Get("X-Request-Id") != "r1" {
		t.Errorf("链路关了，透传还得在，got=%v", out)
	}
	if out.Get("traceparent") != "" {
		t.Errorf("链路关了就不该注入 traceparent，got=%v", out)
	}
}

func TestNew_开启时装W3C与B3(t *testing.T) {
	tr, closer, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	ctx, span := tr.TracerProvider.Tracer("t").Start(context.Background(), "s")
	defer span.End()
	out := http.Header{}
	tr.Propagator.Inject(ctx, headerCarrier(out))

	for _, h := range []string{"Traceparent", "B3"} {
		if out.Get(h) == "" {
			t.Errorf("应注入 %s，got=%v", h, out)
		}
	}
}

func TestNew_矛盾的透传配置直接失败(t *testing.T) {
	c := DefaultConfig()
	c.ForwardHeaders = []string{"X-Internal-Token"}
	c.ForwardHeaderRules = []ForwardHeaderRule{{Domains: []string{"*.internal.com"}, Headers: []string{"X-Internal-Token"}}}

	if _, _, err := New(c); err == nil {
		t.Fatal("矛盾的配置应当在启动时就失败，而不是猜一个语义跑下去")
	}
}

func TestNew_停止预算为零直接失败(t *testing.T) {
	// 0 不是「不限时」而是「一点都不等」：Shutdown 拿到一个已经过期的 context，
	// 缓冲区里还没发出去的 Span 直接丢掉，而配置文件看上去只是没设上限
	c := DefaultConfig()
	c.ShutdownTimeout = 0
	if _, _, err := New(c); err == nil {
		t.Fatal("ShutdownTimeout=0 应当报错")
	}
}

func TestNew_采样率为零仍然生成并透传TraceID(t *testing.T) {
	// 「不采样」和「关掉链路」是两件事：写 0 时 Span 照常创建、TraceID 照常
	// 生成，只是不落地——下游拿得到 TraceID，本地不存 Span。
	// 要连 Span 都不产生请用 Enable: false
	c := DefaultConfig()
	c.SampleRatio = 0
	tr, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	_, span := tr.TracerProvider.Tracer("t").Start(context.Background(), "s")
	defer span.End()
	sc := span.SpanContext()
	if !sc.IsValid() {
		t.Fatal("采样率为 0 时 SpanContext 仍应有效，否则 TraceID 传不下去")
	}
	if sc.IsSampled() {
		t.Error("采样率为 0 时不该被采样")
	}
}

func TestSamplerOf(t *testing.T) {
	if got := samplerOf(1).Description(); got != sdktrace.AlwaysSample().Description() {
		t.Errorf("比例 >=1 应全采样，got=%s", got)
	}
	if got := samplerOf(1.5).Description(); got != sdktrace.AlwaysSample().Description() {
		t.Errorf("比例超过 1 也是全采样，got=%s", got)
	}
	if got := samplerOf(0.1).Description(); got == sdktrace.AlwaysSample().Description() {
		t.Errorf("比例小于 1 应按比例采样，got=%s", got)
	}
}

func TestNew_采样率生效(t *testing.T) {
	c := DefaultConfig()
	c.SampleRatio = 0.5
	tr, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	// 采样一半时，一千次里既该有采到的也该有没采到的
	var sampled, dropped int
	for i := 0; i < 1000; i++ {
		_, span := tr.TracerProvider.Tracer("t").Start(context.Background(), "s")
		if span.SpanContext().IsSampled() {
			sampled++
		} else {
			dropped++
		}
		span.End()
	}
	if sampled == 0 || dropped == 0 {
		t.Errorf("采样率 0.5 应当有采有丢，sampled=%d dropped=%d", sampled, dropped)
	}
}

func TestClose_超时不挂死(t *testing.T) {
	// 导出端不可达时 Shutdown 会一直阻塞，没有 deadline 就是退出时挂死
	c := DefaultConfig()
	c.ShutdownTimeout = 50 * time.Millisecond
	_, closer, err := New(c, blockingProcessor{})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- closer.Close() }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("导出端卡住时应返回超时错误，而不是假装关干净了")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close 没有在超时后返回")
	}
}

// blockingProcessor 模拟一个连不上导出端、Shutdown 一直等的处理器
type blockingProcessor struct{}

func (blockingProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}
func (blockingProcessor) OnEnd(sdktrace.ReadOnlySpan)                     {}
func (blockingProcessor) ForceFlush(context.Context) error                { return nil }
func (blockingProcessor) Shutdown(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestInstall_日志拿得到TraceID(t *testing.T) {
	// xlog 不依赖 OpenTelemetry，日志里的 trace_id 全靠本包注入这个提取器
	t.Cleanup(func() { xlog.SetTraceExtractor(nil) })

	tr, closer, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	tr.Install()

	ctx, span := otel.Tracer("t").Start(context.Background(), "s")
	defer span.End()

	dir := t.TempDir()
	lc := xlog.DefaultConfig()
	lc.Console = false
	lc.File = xlog.FileConfig{Enable: true, Path: dir, Name: "app.log", RotateTime: time.Hour, Perm: "0644"}
	l, lcloser, err := xlog.New(lc)
	if err != nil {
		t.Fatal(err)
	}
	l.InfoContext(ctx, "带链路的日志")
	lcloser.Close()

	if got := readLog(t, dir); got[traceIDKey] != span.SpanContext().TraceID().String() {
		t.Errorf("日志里的 trace_id 应等于当前 Span 的，got=%v", got)
	}
}

func TestTransport_把目标Host写进ctx(t *testing.T) {
	// 没有它，ForwardHeaderRules 里的 header 一条都不会被注入
	var seen string
	tr := &Transport{Next: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		seen = TargetHostFromContext(r.Context())
		return &http.Response{StatusCode: 204, Body: io.NopCloser(nilReader{})}, nil
	})}

	req, _ := http.NewRequest("GET", "https://api.example.com:8443/x", nil)
	before := req.Context()
	if _, err := tr.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if seen != "api.example.com:8443" {
		t.Errorf("应写入带端口的 host，got=%q", seen)
	}
	if TargetHostFromContext(before) != "" {
		t.Error("按 RoundTripper 约定，不能改动入参请求")
	}
}

func TestTransport_Next为空时用默认(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL, nil)
	resp, err := (&Transport{}).RoundTrip(req)
	if err != nil {
		t.Fatalf("Next 为空时应回落到 http.DefaultTransport，got=%v", err)
	}
	resp.Body.Close()
}

func TestAddSpanProcessor(t *testing.T) {
	t.Run("nil 直接 panic", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("传 nil 应当 panic，不留到运行时才空指针")
			}
		}()
		AddSpanProcessor(nil)
	})

	t.Run("初始化前注册的会被挂上", func(t *testing.T) {
		resetRegistration(t)
		rec := &recorder{}
		AddSpanProcessor(rec)

		closer := initComponent(t)
		otel.Tracer("t").Start(context.Background(), "先注册")
		_, span := otel.Tracer("t").Start(context.Background(), "先注册")
		span.End()
		closer.Close()

		if len(rec.names()) == 0 {
			t.Error("初始化前注册的处理器应在初始化时挂上")
		}
	})

	t.Run("初始化后注册的立即生效", func(t *testing.T) {
		resetRegistration(t)
		closer := initComponent(t)

		rec := &recorder{}
		AddSpanProcessor(rec)
		_, span := otel.Tracer("t").Start(context.Background(), "后注册")
		span.End()
		closer.Close()

		if got := rec.names(); len(got) != 1 || got[0] != "后注册" {
			t.Errorf("初始化后注册的应立即收到 Span，got=%v", got)
		}
	})

	t.Run("关闭之后注册的进待办，不挂到已关闭的实例上", func(t *testing.T) {
		resetRegistration(t)
		initComponent(t).Close()

		rec := &recorder{}
		AddSpanProcessor(rec)

		mu.Lock()
		n, l := len(pending), live
		mu.Unlock()
		if l != nil {
			t.Error("关闭后应清掉 provider")
		}
		if n != 1 {
			t.Errorf("关闭后注册的应进待办队列，got=%d", n)
		}
	})
}

func TestAddSpanProcessor_与初始化并发也不会被吞(t *testing.T) {
	// 注册分两支：初始化前进 pending 等着被取走，初始化后直接挂到 live 上。
	// 如果取 pending 和装 live 之间放开了锁，落在那个窗口里的注册两边都不占——
	// 它进了一个再也不会被读的 pending，然后被静默丢掉。
	// 这是一次完全无声的失败：Span 照常产生，只是永远到不了上报端。
	for i := 0; i < 50; i++ {
		resetRegistration(t)

		rec := &recorder{}
		var closer io.Closer
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); closer = initComponent(t) }()
		go func() { defer wg.Done(); AddSpanProcessor(rec) }()
		wg.Wait()

		// 无论两者谁先，注册完成之后处理器都该是挂上了的：
		// 要么被 Init 从 pending 里取走，要么直接挂到了 live 上
		mu.Lock()
		leftover := len(pending)
		mu.Unlock()
		if leftover > 0 {
			closer.Close()
			t.Fatalf("第 %d 轮：处理器落在窗口里没人认领，pending 还剩 %d 个", i, leftover)
		}

		_, span := otel.Tracer("t").Start(context.Background(), "s")
		span.End()
		got := len(rec.names())
		closer.Close()
		if got == 0 {
			t.Fatalf("第 %d 轮：处理器被吞了，一个 Span 都没收到", i)
		}
	}
}

func TestRegister_登记内容与框架对得上(t *testing.T) {
	var got *registry.Component
	for _, c := range registry.Snapshot() {
		if c.Key == ConfigKey {
			got = &c
			break
		}
	}
	if got == nil {
		t.Fatalf("没有以 %s 登记", ConfigKey)
	}
	if got.Stage != registry.StageTelemetry {
		t.Errorf("链路要早于各类客户端就绪，否则它们的 Span 挂不上，got=%v", got.Stage)
	}
	if got.Config != &cfg {
		t.Error("登记的必须是包级配置变量本身，否则框架解出来的配置写不回来")
	}
}

func TestInit_读到应用名做ServiceName(t *testing.T) {
	// 服务名放在共用的 App 块里，链路和指标读同一份，不会各配一遍再对不上。
	// 这里走完整路径：配置文件 → config.Load → 组件 Init → OTel resource
	resetRegistration(t)
	loadConfig(t, "App:\n  Name: xone.demo.app\n  Version: v1.2.0\nXTrace:\n  SampleRatio: 1\n")

	if xapp.Name() != "xone.demo.app" {
		t.Fatalf("配置没进到 xapp，got=%q", xapp.Name())
	}

	exp := tracetest.NewInMemoryExporter()
	AddSpanProcessor(sdktrace.NewSimpleSpanProcessor(exp))

	closer := initComponent(t)
	_, span := otel.Tracer("t").Start(context.Background(), "s")
	span.End()
	// 先取再关：InMemoryExporter 的 Shutdown 会清空已收集的 Span
	spans := exp.GetSpans()
	closer.Close()

	if len(spans) == 0 {
		t.Fatal("没收到 Span")
	}
	attrs := map[string]string{}
	for _, kv := range spans[0].Resource.Attributes() {
		attrs[string(kv.Key)] = kv.Value.AsString()
	}
	if attrs["service.name"] != "xone.demo.app" {
		t.Errorf("service.name 应取自 App.Name，got=%q", attrs["service.name"])
	}
	if attrs["service.version"] != "v1.2.0" {
		t.Errorf("service.version 应取自 App.Version，got=%q", attrs["service.version"])
	}
}

// ---- 助手 ----

// resetRegistration 把包级登记状态清干净，让每个子测试从同一起点开始
func resetRegistration(t *testing.T) {
	t.Helper()
	reset := func() {
		mu.Lock()
		pending, live = nil, nil
		mu.Unlock()
	}
	reset()
	t.Cleanup(reset)
	t.Cleanup(func() { cfg = DefaultConfig() })
}

// initComponent 走一遍框架真正会走的路径：取出登记的组件并执行它的 Init
func initComponent(t *testing.T) io.Closer {
	t.Helper()
	for _, c := range registry.Snapshot() {
		if c.Key != ConfigKey {
			continue
		}
		closer, err := c.Init()
		if err != nil {
			t.Fatalf("初始化失败：%v", err)
		}
		return closer
	}
	t.Fatalf("没有以 %s 登记", ConfigKey)
	return nil
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type nilReader struct{}

func (nilReader) Read([]byte) (int, error) { return 0, errors.New("EOF") }

var _ oteltrace.TracerProvider = (*sdktrace.TracerProvider)(nil)

// headerCarrier 让测试少写一个 import
func headerCarrier(h http.Header) propagation.HeaderCarrier { return propagation.HeaderCarrier(h) }

// traceIDKey 日志里链路字段的名字，由 xlog 的 handler 决定
const traceIDKey = "trace_id"

// readLog 读出目录下唯一一条日志，解析成 map
func readLog(t *testing.T, dir string) map[string]any {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "app.log.*"))
	if err != nil || len(files) == 0 {
		t.Fatalf("没找到日志文件：%v", err)
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(b))
	out := map[string]any{}
	if err := json.Unmarshal([]byte(line), &out); err != nil {
		t.Fatalf("日志不是 JSON：%v，内容=%q", err, line)
	}
	return out
}

// loadConfig 把一段 YAML 走真实的加载路径灌进各组件，测试结束后还原。
//
// 组件的配置是包级变量，测试之间会互相污染，所以先存一份再改。
func loadConfig(t *testing.T, yml string) {
	t.Helper()
	list := registry.Snapshot()

	saved := make([]reflect.Value, len(list))
	for i, c := range list {
		cur := reflect.ValueOf(c.Config).Elem()
		saved[i] = reflect.New(cur.Type()).Elem()
		saved[i].Set(cur)
	}
	t.Cleanup(func() {
		for i, c := range list {
			reflect.ValueOf(c.Config).Elem().Set(saved[i])
		}
	})

	path := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(path, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := config.Load(path, list); err != nil {
		t.Fatalf("加载配置失败：%v", err)
	}
}
