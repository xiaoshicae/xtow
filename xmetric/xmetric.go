package xmetric

import (
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
// 包的是 slog.Default()，不是本框架的某个类型——不用 xlog 也一样生效。
func (m *Metrics) Install() {
	mu.Lock()
	current = m
	mu.Unlock()

	// 换了 registry，缓存里的 collector 还挂在旧的那个上，
	// 通过它们记的值不会出现在 /metrics 里。清掉，让下次打点重新建。
	if n := clearCollectors(); n > 0 {
		slog.Warn("xmetric 初始化之前已有打点，那些值记在临时 registry 上、不会被导出",
			"指标数", n)
	}

	if m.cfg.LogErrorMetric {
		m.logCounter = newLogCounter(m)
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
		// 兜底实例不采集 Go / 进程指标：那些由真正的实例负责，
		// 这里只是给早到的打点一个不会丢的落点
		c := DefaultConfig()
		c.GoMetrics, c.ProcessMetrics, c.LogErrorMetric = false, false, false
		fallback, _, _ = New(c)
	})
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
func Register(c prometheus.Collector) prometheus.Collector {
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
func register(reg *prometheus.Registry, c prometheus.Collector) prometheus.Collector {
	err := reg.Register(c)
	if err == nil {
		return c
	}
	var are prometheus.AlreadyRegisteredError
	if ok := asAlreadyRegistered(err, &are); ok {
		return are.ExistingCollector
	}
	// 同名不同标签之类的冲突：这个 collector 不在 registry 里，
	// 通过它记的值永远导不出去——必须说出来，否则是一次完全静默的数据丢失
	slog.Error("xmetric 注册指标失败，通过它记录的值不会被导出", "错误", err)
	return c
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
		Init: func() (io.Closer, error) {
			m, closer, err := New(cfg)
			if err != nil {
				return nil, err
			}
			m.Install()
			return closer, nil
		},
	})
}
