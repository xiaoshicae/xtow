package xhttp

import (
	"context"
	"strconv"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/xiaoshicae/xtow/xmetric"
)

// newDurationHistogram 出站请求耗时直方图
//
// 桶边界取 xmetric 配的那一份，好让出站和入站的耗时在同一把刻度上看。
func newDurationHistogram() *prometheus.HistogramVec {
	return prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   xmetric.Namespace(),
		Name:        "http_client_request_duration_seconds",
		Help:        "Outbound HTTP request duration",
		Buckets:     xmetric.HTTPDurationBuckets(),
		ConstLabels: xmetric.ConstLabels(),
	}, []string{"method", "host", "status"})
}

// startKey 整次逻辑请求的起点在 context 里的 key
type startKey struct{}

// installMetrics 挂上记录指标的钩子
//
// 用 OnSuccess / OnError 而不是中间件：它们在所有重试结束后只调用一次，
// 挂在中间件上会把每次重试都记一遍，请求量和耗时分布都虚高。
func installMetrics(client *resty.Client, hist *prometheus.HistogramVec) {
	// 自己记一个起点，不能用 resp.Time()。
	//
	// resty 每次尝试都会重置 Request.Time，于是 resp.Time() 只是最后一次
	// 尝试的耗时——前面几次失败的尝试和它们之间的退避等待全都不算。
	// 计数是按「一次逻辑请求」记的，耗时也必须是，否则故障时
	// 请求数照涨、耗时却纹丝不动，监控看上去异常地健康。
	client.OnBeforeRequest(func(_ *resty.Client, req *resty.Request) error {
		if _, ok := req.Context().Value(startKey{}).(time.Time); !ok {
			req.SetContext(context.WithValue(req.Context(), startKey{}, time.Now()))
		}
		return nil
	})
	client.OnSuccess(func(_ *resty.Client, resp *resty.Response) {
		if resp == nil || resp.Request == nil || resp.Request.RawRequest == nil {
			return
		}
		raw := resp.Request.RawRequest
		observe(hist, raw.Method, raw.URL.Host, strconv.Itoa(resp.StatusCode()),
			elapsed(resp.Request, resp.Time()))
	})

	client.OnError(func(req *resty.Request, err error) {
		if req == nil || req.RawRequest == nil {
			return
		}
		raw := req.RawRequest
		// 没拿到响应时状态码记 0：把它和真实状态码混在一起会让
		// 「5xx 比例」这类告警在网络故障时反而看不出问题
		status, dur := "0", time.Since(req.Time)
		var re *resty.ResponseError
		if errorsAs(err, &re) && re.Response != nil {
			status, dur = strconv.Itoa(re.Response.StatusCode()), re.Response.Time()
		}
		observe(hist, raw.Method, raw.URL.Host, status, elapsed(req, dur))
	})
}

// elapsed 取整次逻辑请求的耗时，拿不到起点时退回这一次尝试的耗时
func elapsed(req *resty.Request, fallback time.Duration) time.Duration {
	if req == nil {
		return fallback
	}
	if start, ok := req.Context().Value(startKey{}).(time.Time); ok {
		return time.Since(start)
	}
	return fallback
}

func observe(hist *prometheus.HistogramVec, method, host, status string, d time.Duration) {
	hist.WithLabelValues(method, host, status).Observe(d.Seconds())
}
