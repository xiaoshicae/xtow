// 一个最小的可运行示例：配置文件决定行为，main 里没有装配代码。
//
//	go run ./example --config=example/application.yml
package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/xiaoshicae/xtow"
	"github.com/xiaoshicae/xtow/xmetric"

	// 匿名 import 就是全部「装配」：各包在 init 里登记自己，
	// 框架按 Stage 决定谁先起、谁后关。import 的书写顺序不影响任何事。
	_ "github.com/xiaoshicae/xtow/xlog"
	_ "github.com/xiaoshicae/xtow/xmetric"
	_ "github.com/xiaoshicae/xtow/xtrace"
)

func main() {
	xtow.MustRun(&server{})
}

// server 一个普通的 HTTP 服务，实现 xtow.Runnable
type server struct{ s *http.Server }

func (x *server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", xmetric.Handler())
	mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		defer xmetric.Timer("hello_handle")()

		// 业务代码直接用原生的 OpenTelemetry 和标准库 slog，都不认识本框架的包
		ctx, span := otel.Tracer("demo").Start(r.Context(), "hello")
		defer span.End()

		// 日志自动带上这个 Span 的 trace_id —— xlog 和 xtrace 之间
		// 只有一个函数类型的扩展点，xlog 并不依赖 OpenTelemetry
		slog.InfoContext(ctx, "收到请求", "路径", r.URL.Path)
		w.Write([]byte("hello\n"))
	})

	x.s = &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := x.s.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (x *server) Stop(ctx context.Context) error { return x.s.Shutdown(ctx) }
