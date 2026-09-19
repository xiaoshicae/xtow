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
	return &XGin{settings: s}
}

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
			// 每次请求再取 handler，不在这里定死：装配可能发生在 xmetric
			// 初始化之前，那时拿到的是兜底 registry，/metrics 会一直是空的
			e.GET(g.settings.metricPath, func(c *gin.Context) {
				xmetric.Handler().ServeHTTP(c.Writer, c.Request)
			})
		}
		e.Use(middleware.Recover(g.recover))

		e.Use(g.extra...)
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

// Start 启动服务并阻塞到它停止。由 xtow.Run 调用。
func (g *XGin) Start(ctx context.Context) error {
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("xgin: invalid config: %w", err)
	}
	// 在这里设而不是在装配里：装配可能发生在配置加载之前，
	// 那时读到的是默认值，配置里写的 Mode 从此再也不生效
	gin.SetMode(cfg.Mode)
	g.build()

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	srv := g.newServer(addr)

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

	slog.Info("xgin listening", "addr", addr, "tls", cfg.tlsEnabled(), "h2c", cfg.UseH2C && !cfg.tlsEnabled())

	var err error
	if cfg.tlsEnabled() {
		err = srv.ListenAndServeTLS(cfg.CertFile, cfg.KeyFile)
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

	// 自带一个上限，不完全依赖调用方的 ctx：框架给的停止预算是所有组件共享的，
	// HTTP 服务占满了它，后面的数据库、缓存就没时间关了
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(stopCtx); err != nil {
		return fmt.Errorf("xgin: graceful shutdown failed: %w", err)
	}
	return nil
}

// newServer 按配置构建 http.Server
func (g *XGin) newServer(addr string) *http.Server {
	handler := g.engine.Handler()
	if cfg.UseH2C && !cfg.tlsEnabled() {
		handler = h2c.NewHandler(handler, &http2.Server{})
	}

	// 超时必须显式设置：零值是「永不超时」，慢客户端可以一直占着连接，
	// 连接数打满之后服务整体不可用
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
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
