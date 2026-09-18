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

// Run 读配置、初始化全部组件、启动 r，阻塞到退出信号，然后逆序关闭。
func Run(r Runnable, opts ...Option) error {
	o := options{
		stopTimeout: 15 * time.Second,
		logger:      slog.Default(),
	}
	for _, f := range opts {
		f(&o)
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

	closers, err := initAll(list, o.logger)
	if err != nil {
		return errors.Join(err, shutdown(closers, o))
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	runErr := make(chan error, 1)
	go func() { runErr <- safe("server", func() error { return r.Start(ctx) }) }()

	var first error
	select {
	case <-ctx.Done():
		o.logger.Info("收到退出信号")
	case e := <-runErr:
		first = e
	}

	stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(ctx), o.stopTimeout)
	defer stopCancel()
	first = errors.Join(first, safe("server", func() error { return r.Stop(stopCtx) }))

	return errors.Join(first, shutdown(closers, o))
}

// loadConfigInto 定位并加载配置。
//
// 显式指定的路径找不到是错误——那是使用者点名要的；
// 自动查找一个都没命中则只告警，全用默认值起，这对一个没有任何外部依赖的
// 服务是合理的。两者的区别与「你要的」和「约定俗成的」一致。
func loadConfigInto(list []registry.Component, o options) error {
	path, explicit := o.configPath, o.configPath != ""
	if !explicit {
		path = config.Locate()
		explicit = path != "" && !isSearchPath(path)
	}

	if path == "" {
		o.logger.Warn("未找到配置文件，全部使用默认值",
			"查找过的位置", config.SearchPaths,
			"也可用", "--"+config.ArgKey+"=<path> 或 "+config.EnvKey)
		return nil
	}
	if !xutil.FileExist(path) {
		return fmt.Errorf("xtow: 指定的配置文件不存在: %s", path)
	}

	o.logger.Info("加载配置", "文件", path)
	return config.Load(path, list)
}

// isSearchPath 判断路径是不是自动查找命中的约定位置
func isSearchPath(path string) bool {
	for _, p := range config.SearchPaths {
		if p == path {
			return true
		}
	}
	return false
}

type named struct {
	key string
	c   io.Closer
}

func initAll(list []registry.Component, logger *slog.Logger) ([]named, error) {
	var closers []named
	for _, c := range list {
		if c.Init == nil {
			continue
		}
		logger.Info("初始化", "组件", c.Key)
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
		o.logger.Info("关闭", "组件", n.key)
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
	logger      *slog.Logger

	// components 指定要装配的组件，留空则取全局登记板。
	//
	// 目前只有测试在用：它让「只装配一部分」成为可能，而这正是
	// 全局登记板本身做不到的事。等有真实需求时再导出。
	components []registry.Component
}

type Option func(*options)

func WithConfigPath(p string) Option         { return func(o *options) { o.configPath = p } }
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
