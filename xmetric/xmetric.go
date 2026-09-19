package xmetric

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	promcollectors "github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/xiaoshicae/xtow/registry"
)

// Metrics 一份配置装配出来的指标设施。
//
// New 只构造、不安装；装成进程级的是 Install 的事。
// 分开是为了让测试能拿到一套独立的 Registry，而不必动全局状态。
type Metrics struct {
	// Registry 原生的 Prometheus Registry，需要完整控制时直接用它
	Registry *prometheus.Registry

	// Handler /metrics 的 HTTP handler
	Handler http.Handler

	cfg Config

	// logCounter 日志错误计数器，未开启该特性时为 nil
	logCounter *prometheus.CounterVec

	// collectors 快捷方法（CounterInc / GaugeSet / …）建过的 collector，
	// 按「类型 + 指标名 + 标签名」缓存，见 shortcut.go 的说明。
	//
	// 跟着实例走而不是做成包级的：collector 是注册在本实例的 Registry 上的，
	// 换了实例，旧的那些就再也导不出去了。缓存是包级的时候，
	// Install 必须记得去清另一处的全局状态，忘了就是静默的数据丢失。
	collectors sync.Map   // map[string]prometheus.Collector
	createMu   sync.Mutex // 保证「创建 + 注册 + 入缓存」是一步
}

// New 按配置构造指标设施，不触碰任何全局变量。
//
// 返回的 io.Closer 永不为 nil。
func New(cfg Config) (*Metrics, io.Closer, error) {
	reg := prometheus.NewRegistry()

	if cfg.GoMetrics {
		if err := reg.Register(promcollectors.NewGoCollector()); err != nil {
			return nil, nil, err
		}
	}
	if cfg.ProcessMetrics {
		if err := reg.Register(promcollectors.NewProcessCollector(promcollectors.ProcessCollectorOpts{})); err != nil {
			return nil, nil, err
		}
	}

	return &Metrics{
		Registry: reg,
		Handler:  promhttp.HandlerFor(reg, promhttp.HandlerOpts{EnableOpenMetrics: true}),
		cfg:      cfg,
	}, noopCloser{}, nil
}

// Install 把这套设施装成进程级的：此后快捷方法和 Handler() 都走它。
//
// 开启 LogErrorMetric 时还会把当前的 slog 默认 logger 包一层，
// 让 Error 及以上级别的日志自动计入 log_errors_total。
// 计数走的是 xlog.AddObserver，所以它只统计经 xlog 写出去的日志：
// 自己另起一套 slog handler 的话，这个指标是空的。
func (m *Metrics) Install() {
	// 先把 logCounter 填好，最后才发布 —— 顺序反过来的话，current 已经指向
	// 本实例、而 logCounter 还在被写，这中间每一条错误日志都在读一个
	// 正在被写的字段。不只是 -race 会报：观察者是在赋值之前就挂上的，
	// 所以那段窗口里的错误日志一条都计不进去，面板上看不出任何区别。
	//
	// 发布走的是 mu，读的一侧 active() 也走 mu，赋值因此对读者可见。
	if m.cfg.LogErrorMetric {
		m.logCounter = newLogCounter(m)
	}

	mu.Lock()
	prev := current
	current = m
	if prev == nil {
		prev = fallback // 之前的打点都落在兜底实例上
	}
	mu.Unlock()

	// 换实例之前记的点是记在上一个 Registry 上的，不会出现在 /metrics 里。
	// 缓存已经跟着实例走，不需要清任何东西——但这件事仍然要说出来，
	// 否则就是一次完全静默的数据丢失。
	if prev != nil && prev != m {
		if n := prev.cachedCount(); n > 0 {
			slog.Warn("metrics were recorded before xmetric was initialized; those values went to a temporary registry and will not be exported",
				"metrics", n)
		}
	}
}

// ---- 全局状态 ----

var (
	mu      sync.RWMutex
	current *Metrics

	// fallback 未 Install 时用的兜底设施。
	//
	// 打点代码可能在初始化之前就跑起来（比如另一个组件 Init 里打的点），
	// 那时既不该 panic 也不该把数据丢进空气里。
	fallbackOnce sync.Once
	fallback     *Metrics
)

// active 返回当前生效的设施，未 Install 时返回兜底实例
func active() *Metrics {
	mu.RLock()
	m := current
	mu.RUnlock()
	if m != nil {
		return m
	}
	fallbackOnce.Do(func() {
		// 直接建，不走 New：New 会返回 error，而这里没有能把错误交出去的地方，
		// 吞掉它就意味着 fallback 可能是 nil，之后每一次打点都空指针。
		// 兜底实例不采集 Go / 进程指标——那些由真正的实例负责，
		// 这里只是给早到的打点一个不会丢的落点，没有会失败的步骤。
		reg := prometheus.NewRegistry()
		f := &Metrics{
			Registry: reg,
			Handler:  promhttp.HandlerFor(reg, promhttp.HandlerOpts{EnableOpenMetrics: true}),
			cfg:      DefaultConfig(),
		}
		// 赋值也走 mu：Install 要读它来数「初始化之前记了多少点」，
		// 两边不用同一把锁就是一次数据竞争
		mu.Lock()
		fallback = f
		mu.Unlock()
	})

	mu.RLock()
	defer mu.RUnlock()
	return fallback
}

// Registry 返回当前生效的 Prometheus Registry
func Registry() *prometheus.Registry { return active().Registry }

// Handler 返回 /metrics 的 HTTP handler
func Handler() http.Handler { return active().Handler }

// MustRegister 把自定义 collector 注册到当前 Registry，重复注册会 panic
func MustRegister(cs ...prometheus.Collector) { Registry().MustRegister(cs...) }

// Register 注册 collector，已注册过同名同标签的则复用已有实例而不是 panic。
//
// 返回实际生效的那个 collector——可能不是传进来的这个，务必用返回值。
//
// 同名但类型或标签不同时返回错误。那种情况下传进来的这个 collector
// 不在 registry 里，通过它记的值永远导不出去；调用方要么让启动失败，
// 要么至少知道自己在往空气里写。
func Register(c prometheus.Collector) (prometheus.Collector, error) {
	return register(Registry(), c)
}

// ConstLabels 返回配置的全局常量标签（拷贝）。
//
// 给自己建指标的包用：把它填进 prometheus.Opts.ConstLabels，
// 你的指标就和框架内置指标带上同样的环境/集群标签。
func ConstLabels() prometheus.Labels {
	m := active()
	if len(m.cfg.ConstLabels) == 0 {
		return nil
	}
	out := make(prometheus.Labels, len(m.cfg.ConstLabels))
	for k, v := range m.cfg.ConstLabels {
		out[k] = v
	}
	return out
}

// HTTPDurationBuckets 返回 HTTP 耗时直方图的桶边界（拷贝）
func HTTPDurationBuckets() []float64 { return slices.Clone(active().cfg.HTTPDurationBuckets) }

// Namespace 返回配置的指标名前缀
func Namespace() string { return active().cfg.Namespace }

// register 注册 collector，重复注册时复用已有实例
func register(reg *prometheus.Registry, c prometheus.Collector) (prometheus.Collector, error) {
	err := reg.Register(c)
	if err == nil {
		return c, nil
	}
	var are prometheus.AlreadyRegisteredError
	if errors.As(err, &are) {
		return are.ExistingCollector, nil
	}
	// 同名不同标签之类的冲突：这个 collector 不在 registry 里，
	// 通过它记的值永远导不出去
	return c, fmt.Errorf("xmetric: metric registration failed, values recorded through it will not be exported: %w", err)
}

type noopCloser struct{}

func (noopCloser) Close() error { return nil }

// ---- 登记 ----

var cfg = DefaultConfig()

// init 只登记，不初始化。真正的初始化由框架在 StageTelemetry 执行。
func init() {
	registry.Register(registry.Component{
		Key:    ConfigKey,
		Stage:  registry.StageTelemetry,
		Config: &cfg,
		Init: func(context.Context) (io.Closer, error) {
			m, closer, err := New(cfg)
			if err != nil {
				return nil, err
			}
			m.Install()
			return closer, nil
		},
	})
}

// RegisterAs 注册 c 并把实际生效的那个断言回 T。
//
// 比 Register 好用的地方：调用方几乎总是需要具体类型（*CounterVec 之类）
// 才能打点，而 Register 返回的是接口，每个调用点都要重复一遍
// 「注册 → 判错 → 类型断言 → 断言失败怎么办」。
//
//	total, err := xmetric.RegisterAs(prometheus.NewCounterVec(opts, labels))
//
// 出错时返回传进来的那个（可以照常打点，只是导不出去），
// 调用方据此决定是让启动失败还是记一条日志继续。
func RegisterAs[T prometheus.Collector](c T) (T, error) {
	registered, err := Register(c)
	if err != nil {
		return c, err
	}
	typed, ok := registered.(T)
	if !ok {
		return c, fmt.Errorf("xmetric: metric name is already registered as %T, values recorded through it will not be exported", registered)
	}
	return typed, nil
}
