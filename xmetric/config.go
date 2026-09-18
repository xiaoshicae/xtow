// Package xmetric 按配置装好 Prometheus 的 Registry 与 /metrics handler，
// 并提供一组免去样板的打点快捷方法。
//
// 需要完整控制时，Registry() 返回的就是原生的 *prometheus.Registry，
// 自己 NewCounterVec 再 MustRegister 即可，本包不挡路。
package xmetric

import "github.com/prometheus/client_golang/prometheus"

// ConfigKey 本模块在配置文件里的顶层 key
const ConfigKey = "XMetric"

// defaultHTTPDurationBuckets HTTP 请求耗时默认桶边界（秒）
//
// 与 prometheus.DefBuckets 同构，头部补一档 1ms 以便观察极快的接口。
// 用秒而非毫秒：Prometheus 约定以基准单位记录，各类 exporter、
// 社区看板与告警模板都按秒来，混用单位会让同一个服务导出两套刻度。
var defaultHTTPDurationBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// Config 指标配置
type Config struct {
	// Namespace 指标名前缀。默认无。
	Namespace string `yaml:"Namespace"`

	// ConstLabels 附加到所有指标上的常量标签。默认无。
	//
	// 典型用途是区分环境和集群：env: "${ENV:dev}"、cluster: "${CLUSTER:local}"。
	ConstLabels map[string]string `yaml:"ConstLabels"`

	// HTTPDurationBuckets HTTP 出入站耗时 Histogram 的桶边界（秒）。
	// 默认 [0.001 0.005 0.01 0.025 0.05 0.1 0.25 0.5 1 2.5 5 10]。
	HTTPDurationBuckets []float64 `yaml:"HTTPDurationBuckets"`

	// HistogramBuckets 快捷方法建出来的业务 Histogram 的桶边界（秒）。
	// 默认 prometheus.DefBuckets。
	HistogramBuckets []float64 `yaml:"HistogramBuckets"`

	// GoMetrics 是否采集 Go 运行时指标（goroutine 数、GC 等）。默认开启。
	GoMetrics bool `yaml:"GoMetrics"`

	// ProcessMetrics 是否采集进程指标（CPU、内存、文件描述符等）。默认开启。
	ProcessMetrics bool `yaml:"ProcessMetrics"`

	// LogErrorMetric 是否把 Error 及以上级别的日志计入 log_errors_total。默认开启。
	//
	// 它让「错误率」这条最常用的告警不必等业务先埋点。
	// 统计的是走 xlog 的日志：不用 xlog 的应用这项不会报错，只是数不到。
	LogErrorMetric bool `yaml:"LogErrorMetric"`
}

// DefaultConfig 全部默认值集中在这里。
func DefaultConfig() Config {
	return Config{
		HTTPDurationBuckets: append([]float64(nil), defaultHTTPDurationBuckets...),
		HistogramBuckets:    append([]float64(nil), prometheus.DefBuckets...),
		GoMetrics:           true,
		ProcessMetrics:      true,
		LogErrorMetric:      true,
	}
}
