package xlog

import (
	"context"
	"sync"
	"sync/atomic"
)

// ctxScopeKey KV 作用域在 context 中的 key。
// 用私有类型而不是字符串，避免与其它包的 context key 撞上。
type ctxScopeKey struct{}

// scope 一次请求（或一段调用链）共享的字段集合
//
// 存进 context 的是**指针**，所以 AddKV 的写入对所有持有该 ctx 的地方立即可见，
// 不需要把新 context 回传给调用方——业务函数在调用栈深处拿不到 *gin.Context，
// 本来也没机会回传。这正是「返回新 context」那种写法解决不了的场景。
//
// 日志可能在任意 goroutine 写出，读写都要加锁。
type scope struct {
	mu sync.RWMutex
	kv map[string]any
}

// scopeInitCap 首次写入时 map 的初始容量
const scopeInitCap = 8

func (s *scope) add(k string, v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kv == nil {
		s.kv = make(map[string]any, scopeInitCap)
	}
	s.kv[k] = v
}

func (s *scope) addAll(kvs map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kv == nil {
		s.kv = make(map[string]any, max(len(kvs), scopeInitCap))
	}
	for k, v := range kvs {
		s.kv[k] = v
	}
}

// each 在读锁内遍历，避免为每一行日志复制一次 map
func (s *scope) each(f func(k string, v any)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for k, v := range s.kv {
		f(k, v)
	}
}

func (s *scope) len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.kv)
}

// CtxWithScope 开启一个字段作用域。
//
// 之后在这条 context 上任何位置调用 AddKV 写入的字段，都会出现在该 context
// 记录的每一条日志里——包括请求入口在 handler 返回之后写的那条访问日志。
//
// 幂等：已经有作用域时原样返回，重复调用（比如中间件被注册了两次）不会
// 清空已经写入的字段。
func CtxWithScope(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if scopeFrom(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, ctxScopeKey{}, &scope{})
}

// AddKV 往当前作用域写一个字段。
//
// 没有作用域时丢弃并计数——否则「字段就是没出现在日志里」不留任何痕迹。
func AddKV(ctx context.Context, k string, v any) {
	s := scopeFrom(ctx)
	if s == nil {
		droppedKV.Add(1)
		return
	}
	s.add(k, v)
}

// AddKVs 往当前作用域批量写字段
func AddKVs(ctx context.Context, kvs map[string]any) {
	if len(kvs) == 0 {
		return
	}
	s := scopeFrom(ctx)
	if s == nil {
		droppedKV.Add(int64(len(kvs)))
		return
	}
	s.addAll(kvs)
}

// droppedKV 在没有作用域的 context 上被丢弃的字段数。
// 供排查「我明明 AddKV 了但日志里没有」——多半是漏了 CtxWithScope。
var droppedKV atomic.Int64

// DroppedKVCount 返回被丢弃的字段数，不为零说明有 AddKV 调用没有对应的作用域
func DroppedKVCount() int64 { return droppedKV.Load() }

func scopeFrom(ctx context.Context) *scope {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(ctxScopeKey{}).(*scope)
	return s
}
