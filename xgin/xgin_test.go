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
	// 这是「HTTP 服务能占用的那一份」，必须小于框架的总预算（默认 15s），
	// 否则这一项是死配置——两者取更早的那个截止时间
	if c.ShutdownTimeout != 10*time.Second || c.ShutdownTimeout >= 15*time.Second {
		t.Errorf("服务那一份预算应小于框架总预算，got=%v", c.ShutdownTimeout)
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

func TestValidate_停止预算为零要拦住(t *testing.T) {
	// 0 在这里不是「不限时」而是「一点都不等」：Shutdown 拿到的是一个已经
	// 过期的 context，在途请求当场被切断，而配置文件看上去只是没设上限。
	// xflow 的 RollbackTimeout 早就按这条规矩拦了，这里漏掉了
	c := DefaultConfig()
	c.ShutdownTimeout = 0
	if err := c.validate(); err == nil {
		t.Fatal("ShutdownTimeout=0 应当报错")
	}

	withConfig(t, func(c *Config) { c.ShutdownTimeout = 0 })
	if err := New().Start(context.Background()); err == nil {
		t.Fatal("配置非法时不该起服务")
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

// servingWithHungRequest 起一个服务，并让一个请求挂在 handler 里不返回，
// 这样 Shutdown 必须等它 —— 才测得出等多久
func servingWithHungRequest(t *testing.T) *XGin {
	g, _ := servingWithHungRequestDone(t)
	return g
}

// servingWithHungRequestDone 同上，另外返回一个在「那个挂住的请求结束时」
// 关闭的 channel —— 强制断连有没有生效，只有它看得出来
func servingWithHungRequestDone(t *testing.T) (*XGin, <-chan struct{}) {
	t.Helper()
	port := freePort(t)
	withConfig(t, func(c *Config) { c.Host, c.Port = "127.0.0.1", port })

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	g := New(WithLog(false), WithMetric(false)).WithRoutes(func(e *gin.Engine) {
		e.GET("/ping", func(c *gin.Context) { c.Status(200) })
		e.GET("/hang", func(c *gin.Context) { <-release; c.Status(200) })
	})
	go g.Start(context.Background())
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitServing(t, base+"/ping")

	hung := make(chan struct{})
	go func() {
		defer close(hung)
		if resp, err := http.Get(base + "/hang"); err == nil {
			resp.Body.Close()
		}
	}()
	// 等这个请求真的到了 handler 里，否则 Shutdown 可能在它之前就走完了
	time.Sleep(100 * time.Millisecond)
	return g, hung
}

func TestStop_不超过调用方给的截止时间(t *testing.T) {
	// 回归用例。这里曾经用 context.WithoutCancel 换掉调用方的 ctx，于是
	// ShutdownTimeout 配得比框架总预算大时，会实打实地等满自己那一份——
	// 「所有组件共享一份预算」就成了一句空话
	g := servingWithHungRequest(t)
	withConfig(t, func(c *Config) { c.ShutdownTimeout = 30 * time.Second })

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := g.Stop(ctx)
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Fatalf("该按调用方的截止时间收手，实际等了 %v", elapsed)
	}
	if err == nil {
		t.Error("在途请求没做完就到点了，该如实报错")
	}
}

func TestStop_也不超过自己那一份预算(t *testing.T) {
	// 另一半：调用方给的很宽时，服务自己的上限仍然生效，
	// 不然后面的数据库、缓存就没时间关了
	g := servingWithHungRequest(t)
	withConfig(t, func(c *Config) { c.ShutdownTimeout = 200 * time.Millisecond })

	start := time.Now()
	err := g.Stop(context.Background())
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Fatalf("自己那一份上限没生效，实际等了 %v", elapsed)
	}
	if err == nil {
		t.Error("在途请求没做完就到点了，该如实报错")
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

func TestWithConfig_两个实例监听各自的端口(t *testing.T) {
	// 没有这个选项时两个实例读的是同一份包级配置，只能监听同一个端口——
	// 「需要两套配置时也有出路」这条承诺对 xgin 就是假的
	withConfig(t, nil) // 包级配置故意留在默认端口上，证明谁都没读它

	portA, portB := freePort(t), freePort(t)
	newOn := func(port int, body string) *XGin {
		c := CurrentConfig()
		c.Host, c.Port = "127.0.0.1", port
		g := New(WithConfig(c), WithLog(false), WithMetric(false)).
			WithRoutes(func(e *gin.Engine) {
				e.GET("/who", func(c *gin.Context) { c.String(200, body) })
			})
		go g.Start(context.Background())
		t.Cleanup(func() { g.Stop(context.Background()) })
		return g
	}
	newOn(portA, "A")
	newOn(portB, "B")

	for _, c := range []struct {
		port int
		want string
	}{{portA, "A"}, {portB, "B"}} {
		url := fmt.Sprintf("http://127.0.0.1:%d/who", c.port)
		waitServing(t, url)
		resp, err := http.Get(url)
		if err != nil {
			t.Fatalf("端口 %d 打不通：%v", c.port, err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(got) != c.want {
			t.Errorf("端口 %d 该是实例 %s，got=%q", c.port, c.want, got)
		}
	}
}

func TestWithConfig_不给就跟着配置文件走(t *testing.T) {
	// 默认路径不能因为多了这个选项而改变
	port := freePort(t)
	withConfig(t, func(c *Config) { c.Host, c.Port = "127.0.0.1", port })

	g := New(WithLog(false), WithMetric(false)).WithRoutes(func(e *gin.Engine) {
		e.GET("/ping", func(c *gin.Context) { c.Status(200) })
	})
	go g.Start(context.Background())
	t.Cleanup(func() { g.Stop(context.Background()) })
	waitServing(t, fmt.Sprintf("http://127.0.0.1:%d/ping", port))
}

func TestConf_配置在用的时候才读(t *testing.T) {
	// New 可能发生在配置加载之前（使用者在 main 顶上就把 XGin 建好了），
	// 那时候读一次的话，配置文件从此再也不生效
	withConfig(t, func(c *Config) { c.Port = 1 })
	g := New()
	withConfig(t, func(c *Config) { c.Port = 2 })

	if got := g.conf().Port; got != 2 {
		t.Errorf("该读到 New 之后才加载进来的配置，got=%d", got)
	}
}

func TestStop_超时后强制断掉在途连接(t *testing.T) {
	// Shutdown 超时只返回错误，它不动那些连接。就这么走的话 handler 还在跑，
	// 而框架紧接着就去关数据库和缓存了——那些请求会摸到已经关掉的连接池。
	//
	// 只看端口连不连得上是测不出来的：Shutdown 一进去就把监听关了，
	// 连不上是两种情况共有的表现。要看的是那个在途请求有没有被断掉。
	g, hung := servingWithHungRequestDone(t)
	withConfig(t, func(c *Config) { c.ShutdownTimeout = 200 * time.Millisecond })

	if err := g.Stop(context.Background()); err == nil {
		t.Fatal("在途请求没做完就到点了，该报错")
	}

	select {
	case <-hung:
	case <-time.After(2 * time.Second):
		t.Error("超时之后在途请求仍在继续——框架接着就去关数据库了，它会摸到已关闭的连接池")
	}
}

func TestBuild_metrics端点也走用户中间件(t *testing.T) {
	// gin 在注册路由那一刻就把处理链定死了。指标端点原先注册在
	// e.Use(g.extra...) 之前，于是 WithMiddleware 挂的统一鉴权
	// 对业务路由生效、对 /metrics 不生效——一个以为被保护的端点其实敞着
	withConfig(t, nil)
	auth := func(c *gin.Context) { c.AbortWithStatus(http.StatusUnauthorized) }

	e := New(WithLog(false)).
		WithMiddleware(auth).
		WithRoutes(func(e *gin.Engine) {
			e.GET("/biz", func(c *gin.Context) { c.Status(200) })
		}).Engine()

	for _, path := range []string{"/biz", "/metrics"} {
		w := httptest.NewRecorder()
		e.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s 该被鉴权中间件拦住，got=%d", path, w.Code)
		}
	}
}

func TestBuild_默认不信任何代理(t *testing.T) {
	// gin 自己的默认是 trustedProxies = 0.0.0.0/0 + ::/0，也就是全都信。
	// 那意味着任何人发一个 X-Forwarded-For 就能决定访问日志里的
	// client_ip 是什么——日志可以伪造，建在这个字段上的限流和审计一起失效
	withConfig(t, nil)

	var got string
	e := New(WithLog(false), WithMetric(false)).
		WithRoutes(func(e *gin.Engine) {
			e.GET("/", func(c *gin.Context) { got = c.ClientIP() })
		}).Engine()

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.5:1234"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	e.ServeHTTP(httptest.NewRecorder(), req)

	if got != "10.0.0.5" {
		t.Errorf("client_ip 应该是对端地址本身，got=%q（请求头里伪造的是 1.2.3.4）", got)
	}
}

func TestStart_配了代理网段才认转发头(t *testing.T) {
	withConfig(t, func(c *Config) { c.TrustedProxies = []string{"10.0.0.0/8"} })

	var got string
	g := New(WithLog(false), WithMetric(false)).
		WithRoutes(func(e *gin.Engine) {
			e.GET("/", func(c *gin.Context) { got = c.ClientIP() })
		})
	startForConfig(t, g)

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.5:1234"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	g.Engine().ServeHTTP(httptest.NewRecorder(), req)

	if got != "1.2.3.4" {
		t.Errorf("对端在信任网段内，应该认转发头里的地址，got=%q", got)
	}
}

// startForConfig 真的把服务起起来，好让配置驱动的那几项落到 engine 上。
// 配置一律在 Start 生效——装配可能发生在配置加载之前
func startForConfig(t *testing.T, g *XGin) {
	t.Helper()
	port := freePort(t)
	cfg.Host, cfg.Port = "127.0.0.1", port
	go func() { _ = g.Start(context.Background()) }()
	waitServing(t, fmt.Sprintf("http://127.0.0.1:%d/nothing", port))
	t.Cleanup(func() { _ = g.Stop(context.Background()) })
}

func TestStart_装配早于配置加载时配置照样生效(t *testing.T) {
	// 回归用例。使用者在 main 顶上建好 XGin 并调 Engine()（README 允许），
	// 之后框架才把配置文件解进来。曾经 TrustedProxies 和
	// MaxMultipartMemory 是在装配里读的，于是这两项永远停在默认值，
	// 而同一份配置里的 Port / Mode 照常生效——一半生效一半不生效，
	// 其中一项还是安全设置，静默退回默认值
	withConfig(t, nil)

	var got string
	g := New(WithLog(false), WithMetric(false)).
		WithRoutes(func(e *gin.Engine) {
			e.GET("/", func(c *gin.Context) { got = c.ClientIP() })
		})
	g.Engine() // 装配发生在配置加载之前

	cfg.TrustedProxies = []string{"10.0.0.0/8"}
	cfg.MaxMultipartMemory = 1 << 20
	startForConfig(t, g)

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.5:1234"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	g.Engine().ServeHTTP(httptest.NewRecorder(), req)

	if got != "1.2.3.4" {
		t.Errorf("配置里的 TrustedProxies 没生效，client_ip=%q", got)
	}
	if g.Engine().MaxMultipartMemory != 1<<20 {
		t.Errorf("配置里的 MaxMultipartMemory 没生效，got=%d", g.Engine().MaxMultipartMemory)
	}
}

func TestValidate_代理网段写错直接起不来(t *testing.T) {
	// gin 的 SetTrustedProxies 解析到出错为止、把已经解出来的留下，
	// 于是前半段代理被信任、后半段被悄悄丢掉——日志里的 client_ip
	// 一半真一半假，比起不来难查得多
	c := DefaultConfig()
	c.TrustedProxies = []string{"10.0.0.0/8", "10.0.0.0/33"}
	if err := c.validate(); err == nil {
		t.Fatal("网段写错了应该报错")
	}

	c.TrustedProxies = []string{"10.0.0.0/8", "192.168.1.1", "::1"}
	if err := c.validate(); err != nil {
		t.Errorf("这几个都是合法写法，不该报错: %v", err)
	}
}

func TestStart_multipart的内存阈值来自配置(t *testing.T) {
	// gin 自己默认 32MB，而这个数不是「请求体上限」是「超过多少才落盘」，
	// 实际代价约是它的三倍：一次 60MB 的上传，配 32MB 时解析这一步
	// 让堆多占 96MB，二十个并发就是两个 G
	withConfig(t, func(c *Config) { c.MaxMultipartMemory = 2 << 20 })

	g := New(WithLog(false), WithMetric(false))
	startForConfig(t, g)
	if got := g.Engine().MaxMultipartMemory; got != 2<<20 {
		t.Errorf("该用配置里的阈值，got=%d want=%d", got, 2<<20)
	}
}

func TestEngine_不经过Start时用的是偏安全的默认值(t *testing.T) {
	// 单独拿 Engine() 去用、不经过 Start 的话，拿到的是一份默认值配好的
	// engine：不信任何代理、8MB 的 multipart 阈值，都是偏安全的那一侧
	withConfig(t, func(c *Config) {
		c.MaxMultipartMemory = 64 << 20
		c.TrustedProxies = []string{"0.0.0.0/0"}
	})

	e := New(WithLog(false), WithMetric(false)).Engine()
	if e.MaxMultipartMemory != 8<<20 {
		t.Errorf("没经过 Start，该是默认的 8MB，got=%d", e.MaxMultipartMemory)
	}

	var got string
	e.GET("/", func(c *gin.Context) { got = c.ClientIP() })
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.5:1234"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	e.ServeHTTP(httptest.NewRecorder(), req)
	if got != "10.0.0.5" {
		t.Errorf("没经过 Start，该谁都不信，got=%q", got)
	}
}
