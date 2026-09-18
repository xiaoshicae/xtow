package xhttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/xiaoshicae/xtow/xmetric"
)

// withMetrics 装一套独立的指标设施并让本包用上它
func withMetrics(t *testing.T) *xmetric.Metrics {
	t.Helper()
	m, closer, err := xmetric.New(xmetric.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closer.Close() })
	m.Install()
	return m
}

func scrape(t *testing.T, m *xmetric.Metrics) string {
	t.Helper()
	w := httptest.NewRecorder()
	m.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	return w.Body.String()
}

func TestMetric_记录状态码与耗时(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv.Close()

	client, m := newQuiet(t, DefaultConfig())
	if _, err := client.R().SetContext(context.Background()).Get(srv.URL); err != nil {
		t.Fatal(err)
	}

	out := scrape(t, m)
	if !strings.Contains(out, `status="503"`) || !strings.Contains(out, `method="GET"`) {
		t.Errorf("应按方法和状态码分标签\n实际=\n%s", out)
	}
	if !strings.Contains(out, "http_client_request_duration_seconds_count") {
		t.Errorf("应记录耗时\n实际=\n%s", out)
	}
}

func TestMetric_网络错误记状态码0(t *testing.T) {
	// 把它和真实状态码混在一起，会让「5xx 比例」这类告警在网络故障时反而看不出问题
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer srv.Close()

	client, m := newQuiet(t, DefaultConfig())
	client.R().SetContext(context.Background()).Get(srv.URL)

	if out := scrape(t, m); !strings.Contains(out, `status="0"`) {
		t.Errorf("网络错误应记状态码 0\n实际=\n%s", out)
	}
}

func TestMetric_重试只记一次(t *testing.T) {
	// 挂在中间件上会把每次重试都记一遍，请求量和耗时分布都虚高
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
	client, m := newQuiet(t, c)
	client.R().SetContext(context.Background()).Get(srv.URL)

	if got := hits.Load(); got != 3 {
		t.Fatalf("应当真的重试了，got=%d 次请求", got)
	}
	out := scrape(t, m)
	if !strings.Contains(out, `status="0"} 1`) {
		t.Errorf("重试 3 次也只该记 1 个样本\n实际=\n%s", out)
	}
}

func TestMetric_用xmetric的桶与标签(t *testing.T) {
	// 出站和入站的耗时要在同一把刻度上，看板才对得起来
	m, closer, err := xmetric.New(func() xmetric.Config {
		c := xmetric.DefaultConfig()
		c.GoMetrics, c.ProcessMetrics, c.LogErrorMetric = false, false, false
		c.Namespace = "demo"
		c.ConstLabels = map[string]string{"env": "prod"}
		c.HTTPDurationBuckets = []float64{0.5}
		return c
	}())
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	m.Install()

	srv, _ := echo(t, nil)
	client, closer, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	client.SetLogger(discardLogger{})
	client.R().SetContext(context.Background()).Get(srv.URL)

	out := scrape(t, m)
	if !strings.Contains(out, `demo_http_client_request_duration_seconds_bucket{env="prod"`) {
		t.Errorf("应带上前缀和常量标签\n实际=\n%s", out)
	}
	if strings.Contains(out, `le="0.25"`) {
		t.Errorf("应当用配置里的桶\n实际=\n%s", out)
	}
}

func TestMetric_重复注册复用已有实例(t *testing.T) {
	// 建两个 client 时第二个的指标必须落在已注册的那个上，否则记的值导不出去
	m := withMetrics(t)
	srv, _ := echo(t, nil)

	for i := 0; i < 2; i++ {
		client, closer, err := New(DefaultConfig())
		if err != nil {
			t.Fatal(err)
		}
		defer closer.Close()
		client.SetLogger(discardLogger{})
		if _, err := client.R().SetContext(context.Background()).Get(srv.URL); err != nil {
			t.Fatal(err)
		}
	}

	if out := scrape(t, m); !strings.Contains(out, `status="204"} 2`) {
		t.Errorf("两个 client 的请求应记在同一条序列上\n实际=\n%s", out)
	}
}

func TestNewDurationHistogram_类型冲突时报错(t *testing.T) {
	m := withMetrics(t)
	// 先用同名注册一个 Counter，New 应当报错而不是默默记不出去
	m.Registry.MustRegister(prometheus.NewCounter(prometheus.CounterOpts{
		Name: "http_client_request_duration_seconds", Help: "占位",
	}))

	if _, _, err := New(DefaultConfig()); err == nil {
		t.Fatal("指标名被占成别的类型时应当报错")
	}
}
