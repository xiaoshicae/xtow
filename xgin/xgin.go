package xgin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"

	"github.com/gin-gonic/gin"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/xiaoshicae/xtow/registry"
	"github.com/xiaoshicae/xtow/xgin/middleware"
	"github.com/xiaoshicae/xtow/xgin/trans"
	"github.com/xiaoshicae/xtow/xmetric"
)

// XGin 一个待启动的 HTTP 服务。
//
// 它满足 xtow.Runnable，直接交给 xtow.Run 即可：
//
//	xtow.MustRun(xgin.New().WithRoutes(register))
//
// 注意这里没有 import 根包——Go 的接口是结构化的，方法对得上就行。
// 这样「集成不依赖框架」这条在编译层面仍然成立。
type XGin struct {
	settings settings

	// override 实例级配置，非 nil 时压过配置文件里的 XGin 块。
	// 由 WithConfig 设置，见那里的说明。
	override *Config
	routes   []func(*gin.Engine)
	extra    []gin.HandlerFunc
	recover  gin.RecoveryFunc

	buildOnce sync.Once
	engine    *gin.Engine

	mu       sync.Mutex
	srv      *http.Server
	stopping bool
}

// New 创建一个 XGin
func New(opts ...Option) *XGin {
	s := defaultSettings()
	for _, o := range opts {
		o(&s)
	}
	return &XGin{settings: s, override: s.override}
}

// CurrentConfig 返回配置文件里 XGin 那一块解出来的配置（拷贝）。
//
// 配合 WithConfig 用：想在文件配置的基础上只改两项，取一份改完再传回去。
// 要在配置加载之后调用，否则拿到的是默认值。
func CurrentConfig() Config { return cfg }

// WithRoutes 注册路由。可以调用多次，按调用顺序生效。
func (g *XGin) WithRoutes(f ...func(*gin.Engine)) *XGin {
	g.routes = append(g.routes, f...)
	return g
}

// WithMiddleware 追加自定义中间件，排在所有内置中间件之后。
func (g *XGin) WithMiddleware(m ...gin.HandlerFunc) *XGin {
	g.extra = append(g.extra, m...)
	return g
}

// WithRecoverFunc 自定义 panic 之后的响应。默认返回 500。
func (g *XGin) WithRecoverFunc(f gin.RecoveryFunc) *XGin {
	g.recover = f
	return g
}

// Engine 返回原生的 *gin.Engine，需要做本包没覆盖的事情时用它。
//
// 调用它会触发一次装配，之后再 WithRoutes / WithMiddleware 就不生效了。
func (g *XGin) Engine() *gin.Engine {
	g.build()
	return g.engine
}

// build 装配 engine。用 Once 而不是布尔标志：并发调用时后者会把
// 中间件注册两遍，表现是每个请求打两条日志、指标翻倍。
func (g *XGin) build() {
	g.buildOnce.Do(func() {
		e := gin.New()
		e.HandleMethodNotAllowed = true // 不开的话，方法不对会返回 404 而不是 405

		// 默认谁都不信。gin 的默认是全都信，于是任何人发一个
		// X-Forwarded-For 就能决定访问日志里的 client_ip 是什么。
		// 返回的错误只会在网段写错时出现，那是配置问题，留给 Start 的校验报
		if err := e.SetTrustedProxies(g.conf().TrustedProxies); err != nil {
			slog.Warn("xgin invalid TrustedProxies, trusting none", "error", err)
			_ = e.SetTrustedProxies([]string{})
		}

		// 洋葱模型，自外向内：
		//   LogScope → Trace → Log → Metric → Recover → 用户中间件 → handler
		//
		// Recover 必须是内置里最内层的：panic 在哪一层被兜住，
		// 比它更内层的中间件里 c.Next() 之后的代码就都不执行了。
		// 放最内层，外面几层的收尾（记指标、写访问日志）才还跑得到。
		if g.settings.log {
			e.Use(middleware.LogScope())
		}
		if g.settings.trace {
			e.Use(middleware.Trace())
		}
		if g.settings.log {
			skip := append([]string{}, g.settings.skipPaths...)
			if g.settings.metric {
				// 指标端点会被抓取系统按秒轮询，记日志纯属刷屏
				skip = append(skip, g.settings.metricPath)
			}
			e.Use(middleware.Log(
				middleware.WithSkipPaths(skip...),
				middleware.WithBody(g.settings.logBody, g.settings.respBody),
			))
		}
		if g.settings.metric {
			e.Use(middleware.Metric())
		}
		e.Use(middleware.Recover(g.recover))
		e.Use(g.extra...)

		// 路由一律注册在所有中间件之后。
		//
		// gin 在注册路由的那一刻就把处理链定死了：那之后再 Use 的中间件
		// 对它不生效。指标端点原先注册在 g.extra 之前，于是使用者用
		// WithMiddleware 挂的统一鉴权对业务路由生效、对 /metrics 不生效——
		// 一个以为被保护起来的端点其实是敞开的。
		if g.settings.metric {
			// 每次请求再取 handler，不在这里定死：装配可能发生在 xmetric
			// 初始化之前，那时拿到的是兜底 registry，/metrics 会一直是空的
			e.GET(g.settings.metricPath, func(c *gin.Context) {
				xmetric.Handler().ServeHTTP(c.Writer, c.Request)
			})
		}
		for _, f := range g.routes {
			f(e)
		}

		if g.settings.zhTrans {
			if err := trans.RegisterZH(); err != nil {
				slog.Warn("xgin failed to register the zh validation translator", "error", err)
			}
		}

		g.engine = e
	})
}

// conf 取这个实例该用的配置。
//
// 默认读配置文件里的 XGin 块，而且是在用的时候才读、不在 New 里读：
// New 可能发生在配置加载之前（使用者在 main 顶上就把 XGin 建好了），
// 那时候读到的是一份默认值，配置文件从此再也不生效。
func (g *XGin) conf() Config {
	if g.override != nil {
		return *g.override
	}
	return cfg
}

// Start 启动服务并阻塞到它停止。由 xtow.Run 调用。
func (g *XGin) Start(ctx context.Context) error {
	c := g.conf()
	if err := c.validate(); err != nil {
		return fmt.Errorf("xgin: invalid config: %w", err)
	}
	// 在这里设而不是在装配里：装配可能发生在配置加载之前，
	// 那时读到的是默认值，配置里写的 Mode 从此再也不生效
	gin.SetMode(c.Mode)
	g.build()

	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	srv := g.newServer(c, addr)

	g.mu.Lock()
	if g.stopping {
		g.mu.Unlock()
		// 退出信号早于启动到达。照常监听的话，服务会在「已经收到停止信号」
		// 之后才起来，然后一直跑到框架等超时为止
		slog.Warn("xgin received the shutdown signal before starting, the server will not start")
		return nil
	}
	if g.srv != nil {
		g.mu.Unlock()
		return fmt.Errorf("xgin: server is already running on %s", g.srv.Addr)
	}
	g.srv = srv
	g.mu.Unlock()

	slog.Info("xgin listening", "addr", addr, "tls", c.tlsEnabled(), "h2c", c.UseH2C && !c.tlsEnabled())

	var err error
	if c.tlsEnabled() {
		err = srv.ListenAndServeTLS(c.CertFile, c.KeyFile)
	} else {
		err = srv.ListenAndServe()
	}
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("xgin: listen on %s failed: %w", addr, err)
}

// Stop 优雅关闭服务。由 xtow.Run 调用。
func (g *XGin) Stop(ctx context.Context) error {
	g.mu.Lock()
	g.stopping = true // 先置位：Start 若还没开始监听，到达时会直接返回
	srv := g.srv
	g.mu.Unlock()

	if srv == nil {
		return nil // 信号在服务起来之前就到了
	}

	// 在调用方的 ctx 上再收紧一层，而不是换掉它。
	//
	// WithTimeout 天然取两者中更早的那个截止时间，于是两条都成立：
	// 自己不会占满框架给的总预算（后面的数据库、缓存还有时间关），
	// 也不会超出它——ShutdownTimeout 配得比总预算大时，以总预算为准。
	// 换成 WithoutCancel 的话后一条就没了：配 25s 而总预算 15s 时，
	// 这里会实打实地等满 25 秒，所谓「共享预算」是句空话。
	stopCtx, cancel := context.WithTimeout(ctx, g.conf().ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(stopCtx); err != nil {
		// 到点了还有请求没做完。Shutdown 只是返回错误，它不动那些连接——
		// 就这么走的话，handler 还在跑，而框架紧接着就去关数据库和缓存了，
		// 那些请求会摸到已经关掉的连接池。
		//
		// 所以这里补一刀 Close()：强行断掉所有连接。在途请求会失败，
		// 但那本来就是超时的含义，好过让它们带着半个坏掉的进程继续跑。
		if cerr := srv.Close(); cerr != nil {
			slog.Warn("xgin force close failed", "error", cerr)
		}
		return fmt.Errorf("xgin: graceful shutdown timed out, connections were force closed: %w", err)
	}
	return nil
}

// newServer 按配置构建 http.Server
func (g *XGin) newServer(c Config, addr string) *http.Server {
	handler := g.engine.Handler()
	if c.UseH2C && !c.tlsEnabled() {
		handler = h2c.NewHandler(handler, &http2.Server{})
	}

	// 超时必须显式设置：零值是「永不超时」，慢客户端可以一直占着连接，
	// 连接数打满之后服务整体不可用
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: c.ReadHeaderTimeout,
		ReadTimeout:       c.ReadTimeout,
		WriteTimeout:      c.WriteTimeout,
		IdleTimeout:       c.IdleTimeout,
	}
}

// ---- 登记 ----

var cfg = DefaultConfig()

// init 只认领配置。服务本身由使用者交给 xtow.Run 启动，
// 不在这里登记 Init——框架的 Runnable 只有一个，那个位置是使用者的。
func init() {
	registry.Register(registry.Component{
		Key:    ConfigKey,
		Stage:  registry.StageServer,
		Config: &cfg,
	})
}
