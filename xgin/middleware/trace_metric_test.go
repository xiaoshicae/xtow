package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/xiaoshicae/xtow/xmetric"
)

// recording 装一套独立的链路设施，返回取已结束 Span 的函数
func recording(t *testing.T) func() tracetest.SpanStubs {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exp)))

	oldTP, oldProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(oldTP)
		otel.SetTextMapPropagator(oldProp)
	})

	return exp.GetSpans
}

func TestTrace_开Span并回带TraceID(t *testing.T) {
	spans := recording(t)
	w := serve(t, get("/hello/42"), []gin.HandlerFunc{Trace()}, func(c *gin.Context) {
		c.String(200, "ok")
	})

	if w.Header().Get(TraceIDHeader) == "" {
		t.Error("应在响应头里回带 TraceID，否则从一次调用没法跳到链路")
	}
	got := spans()
	if len(got) != 1 {
		t.Fatalf("应产出一个 Span，got=%d", len(got))
	}
	// 用路由模板而不是真实路径：按真实路径命名会让 Span 名随用户数增长
	if got[0].Name != "GET /hello/:id" {
		t.Errorf("Span 名应用路由模板，got=%q", got[0].Name)
	}
	attrs := map[string]string{}
	for _, kv := range got[0].Attributes {
		attrs[string(kv.Key)] = kv.Value.Emit()
	}
	if attrs["http.route"] != "/hello/:id" || attrs["url.path"] != "/hello/42" {
		t.Errorf("路由与路径属性不对，got=%v", attrs)
	}
	if attrs["http.response.status_code"] != "200" {
		t.Errorf("应记状态码，got=%v", attrs)
	}
}

func TestTrace_接上上游链路(t *testing.T) {
	spans := recording(t)
	req := get("/hello")
	// 一条合法的 W3C traceparent
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")

	serve(t, req, []gin.HandlerFunc{Trace()}, func(c *gin.Context) { c.Status(200) })

	got := spans()
	if len(got) != 1 {
		t.Fatalf("应产出一个 Span，got=%d", len(got))
	}
	if id := got[0].SpanContext.TraceID().String(); id != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("应接上上游的 TraceID，got=%s", id)
	}
}

func TestTrace_只有5xx算错(t *testing.T) {
	// 4xx 是客户端传错了，标成错误会让链路里满屏是错，真故障反而看不出来
	for status, wantErr := range map[int]bool{200: false, 404: false, 400: false, 500: true, 503: true} {
		spans := recording(t)
		serve(t, get("/hello"), []gin.HandlerFunc{Trace()}, func(c *gin.Context) {
			c.Status(status)
		})
		got := spans()
		if len(got) != 1 {
			t.Fatalf("status=%d 应产出一个 Span", status)
		}
		isErr := got[0].Status.Code == codes.Error
		if isErr != wantErr {
			t.Errorf("status=%d 应当标错=%v，实际=%v", status, wantErr, isErr)
		}
	}
}

func TestTrace_未匹配路由用固定值(t *testing.T) {
	// 用真实路径的话，扫描器随便打几个 URL 就能把链路和指标的基数撑爆
	spans := recording(t)
	e := gin.New()
	e.Use(Trace())
	e.ServeHTTP(httptest.NewRecorder(), get("/nope/whatever/123"))

	got := spans()
	if len(got) != 1 {
		t.Fatalf("应产出一个 Span，got=%d", len(got))
	}
	if strings.Contains(got[0].Name, "whatever") {
		t.Errorf("未匹配路由不该把真实路径写进 Span 名，got=%q", got[0].Name)
	}
}

// ---- Metric ----

// withMetrics 装一套独立的指标设施
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

func TestMetric_记请求数与耗时(t *testing.T) {
	m := withMetrics(t)
	serve(t, get("/hello/42"), []gin.HandlerFunc{Metric()}, func(c *gin.Context) {
		c.Status(201)
	})

	out := scrape(t, m)
	if !strings.Contains(out, `http_requests_total{method="GET",route="/hello/:id",status="201"} 1`) {
		t.Errorf("应按方法、路由、状态码记请求数\n实际=\n%s", out)
	}
	if !strings.Contains(out, "http_request_duration_seconds_count") {
		t.Errorf("应记耗时\n实际=\n%s", out)
	}
}

func TestMetric_路由用模板(t *testing.T) {
	// 按真实路径打标签会让时间序列随 URL 里的 id 无限增长，Prometheus 会被撑垮
	m := withMetrics(t)
	for _, p := range []string{"/hello/1", "/hello/2", "/hello/3"} {
		serve(t, get(p), []gin.HandlerFunc{Metric()}, func(c *gin.Context) { c.Status(200) })
	}

	out := scrape(t, m)
	if !strings.Contains(out, `route="/hello/:id",status="200"} 3`) {
		t.Errorf("三个请求应聚合成一条序列\n实际=\n%s", out)
	}
	if strings.Contains(out, `route="/hello/1"`) {
		t.Errorf("不该把真实路径写进标签\n实际=\n%s", out)
	}
}

func TestMetric_未匹配路由用固定值(t *testing.T) {
	m := withMetrics(t)
	e := gin.New()
	e.Use(Metric())
	e.ServeHTTP(httptest.NewRecorder(), get("/nope/12345"))

	out := scrape(t, m)
	if !strings.Contains(out, `route="unmatched"`) {
		t.Errorf("未匹配路由应记成固定值\n实际=\n%s", out)
	}
}

func TestMetric_panic穿过时仍计入(t *testing.T) {
	// 不计入的话，出问题的请求会在错误率指标里凭空消失
	m := withMetrics(t)
	func() {
		defer func() { recover() }()
		serve(t, get("/hello"), []gin.HandlerFunc{Metric()}, func(c *gin.Context) { panic("炸了") })
	}()

	if out := scrape(t, m); !strings.Contains(out, "http_requests_total") {
		t.Errorf("panic 的请求也该计入\n实际=\n%s", out)
	}
}

var _ = http.StatusOK
