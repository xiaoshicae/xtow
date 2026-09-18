// Package middleware 提供 xgin 内置的中间件。
//
// 洋葱模型，自外向内的顺序是：
//
//	LogScope → Trace → Log → Metric → Recover → 用户中间件 → handler
//
// Recover 必须是框架中间件里最内层的一个：panic 一路向外抛，
// 在哪一层被兜住，比它更内层的中间件里 c.Next() 之后的代码就都不执行。
// 放在最内层，外面几层的收尾（记指标、写访问日志）才都还跑得到。
package middleware

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/xiaoshicae/xtow/xlog"
)

// LogScope 为本次请求开一个日志 KV 作用域。
//
// 装上之后，业务代码在任意调用层级都可以 xlog.AddKV(ctx, ...) 补字段，
// 不用把新 context 逐层回传——调用栈深处拿不到 *gin.Context，本来也没机会回传。
// 写进去的字段对整条请求可见，所以 Log 在 c.Next() 之后打的访问日志也带得上。
//
// 必须排在所有中间件最前面：在它之后才有作用域可写。
func LogScope() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request = c.Request.WithContext(xlog.CtxWithScope(c.Request.Context()))
		c.Next()
	}
}

// maxStack panic 栈信息的上限
const maxStack = 16 * 1024

// Recover 兜住 panic，把它变成一条错误日志和一个 500。
//
// handle 为 nil 时返回 500。
func Recover(handle gin.RecoveryFunc) gin.HandlerFunc {
	if handle == nil {
		handle = func(c *gin.Context, _ any) { c.AbortWithStatus(http.StatusInternalServerError) }
	}

	return func(c *gin.Context) {
		defer func() {
			err := recover()
			if err == nil {
				return
			}

			// 连接断了不算故障，不值得打一份完整栈
			broken := isBrokenPipe(err)
			ctx := c.Request.Context()
			if broken {
				slog.ErrorContext(ctx, "连接已断开", "错误", err)
				_ = c.Error(err.(error)) //nolint:errcheck // isBrokenPipe 保证它是 *net.OpError
				c.Abort()
				return
			}

			slog.ErrorContext(ctx, "请求处理中 panic",
				"错误", err,
				"栈", stack(),
				"路径", c.Request.URL.Path,
				"方法", c.Request.Method)

			if c.Writer.Written() {
				// 响应已经开始往外写了，再改状态码只会得到一个半截的响应
				c.Abort()
				return
			}
			handle(c, err)
		}()
		c.Next()
	}
}

// isBrokenPipe 判断是不是客户端提前断开连接
func isBrokenPipe(err any) bool {
	ne, ok := err.(*net.OpError)
	if !ok {
		return false
	}
	var se *os.SyscallError
	if !errors.As(ne, &se) {
		return false
	}
	msg := strings.ToLower(se.Error())
	return strings.Contains(msg, "broken pipe") || strings.Contains(msg, "connection reset by peer")
}

// stack 当前协程的栈
//
// 用 runtime.Stack 而不是逐帧读源码文件：panic 恢复期间去做文件 I/O，
// 磁盘一慢就把这条错误日志也拖住了。
func stack() string {
	buf := make([]byte, maxStack)
	return string(buf[:runtime.Stack(buf, false)])
}
