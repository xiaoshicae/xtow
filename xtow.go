// Package xtow 统一读配置、按阶段初始化所有已登记的组件、逆序关闭。
//
// 使用者只需要 import 想用的 contrib 包，然后调用 Run。
package xtow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/xiaoshicae/xtow/internal/config"
	"github.com/xiaoshicae/xtow/registry"
	"github.com/xiaoshicae/xtow/xutil"
)

// Runnable 需要持续运行的东西，通常就是你的服务器。
//
// Start 收到的 ctx 在退出信号到达时被取消，Start 应当据此返回。
// Stop 收到的是一个独立的、带停止预算的 ctx——它不继承那次取消，
// 否则每个关闭动作一进去就被拒绝，等于没有优雅退出。
type Runnable interface {
	Start(context.Context) error
	Stop(context.Context) error
}

// defaultStopTimeout 所有组件共享的停止预算默认值
const defaultStopTimeout = 15 * time.Second

// Run 读配置、初始化全部组件、启动 r，阻塞到退出信号，然后逆序关闭。
func Run(r Runnable, opts ...Option) error {
	o := options{stopTimeout: defaultStopTimeout}
	for _, f := range opts {
		f(&o)
	}
	// 0 在这里不是「不限时」而是「一点都不等」：Stop 会拿到一个已经过期的
	// context，服务当场被切断，后面每个组件的关闭也都在超时状态下跑。
	// 让它在做任何事之前就失败，好过退出时才发现没有优雅退出这回事
	if o.stopTimeout <= 0 {
		return fmt.Errorf("xtow: stop budget must be > 0 (0 is not unlimited, it is no wait at all), got=%v", o.stopTimeout)
	}

	// 退出信号在做任何事之前就接管，配置加载和初始化都在它的保护之内。
	// 装在初始化之后的话，启动期间（连库、连 Redis、Ping 重试）收到 SIGTERM
	// 走的是系统默认处置：进程当场暴毙，已经建好的资源一个都来不及注销——
	// 注册中心里那条记录、那把分布式锁，只能等对端超时过期。
	ctx, stopSignals := notifyShutdown(o)
	defer stopSignals()

	list := o.components
	if list == nil {
		list = registry.Snapshot()
	}

	// 同一档内保持登记顺序，档间按 Stage 升序
	sort.SliceStable(list, func(i, j int) bool { return list[i].Stage < list[j].Stage })

	if err := loadConfigInto(list, o); err != nil {
		return err
	}

	closers, err := initAll(ctx, list, o)
	switch {
	case ctx.Err() != nil:
		// 初始化期间收到退出信号：不启动服务，把已经建好的逆序关干净。
		// 即便 initAll 带回了错误也不往上报——被取消的建连必然失败，
		// 那是按要求退出的结果而不是故障。报上去的话，每次滚动更新
		// 撞上这个窗口都会在面板上留一条「启动失败」。
		attrs := []any{"ready_components", len(closers)}
		if err != nil {
			attrs = append(attrs, "interrupted_init", err)
		}
		o.log().Info("shutdown signal received during init, not starting the server", attrs...)
		return shutdownWithin(o, closers)
	case err != nil:
		return errors.Join(err, shutdownWithin(o, closers))
	}

	runErr := make(chan error, 1)
	go func() { runErr <- safe("server", func() error { return r.Start(ctx) }) }()

	var first error
	var serverExited bool
	select {
	case <-ctx.Done(): // 信号已经由 notifyShutdown 记过日志了
	case e := <-runErr:
		first, serverExited = e, true
	}

	stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(ctx), o.stopTimeout)
	defer stopCancel()
	first = errors.Join(first, safe("server", func() error { return r.Stop(stopCtx) }))

	// 等 Start 真正返回再关其余组件。
	//
	// Stop 返回不等于服务已经停干净：Stop 只负责「让它停」，
	// 有没有等在处理的请求做完是各实现自己的事。不等就往下关的话，
	// 还在跑的请求会摸到已经关掉的数据库和缓存。
	// 同时这也是唯一能拿到 Start 错误的地方——走信号分支时它还没被读过。
	//
	// 服务自己退出时上面那次 select 已经把 runErr 取走了，这里不能再取：
	// channel 里没有第二个值，等下去就是白等满整个停止预算。
	if !serverExited {
		select {
		case e := <-runErr:
			first = errors.Join(first, e)
		case <-stopCtx.Done():
			o.log().Warn("server did not exit within the stop budget, closing the rest anyway", "budget", o.stopTimeout)
		}
	}

	return errors.Join(first, shutdown(stopCtx, closers, o))
}

// shutdownWithin 启动阶段失败时的关闭，自己开一份停止预算。
//
// 这一支没有 stopCtx——服务还没起来，那个 ctx 还没造出来。
// 但预算同样要有：初始化到一半失败时，已经建好的那几个照样可能关不掉。
func shutdownWithin(o options, closers []named) error {
	ctx, cancel := context.WithTimeout(context.Background(), o.stopTimeout)
	defer cancel()
	return shutdown(ctx, closers, o)
}

// loadConfigInto 定位并加载配置。
//
// 点名要的路径（WithConfigPath、--config、XTOW_CONFIG）找不到是错误；
// 约定路径一个都没命中则只告警，全用默认值起——这对一个没有任何外部依赖的
// 服务是合理的。两者的区别与「你要的」和「约定俗成的」一致。
//
// Locate 只在文件确实存在时才返回约定路径，所以下面那次判存只会落在
// 点名要的路径上，不需要再分一次支。
func loadConfigInto(list []registry.Component, o options) error {
	path := o.configPath
	if path == "" {
		path = config.Locate()
	}

	if path == "" {
		o.log().Warn("no config file found, using defaults for everything",
			"searched", config.SearchPaths,
			"or_use", "--"+config.ArgKey+"=<path> or "+config.EnvKey)
		return nil
	}
	if !xutil.FileExist(path) {
		return fmt.Errorf("xtow: config file does not exist: %s", path)
	}

	o.log().Info("loading config", "file", path)
	return config.Load(path, list)
}

type named struct {
	key string
	c   io.Closer
}

func initAll(ctx context.Context, list []registry.Component, o options) ([]named, error) {
	var closers []named
	for _, c := range list {
		if c.Init == nil {
			continue
		}
		// 每个组件之前看一眼：已经决定退出了就别再去连三个库。
		// 打断不了正在跑的那一个——那要靠它自己把 ctx 传下去，
		// 所以 Init 的签名里有 ctx
		if ctx.Err() != nil {
			o.log().Warn("shutdown signal received, skipping init of the remaining components", "ready", len(closers))
			return closers, nil
		}
		// 每次重新取 logger：xlog 就在这个循环里把全局默认 logger 换掉，
		// 它之后的组件应该用新的那个
		o.log().Info("initializing", "component", c.Key)
		cl, err := safeInit(ctx, c)
		if err != nil {
			return closers, fmt.Errorf("%s init failed: %w", c.Key, err)
		}
		if cl != nil {
			closers = append(closers, named{c.Key, cl})
		}
	}
	return closers, nil
}

// shutdown 逆序关闭已初始化的组件，整个过程共享 ctx 里剩下的预算。
//
// 预算必须落到这里才算数：WithStopTimeout 说的是「所有组件共享的停止预算」，
// 而 io.Closer.Close() 没有 ctx，不看着它就等于没有上限——一个连接池
// 关不掉，整个进程就陪着它挂到部署环境来 SIGKILL 为止。
func shutdown(ctx context.Context, closers []named, o options) error {
	var errs []error
	for i := len(closers) - 1; i >= 0; i-- {
		n := closers[i]
		o.log().Info("closing", "component", n.key)
		if err := closeWithin(ctx, n); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// closeWithin 在预算内关掉一个组件，超时就不再等它。
//
// Close 没有 ctx 可传，所以只能另起一个协程去等。超时之后那个协程还挂在
// 原地——这是有意的：强行放弃它，好过让后面每一个组件、以及进程本身，
// 都排在一个关不掉的资源后面。进程马上就要退出了，漏一个协程没有下文。
func closeWithin(ctx context.Context, n named) error {
	done := make(chan error, 1)
	go func() { done <- safe(n.key, n.c.Close) }()

	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("%s: %w", n.key, err)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%s: close did not finish within the stop budget: %w", n.key, ctx.Err())
	}
}

// safeInit 执行组件的 Init 并隔离 panic
//
// 一个组件初始化时炸了，不该把整个进程打穿——它应该变成一个普通的启动错误，
// 让已经初始化的部分有机会被逆序关闭。
func safeInit(ctx context.Context, c registry.Component) (cl io.Closer, err error) {
	defer func() {
		if r := recover(); r != nil {
			cl, err = nil, fmt.Errorf("%s panic: %v", c.Key, r)
		}
	}()
	return c.Init(ctx)
}

func safe(name string, f func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%s panic: %v", name, r)
		}
	}()
	return f()
}

// shutdownSignals 触发优雅退出的信号
var shutdownSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM}

// notifyShutdown 接管退出信号，返回一个收到信号时被取消的 context。
//
// 与 signal.NotifyContext 的差别在于第一个信号之后会把默认处置还回去，
// 于是再发一次信号由系统直接终止进程。这一手是必须的：注册信号处理本身
// 就取消了系统原本的「收到就死」，而框架在初始化和关闭阶段都可能卡在
// 某个不看 ctx 的第三方调用里——没有这条逃生口，那些情况下进程会变成
// 只有 kill -9 才能收掉，K8s 得等满整个终止宽限期。
//
// 返回的 stop 用于正常退出时注销，它会等接管协程真正退出再返回，
// 保证 Run 返回之后信号已经回到默认处置（同一进程里反复 Run 的测试依赖这点）。
func notifyShutdown(o options) (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, shutdownSignals...)

	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case s := <-ch:
			// 先还原默认处置再取消：这中间要是又来一个信号，
			// 要的就是它直接把进程终止掉，而不是被一个已经没人看的 handler 收走
			signal.Stop(ch)
			o.log().Info("shutdown signal received, closing gracefully; send it again to terminate now",
				"signal", s.String())
			cancel()
		case <-ctx.Done():
			signal.Stop(ch)
		}
	}()

	return ctx, func() {
		cancel()
		<-done
	}
}

type options struct {
	configPath  string
	stopTimeout time.Duration

	// logger 留空表示每次取 slog.Default()。
	//
	// 必须延迟到用的时候才取：xlog 是在 StageLog 初始化时才调用
	// slog.SetDefault 的，Run 开头捕获一次的话，后面所有框架日志
	// 都还写在初始化之前那个默认 logger 上。
	logger *slog.Logger

	// components 指定要装配的组件，留空则取全局登记板。
	//
	// 目前只有测试在用：它让「只装配一部分」成为可能，而这正是
	// 全局登记板本身做不到的事。等有真实需求时再导出。
	components []registry.Component
}

// log 取当前该用的 logger。未显式指定时每次都取标准库的全局默认值，
// 这样 xlog 初始化完成后，框架自己的日志会自动跟着走新的那个。
func (o options) log() *slog.Logger {
	if o.logger != nil {
		return o.logger
	}
	return slog.Default()
}

type Option func(*options)

func WithConfigPath(p string) Option { return func(o *options) { o.configPath = p } }

// WithStopTimeout 设置所有组件共享的停止预算，默认 15s。必须大于 0——
// 0 不是「不限时」而是「一点都不等」，Run 会直接返回错误。
func WithStopTimeout(d time.Duration) Option { return func(o *options) { o.stopTimeout = d } }
func WithLogger(l *slog.Logger) Option       { return func(o *options) { o.logger = l } }

// MustRun 同 Run，出错直接退出。
func MustRun(r Runnable, opts ...Option) {
	if err := Run(r, opts...); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// withComponents 指定要装配的组件，不读全局登记板。
//
// 目前只有测试在用：它让「只装配一部分」成为可能，而这正是全局登记板
// 本身做不到的事。等有真实需求时再导出。
func withComponents(cs ...registry.Component) Option {
	return func(o *options) { o.components = cs }
}
