package xtrace

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/propagators/b3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	oteltrace "go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/xiaoshicae/xtow/registry"
	"github.com/xiaoshicae/xtow/xapp"
	"github.com/xiaoshicae/xtow/xlog"
)

// Tracing 一份配置装配出来的链路设施。
//
// New 只构造、不安装；装进 OpenTelemetry 的全局变量是 Install 的事。
// 分开是为了让测试能拿到一套独立的链路设施，而不必动全局状态。
type Tracing struct {
	// TracerProvider 链路关闭时是 noop 实现，不是 nil
	TracerProvider oteltrace.TracerProvider

	// Propagator 跨进程传递链路标识与透传 Header
	Propagator propagation.TextMapPropagator
}

// Install 把这套设施装成进程级的。
//
// 之后业务代码用原生的 otel.Tracer("...") 开 Span 即可，不需要认识本包。
func (t *Tracing) Install() {
	otel.SetTracerProvider(t.TracerProvider)
	otel.SetTextMapPropagator(t.Propagator)
	// 让日志带上 TraceID。xlog 不依赖 OpenTelemetry，这个能力由本包注入
	xlog.SetTraceExtractor(traceIDsFromContext)
}

// New 按配置构造链路设施，不触碰任何全局变量。
//
// procs 是要挂上去的 SpanProcessor。框架不内置任何上报 exporter——
// OTLP 一个就带进上百个构建依赖，不该由所有使用者承担。
//
// 返回的 io.Closer 永不为 nil，关闭时会把 Span 冲刷出去，
// 等待上限由 cfg.ShutdownTimeout 控制。
func New(cfg Config, procs ...sdktrace.SpanProcessor) (*Tracing, io.Closer, error) {
	prop, err := newPropagator(cfg)
	if err != nil {
		return nil, nil, err
	}

	if !cfg.Enable {
		if len(procs) > 0 {
			slog.Warn("xtrace 已关闭，注册的 SpanProcessor 收不到任何 Span",
				"数量", len(procs), "开关", ConfigKey+".Enable")
		}
		return &Tracing{TracerProvider: noop.NewTracerProvider(), Propagator: prop}, noopCloser{}, nil
	}

	res, err := newResource()
	if err != nil {
		return nil, nil, err
	}

	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithSampler(samplerOf(cfg.SampleRatio)),
		sdktrace.WithResource(res),
	}
	if cfg.Console {
		exp, err := stdouttrace.New(stdouttrace.WithPrettyPrint())
		if err != nil {
			return nil, nil, fmt.Errorf("xtrace: 创建标准输出 exporter 失败: %w", err)
		}
		// Simple 而非 Batch：本地调试要的是立刻看见，不是攒够了再刷
		opts = append(opts, sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exp)))
	}
	for _, sp := range procs {
		opts = append(opts, sdktrace.WithSpanProcessor(sp))
	}

	tp := sdktrace.NewTracerProvider(opts...)
	return &Tracing{TracerProvider: tp, Propagator: prop}, &providerCloser{tp: tp, timeout: cfg.ShutdownTimeout}, nil
}

// newPropagator 组装 Propagator。
//
// 链路关掉时仍然保留 Header 透传：X-Request-Id 该不该带给下游，
// 跟要不要采样 Span 是两个问题，关掉一个不该让另一个静默失效。
func newPropagator(cfg Config) (propagation.TextMapPropagator, error) {
	var list []propagation.TextMapPropagator
	if cfg.Enable {
		list = append(list, propagation.TraceContext{}, propagation.Baggage{}, b3.New())
	}
	if cfg.forwardEnabled() {
		hp, err := NewHeaderPropagator(cfg.ForwardHeaders, cfg.ForwardHeaderRules)
		if err != nil {
			return nil, err
		}
		list = append(list, hp)
	}
	return propagation.NewCompositeTextMapPropagator(list...), nil
}

func newResource() (*resource.Resource, error) {
	res, err := resource.New(context.Background(),
		resource.WithFromEnv(),
		resource.WithProcess(),
		resource.WithTelemetrySDK(),
		resource.WithHost(),
		resource.WithAttributes(
			semconv.ServiceName(xapp.Name()),
			semconv.ServiceVersion(xapp.Version()),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("xtrace: 构造 resource 失败: %w", err)
	}
	return res, nil
}

// samplerOf 按采样率构造 Sampler，>= 1 时全采样
func samplerOf(ratio float64) sdktrace.Sampler {
	if ratio >= 1 {
		return sdktrace.AlwaysSample()
	}
	return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))
}

// traceIDsFromContext 从 ctx 的 Span 里取出链路标识，供 xlog 注入日志
func traceIDsFromContext(ctx context.Context) (traceID, spanID string) {
	sc := oteltrace.SpanFromContext(ctx).SpanContext()
	if !sc.IsValid() {
		return "", ""
	}
	return sc.TraceID().String(), sc.SpanID().String()
}

// ---- 关闭 ----

type noopCloser struct{}

func (noopCloser) Close() error { return nil }

// providerCloser 关闭 TracerProvider，顺带关掉挂在它上面的全部 SpanProcessor
type providerCloser struct {
	tp      *sdktrace.TracerProvider
	timeout time.Duration
}

func (c *providerCloser) Close() error {
	// 必须限时：导出端不可达时 Shutdown 会一直阻塞，没有 deadline 就是退出时挂死
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	detach()
	if err := c.tp.Shutdown(ctx); err != nil {
		return fmt.Errorf("xtrace: 关闭失败: %w", err)
	}
	return nil
}

// ---- Transport ----

// Transport 把目标请求的 Host 写进 context，让按域名透传的规则能生效。
//
// 没有它，ForwardHeaderRules 里的 header 一条都不会被注入——
// Inject 不知道这个请求要发给谁。
//
// 只需要这一层。otelhttp 无论 TracerProvider 是不是 noop，都会调用全局
// Propagator 注入，所以不存在「链路关了得自己注入」的第二种包装——
// 关掉链路时它注入透传 header、不注入 traceparent，正是想要的行为。
// 这个前提由 xhttp 的测试钉住（它本来就依赖 otelhttp，放在那里不额外增加依赖）。
type Transport struct {
	// Next 实际执行请求的 RoundTripper，为 nil 时用 http.DefaultTransport
	Next http.RoundTripper
}

// RoundTrip 实现 http.RoundTripper。
// 按约定不修改入参请求：WithContext 返回的是浅拷贝。
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	next := t.Next
	if next == nil {
		next = http.DefaultTransport
	}
	return next.RoundTrip(req.WithContext(WithTargetHost(req.Context(), req.URL.Host)))
}

// ---- 登记 ----

var cfg = DefaultConfig()

// 已登记的 SpanProcessor，以及初始化之后的 TracerProvider。
//
// 一把锁管两件事：注册与初始化必须互斥，否则并发注册可能落在
// 「已经取走 pending、还没装好 provider」的窗口里，那个处理器就被吞了。
var (
	mu      sync.Mutex
	pending []sdktrace.SpanProcessor
	live    *sdktrace.TracerProvider
)

// AddSpanProcessor 挂一个 SpanProcessor，用于把 Span 上报到远端。
//
// 框架不内置任何 exporter：OTLP 一个就带进上百个构建依赖，
// 不需要上报的服务不该为此付钱。需要的服务自己引入并在此注册：
//
//	exp, err := otlptracegrpc.New(ctx, otlptracegrpc.WithEndpoint(ep), otlptracegrpc.WithInsecure())
//	if err != nil { return err }
//	xtrace.AddSpanProcessor(sdktrace.NewBatchSpanProcessor(exp))
//
// 在 xtow.Run 之前调用即可。初始化之后注册的会立即挂上。
// 关闭时由本包统一 Shutdown，等待上限由 XTrace.ShutdownTimeout 控制。
func AddSpanProcessor(sp sdktrace.SpanProcessor) {
	if sp == nil {
		panic("xtrace: SpanProcessor 不能为 nil")
	}
	mu.Lock()
	defer mu.Unlock()

	if live != nil {
		live.RegisterSpanProcessor(sp)
		return
	}
	pending = append(pending, sp)
}

// detach 关闭后清掉 provider，避免后来的注册挂到已关闭的实例上
func detach() {
	mu.Lock()
	live = nil
	mu.Unlock()
}

// init 只登记，不初始化。真正的初始化由框架在 StageTelemetry 执行。
func init() {
	registry.Register(registry.Component{
		Key:    ConfigKey,
		Stage:  registry.StageTelemetry,
		Config: &cfg,
		Init: func() (io.Closer, error) {
			mu.Lock()
			procs := pending
			pending = nil
			mu.Unlock()

			t, closer, err := New(cfg, procs...)
			if err != nil {
				return nil, err
			}
			t.Install()

			if tp, ok := t.TracerProvider.(*sdktrace.TracerProvider); ok {
				mu.Lock()
				live = tp
				mu.Unlock()
			}
			return closer, nil
		},
	})
}
