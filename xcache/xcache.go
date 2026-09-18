package xcache

import (
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dgraph-io/ristretto/v2"

	"github.com/xiaoshicae/xtow/registry"
)

// Cache 就是原生的 ristretto 缓存，这里只是给它起个短名字。
//
// 值类型是 any：配置驱动的全局实例没法带上业务类型。
// 想要类型安全就自己 ristretto.NewCache[string, *User] 建一个，本包不挡路。
type Cache = ristretto.Cache[string, any]

// New 按配置建一个缓存实例，不触碰任何全局变量
func New(cfg ClientConfig) (*Cache, io.Closer, error) {
	if err := cfg.validate(); err != nil {
		return nil, nil, fmt.Errorf("xcache: 配置有误: %w", err)
	}

	c, err := ristretto.NewCache(&ristretto.Config[string, any]{
		NumCounters: cfg.NumCounters,
		MaxCost:     cfg.MaxCost,
		BufferItems: cfg.BufferItems,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("xcache: 建缓存失败: %w", err)
	}
	return c, closerFunc(c.Close), nil
}

type closerFunc func()

func (f closerFunc) Close() error { f(); return nil }

// ---- 全局实例 ----

type instance struct {
	cache *Cache
	ttl   time.Duration
}

var (
	mu        sync.RWMutex
	instances = map[string]instance{}
)

// C 取一个缓存实例，不带参数时取名为 default 的那个。
//
// 取不到直接 panic，理由同 xgorm / xredis：返回 nil 只是把同一个 panic
// 推迟到调用方第一次用它的时候，那里的栈里看不出根因是配置没配。
func C(name ...string) *Cache {
	inst, ok := get(nameOf(name))
	if !ok {
		panic(missingMsg(nameOf(name)))
	}
	return inst.cache
}

// Has 报告指定实例是否已配置，供可选依赖判断
func Has(name ...string) bool {
	_, ok := get(nameOf(name))
	return ok
}

// Names 返回已配置的实例名
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	return slices.Sorted(maps.Keys(instances))
}

// DefaultTTL 返回指定实例配置的默认过期时间
func DefaultTTL(name ...string) time.Duration {
	inst, _ := get(nameOf(name))
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
	inst, ok := get(DefaultName)
	if !ok {
		panic(missingMsg(DefaultName))
	}
	return inst.cache.SetWithTTL(key, value, 1, inst.ttl)
}

// SetWithTTL 往默认实例写一个值并指定过期时间，cost 为 1。返回值含义同 Set。
func SetWithTTL(key string, value any, ttl time.Duration) bool {
	return C().SetWithTTL(key, value, 1, ttl)
}

// Del 从默认实例删一个键
func Del(key string) { C().Del(key) }

func nameOf(name []string) string {
	if len(name) > 0 {
		return name[0]
	}
	return DefaultName
}

func get(name string) (instance, bool) {
	mu.RLock()
	defer mu.RUnlock()
	inst, ok := instances[name]
	return inst, ok
}

func missingMsg(want string) string {
	got := Names()
	if len(got) == 0 {
		return fmt.Sprintf("xcache: 没有名为 %q 的实例，而且一个实例都没配——检查配置里的 %s 块", want, ConfigKey)
	}
	return fmt.Sprintf("xcache: 没有名为 %q 的实例，已配置的有 [%s]", want, strings.Join(got, " "))
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
	built := map[string]instance{}
	var closers []io.Closer

	for _, name := range slices.Sorted(maps.Keys(cfg.Clients)) {
		c := cfg.Clients[name]
		cache, closer, err := New(c)
		if err != nil {
			closeAll(closers)
			return nil, fmt.Errorf("实例 %q: %w", name, err)
		}
		built[name] = instance{cache: cache, ttl: c.DefaultTTL}
		closers = append(closers, closer)
	}

	mu.Lock()
	instances = built
	mu.Unlock()

	if len(built) > 0 {
		slog.Info("xcache 就绪", "实例", slices.Sorted(maps.Keys(built)))
	}
	return &groupCloser{closers: closers}, nil
}

type groupCloser struct{ closers []io.Closer }

func (g *groupCloser) Close() error {
	mu.Lock()
	instances = map[string]instance{}
	mu.Unlock()
	return closeAll(g.closers)
}

func closeAll(closers []io.Closer) error {
	for i := len(closers) - 1; i >= 0; i-- {
		closers[i].Close() // ristretto 的 Close 不返回错误
	}
	return nil
}
