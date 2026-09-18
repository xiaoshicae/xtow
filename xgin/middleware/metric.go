package middleware

import (
	"log/slog"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/xiaoshicae/xtow/xmetric"
)

// Metric 记录请求数和耗时，按方法、路由、状态码分。
//
// 指标在这里建、这里注册，而不是放成包级变量只建一次：
// 包级变量会把 collector 绑死在「第一次建中间件时装的那个 registry」上，
// 之后再换 registry，记的值就永远导不出去了。
// 重复注册由 xmetric.Register 处理——它返回已有的那个实例。
func Metric() gin.HandlerFunc {
	total := registerCounter(prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace:   xmetric.Namespace(),
		Name:        "http_requests_total",
		Help:        "HTTP 请求总数",
		ConstLabels: xmetric.ConstLabels(),
	}, []string{"method", "route", "status"}))

	latency := registerHistogram(prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   xmetric.Namespace(),
		Name:        "http_request_duration_seconds",
		Help:        "HTTP 请求耗时",
		Buckets:     xmetric.HTTPDurationBuckets(),
		ConstLabels: xmetric.ConstLabels(),
	}, []string{"method", "route", "status"}))

	return func(c *gin.Context) {
		start := time.Now()

		// 用 defer 记：即使 panic 穿过本层（比如用户自定义的 RecoveryFunc 自己炸了），
		// 这个请求也仍然会被计入，不会在错误率里凭空消失
		defer func() {
			// 用路由模板而不是真实路径：按真实路径打标签会让时间序列
			// 随 URL 里的 id 无限增长，Prometheus 会被撑垮
			route := c.FullPath()
			if route == "" {
				route = "unmatched"
			}
			method, status := c.Request.Method, strconv.Itoa(c.Writer.Status())

			total.WithLabelValues(method, route, status).Inc()
			// 用秒而不是毫秒：毫秒取整会把 0.4ms 的请求记成 0
			latency.WithLabelValues(method, route, status).Observe(time.Since(start).Seconds())
		}()

		c.Next()
	}
}

func registerCounter(c *prometheus.CounterVec) *prometheus.CounterVec {
	registered, err := xmetric.Register(c)
	if err != nil {
		slog.Error("xgin 请求数指标注册失败，通过它记录的值不会被导出", "错误", err)
		return c
	}
	if typed, ok := registered.(*prometheus.CounterVec); ok {
		return typed
	}
	return c
}

func registerHistogram(h *prometheus.HistogramVec) *prometheus.HistogramVec {
	registered, err := xmetric.Register(h)
	if err != nil {
		slog.Error("xgin 请求耗时指标注册失败，通过它记录的值不会被导出", "错误", err)
		return h
	}
	if typed, ok := registered.(*prometheus.HistogramVec); ok {
		return typed
	}
	return h
}
