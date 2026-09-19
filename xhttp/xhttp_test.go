package xhttp

import (
	"context"
	"errors"
	"fmt"
	"github.com/prometheus/client_golang/prometheus"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-resty/resty/v2"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/xiaoshicae/xtow/registry"
	"github.com/xiaoshicae/xtow/xmetric"
	"github.com/xiaoshicae/xtow/xtrace"
)

// TestMain 关掉 resty 的内部日志：连不上的用例本来就会刷一屏重试告警
func TestMain(m *testing.M) {
	silent = true
	os.Exit(m.Run())
}

// silent 让测试里新建的 client 闭嘴
var silent bool

// echo 起一个记下收到的请求的服务端
func echo(t *testing.T, h http.HandlerFunc) (*httptest.Server, *atomic.Pointer[http.Header]) {
	t.Helper()
	var last atomic.Pointer[http.Header]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := r.Header.Clone()
		last.Store(&hdr)
		if h != nil {
			h(w, r)
			return
		}
		w.WriteHeader(204)
	}))
	t.Cleanup(srv.Close)
	return srv, &last
}

func TestNew_拿到的是可用的resty(t *testing.T) {
	srv, _ := echo(t, nil)
	client, _ := newQuiet(t, DefaultConfig())

	resp, err := client.R().SetContext(context.Background()).Get(srv.URL)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	if resp.StatusCode() != 204 {
		t.Errorf("状态码不对，got=%d", resp.StatusCode())
	}
}

func TestNew_连接池参数传下去了(t *testing.T) {
	c := DefaultConfig()
	c.MaxIdleConnsPerHost, c.MaxIdleConns, c.IdleConnTimeout = 33, 77, 11*time.Second
	c.Trace = false // otelhttp.Transport 不导出内层，要看底层就别包它
	client, _ := newQuiet(t, c)

	// 标准库默认每主机只留 2 条空闲连接，对只调几个下游的服务太小
	tr := unwrapTransport(t, client)
	if tr.MaxIdleConnsPerHost != 33 || tr.MaxIdleConns != 77 || tr.IdleConnTimeout != 11*time.Second {
		t.Errorf("连接池参数没传下去，got=%+v", tr)
	}
	if tr.DialContext == nil {
		t.Error("应当装上带超时的 Dialer")
	}
	// 从 DefaultTransport 克隆而不是新建，代理和 TLS 这些默认设置才不会丢
	if tr.Proxy == nil {
		t.Error("应保留 DefaultTransport 的代理设置")
	}
}

// unwrapTransport 剥掉链路那几层包装，拿到底层的 *http.Transport
func unwrapTransport(t *testing.T, client *resty.Client) *http.Transport {
	t.Helper()
	rt := client.GetClient().Transport
	for i := 0; i < 5; i++ {
		switch v := rt.(type) {
		case *http.Transport:
			return v
		case *xtrace.Transport:
			rt = v.Next
		default:
			t.Fatalf("剥不开的 Transport 类型：%T（otelhttp 不导出内层，测这层时把 Trace 关掉）", rt)
		}
	}
	t.Fatal("包装层数太多")
	return nil
}

func TestNew_不开链路时是干净的Transport(t *testing.T) {
	c := DefaultConfig()
	c.Trace = false
	client, _ := newQuiet(t, c)

	if _, ok := client.GetClient().Transport.(*http.Transport); !ok {
		t.Errorf("关掉链路后不该有包装层，got=%T", client.GetClient().Transport)
	}
}

func TestTransport_otelhttp在noop下仍然注入透传Header(t *testing.T) {
	// 单 Transport 的简化依赖这个前提：otelhttp 无论 TracerProvider 是不是 noop，
	// 都会调用全局 Propagator 注入。前提不成立就得补回「链路关了自己注入」那一层。
	// 钉在这里而不是 xtrace：那边为此引 otelhttp 会让每个 xtrace 使用者
	// 的模块图凭空多四个模块。
	old := otel.GetTracerProvider()
	oldProp := otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTracerProvider(old); otel.SetTextMapPropagator(oldProp) })
	otel.SetTracerProvider(noop.NewTracerProvider())

	tc := xtrace.DefaultConfig()
	tc.Enable = false // 链路关掉
	tc.ForwardHeaders = []string{"X-Request-Id"}
	tr, tcloser, err := xtrace.New(context.Background(), tc)
	if err != nil {
		t.Fatal(err)
	}
	defer tcloser.Close()
	tr.Install()

	srv, last := echo(t, nil)
	client, _ := newQuiet(t, DefaultConfig())

	ctx := tr.Propagator.Extract(context.Background(),
		propagation.HeaderCarrier(http.Header{"X-Request-Id": {"req-1"}}))
	if _, err := client.R().SetContext(ctx).Get(srv.URL); err != nil {
		t.Fatal(err)
	}

	got := *last.Load()
	if got.Get("X-Request-Id") != "req-1" {
		t.Fatalf("链路关着也该透传 header，实际收到的=%v", got)
	}
	if got.Get("traceparent") != "" {
		t.Errorf("链路关着就不该注入 traceparent，got=%q", got.Get("traceparent"))
	}
}

func TestTransport_开链路时注入traceparent并开Span(t *testing.T) {
	old := otel.GetTracerProvider()
	oldProp := otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTracerProvider(old); otel.SetTextMapPropagator(oldProp) })

	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exp)))
	defer tp.Shutdown(context.Background())
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	srv, last := echo(t, nil)
	client, _ := newQuiet(t, DefaultConfig())

	if _, err := client.R().SetContext(context.Background()).Get(srv.URL + "/api/orders"); err != nil {
		t.Fatal(err)
	}

	if got := (*last.Load()).Get("traceparent"); got == "" {
		t.Error("应当注入 traceparent")
	}
	spans := exp.GetSpans()
	if len(spans) == 0 {
		t.Fatal("应当产出 Span")
	}
	if name := spans[0].Name; name != "GET /api/orders" {
		t.Errorf("Span 名应为「方法 路径」，got=%q", name)
	}
}

func TestTransport_目标host写进了ctx(t *testing.T) {
	// 没有它，按域名透传的规则一条都不会命中
	var seen string
	tr := &xtrace.Transport{Next: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		seen = xtrace.TargetHostFromContext(r.Context())
		return &http.Response{StatusCode: 204, Body: http.NoBody}, nil
	})}
	req, _ := http.NewRequest("GET", "https://api.example.com/x", nil)
	if _, err := tr.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if seen != "api.example.com" {
		t.Errorf("应写入目标 host，got=%q", seen)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// ---- 重试 ----

func TestRetry_只重试幂等方法(t *testing.T) {
	// 超时分不出「请求没到」和「处理完了但响应丢了」，
	// 重发一个 POST 就可能变成重复下单
	if !retryOnlyIdempotent(respFor("GET"), errors.New("超时")) {
		t.Error("GET 应当允许重试")
	}
	for _, m := range []string{"POST", "PATCH"} {
		if retryOnlyIdempotent(respFor(m), errors.New("超时")) {
			t.Errorf("%s 不该重试", m)
		}
	}
	if retryOnlyIdempotent(respFor("GET"), nil) {
		t.Error("拿到响应就不该重试，与 resty 默认条件一致")
	}
	if retryOnlyIdempotent(nil, errors.New("超时")) {
		t.Error("认不出方法时应当保守地不重试")
	}
}

func respFor(method string) *resty.Response {
	return &resty.Response{Request: &resty.Request{Method: method}}
}

func TestRetry_真的会重发(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		// 直接断开连接，制造传输层错误
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer srv.Close()

	c := DefaultConfig()
	c.RetryCount, c.RetryWaitTime, c.RetryMaxWaitTime = 2, time.Millisecond, 5*time.Millisecond
	client, _ := newQuiet(t, c)

	client.R().SetContext(context.Background()).Get(srv.URL)
	if got := hits.Load(); got != 3 {
		t.Errorf("重试 2 次应当一共请求 3 次，got=%d", got)
	}

	hits.Store(0)
	client.R().SetContext(context.Background()).Post(srv.URL)
	if got := hits.Load(); got != 1 {
		t.Errorf("POST 不该重试，应当只请求 1 次，got=%d", got)
	}
}

func TestRetry_关掉幂等限制后POST也重试(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer srv.Close()

	c := DefaultConfig()
	c.RetryCount, c.RetryWaitTime, c.RetryMaxWaitTime = 2, time.Millisecond, 5*time.Millisecond
	c.RetryOnlyIdempotent = false
	client, _ := newQuiet(t, c)

	client.R().SetContext(context.Background()).Post(srv.URL)
	if got := hits.Load(); got != 3 {
		t.Errorf("关掉限制后 POST 也该重试，got=%d", got)
	}
}

// ---- 全局实例 ----

func TestC_初始化前有带超时的兜底实例(t *testing.T) {
	// 零值超时是「永不超时」：关闭阶段发一个这样的请求，整个退出流程就卡住了
	mu.Lock()
	old := current
	current = fallbackClient()
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		current = old
		mu.Unlock()
	})

	if C() == nil {
		t.Fatal("任何时候都该返回可用实例")
	}
	if got := C().GetClient().Timeout; got != fallbackTimeout {
		t.Errorf("兜底实例必须带超时，got=%v", got)
	}
	if got := RawClient().Timeout; got != fallbackTimeout {
		t.Errorf("兜底的原生 client 也必须带超时，got=%v", got)
	}
}

func TestR_绑定ctx(t *testing.T) {
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "v")
	if got := R(ctx).Context().Value(key{}); got != "v" {
		t.Errorf("R 应当绑定传进来的 ctx，got=%v", got)
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
	if got.Stage != registry.StageClient {
		t.Errorf("出站客户端要在服务对外之前就绪，got=%v", got.Stage)
	}
	if got.Config != &cfg {
		t.Error("登记的必须是包级配置变量本身")
	}
}

func TestInit_没配也能用(t *testing.T) {
	// 与 xgorm / xredis 不同：HTTP 客户端不连任何外部资源，没配也该给一个能用的
	withMetrics(t)
	mu.Lock()
	old := current
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		current = old
		mu.Unlock()
	})

	closer, err := initClient(context.Background())
	if err != nil {
		t.Fatalf("没配不该报错：%v", err)
	}
	if C().GetClient().Timeout != DefaultConfig().Timeout {
		t.Errorf("应当用默认超时，got=%v", C().GetClient().Timeout)
	}
	if RawClient() == nil {
		t.Error("原生 client 也该可用")
	}

	if err := closer.Close(); err != nil {
		t.Errorf("关闭不该报错：%v", err)
	}
	// 关闭后换回兜底实例而不是置空：关闭阶段仍可能有组件发请求
	if got := C().GetClient().Timeout; got != fallbackTimeout {
		t.Errorf("关闭后应换回兜底实例，got=%v", got)
	}
}

func TestValidate(t *testing.T) {
	if err := DefaultConfig().validate(); err != nil {
		t.Errorf("默认配置应当合法：%v", err)
	}
	bad := DefaultConfig()
	bad.RetryCount = -1
	if err := bad.validate(); err == nil {
		t.Error("重试次数为负应当报错")
	}
	bad = DefaultConfig()
	bad.MaxIdleConns = -1
	if err := bad.validate(); err == nil {
		t.Error("连接数为负应当报错")
	}
}

func TestSpanName_不带query(t *testing.T) {
	// query 里常有 id 和令牌，放进 Span 名会撑爆基数，也会把敏感值带出去
	req, _ := http.NewRequest("GET", "https://h/api/orders?token=hunter2&id=1", nil)
	if got := spanName("", req); got != "GET /api/orders" {
		t.Errorf("Span 名不该带 query，got=%q", got)
	}
	if strings.Contains(spanName("", req), "hunter2") {
		t.Error("Span 名里出现了令牌")
	}
}

// newQuiet 建一个不打日志的 client，同时给它装一套独立的指标设施。
//
// 每个用例都换一套 registry：指标是进程级的全局状态，一个用例注册出的冲突
// 会留在那里，让后面所有用例的 New 都失败——上一版就是这么串味的。
func newQuiet(t *testing.T, c Config) (*resty.Client, *xmetric.Metrics) {
	t.Helper()
	m := withMetrics(t)
	client, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	if silent {
		client.SetLogger(discardLogger{})
	}
	t.Cleanup(func() { closer.Close() })
	return client, m
}

type discardLogger struct{}

func (discardLogger) Errorf(string, ...any) {}
func (discardLogger) Warnf(string, ...any)  {}
func (discardLogger) Debugf(string, ...any) {}

func TestNew_关闭时真的清掉空闲连接(t *testing.T) {
	// 回归用例。上一版让 Closer 去调 http.Client.CloseIdleConnections()，
	// 那个方法靠类型断言往下找；链路开着时中间隔着 otelhttp.Transport，
	// 而它没实现这个方法，断言到那里就断了——整条调用是空操作，
	// 而链路默认就是开着的。只断言「包装层实现了这个方法」测不出来，
	// 得看连接有没有真的被释放
	for _, trace := range []bool{false, true} {
		t.Run(fmt.Sprintf("Trace=%v", trace), func(t *testing.T) {
			var idle atomic.Int64
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(200)
			}))
			// 必须在 Start 之前设：起来之后再改，服务端协程已经在读它了
			srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
				switch s {
				case http.StateIdle:
					idle.Add(1)
				case http.StateClosed, http.StateHijacked:
					idle.Add(-1)
				}
			}
			srv.Start()
			defer srv.Close()

			c := DefaultConfig()
			c.Trace = trace
			client, closer, err := New(c)
			if err != nil {
				t.Fatal(err)
			}

			resp, err := client.R().Get(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp

			waitFor(t, func() bool { return idle.Load() > 0 }, "请求完成后该有一个空闲连接")
			if err := closer.Close(); err != nil {
				t.Fatal(err)
			}
			waitFor(t, func() bool { return idle.Load() == 0 }, "关闭之后空闲连接该被释放")
		})
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(what)
}

func TestMetric_重试耗时算整次逻辑请求(t *testing.T) {
	// resty 每次尝试都会重置 Request.Time，resp.Time() 只是最后一次尝试的耗时。
	// 计数是按「一次逻辑请求」记的，耗时也必须是——否则故障时请求数照涨、
	// 耗时却纹丝不动，监控看上去异常地健康
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError) // 第一次失败，触发重试
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	const backoff = 150 * time.Millisecond
	c := DefaultConfig()
	c.Trace, c.Metric = false, false
	c.RetryCount, c.RetryWaitTime, c.RetryMaxWaitTime = 2, backoff, backoff
	client, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	hist := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "probe_duration_seconds", Buckets: []float64{0.001, 10}},
		[]string{"method", "host", "status"})
	installMetrics(client, hist)
	client.AddRetryCondition(func(r *resty.Response, _ error) bool { return r.StatusCode() >= 500 })

	if _, err := client.R().Get(srv.URL); err != nil {
		t.Fatal(err)
	}
	if hits.Load() < 2 {
		t.Fatalf("应当重试过，服务端只收到 %d 次", hits.Load())
	}

	got := histogramSum(t, hist)
	if got < backoff.Seconds() {
		t.Errorf("耗时该覆盖整次逻辑请求（含退避 %v），got=%.3fs", backoff, got)
	}
}

func histogramSum(t *testing.T, h *prometheus.HistogramVec) float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	if err := reg.Register(h); err != nil {
		t.Fatal(err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			if m.GetHistogram() != nil {
				return m.GetHistogram().GetSampleSum()
			}
		}
	}
	t.Fatal("没采到直方图样本")
	return 0
}

func TestNew_ctx能给整个逻辑请求封顶(t *testing.T) {
	// Timeout 管的是一次尝试。开了 RetryCount 之后，最坏情况是
	// (RetryCount+1) × Timeout 再加退避——配 300ms 实际能跑到 1.2s。
	// 唯一能给整次逻辑请求封顶的是调用方的 ctx，这里把它钉住
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	c := DefaultConfig()
	c.Timeout = 300 * time.Millisecond
	c.RetryCount = 3
	c.RetryWaitTime, c.RetryMaxWaitTime = 10*time.Millisecond, 20*time.Millisecond
	c.Trace, c.Metric = false, false
	cli, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := cli.R().SetContext(ctx).Get(srv.URL); err == nil {
		t.Fatal("该超时的")
	}
	if elapsed := time.Since(start); elapsed > 900*time.Millisecond {
		t.Errorf("ctx 给了 400ms 的预算，重试不该把它撑到 %v", elapsed.Round(10*time.Millisecond))
	}
}
