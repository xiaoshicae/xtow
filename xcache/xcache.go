package xcache

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/dgraph-io/ristretto/v2"

	"github.com/xiaoshicae/xtow/registry"
	"github.com/xiaoshicae/xtow/xclient"
	"github.com/xiaoshicae/xtow/xerror"
)

// Cache 就是原生的 ristretto 缓存，这里只是给它起个短名字。
//
// 值类型是 any：配置驱动的全局实例没法带上业务类型。
// 想要类型安全就自己 ristretto.NewCache[string, *User] 建一个，本包不挡路。
type Cache = ristretto.Cache[string, any]

// New 按配置建一个缓存实例，不触碰任何全局变量
func New(cfg ClientConfig) (*Cache, io.Closer, error) {
	if err := cfg.validate(); err != nil {
		return nil, nil, xerror.Newf("xcache", "config", "invalid config: %w", err)
	}

	c, err := ristretto.NewCache(&ristretto.Config[string, any]{
		NumCounters: cfg.NumCounters,
		MaxCost:     cfg.MaxCost,
		BufferItems: cfg.BufferItems,
		// ristretto 默认会把每条的内部开销（56 字节）加进 cost，
		// 于是 cost=1 的写入实际占 57。MaxCost: 100000 配出来的缓存
		// 只能存下一千七百多条，配置里写的数字和实际容量差着五十多倍，
		// 而且没有任何地方会提到这件事。关掉它，cost 才是 cost
		IgnoreInternalCost: true,
	})
	if err != nil {
		return nil, nil, xerror.Newf("xcache", "new", "create cache: %w", err)
	}
	return c, closerFunc(c.Close), nil
}

type closerFunc func()

func (f closerFunc) Close() error { f(); return nil }

// ---- 全局实例 ----

// instance 一个缓存实例连同它自己的默认 TTL
type instance struct {
	cache *Cache
	ttl   time.Duration
}

// reg 具名实例注册表，与 xgorm / xredis 共用同一份实现，语义因此一致
var reg = xclient.NewRegistry[instance]("xcache", ConfigKey)

// C 取一个缓存实例，不带参数时取名为 default 的那个。
//
// 取不到直接 panic，理由见 xclient.Registry.Get。
func C(name ...string) *Cache { return reg.Get(name...).cache }

// Has 报告指定实例是否已配置，供可选依赖判断
func Has(name ...string) bool { return reg.Has(name...) }

// Names 返回已配置的实例名
func Names() []string { return reg.Names() }

// DefaultTTL 返回指定实例配置的默认过期时间
func DefaultTTL(name ...string) time.Duration {
	inst, _ := reg.Lookup(name...)
	return inst.ttl
}

// ---- 默认实例上的便利写法 ----
//
// 只作用于名为 default 的实例。具名实例用 C("name") 拿原生对象操作。

// Get 从默认实例读一个值
func Get(key string) (any, bool) { return C().Get(key) }

// Set 往默认实例写一个值，用配置里的 DefaultTTL，cost 为 1。
//
// 返回值是「有没有被收下」，不是「有没有存进去」，这两件事在 ristretto 里不一样：
//
//   - 返回 true 只说明写入请求进了缓冲区。准入策略仍可能判定这个键不值得留，
//     然后悄悄丢掉它，不会有任何返回值或日志提到。
//   - 返回 true 之后立刻 Get 也可能读不到：写入走环形缓冲异步生效，
//     要确定性地读到刚写的值（多半是测试里）得先 Wait。
//   - 返回 false 说明缓冲区满了、这次写入被直接丢弃，是瞬时状态，可以重试。
//
// 所以它适合用来观察「缓存是不是在丢写入」，不适合判断某个键此刻在不在缓存里
// ——那只有 Get 能回答。缓存本来就允许丢，正常业务路径忽略返回值即可。
func Set(key string, value any) bool {
	inst := reg.Get()
	return inst.cache.SetWithTTL(key, value, 1, inst.ttl)
}

// SetWithTTL 往默认实例写一个值并指定过期时间，cost 为 1。返回值含义同 Set。
func SetWithTTL(key string, value any, ttl time.Duration) bool {
	return C().SetWithTTL(key, value, 1, ttl)
}

// Del 从默认实例删一个键
func Del(key string) { C().Del(key) }

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
	closer, err := xclient.Build(ctx, reg, cfg.Clients,
		func(_ context.Context, c ClientConfig) (instance, io.Closer, error) {
			cache, closer, err := New(c)
			return instance{cache: cache, ttl: c.DefaultTTL}, closer, err
		})
	if err != nil {
		return nil, err
	}
	if names := reg.Names(); len(names) > 0 {
		slog.Info("xcache ready", "instances", names)
	}
	return closer, nil
}
