// 一个最小的可运行示例：配置文件决定行为，main 里没有装配代码。
//
//	go run ./example --config=example/application.yml
package main

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/xiaoshicae/xtow"
	"github.com/xiaoshicae/xtow/xgin"
	"github.com/xiaoshicae/xtow/xmetric"

	// 匿名 import 就是全部「装配」：各包在 init 里登记自己，
	// 框架按 Stage 决定谁先起、谁后关。import 的书写顺序不影响任何事。
	_ "github.com/xiaoshicae/xtow/xcache"
	_ "github.com/xiaoshicae/xtow/xhttp"
	_ "github.com/xiaoshicae/xtow/xlog"
	_ "github.com/xiaoshicae/xtow/xmetric"
	_ "github.com/xiaoshicae/xtow/xtrace"
)

func main() {
	// xgin 内置了日志、链路、指标、panic 恢复四个中间件，
	// 顺序由框架管；/metrics 也是自动挂上的
	xtow.MustRun(xgin.New().WithRoutes(routes))
}

func routes(e *gin.Engine) {
	e.GET("/hello", func(c *gin.Context) {
		defer xmetric.Timer("hello_handle")()

		// 业务代码只认识标准库 slog 和原生 gin，不认识本框架的包。
		// 日志会自动带上这次请求的 trace_id
		slog.InfoContext(c.Request.Context(), "request received", "path", c.Request.URL.Path)
		c.JSON(http.StatusOK, gin.H{"msg": "hello"})
	})

	e.GET("/boom", func(c *gin.Context) {
		panic("deliberate panic, to exercise the recover middleware")
	})
}
