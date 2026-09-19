// Package xclient 提供「一组按名字组织的实例」这件事本身。
//
// xgorm、xredis、xcache 都是同一个形状：配置里可以写一个实例，也可以按名字写
// 好几个；运行时用 C() 按名字取；启动时挨个建、有一个建不起来就把已建好的全关掉。
// 这些语义本该处处一致——「名字找不到时说什么」「两种写法混用怎么办」
// 都是一次决定，分散在三个模块里就变成了三份会各自漂移的实现。
//
// 集成包这样用：
//
//	var reg = xclient.NewRegistry[*gorm.DB]("xgorm", ConfigKey)
//
//	func C(name ...string) *gorm.DB { return reg.Get(name...) }
//	func Has(name ...string) bool   { return reg.Has(name...) }
//	func Names() []string           { return reg.Names() }
//
// 本包零第三方依赖。
package xclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
)

// DefaultName 不带参数取实例时用的名字
const DefaultName = "default"

// Registry 一组按名字组织的实例
type Registry[T any] struct {
	module string // 出现在 panic 文案里，如 "xgorm"
	key    string // 配置块的顶层 key，如 "XGorm"

	mu    sync.RWMutex
	items map[string]T
}

// NewRegistry 创建注册表。module 与 key 只用于拼错误信息。
func NewRegistry[T any](module, key string) *Registry[T] {
	return &Registry[T]{module: module, key: key, items: map[string]T{}}
}

// Get 取一个实例，不带参数时取名为 default 的那个。取不到直接 panic。
//
// 不返回零值：返回 nil 不会让程序走得更远——客户端上的任何方法在 nil 上
// 都是空指针解引用，只是把同一个 panic 推迟到调用方第一次用它的时候，
// 而那里的栈里只剩 "invalid memory address"，看不出根因是配置没配。
// 这是启动期的配置问题，不是运行期要处理的错误。
//
// 可选依赖（配了就用、没配就跳过）用 Has 先判断。
func (r *Registry[T]) Get(name ...string) T {
	v, ok := r.Lookup(name...)
	if !ok {
		panic(r.missing(nameOf(name)))
	}
	return v
}

// Lookup 取一个实例，取不到时返回零值和 false
func (r *Registry[T]) Lookup(name ...string) (T, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.items[nameOf(name)]
	return v, ok
}

// Has 报告指定实例是否已配置，供可选依赖判断
func (r *Registry[T]) Has(name ...string) bool {
	_, ok := r.Lookup(name...)
	return ok
}

// Names 返回已配置的实例名，按名字排序
//
// 手写取 key 再排序，不用 slices.Sorted(maps.Keys(...))：后者要 Go 1.23，
// 而核心模块的下限是 1.22，抬上去会让所有使用者跟着抬。
func (r *Registry[T]) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return sortedKeys(r.items)
}

// Publish 发布一组实例，替换掉原有的
func (r *Registry[T]) Publish(items map[string]T) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items = items
}

// missing 只报「没找到」帮助有限：名字写错和整块没配是两个不同的问题，
// 把实际配了哪些列出来，两者一眼可分。
func (r *Registry[T]) missing(want string) string {
	got := r.Names()
	if len(got) == 0 {
		return fmt.Sprintf("%s: no instance named %q, and none is configured at all — check the %s block in the config",
			r.module, want, r.key)
	}
	return fmt.Sprintf("%s: no instance named %q, configured ones are [%s]",
		r.module, want, strings.Join(got, " "))
}

func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func nameOf(name []string) string {
	if len(name) > 0 {
		return name[0]
	}
	return DefaultName
}

// Build 按名字顺序建出全部实例，并把它们发布到注册表。
//
// 任何一个建不起来就把已经建好的全关掉再返回错误：组件 Init 返回错误时
// 框架拿不到 Closer，不自己收拾就会漏掉那几个连接池。
//
// ctx 一路传给 new，并在每个实例之前检查一次：配了五个库、第一个就要
// 重试到超时的话，收到退出信号应当就此打住，而不是把剩下四个也挨个试一遍。
//
// 返回的 Closer 会在关闭时清空注册表，然后逆序关掉各实例。
func Build[C, T any](ctx context.Context, r *Registry[T], cfgs map[string]C,
	new func(context.Context, C) (T, io.Closer, error)) (io.Closer, error) {
	built := make(map[string]T, len(cfgs))
	var closers []io.Closer

	// 名字排序后再建，让失败顺序可复现，日志顺序也稳定
	for _, name := range sortedKeys(cfgs) {
		if err := ctx.Err(); err != nil {
			closeAll(closers)
			return nil, fmt.Errorf("shutdown signal received before building instance %q: %w", name, err)
		}
		v, closer, err := safeNew(ctx, name, cfgs[name], new)
		if err != nil {
			closeAll(closers)
			return nil, err
		}
		built[name] = v
		closers = append(closers, closer)
	}

	r.Publish(built)
	return &groupCloser{clear: func() { r.Publish(map[string]T{}) }, closers: closers}, nil
}

type groupCloser struct {
	clear   func()
	closers []io.Closer
}

func (g *groupCloser) Close() error {
	// 先摘掉再关：反过来的话，关到一半时 C() 还能取到正在被关闭的实例
	g.clear()
	return closeAll(g.closers)
}

// safeNew 建一个实例并隔离 panic。
//
// 不隔离的话，panic 会穿过 Build 往上抛，而已经建好的那几个实例的 Closer
// 还只存在于 Build 这一帧的局部变量里——栈一展开就找不回来了，
// 那是几个再也关不掉的连接池。
func safeNew[C, T any](ctx context.Context, name string, cfg C,
	new func(context.Context, C) (T, io.Closer, error)) (v T, closer io.Closer, err error) {
	defer func() {
		if r := recover(); r != nil {
			var zero T
			v, closer, err = zero, nil, fmt.Errorf("instance %q panicked: %v", name, r)
		}
	}()

	v, closer, err = new(ctx, cfg)
	if err != nil {
		err = fmt.Errorf("instance %q: %w", name, err)
	}
	return v, closer, err
}

// safeClose 关一个实例并隔离 panic。
//
// 理由与 safeNew 对称：一个实例的 Close 炸了，不该让同一组里
// 剩下的实例跟着关不掉。
func safeClose(c io.Closer) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("close panicked: %v", r)
		}
	}()
	return c.Close()
}

// closeAll 逆序关闭，一个失败不影响其余
func closeAll(closers []io.Closer) error {
	var errs []error
	for i := len(closers) - 1; i >= 0; i-- {
		if closers[i] == nil {
			continue
		}
		if err := safeClose(closers[i]); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
