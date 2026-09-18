package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const (
	tracerName = "github.com/xiaoshicae/xtow/xgin"

	// TraceIDHeader 响应里回带的链路标识头，方便从一次调用直接跳到链路
	TraceIDHeader = "X-Trace-Id"
)

// Trace 为每个请求开一个服务端 Span，并接上上游传来的链路。
func Trace() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 用路由模板而不是真实路径：/user/123 和 /user/456 是同一个接口，
		// 按真实路径命名会让 Span 名和指标标签的基数随用户数增长
		route := c.FullPath()
		if route == "" {
			route = "unmatched" // 没匹配上任何路由，用固定值而不是真实路径
		}

		// 每次都取当前的全局 Propagator：构造中间件时链路可能还没初始化
		ctx := otel.GetTextMapPropagator().Extract(c.Request.Context(),
			propagation.HeaderCarrier(c.Request.Header))

		ctx, span := otel.Tracer(tracerName).Start(ctx, c.Request.Method+" "+route,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.request.method", c.Request.Method),
				attribute.String("http.route", route),
				attribute.String("url.path", c.Request.URL.Path),
			),
		)
		defer span.End()

		c.Request = c.Request.WithContext(ctx)

		// 必须在 c.Next() 之前写：响应一旦开始发送，header 就改不动了
		if sc := span.SpanContext(); sc.IsValid() {
			c.Header(TraceIDHeader, sc.TraceID().String())
		}

		c.Next()

		status := c.Writer.Status()
		span.SetAttributes(attribute.Int("http.response.status_code", status))
		// 只有 5xx 算服务端的错。4xx 是客户端传错了，标成错误会让链路里
		// 满屏是「错误」，真正的故障反而看不出来
		if status >= 500 {
			span.SetStatus(codes.Error, http.StatusText(status))
		}
		if len(c.Errors) > 0 {
			span.SetAttributes(attribute.String("gin.errors", c.Errors.String()))
		}
	}
}
