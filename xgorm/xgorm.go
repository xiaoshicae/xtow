package xgorm

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/xiaoshicae/xtow/registry"
	"github.com/xiaoshicae/xtow/xmetric"
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
// 返回的 io.Closer 关闭底层连接池。建连失败时不会留下连接池——
// gorm.Open 在自动 ping 失败时不关它自己建的池子，那会漏一个常驻协程。
func New(cfg ClientConfig) (*gorm.DB, io.Closer, error) {
	if err := cfg.validate(); err != nil {
		return nil, nil, fmt.Errorf("xgorm: 配置有误: %w", err)
	}

	dsn, info, err := resolveDSN(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("xgorm: %w", err)
	}

	gormCfg := &gorm.Config{}
	if cfg.Log {
		gormCfg.Logger = newGormLogger(cfg)
	}

	db, err := gorm.Open(dialector(cfg.Driver, dsn), gormCfg)
	if err != nil {
		// gorm 自己会在 Initialize 和自动 ping 失败时关掉池子（v1.31 起），
		// 这里再关一次是兜底：*sql.DB 允许重复 Close，代价是一次空调用，
		// 而万一哪个版本不关，漏的是一个再也不会退出的常驻协程
		closePool(db)
		return nil, nil, fmt.Errorf("xgorm: 连接失败 %s: %w", info.Addr, err)
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
		return nil, nil, fmt.Errorf("xgorm: 取底层连接池失败: %w", err)
	}
	pool.SetMaxOpenConns(cfg.MaxOpenConns)
	pool.SetMaxIdleConns(cfg.MaxIdleConns)
	pool.SetConnMaxLifetime(cfg.MaxLifetime)
	pool.SetConnMaxIdleTime(cfg.MaxIdleTime)

	if err := ping(pool, cfg); err != nil {
		return nil, nil, fmt.Errorf("xgorm: 连不上 %s: %w", info.Addr, err)
	}

	if cfg.Trace {
		if err := installTracing(db, info); err != nil {
			return nil, nil, fmt.Errorf("xgorm: 挂链路回调失败: %w", err)
		}
	}

	logConn(info, cfg)

	ok = true
	return db, &poolCloser{pool: pool, info: info}, nil
}

func dialector(d Driver, dsn string) gorm.Dialector {
	if d == DriverMySQL {
		return mysql.Open(dsn)
	}
	return postgres.Open(dsn)
}

// ping 建连验证，失败按固定间隔重试
//
// 带 context 是为了让启动期收到的退出信号能立即生效：不可中断的重试
// 会让进程必须等满 attempts×interval 才肯退出。
func ping(pool *sql.DB, cfg ClientConfig) error {
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
		last = pool.PingContext(attemptCtx)
		attemptCancel()
		if last == nil {
			return nil
		}
	}
	return last
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
		slog.Warn("xgorm 关闭连接池失败", "错误", cerr)
	}
}

type poolCloser struct {
	pool *sql.DB
	info connInfo
}

func (c *poolCloser) Close() error {
	if err := c.pool.Close(); err != nil {
		return fmt.Errorf("xgorm: 关闭 %s 失败: %w", c.info.Addr, err)
	}
	return nil
}

// ---- 全局实例 ----

var (
	mu      sync.RWMutex
	clients = map[string]*gorm.DB{}
)

// C 取一个 GORM 实例，不带参数时取名为 default 的那个。
//
// 取不到直接 panic。返回 nil 不会让程序走得更远——*gorm.DB 上任何方法在 nil 上
// 都是空指针解引用，只是把同一个 panic 推迟到调用方第一次用它的时候，
// 而那里的栈里只剩 "invalid memory address"，看不出根因是配置没配。
// 这是启动期的配置问题，不是运行期要处理的错误。
//
// 可选依赖（配了就用、没配就跳过）用 Has 先判断。
func C(name ...string) *gorm.DB {
	db := get(nameOf(name))
	if db == nil {
		panic(missingMsg(nameOf(name)))
	}
	return db
}

// CWithCtx 取实例并绑定 ctx，链路和超时才能传到下游。
//
// GORM 必须这样传 ctx（不像 go-redis 每个方法都收 ctx），所以这个方法是必要的。
func CWithCtx(ctx context.Context, name ...string) *gorm.DB {
	return C(name...).WithContext(ctx)
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

func get(name string) *gorm.DB {
	mu.RLock()
	defer mu.RUnlock()
	return clients[name]
}

// missingMsg 只报「没找到」帮助有限：名字写错和整块没配是两个不同的问题，
// 把实际配了哪些列出来，两者一眼可分
func missingMsg(want string) string {
	got := Names()
	if len(got) == 0 {
		return fmt.Sprintf("xgorm: 没有名为 %q 的实例，而且一个实例都没配——检查配置里的 %s 块", want, ConfigKey)
	}
	return fmt.Sprintf("xgorm: 没有名为 %q 的实例，已配置的有 [%s]", want, strings.Join(got, " "))
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
	built := map[string]*gorm.DB{}
	var closers []io.Closer

	// 名字排序后再建，让失败顺序可复现，也让日志顺序稳定
	for _, name := range slices.Sorted(maps.Keys(cfg.Clients)) {
		c := cfg.Clients[name]
		db, closer, err := New(c)
		if err != nil {
			// 已经建好的必须关掉：Init 返回错误时框架拿不到 closer，
			// 不自己收拾就会漏掉那几个连接池
			closeAll(closers)
			return nil, fmt.Errorf("实例 %q: %w", name, err)
		}
		built[name] = db
		closers = append(closers, closer)
	}

	mu.Lock()
	clients = built
	mu.Unlock()

	if len(built) > 0 {
		slog.Info("xgorm 就绪", "实例", slices.Sorted(maps.Keys(built)))
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
func poolStats() map[string]sql.DBStats {
	mu.RLock()
	defer mu.RUnlock()

	out := make(map[string]sql.DBStats, len(clients))
	for name, db := range clients {
		pool, err := db.DB()
		if err != nil || pool == nil {
			continue
		}
		out[name] = pool.Stats()
	}
	return out
}

type groupCloser struct{ closers []io.Closer }

func (g *groupCloser) Close() error {
	mu.Lock()
	clients = map[string]*gorm.DB{}
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
