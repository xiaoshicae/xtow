package xredis

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/redis/go-redis/extra/redisotel/v9"
	"github.com/redis/go-redis/v9"

	"github.com/xiaoshicae/xtow/registry"
	"github.com/xiaoshicae/xtow/xclient"
	"github.com/xiaoshicae/xtow/xerror"
	"github.com/xiaoshicae/xtow/xmetric"
	"github.com/xiaoshicae/xtow/xutil"
)

const (
	// pingAttempts 建连验证的尝试次数
	pingAttempts = 3

	// fallbackPingTimeout 配置里推算不出预算时的兜底超时
	fallbackPingTimeout = time.Second
)

// pingInterval 两次尝试之间的间隔。是变量而不是常量，只为让测试能调短——
// 连不上的用例要跑满整轮重试，按一秒算一次就是几十秒。
var pingInterval = time.Second

// New 按配置建一个 Redis 实例，不触碰任何全局变量。
//
// 会先 Ping 一次确认连得上：地址写错、密码不对这类问题应该在启动时暴露，
// 而不是等到线上第一次读缓存。
//
// ctx 限定这轮建连验证的生命期：连不上时要走满一轮重试，
// 收到退出信号就该当场放弃，而不是让进程卡在那里。
func New(ctx context.Context, cfg ClientConfig) (*redis.Client, io.Closer, error) {
	if err := cfg.validate(); err != nil {
		return nil, nil, xerror.Newf("xredis", "config", "invalid config: %w", err)
	}

	client := redis.NewClient(&redis.Options{
		Addr:            cfg.Addr,
		Username:        cfg.Username,
		Password:        cfg.Password,
		DB:              cfg.DB,
		DialTimeout:     cfg.DialTimeout,
		ReadTimeout:     cfg.ReadTimeout,
		WriteTimeout:    cfg.WriteTimeout,
		PoolSize:        cfg.PoolSize,
		MinIdleConns:    cfg.MinIdleConns,
		MaxIdleConns:    cfg.MaxIdleConns,
		MaxActiveConns:  cfg.MaxActiveConns,
		PoolTimeout:     cfg.PoolTimeout,
		ConnMaxIdleTime: cfg.ConnMaxIdleTime,
		ConnMaxLifetime: cfg.ConnMaxLifetime,
		MaxRetries:      cfg.MaxRetries,
		MinRetryBackoff: cfg.MinRetryBackoff,
		MaxRetryBackoff: cfg.MaxRetryBackoff,

		// 让请求的 context 管住 socket 读写。
		//
		// go-redis 默认不这么做：不开的话，每个命令用的是 ReadTimeout /
		// WriteTimeout 这组固定值，调用方给的 deadline 只是摆设——
		// 一个 20ms 超时的请求照样会在一个慢 Redis 上等满几百毫秒，
		// 上游的超时预算和级联保护跟着一起失效。
		ContextTimeoutEnabled: true,
	})

	// 这之后任何一步失败都要关掉 client，否则漏一个连接池
	ok := false
	defer func() {
		if !ok {
			client.Close()
		}
	}()

	if cfg.Trace {
		if err := redisotel.InstrumentTracing(client); err != nil {
			return nil, nil, xerror.Newf("xredis", "new", "install tracing hook: %w", err)
		}
	}

	if err := ping(ctx, client, cfg); err != nil {
		// 不带上原始错误的全部内容：go-redis 的认证错误里可能回显配置
		return nil, nil, xerror.Newf("xredis", "connect", "cannot reach %s: %w", cfg.Addr, err)
	}

	// 日志里只写地址和库号，密码不进日志——所以也就不需要脱敏
	slog.Info("xredis connected", "addr", cfg.Addr, "db", cfg.DB, "min_idle_conns", cfg.MinIdleConns)

	ok = true
	return client, &clientCloser{client: client, addr: cfg.Addr}, nil
}

// ping 建连验证，失败按固定间隔重试；parent 取消时立即放弃
func ping(parent context.Context, client *redis.Client, cfg ClientConfig) error {
	return xutil.Retry(parent, pingAttempts, pingTimeout(cfg), pingInterval, func(ctx context.Context) error {
		return client.Ping(ctx).Err()
	})
}

// pingTimeout 单次 Ping 的超时：建连加一个往返
func pingTimeout(cfg ClientConfig) time.Duration {
	d := cfg.DialTimeout + cfg.ReadTimeout
	if d <= 0 {
		return fallbackPingTimeout
	}
	return d
}

type clientCloser struct {
	client *redis.Client
	addr   string
}

func (c *clientCloser) Close() error {
	if err := c.client.Close(); err != nil {
		return xerror.Newf("xredis", "close", "close %s failed: %w", c.addr, err)
	}
	return nil
}

// ---- 全局实例 ----

// reg 具名实例注册表，与 xgorm / xcache 共用同一份实现，语义因此一致
var reg = xclient.NewRegistry[*redis.Client]("xredis", ConfigKey)

// C 取一个 Redis 实例，不带参数时取名为 default 的那个。
//
// 取不到直接 panic，理由见 xclient.Registry.Get。
// 不提供 CWithCtx：go-redis 的每个方法本来就收 ctx，再包一层没有意义。
//
// 可选依赖（配了就用、没配就跳过）用 Has 先判断。
func C(name ...string) *redis.Client { return reg.Get(name...) }

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
		slog.Info("xredis ready", "instances", names)
	}
	if metricEnabled(cfg.Clients) {
		// 不让启动失败：指标导不出去是可观测性问题，不该拦住服务起来
		if _, err := xmetric.RegisterAs(newPoolCollector(xmetric.Namespace(), xmetric.ConstLabels(), poolStats)); err != nil {
			slog.Error("xredis failed to register pool metrics", "error", err)
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
func poolStats() map[string]*redis.PoolStats {
	out := map[string]*redis.PoolStats{}
	for _, name := range reg.Names() {
		if c, ok := reg.Lookup(name); ok {
			out[name] = c.PoolStats()
		}
	}
	return out
}
