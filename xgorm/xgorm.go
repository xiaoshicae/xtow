package xgorm

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"time"

	"gorm.io/gorm"

	"github.com/xiaoshicae/xtow/registry"
	"github.com/xiaoshicae/xtow/xclient"
	"github.com/xiaoshicae/xtow/xmetric"
	"github.com/xiaoshicae/xtow/xutil"
)

const (
	// pingAttempts 建连验证的尝试次数
	pingAttempts = 3

	// fallbackPingTimeout 配置里推算不出预算时，单次 Ping 的兜底超时
	fallbackPingTimeout = time.Second
)

// pingInterval 两次尝试之间的间隔。是变量而不是常量，只为让测试能调短——
// 连不上的用例要跑满整轮重试，按一秒算一次就是几十秒。
var pingInterval = time.Second

// New 按配置建一个 GORM 实例，不触碰任何全局变量。
//
// ctx 限定建连验证的生命期：地址不通时这里要走满一轮 Ping 重试，
// 收到退出信号就该当场放弃，而不是让进程卡在一个注定连不上的库上。
//
// 返回的 io.Closer 关闭底层连接池。建连失败时不会留下连接池——
// gorm.Open 在自动 ping 失败时不关它自己建的池子，那会漏一个常驻协程。
func New(ctx context.Context, cfg ClientConfig) (*gorm.DB, io.Closer, error) {
	if err := cfg.validate(); err != nil {
		return nil, nil, fmt.Errorf("xgorm: invalid config: %w", err)
	}

	dsn, info, err := resolveDSN(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("xgorm: %w", err)
	}

	gormCfg := &gorm.Config{}
	if cfg.Log {
		gormCfg.Logger = newGormLogger(cfg)
	}

	dialect, known := lookupDialect(cfg.Driver)
	if !known {
		return nil, nil, fmt.Errorf("xgorm: %w", unknownDriver(cfg.Driver))
	}

	db, err := gorm.Open(dialect.Open(dsn), gormCfg)
	if err != nil {
		// gorm 自己会在 Initialize 和自动 ping 失败时关掉池子（v1.31 起），
		// 这里再关一次是兜底：*sql.DB 允许重复 Close，代价是一次空调用，
		// 而万一哪个版本不关，漏的是一个再也不会退出的常驻协程
		closePool(db)
		return nil, nil, fmt.Errorf("xgorm: connect to %s failed: %w", info.Addr, err)
	}

	// 从这里往后的每一步都是我们自己的，失败了没人替我们收拾：
	// 连接池已经活着，不关就漏一个常驻协程
	ok := false
	defer func() {
		if !ok {
			closePool(db)
		}
	}()

	pool, err := db.DB()
	if err != nil {
		return nil, nil, fmt.Errorf("xgorm: get underlying pool: %w", err)
	}
	pool.SetMaxOpenConns(cfg.MaxOpenConns)
	pool.SetMaxIdleConns(cfg.MaxIdleConns)
	pool.SetConnMaxLifetime(cfg.MaxLifetime)
	pool.SetConnMaxIdleTime(cfg.MaxIdleTime)

	if err := ping(ctx, pool, cfg); err != nil {
		return nil, nil, fmt.Errorf("xgorm: cannot reach %s: %w", info.Addr, err)
	}

	if cfg.Trace {
		if err := installTracing(db, info); err != nil {
			return nil, nil, fmt.Errorf("xgorm: install tracing callbacks: %w", err)
		}
	}

	logConn(info, cfg)

	ok = true
	return db, &poolCloser{pool: pool, info: info}, nil
}

// ping 建连验证，失败按固定间隔重试；ctx 取消时立即放弃
func ping(ctx context.Context, pool *sql.DB, cfg ClientConfig) error {
	return xutil.Retry(ctx, pingAttempts, pingTimeout(cfg), pingInterval, pool.PingContext)
}

// pingTimeout 单次 Ping 的超时
//
// 不能只用建连超时：Ping 的耗时是建连加一个往返，
// 拿建连预算当整体预算，连接刚建成就会被判超时。
func pingTimeout(cfg ClientConfig) time.Duration {
	d := cfg.DialTimeout
	if cfg.Driver == DriverMySQL {
		d += cfg.MySQL.ReadTimeout
	}
	if d <= 0 {
		return fallbackPingTimeout
	}
	return d
}

// closePool 关掉底层连接池，用于建连失败时兜底
func closePool(db *gorm.DB) {
	if db == nil {
		return
	}
	pool, err := db.DB()
	if err != nil || pool == nil {
		return
	}
	if cerr := pool.Close(); cerr != nil {
		slog.Warn("xgorm failed to close the pool", "error", cerr)
	}
}

type poolCloser struct {
	pool *sql.DB
	info ConnInfo
}

func (c *poolCloser) Close() error {
	if err := c.pool.Close(); err != nil {
		return fmt.Errorf("xgorm: close %s failed: %w", c.info.Addr, err)
	}
	return nil
}

// ---- 全局实例 ----

// reg 具名实例注册表。取实例、找不到时的报错、关闭时摘干净，
// 这些语义在 xgorm / xredis / xcache 之间必须一致，所以共用一份实现。
var reg = xclient.NewRegistry[*gorm.DB]("xgorm", ConfigKey)

// C 取一个 GORM 实例，不带参数时取名为 default 的那个。
//
// 取不到直接 panic，理由见 xclient.Registry.Get：返回 nil 只会把同一个 panic
// 推迟到调用方第一次用它的时候，而那里看不出根因是配置没配。
//
// 可选依赖（配了就用、没配就跳过）用 Has 先判断。
func C(name ...string) *gorm.DB { return reg.Get(name...) }

// CWithCtx 取实例并绑定 ctx，链路和超时才能传到下游。
//
// GORM 必须这样传 ctx（不像 go-redis 每个方法都收 ctx），所以这个方法是必要的。
func CWithCtx(ctx context.Context, name ...string) *gorm.DB {
	return C(name...).WithContext(ctx)
}

// Has 报告指定实例是否已配置，供可选依赖判断
func Has(name ...string) bool { return reg.Has(name...) }

// Names 返回已配置的实例名
func Names() []string { return reg.Names() }

// ---- 登记 ----

var cfg = DefaultConfig()

// init 只登记，不初始化。真正的初始化由框架在 StageClient 执行。
func init() {
	registry.Register(registry.Component{
		Key:    ConfigKey,
		Stage:  registry.StageClient,
		Config: &cfg,
		Init:   initAll,
	})
}

func initAll(ctx context.Context) (io.Closer, error) {
	closer, err := xclient.Build(ctx, reg, cfg.Clients, New)
	if err != nil {
		return nil, err
	}

	if names := reg.Names(); len(names) > 0 {
		slog.Info("xgorm ready", "instances", names)
	}
	if metricEnabled(cfg.Clients) {
		// 不让启动失败：指标导不出去是可观测性问题，不该拦住服务起来
		if _, err := xmetric.RegisterAs(newPoolCollector(xmetric.Namespace(), xmetric.ConstLabels(), poolStats)); err != nil {
			slog.Error("xgorm failed to register pool metrics", "error", err)
		}
	}
	return closer, nil
}

// metricEnabled 任一实例开了指标就注册——collector 是进程级的一个
func metricEnabled(m map[string]ClientConfig) bool {
	for _, c := range m {
		if c.Metric {
			return true
		}
	}
	return false
}

// poolStats 读各实例连接池的实时状态
func poolStats() map[string]sql.DBStats {
	out := map[string]sql.DBStats{}
	for _, name := range reg.Names() {
		db, ok := reg.Lookup(name)
		if !ok {
			continue
		}
		pool, err := db.DB()
		if err != nil || pool == nil {
			continue
		}
		out[name] = pool.Stats()
	}
	return out
}
