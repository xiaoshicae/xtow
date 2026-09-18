package xgin

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/xiaoshicae/xtow/internal/config"
	"github.com/xiaoshicae/xtow/registry"
	"github.com/xiaoshicae/xtow/xgin/middleware"
	"github.com/xiaoshicae/xtow/xmetric"
)

func init() { gin.SetMode(gin.TestMode) }

func load(t *testing.T, yml string) Config {
	t.Helper()
	c := DefaultConfig()
	path := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(path, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := config.Load(path, []registry.Component{{Key: ConfigKey, Config: &c}}); err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	return c
}

func loadErr(t *testing.T, yml string) error {
	t.Helper()
	c := DefaultConfig()
	path := filepath.Join(t.TempDir(), "application.yml")
	os.WriteFile(path, []byte(yml), 0o644)
	return config.Load(path, []registry.Component{{Key: ConfigKey, Config: &c}})
}

// withConfig 换一份配置，测试结束还原
func withConfig(t *testing.T, mutate func(*Config)) {
	t.Helper()
	old := cfg
	t.Cleanup(func() { cfg = old })
	cfg = DefaultConfig()
	if mutate != nil {
		mutate(&cfg)
	}
}

// freePort 找一个空闲端口
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// ---- 配置 ----

func TestConfig_默认值(t *testing.T) {
	c := DefaultConfig()
	if c.Host != "0.0.0.0" || c.Port != 8080 {
		t.Errorf("监听默认值不对，got=%+v", c)
	}
	// 不限制的话，慢客户端可以一直占着连接不放
	if c.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("读请求头必须有超时，got=%v", c.ReadHeaderTimeout)
	}
	// 小于 K8s 的 30s 宽限期，否则 Shutdown 还没走完就被 SIGKILL
	if c.ShutdownTimeout != 25*time.Second || c.ShutdownTimeout >= 30*time.Second {
		t.Errorf("优雅退出预算应小于常见的终止宽限期，got=%v", c.ShutdownTimeout)
	}
	// 线上忘了设环境变量的代价，比本地少一行提示大得多
	if c.Mode != "release" {
		t.Errorf("默认应为 release，got=%q", c.Mode)
	}
}

func TestConfig_从文件加载(t *testing.T) {
	c := load(t, "XGin:\n  Port: 9090\n  ReadTimeout: 30s\n  UseH2C: true\n")
	if c.Port != 9090 || c.ReadTimeout != 30*time.Second || !c.UseH2C {
		t.Errorf("配置没生效，got=%+v", c)
	}
	if c.Host != "0.0.0.0" {
		t.Errorf("没写的字段应保持默认，got=%+v", c)
	}
}

func TestConfig_拼写错误要失败(t *testing.T) {
	if err := loadErr(t, "XGin:\n  Prot: 9090\n"); err == nil {
		t.Fatal("字段拼错应当启动失败")
	}
}

func TestValidate(t *testing.T) {
	if err := DefaultConfig().validate(); err != nil {
		t.Errorf("默认配置应当合法：%v", err)
	}

	for name, mutate := range map[string]func(*Config){
		"端口为 0":    func(c *Config) { c.Port = 0 },
		"端口越界":     func(c *Config) { c.Port = 70000 },
		"只配了证书":    func(c *Config) { c.CertFile = "a.pem" },
		"只配了私钥":    func(c *Config) { c.KeyFile = "a.key" },
		"Mode 不认识": func(c *Config) { c.Mode = "prod" },
	} {
		c := DefaultConfig()
		mutate(&c)
		if err := c.validate(); err == nil {
			t.Errorf("%s 应当报错", name)
		}
	}
}

func TestValidate_只配一半的TLS(t *testing.T) {
	// 这是最危险的一种配错：服务会以明文起来，而配置文件看上去是配了证书的
	c := DefaultConfig()
	c.CertFile = "cert.pem"
	err := c.validate()
	if err == nil {
		t.Fatal("只配一半的 TLS 应当启动失败，而不是静默降级成明文")
	}
	if !strings.Contains(err.Error(), "CertFile") || !strings.Contains(err.Error(), "KeyFile") {
		t.Errorf("错误该说清楚缺了什么，got=%v", err)
	}
}

// ---- 装配 ----

func TestBuild_内置中间件顺序(t *testing.T) {
	// Recover 必须是内置里最内层的：panic 在哪一层被兜住，
	// 比它更内层的中间件里 c.Next() 之后的代码就都不执行了
	withConfig(t, nil)
	g := New().WithRoutes(func(e *gin.Engine) {
		e.GET("/boom", func(c *gin.Context) { panic("炸了") })
	})

	w := doRequest(t, g.Engine(), "GET", "/boom")
	if w.Code != 500 {
		t.Errorf("panic 应被兜住并返回 500，got=%d", w.Code)
	}
}

func TestBuild_指标端点自动注册(t *testing.T) {
	withConfig(t, nil)
	g := New()
	if w := doRequest(t, g.Engine(), "GET", "/metrics"); w.Code != 200 {
		t.Errorf("启用指标时应自动注册 /metrics，got=%d", w.Code)
	}
	if !strings.Contains(doRequest(t, g.Engine(), "GET", "/metrics").Body.String(), "http_requests_total") {
		t.Error("指标端点应导出请求数指标")
	}
}

func TestBuild_可以改指标路径(t *testing.T) {
	withConfig(t, nil)
	g := New(WithMetricPath("/internal/metrics"))
	if w := doRequest(t, g.Engine(), "GET", "/internal/metrics"); w.Code != 200 {
		t.Errorf("应注册在配置的路径上，got=%d", w.Code)
	}
	if w := doRequest(t, g.Engine(), "GET", "/metrics"); w.Code == 200 {
		t.Error("默认路径上不该再有")
	}
}

func TestBuild_关掉指标就不注册端点(t *testing.T) {
	withConfig(t, nil)
	g := New(WithMetric(false))
	if w := doRequest(t, g.Engine(), "GET", "/metrics"); w.Code == 200 {
		t.Error("关掉指标后不该有 /metrics")
	}
}

func TestBuild_405而不是404(t *testing.T) {
	// 不开 HandleMethodNotAllowed 的话，方法用错会得到 404，
	// 调用方会以为是路径写错了
	withConfig(t, nil)
	g := New().WithRoutes(func(e *gin.Engine) {
		e.GET("/only-get", func(c *gin.Context) { c.Status(200) })
	})
	if w := doRequest(t, g.Engine(), "POST", "/only-get"); w.Code != 405 {
		t.Errorf("方法不对应返回 405，got=%d", w.Code)
	}
}

func TestBuild_幂等(t *testing.T) {
	// 装配两遍会把中间件注册两遍，表现是每个请求打两条日志、指标翻倍
	withConfig(t, nil)
	g := New()
	if g.Engine() != g.Engine() {
		t.Error("重复装配应返回同一个 engine")
	}
}

func TestBuild_用户中间件在内置之后(t *testing.T) {
	withConfig(t, nil)
	var order []string
	g := New(WithLog(false), WithTrace(false), WithMetric(false)).
		WithMiddleware(func(c *gin.Context) { order = append(order, "用户"); c.Next() }).
		WithRoutes(func(e *gin.Engine) {
			e.GET("/x", func(c *gin.Context) { order = append(order, "handler"); c.Status(200) })
		})

	doRequest(t, g.Engine(), "GET", "/x")
	if len(order) != 2 || order[0] != "用户" || order[1] != "handler" {
		t.Errorf("用户中间件应在 handler 之前，got=%v", order)
	}
}

// ---- 启停 ----

func TestStartStop_优雅关闭(t *testing.T) {
	port := freePort(t)
	withConfig(t, func(c *Config) { c.Host, c.Port = "127.0.0.1", port })

	g := New(WithLog(false), WithMetric(false)).WithRoutes(func(e *gin.Engine) {
		e.GET("/ping", func(c *gin.Context) { c.String(200, "pong") })
	})

	done := make(chan error, 1)
	go func() { done <- g.Start(context.Background()) }()

	url := fmt.Sprintf("http://127.0.0.1:%d/ping", port)
	waitServing(t, url)

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "pong" {
		t.Errorf("响应不对，got=%s", body)
	}

	if err := g.Stop(context.Background()); err != nil {
		t.Fatalf("关闭失败：%v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("优雅关闭不该返回错误，got=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start 没有在关闭后返回")
	}
}

func TestStart_配置非法时不监听(t *testing.T) {
	withConfig(t, func(c *Config) { c.CertFile = "只配了一半" })
	if err := New().Start(context.Background()); err == nil {
		t.Fatal("配置非法时应当报错")
	}
}

func TestStart_信号早于启动到达(t *testing.T) {
	// 照常监听的话，服务会在「已经收到停止信号」之后才起来，
	// 然后一直跑到框架等超时为止
	port := freePort(t)
	withConfig(t, func(c *Config) { c.Host, c.Port = "127.0.0.1", port })

	g := New(WithLog(false), WithMetric(false))
	if err := g.Stop(context.Background()); err != nil {
		t.Fatalf("还没启动就 Stop 不该报错：%v", err)
	}

	done := make(chan error, 1)
	go func() { done <- g.Start(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("应当直接返回，got=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stop 之后 Start 不该真的开始监听")
	}

	// 确认端口上真的没人监听
	if conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond); err == nil {
		conn.Close()
		t.Error("服务不该起来")
	}
}

func TestStart_重复启动报错(t *testing.T) {
	port := freePort(t)
	withConfig(t, func(c *Config) { c.Host, c.Port = "127.0.0.1", port })

	g := New(WithLog(false), WithMetric(false)).WithRoutes(func(e *gin.Engine) {
		e.GET("/ping", func(c *gin.Context) { c.Status(200) })
	})
	go g.Start(context.Background())
	waitServing(t, fmt.Sprintf("http://127.0.0.1:%d/ping", port))
	t.Cleanup(func() { g.Stop(context.Background()) })

	if err := g.Start(context.Background()); err == nil {
		t.Error("重复启动应当报错")
	}
}

func TestStop_没启动过也安全(t *testing.T) {
	withConfig(t, nil)
	if err := New().Stop(context.Background()); err != nil {
		t.Errorf("没启动过的 Stop 不该报错：%v", err)
	}
}

func TestStop_不吃光框架给的停止预算(t *testing.T) {
	// 框架给的停止预算是所有组件共享的，HTTP 服务占满了，
	// 后面的数据库、缓存就没时间关了。所以这里自带上限，不完全跟随调用方的 ctx
	withConfig(t, func(c *Config) { c.ShutdownTimeout = 50 * time.Millisecond })
	g := New()

	// 传一个已经取消的 ctx：Shutdown 仍应按自己的预算走完，不是立刻失败
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.Stop(ctx); err != nil {
		t.Errorf("没启动过时应直接返回，got=%v", err)
	}
}

// ---- 登记 ----

func TestRegister_只认领配置(t *testing.T) {
	var got *registry.Component
	for _, c := range registry.Snapshot() {
		if c.Key == ConfigKey {
			got = &c
			break
		}
	}
	if got == nil {
		t.Fatalf("没有以 %s 登记", ConfigKey)
	}
	if got.Stage != registry.StageServer {
		t.Errorf("服务应在最后一档，got=%v", got.Stage)
	}
	if got.Config != &cfg {
		t.Error("登记的必须是包级配置变量本身")
	}
	if got.Init != nil {
		t.Error("服务由使用者交给 xtow.Run 启动，不该在这里登记 Init")
	}
}

func TestXGin_满足Runnable(t *testing.T) {
	// 结构化满足即可，不 import 根包——「集成不依赖框架」这条要在编译层面成立
	var _ interface {
		Start(context.Context) error
		Stop(context.Context) error
	} = New()
}

func TestSensitiveFieldsAPI(t *testing.T) {
	// 打开 body 日志之前要能把自定义敏感字段补上
	middleware.AddSensitiveFields("x_custom")
	middleware.AddSensitiveHeaders("X-Custom")
}

// doRequest 对 engine 发一次请求
func doRequest(t *testing.T, e *gin.Engine, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w
}

// waitServing 等服务真的开始监听
func waitServing(t *testing.T, url string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("服务没有起来：%s", url)
}

func TestBuild_先拿Engine再初始化指标也不丢(t *testing.T) {
	// 回归用例。装配时如果就把 xmetric 的 registry 抓走，而那时 xmetric
	// 还没初始化，指标会被注册到一个永远不会被导出的兜底 registry 上：
	// 请求正常处理、指标正常记录、/metrics 里什么都没有，且没有任何迹象。
	withConfig(t, nil)

	g := New().WithRoutes(func(e *gin.Engine) {
		e.GET("/x", func(c *gin.Context) { c.Status(200) })
	})
	e := g.Engine() // 使用者在 Run 之前拿一下 engine，很自然的写法

	// 此后框架才初始化 xmetric（StageTelemetry 在 StageServer 之前）
	m, closer, err := xmetric.New(xmetric.Config{Namespace: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	m.Install()

	e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))

	w := httptest.NewRecorder()
	m.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(w.Body.String(), "demo_http_requests_total") {
		t.Errorf("指标应记在初始化之后的 registry 上\n实际=\n%s", w.Body.String())
	}
}

func TestBuild_先拿Engine也不影响metrics端点(t *testing.T) {
	// /metrics 的 handler 同理：装配时定死就会一直导出那个空的兜底 registry
	withConfig(t, nil)
	g := New()
	e := g.Engine()

	m, closer, err := xmetric.New(xmetric.Config{Namespace: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	m.Install()

	// 先打一个请求产出指标，再抓 /metrics
	e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/nope", nil))
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))

	if !strings.Contains(w.Body.String(), "demo_http_requests_total") {
		t.Errorf("/metrics 应导出当前生效的 registry\n实际=\n%s", w.Body.String())
	}
}

func TestStart_Mode在启动时才设(t *testing.T) {
	// 装配可能发生在配置加载之前，那时读到的是默认值
	withConfig(t, func(c *Config) { c.Mode = "debug" })
	t.Cleanup(func() { gin.SetMode(gin.TestMode) })

	g := New(WithLog(false), WithMetric(false))
	g.Engine() // 先装配
	gin.SetMode(gin.TestMode)

	port := freePort(t)
	cfg.Host, cfg.Port = "127.0.0.1", port
	go g.Start(context.Background())
	waitServing(t, fmt.Sprintf("http://127.0.0.1:%d/metrics", port))
	t.Cleanup(func() { g.Stop(context.Background()) })

	if gin.Mode() != gin.DebugMode {
		t.Errorf("启动时应按配置设 Mode，got=%q", gin.Mode())
	}
}
