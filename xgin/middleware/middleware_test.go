package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/xiaoshicae/xtow/xlog"
)

func init() { gin.SetMode(gin.TestMode) }

// capture 把默认 logger 换成一个真的 xlog 实例，返回取解析结果的函数。
//
// 必须用 xlog 而不是随便一个 slog handler：LogScope 往 context 里塞的字段
// 是 xlog 的 handler 负责取出来的，换成别的 handler 就什么都看不到——
// 这也正是「LogScope 只在用 xlog 时生效」这件事的实际含义。
func capture(t *testing.T) func() []map[string]any {
	t.Helper()
	dir := t.TempDir()
	c := xlog.DefaultConfig()
	c.Console = false
	c.File = xlog.FileConfig{Enable: true, Path: dir, Name: "app.log", RotateTime: time.Hour, Perm: "0644"}

	l, closer, err := xlog.New(c)
	if err != nil {
		t.Fatal(err)
	}
	old := slog.Default()
	slog.SetDefault(l)
	t.Cleanup(func() { slog.SetDefault(old); closer.Close() })

	return func() []map[string]any {
		closer.Close() // 冲刷出来再读
		files, _ := filepath.Glob(filepath.Join(dir, "app.log.*"))
		if len(files) == 0 {
			return nil
		}
		b, err := os.ReadFile(files[0])
		if err != nil {
			t.Fatal(err)
		}
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if line == "" {
				continue
			}
			m := map[string]any{}
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatalf("日志不是 JSON：%v，内容=%q", err, line)
			}
			out = append(out, m)
		}
		return out
	}
}

// serve 装上中间件跑一次请求
func serve(t *testing.T, req *http.Request, mws []gin.HandlerFunc, h gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	e := gin.New()
	e.Use(mws...)
	e.Any("/hello/:id", h)
	e.Any("/hello", h)

	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	return w
}

func get(path string) *http.Request { return httptest.NewRequest("GET", path, nil) }

// ---- LogScope ----

func TestLogScope_业务能在任意层级补字段(t *testing.T) {
	// 调用栈深处拿不到 *gin.Context，本来就没机会把新 context 回传上来
	lines := capture(t)
	serve(t, get("/hello"), []gin.HandlerFunc{LogScope(), Log()}, func(c *gin.Context) {
		deepInBusinessCode(c.Request.Context())
		c.String(200, "ok")
	})

	got := lines()
	if len(got) == 0 {
		t.Fatal("没有访问日志")
	}
	last := got[len(got)-1]
	if last["用户"] != "u9" {
		t.Errorf("业务补的字段应出现在访问日志里，got=%v", last)
	}
}

func deepInBusinessCode(ctx context.Context) { xlog.AddKV(ctx, "用户", "u9") }

// ---- Recover ----

func TestRecover_兜住panic并返回500(t *testing.T) {
	lines := capture(t)
	w := serve(t, get("/hello"), []gin.HandlerFunc{Recover(nil)}, func(c *gin.Context) {
		panic("炸了")
	})

	if w.Code != 500 {
		t.Errorf("应返回 500，got=%d", w.Code)
	}
	got := lines()
	if len(got) == 0 || got[0]["level"] != "ERROR" {
		t.Fatalf("应记一条 error 日志，got=%v", got)
	}
	if got[0]["栈"] == nil || !strings.Contains(got[0]["栈"].(string), "middleware_test.go") {
		t.Errorf("应带上栈信息，got=%v", got[0])
	}
	if got[0]["路径"] != "/hello" {
		t.Errorf("应带上请求路径，got=%v", got[0])
	}
}

func TestRecover_自定义响应(t *testing.T) {
	capture(t)
	w := serve(t, get("/hello"), []gin.HandlerFunc{Recover(func(c *gin.Context, _ any) {
		c.JSON(503, gin.H{"msg": "稍后再试"})
	})}, func(c *gin.Context) { panic("炸了") })

	if w.Code != 503 || !strings.Contains(w.Body.String(), "稍后再试") {
		t.Errorf("应走自定义响应，got=%d %s", w.Code, w.Body.String())
	}
}

func TestRecover_响应已开始写就不再改状态码(t *testing.T) {
	// 响应一旦开始往外发，再改状态码只会得到一个半截的响应
	capture(t)
	w := serve(t, get("/hello"), []gin.HandlerFunc{Recover(nil)}, func(c *gin.Context) {
		c.String(200, "已经写出去一部分")
		panic("写到一半炸了")
	})

	if w.Code != 200 {
		t.Errorf("已经写出的响应不该被改成 500，got=%d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "已经写出去一部分") {
		t.Errorf("已写出的内容应保留，got=%s", w.Body.String())
	}
}

func TestRecover_没有panic时不干预(t *testing.T) {
	w := serve(t, get("/hello"), []gin.HandlerFunc{Recover(nil)}, func(c *gin.Context) {
		c.String(201, "ok")
	})
	if w.Code != 201 {
		t.Errorf("正常请求不该被干预，got=%d", w.Code)
	}
}

func TestIsBrokenPipe(t *testing.T) {
	// 客户端提前断开不算故障，不值得打一份完整栈
	broken := &net.OpError{Err: &os.SyscallError{Syscall: "write", Err: errors.New("broken pipe")}}
	if !isBrokenPipe(broken) {
		t.Error("broken pipe 应当被认出来")
	}
	reset := &net.OpError{Err: &os.SyscallError{Syscall: "read", Err: errors.New("connection reset by peer")}}
	if !isBrokenPipe(reset) {
		t.Error("connection reset 应当被认出来")
	}
	if isBrokenPipe("普通 panic") || isBrokenPipe(&net.OpError{Err: errors.New("其它")}) {
		t.Error("其它错误不该被当成断连")
	}
}

// ---- Log ----

func TestLog_记下关键字段(t *testing.T) {
	lines := capture(t)
	serve(t, get("/hello/42?q=1"), []gin.HandlerFunc{Log()}, func(c *gin.Context) {
		c.String(201, "ok")
	})

	got := lines()
	if len(got) != 1 {
		t.Fatalf("应记一条，got=%v", got)
	}
	l := got[0]
	// 路由用模板而不是真实路径：/hello/42 和 /hello/43 是同一个接口
	if l["路由"] != "/hello/:id" {
		t.Errorf("路由应是模板，got=%v", l["路由"])
	}
	if l["路径"] != "/hello/42" {
		t.Errorf("路径应是真实路径，got=%v", l["路径"])
	}
	if l["状态"] != float64(201) || l["方法"] != "GET" {
		t.Errorf("状态和方法不对，got=%v", l)
	}
	if l["耗时"] == nil || l["客户端"] == nil {
		t.Errorf("应记耗时和客户端，got=%v", l)
	}
}

func TestLog_跳过指定路径(t *testing.T) {
	lines := capture(t)
	mws := []gin.HandlerFunc{Log(WithSkipPaths("/hello", "/internal/"))}
	e := gin.New()
	e.Use(mws...)
	e.GET("/hello", func(c *gin.Context) { c.Status(200) })
	e.GET("/internal/x", func(c *gin.Context) { c.Status(200) })
	e.GET("/other", func(c *gin.Context) { c.Status(200) })

	for _, p := range []string{"/hello", "/internal/x", "/other"} {
		e.ServeHTTP(httptest.NewRecorder(), get(p))
	}

	got := lines()
	if len(got) != 1 || got[0]["路径"] != "/other" {
		t.Errorf("只有 /other 该被记下来，got=%v", got)
	}
}

func TestLog_默认不记body(t *testing.T) {
	// 记 body 要缓存整个请求体并对每个字段脱敏，代价和风险都不小
	lines := capture(t)
	req := httptest.NewRequest("POST", "/hello", strings.NewReader(`{"password":"`+secret+`"}`))
	req.Header.Set("Content-Type", "application/json")
	serve(t, req, []gin.HandlerFunc{Log()}, func(c *gin.Context) { c.String(200, "ok") })

	got := lines()[0]
	if _, has := got["请求体"]; has {
		t.Errorf("默认不该记请求体，got=%v", got)
	}
}

func TestLog_打开后记body且脱敏(t *testing.T) {
	lines := capture(t)
	req := httptest.NewRequest("POST", "/hello", strings.NewReader(`{"user":"alice","password":"`+secret+`"}`))
	req.Header.Set("Content-Type", "application/json")

	serve(t, req, []gin.HandlerFunc{Log(WithBody(true, true))}, func(c *gin.Context) {
		body, _ := c.GetRawData()
		if !strings.Contains(string(body), "alice") {
			t.Errorf("下游应当仍然读得到完整请求体，got=%s", body)
		}
		c.JSON(200, gin.H{"token": secret, "ok": true})
	})

	got := lines()[0]
	reqBody, _ := got["请求体"].(string)
	if strings.Contains(reqBody, secret) {
		t.Errorf("请求体里的密码应被遮掉，got=%v", reqBody)
	}
	if !strings.Contains(reqBody, "alice") {
		t.Errorf("非敏感字段应保留，got=%v", reqBody)
	}
	respBody, _ := got["响应体"].(string)
	if strings.Contains(respBody, secret) {
		t.Errorf("响应体里的令牌也该被遮掉，got=%v", respBody)
	}
}

func TestLog_请求头脱敏(t *testing.T) {
	lines := capture(t)
	req := get("/hello")
	req.Header.Set("Authorization", "Bearer "+secret)
	serve(t, req, []gin.HandlerFunc{Log()}, func(c *gin.Context) { c.Status(200) })

	if h, _ := lines()[0]["请求头"].(string); strings.Contains(h, secret) {
		t.Errorf("请求头里的凭证应被遮掉，got=%v", h)
	}
}

func TestLog_不读文件上传的body(t *testing.T) {
	// 内容对排查没用，读一遍却要付全部的内存和时间
	lines := capture(t)
	req := httptest.NewRequest("POST", "/hello", strings.NewReader("大量二进制内容"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	serve(t, req, []gin.HandlerFunc{Log(WithBody(true, false))}, func(c *gin.Context) { c.Status(200) })

	if b, _ := lines()[0]["请求体"].(string); !strings.Contains(b, "已省略") {
		t.Errorf("文件上传的 body 不该被读进日志，got=%v", b)
	}
}

func TestLog_级别关掉时不做任何准备工作(t *testing.T) {
	// 缓存 body、包装 writer、脱敏序列化，全部只为拼出这一行日志。
	// 级别关掉还照做，就是白付了全部代价再把结果丢掉
	old := slog.Default()
	t.Cleanup(func() { slog.SetDefault(old) })
	var buf strings.Builder
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))

	read := false
	req := httptest.NewRequest("POST", "/hello", strings.NewReader(`{"a":1}`))
	req.Header.Set("Content-Type", "application/json")
	req.Body = &trackingBody{ReadCloser: req.Body, read: &read}

	serve(t, req, []gin.HandlerFunc{Log(WithBody(true, true))}, func(c *gin.Context) { c.Status(200) })

	if buf.Len() != 0 {
		t.Errorf("级别关掉时不该有日志，got=%s", buf.String())
	}
	if read {
		t.Error("级别关掉时不该去读请求体")
	}
}

type trackingBody struct {
	io.ReadCloser
	read *bool
}

func (b *trackingBody) Read(p []byte) (int, error) {
	*b.read = true
	return b.ReadCloser.Read(p)
}

func TestLog_panic穿过时仍写访问日志(t *testing.T) {
	// 用户自定义的 RecoveryFunc 自己炸了的话，panic 会穿过 Log 这一层
	lines := capture(t)
	defer func() { recover() }()

	func() {
		defer func() { recover() }()
		serve(t, get("/hello"), []gin.HandlerFunc{Log()}, func(c *gin.Context) { panic("炸了") })
	}()

	if got := lines(); len(got) == 0 {
		t.Error("panic 穿过时访问日志仍应写出")
	}
}

func TestIsText(t *testing.T) {
	for ct, want := range map[string]bool{
		"application/json":         true,
		"text/plain; charset=utf8": true,
		"application/xml":          true,
		"image/png":                false,
		"application/octet-stream": false,
		"":                         false,
	} {
		if got := isText(ct); got != want {
			t.Errorf("isText(%q)=%v want %v", ct, got, want)
		}
	}
}
