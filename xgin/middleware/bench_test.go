package middleware

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func quiet(b *testing.B) {
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	b.Cleanup(func() { slog.SetDefault(old) })
}

func benchEngine(mw ...gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	e := gin.New()
	e.Use(mw...)
	e.GET("/order/:id", func(c *gin.Context) { c.String(200, "ok") })
	return e
}

func runReqs(b *testing.B, e *gin.Engine) {
	quiet(b)
	req := httptest.NewRequest("GET", "/order/123", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("User-Agent", "bench/1.0")
	req.Header.Set("X-Request-Id", "abc-123")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.ServeHTTP(httptest.NewRecorder(), req)
	}
}

func BenchmarkChain_0_裸gin(b *testing.B)      { runReqs(b, benchEngine()) }
func BenchmarkChain_1_只LogScope(b *testing.B) { runReqs(b, benchEngine(LogScope())) }
func BenchmarkChain_2_加Trace(b *testing.B)    { runReqs(b, benchEngine(LogScope(), Trace())) }
func BenchmarkChain_3_加Log(b *testing.B)      { runReqs(b, benchEngine(LogScope(), Trace(), Log())) }
func BenchmarkChain_4_加Metric(b *testing.B) {
	runReqs(b, benchEngine(LogScope(), Trace(), Log(), Metric()))
}
func BenchmarkChain_5_全量(b *testing.B) {
	runReqs(b, benchEngine(LogScope(), Trace(), Log(), Metric(), Recover(nil)))
}

func BenchmarkRedactHeaders(b *testing.B) {
	h := http.Header{}
	h.Set("Authorization", "Bearer secret")
	h.Set("User-Agent", "bench/1.0")
	h.Set("X-Request-Id", "abc-123")
	h.Set("Accept", "application/json")
	h.Set("Content-Type", "application/json")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = RedactHeaders(h)
	}
}

func benchHeader() http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer secret")
	h.Set("User-Agent", "bench/1.0")
	h.Set("X-Request-Id", "abc-123")
	h.Set("Accept", "application/json")
	h.Set("Content-Type", "application/json")
	return h
}

// BenchmarkRedact_整条日志 盯住「请求头只被序列化一次」这件事。
// 交出序列化好的字符串会让 slog 再转义一遍，时间和分配都翻倍。
func BenchmarkRedact_整条日志(b *testing.B) {
	h := benchHeader()
	l := slog.New(slog.NewJSONHandler(io.Discard, nil))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.Info("请求完成", "请求头", RedactHeaders(h))
	}
}
