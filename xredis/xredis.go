package xredis

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/extra/redisotel/v9"
	"github.com/redis/go-redis/v9"

	"github.com/xiaoshicae/xtow/registry"
	"github.com/xiaoshicae/xtow/xmetric"
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
func New(cfg ClientConfig) (*redis.Client, io.Closer, error) {
	if err := cfg.validate(); err != nil {
		return nil, nil, fmt.Errorf("xredis: 配置有误: %w", err)
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
			return nil, nil, fmt.Errorf("xredis: 挂链路钩子失败: %w", err)
		}
	}

	if err := ping(client, cfg); err != nil {
		// 不带上原始错误的全部内容：go-redis 的认证错误里可能回显配置
		return nil, nil, fmt.Errorf("xredis: 连不上 %s: %w", cfg.Addr, err)
	}

	// 日志里只写地址和库号，密码不进日志——所以也就不需要脱敏
	slog.Info("xredis 连接就绪", "地址", cfg.Addr, "库", cfg.DB, "最小空闲", cfg.MinIdleConns)

	ok = true
	return client, &clientCloser{client: client, addr: cfg.Addr}, nil
}

// ping 建连验证，失败按固定间隔重试
//
// 带 context 是为了让启动期收到的退出信号能立即生效。
func ping(client *redis.Client, cfg ClientConfig) error {
	timeout := pingTimeout(cfg)
	budget := timeout*pingAttempts + pingInterval*(pingAttempts-1)

	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	var last error
	for i := 0; i < pingAttempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return last
			case <-time.After(pingInterval):
			}
		}
		attemptCtx, attemptCancel := context.WithTimeout(ctx, timeout)
		last = client.Ping(attemptCtx).Err()
		attemptCancel()
		if last == nil {
			return nil
		}
	}
	return last
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
		return fmt.Errorf("xredis: 关闭 %s 失败: %w", c.addr, err)
	}
	return nil
}

// ---- 全局实例 ----

var (
	mu      sync.RWMutex
	clients = map[string]*redis.Client{}
)

// C 取一个 Redis 实例，不带参数时取名为 default 的那个。
//
// 取不到直接 panic，理由同 xgorm：返回 nil 只是把同一个 panic 推迟到
// 调用方第一次用它的时候，那里的栈里看不出根因是配置没配。
//
// 可选依赖（配了就用、没配就跳过）用 Has 先判断。
func C(name ...string) *redis.Client {
	c := get(nameOf(name))
	if c == nil {
		panic(missingMsg(nameOf(name)))
	}
	return c
}

// Has 报告指定实例是否已配置，供可选依赖判断
func Has(name ...string) bool { return get(nameOf(name)) != nil }

// Names 返回已配置的实例名
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	return slices.Sorted(maps.Keys(clients))
}

func nameOf(name []string) string {
	if len(name) > 0 {
		return name[0]
	}
	return DefaultName
}

func get(name string) *redis.Client {
	mu.RLock()
	defer mu.RUnlock()
	return clients[name]
}

func missingMsg(want string) string {
	got := Names()
	if len(got) == 0 {
		return fmt.Sprintf("xredis: 没有名为 %q 的实例，而且一个实例都没配——检查配置里的 %s 块", want, ConfigKey)
	}
	return fmt.Sprintf("xredis: 没有名为 %q 的实例，已配置的有 [%s]", want, strings.Join(got, " "))
}

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

func initAll() (io.Closer, error) {
	built := map[string]*redis.Client{}
	var closers []io.Closer

	// 名字排序后再建，让失败顺序可复现
	for _, name := range slices.Sorted(maps.Keys(cfg.Clients)) {
		client, closer, err := New(cfg.Clients[name])
		if err != nil {
			// Init 返回错误时框架拿不到 closer，已经建好的必须自己收拾
			closeAll(closers)
			return nil, fmt.Errorf("实例 %q: %w", name, err)
		}
		built[name] = client
		closers = append(closers, closer)
	}

	mu.Lock()
	clients = built
	mu.Unlock()

	if len(built) > 0 {
		slog.Info("xredis 就绪", "实例", slices.Sorted(maps.Keys(built)))
	}
	if metricEnabled(cfg.Clients) {
		xmetric.Register(newPoolCollector(xmetric.Namespace(), xmetric.ConstLabels(), poolStats))
	}
	return &groupCloser{closers: closers}, nil
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
	mu.RLock()
	defer mu.RUnlock()

	out := make(map[string]*redis.PoolStats, len(clients))
	for name, c := range clients {
		out[name] = c.PoolStats()
	}
	return out
}

type groupCloser struct{ closers []io.Closer }

func (g *groupCloser) Close() error {
	mu.Lock()
	clients = map[string]*redis.Client{}
	mu.Unlock()
	return closeAll(g.closers)
}

func closeAll(closers []io.Closer) error {
	var errs []error
	for i := len(closers) - 1; i >= 0; i-- {
		if err := closers[i].Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
