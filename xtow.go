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
		return fmt.Errorf("xtow: 停止预算必须大于 0（0 不是不限时，是一点都不等），got=%v", o.stopTimeout)
	}

	list := o.components
	if list == nil {
		list = registry.Snapshot()
	}

	// 同一档内保持登记顺序，档间按 Stage 升序
	sort.SliceStable(list, func(i, j int) bool { return list[i].Stage < list[j].Stage })

	if err := loadConfigInto(list, o); err != nil {
		return err
	}

	closers, err := initAll(list, o)
	if err != nil {
		return errors.Join(err, shutdown(closers, o))
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	runErr := make(chan error, 1)
	go func() { runErr <- safe("server", func() error { return r.Start(ctx) }) }()

	var first error
	var serverExited bool
	select {
	case <-ctx.Done():
		o.log().Info("收到退出信号")
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
			o.log().Warn("服务没有在停止预算内退出，继续关闭其余组件", "预算", o.stopTimeout)
		}
	}

	return errors.Join(first, shutdown(closers, o))
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
		o.log().Warn("未找到配置文件，全部使用默认值",
			"查找过的位置", config.SearchPaths,
			"也可用", "--"+config.ArgKey+"=<path> 或 "+config.EnvKey)
		return nil
	}
	if !xutil.FileExist(path) {
		return fmt.Errorf("xtow: 指定的配置文件不存在: %s", path)
	}

	o.log().Info("加载配置", "文件", path)
	return config.Load(path, list)
}

type named struct {
	key string
	c   io.Closer
}

func initAll(list []registry.Component, o options) ([]named, error) {
	var closers []named
	for _, c := range list {
		if c.Init == nil {
			continue
		}
		// 每次重新取：xlog 就在这个循环里把全局默认 logger 换掉，
		// 它之后的组件应该用新的那个
		o.log().Info("初始化", "组件", c.Key)
		cl, err := safeInit(c)
		if err != nil {
			return closers, fmt.Errorf("%s 初始化失败: %w", c.Key, err)
		}
		if cl != nil {
			closers = append(closers, named{c.Key, cl})
		}
	}
	return closers, nil
}

func shutdown(closers []named, o options) error {
	var errs []error
	for i := len(closers) - 1; i >= 0; i-- {
		n := closers[i]
		o.log().Info("关闭", "组件", n.key)
		if err := safe(n.key, func() error { return n.c.Close() }); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", n.key, err))
		}
	}
	return errors.Join(errs...)
}

// safeInit 执行组件的 Init 并隔离 panic
//
// 一个组件初始化时炸了，不该把整个进程打穿——它应该变成一个普通的启动错误，
// 让已经初始化的部分有机会被逆序关闭。
func safeInit(c registry.Component) (cl io.Closer, err error) {
	defer func() {
		if r := recover(); r != nil {
			cl, err = nil, fmt.Errorf("%s panic: %v", c.Key, r)
		}
	}()
	return c.Init()
}

func safe(name string, f func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%s panic: %v", name, r)
		}
	}()
	return f()
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
