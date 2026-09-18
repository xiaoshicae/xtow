package middleware

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	// maxRequestBody 请求体记录上限
	maxRequestBody = 256 * 1024
	// maxResponseBody 响应体记录上限，比请求体小得多：
	// 响应通常大得多，而排查时看开头几 KB 基本够了
	maxResponseBody = 4 * 1024
)

// LogOption 日志中间件的配置项
type LogOption func(*logOptions)

type logOptions struct {
	skipExact  map[string]bool
	skipPrefix []string
	reqBody    bool
	respBody   bool
}

// WithSkipPaths 指定不记访问日志的路径。
//
// 以 / 结尾的按前缀匹配（/health/ 命中 /health/live），其余精确匹配。
func WithSkipPaths(paths ...string) LogOption {
	return func(o *logOptions) {
		for _, p := range paths {
			if strings.HasSuffix(p, "/") {
				o.skipPrefix = append(o.skipPrefix, p)
			} else {
				o.skipExact[p] = true
			}
		}
	}
}

// WithBody 是否记录请求体和响应体。默认都不记。
//
// 默认关掉是因为记 body 的代价和风险都不小：要把请求体缓存一份、
// 要包一层 ResponseWriter 截响应，还要对每个字段做脱敏。
// 需要排查时再打开，并确认脱敏字段配全了。
func WithBody(request, response bool) LogOption {
	return func(o *logOptions) { o.reqBody, o.respBody = request, response }
}

// writerPool 复用截响应用的 writer
var writerPool = sync.Pool{New: func() any { return &captureWriter{buf: &bytes.Buffer{}} }}

// captureWriter 在写响应的同时截一份副本
type captureWriter struct {
	gin.ResponseWriter
	buf     *bytes.Buffer
	capture bool
}

func (w *captureWriter) Write(b []byte) (int, error) {
	if w.capture && w.buf.Len() < maxResponseBody {
		w.buf.Write(b[:min(len(b), maxResponseBody-w.buf.Len())])
	}
	return w.ResponseWriter.Write(b)
}

func (w *captureWriter) WriteString(s string) (int, error) {
	if w.capture && w.buf.Len() < maxResponseBody {
		w.buf.WriteString(s[:min(len(s), maxResponseBody-w.buf.Len())])
	}
	return w.ResponseWriter.WriteString(s)
}

// Log 记访问日志。
func Log(opts ...LogOption) gin.HandlerFunc {
	o := &logOptions{skipExact: map[string]bool{}}
	for _, opt := range opts {
		opt(o)
	}

	return func(c *gin.Context) {
		if o.shouldSkip(c.Request.URL.Path) {
			c.Next()
			return
		}

		// 级别关掉时直接放行。下面这一整套——缓存请求体、包装 ResponseWriter
		// 截响应、结束后的脱敏和序列化——存在的唯一目的就是拼出这一行日志。
		// 等 slog 自己去判级别时，代价已经付完了，只是结果被丢弃。
		//
		// 每请求判一次而不是构造时判一次：级别可能在运行时变。
		if !slog.Default().Enabled(c.Request.Context(), slog.LevelInfo) {
			c.Next()
			return
		}

		start := time.Now()
		var reqBody []byte
		if o.reqBody {
			reqBody = snapshotBody(c.Request)
		}

		orig := c.Writer
		var cw *captureWriter
		if o.respBody {
			cw = writerPool.Get().(*captureWriter)
			cw.ResponseWriter, cw.capture = orig, true
			cw.buf.Reset()
			c.Writer = cw
		}

		// 用 defer 收尾：即使 panic 穿过本层，访问日志仍然写得出去，
		// c.Writer 也一定会还原、writer 一定会还回池子
		defer func() {
			elapsed := time.Since(start)

			route := c.FullPath()
			if route == "" {
				route = c.Request.URL.Path
			}

			attrs := []any{
				"方法", c.Request.Method,
				"路由", route,
				"路径", c.Request.URL.Path,
				"状态", c.Writer.Status(),
				"耗时", elapsed,
				"客户端", c.ClientIP(),
				"请求头", RedactHeaders(c.Request.Header),
			}
			if o.reqBody {
				attrs = append(attrs, "请求体", RedactBody(reqBody, c.Request.Header.Get("Content-Type")))
			}
			if cw != nil {
				ct := c.Writer.Header().Get("Content-Type")
				if isText(ct) && cw.buf.Len() > 0 {
					attrs = append(attrs, "响应体", RedactBody(cw.buf.Bytes(), ct))
				}
				// 先还原 writer，再还回池子：外层中间件可能还要用它
				c.Writer = orig
				cw.ResponseWriter, cw.capture = nil, false
				writerPool.Put(cw)
			}
			if len(c.Errors) > 0 {
				attrs = append(attrs, "错误", c.Errors.String())
			}

			slog.InfoContext(c.Request.Context(), "请求完成", attrs...)
		}()

		c.Next()
	}
}

func (o *logOptions) shouldSkip(path string) bool {
	if o.skipExact[path] {
		return true
	}
	for _, p := range o.skipPrefix {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// isText 判断是不是适合直接记进日志的文本类型
func isText(contentType string) bool {
	ct := strings.ToLower(contentType)
	return strings.Contains(ct, "application/json") ||
		strings.Contains(ct, "text/") ||
		strings.Contains(ct, "application/xml")
}

// snapshotBody 取一份请求体副本，不影响后续读取。
//
// 优先用 GetBody：它返回的是副本，原始 Body 一点没动。
// 拿不到才退回「读出来再塞回去」。
func snapshotBody(req *http.Request) []byte {
	if req == nil || req.Body == nil || req.Body == http.NoBody {
		return nil
	}

	ct := req.Header.Get("Content-Type")
	// 文件上传和二进制流不读：内容对排查没用，读一遍却要付全部的内存和时间
	if strings.Contains(ct, "multipart/form-data") {
		return []byte("[multipart/form-data 已省略]")
	}
	if strings.Contains(ct, "application/octet-stream") {
		return []byte("[二进制内容已省略]")
	}

	if req.GetBody != nil {
		if rc, err := req.GetBody(); err == nil {
			defer rc.Close()
			b, _ := io.ReadAll(io.LimitReader(rc, maxRequestBody))
			return b
		}
	}

	// 退路：整个读出来，再塞一个等价的 Body 回去
	b, err := io.ReadAll(io.LimitReader(req.Body, maxRequestBody))
	if err != nil {
		return nil
	}
	rest, _ := io.ReadAll(req.Body) // 超出上限的部分照样要还给下游
	req.Body.Close()
	full := append(b, rest...)
	req.Body = io.NopCloser(bytes.NewReader(full))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(full)), nil }
	return b
}
