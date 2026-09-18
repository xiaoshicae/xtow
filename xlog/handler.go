package xlog

import (
	"context"
	"log/slog"
	"sync/atomic"
)

// TraceExtractor 从 context 里取出链路标识。
//
// 这是本包唯一的扩展点，由 xtrace 在初始化时注入。契约：
//   - 传 nil 表示取消注入
//   - 后注入的覆盖先注入的（链路系统本来就只该有一个）
//   - 未注入时日志不含 trace 字段，不报错
//
// 这样 xlog 不必依赖 OpenTelemetry，而依赖它的模块也不必反过来依赖 xlog。
type TraceExtractor func(ctx context.Context) (traceID, spanID string)

var traceExtractor atomic.Pointer[TraceExtractor]

// SetTraceExtractor 注入链路标识提取器
func SetTraceExtractor(f TraceExtractor) {
	if f == nil {
		traceExtractor.Store(nil)
		return
	}
	traceExtractor.Store(&f)
}

// groupOrAttrs 记录一次 WithGroup 或 WithAttrs 调用，用于在开过分组时重放调用链
type groupOrAttrs struct {
	group string      // 非空表示这是一次 WithGroup
	attrs []slog.Attr // group 为空时有效
}

// ctxHandler 在标准库 handler 之上补充 context 里携带的字段。
//
// 只做「加字段」这一件事，格式化、级别过滤、分组全部交给被包装的
// slog.JSONHandler / slog.TextHandler —— 它们已经做得很好，没有理由重写。
//
// 分组是唯一的麻烦：标准库会把记录上的属性归到当前打开的分组里，
// 于是 logger.WithGroup("g") 之后 trace_id 会变成 g.trace_id。
// 但 trace_id 是整条记录的身份，不属于用户划的任何一组，必须留在顶层。
// 所以开过分组后走「在 base 上挂 ctx 字段、再重放调用链」的慢路径；
// 没开过分组时属性本来就落在顶层，沿用零额外开销的快路径。
type ctxHandler struct {
	next    slog.Handler   // 已应用 With/WithGroup 的 handler
	base    slog.Handler   // 未应用任何 With/WithGroup 的原始 handler
	chain   []groupOrAttrs // base → next 的调用链，仅 grouped 时有意义
	grouped bool           // 是否调用过 WithGroup
}

func newCtxHandler(h slog.Handler) *ctxHandler {
	return &ctxHandler{next: h, base: h}
}

func (h *ctxHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h *ctxHandler) Handle(ctx context.Context, r slog.Record) error {
	attrs := ctxAttrs(ctx)
	if len(attrs) == 0 {
		return h.next.Handle(ctx, r) // 常见情况：没有任何 ctx 字段，零额外开销
	}
	if !h.grouped {
		r.AddAttrs(attrs...) // 没有分组，记录上的属性就在顶层
		return h.next.Handle(ctx, r)
	}
	return h.replay(attrs).Handle(ctx, r)
}

// replay 把 ctx 字段挂在最外层，再按原顺序重放 With/WithGroup 调用链。
// 每条记录重建一次 handler，只有用过 WithGroup 的 logger 才会付这份开销。
func (h *ctxHandler) replay(attrs []slog.Attr) slog.Handler {
	out := h.base.WithAttrs(attrs)
	for _, g := range h.chain {
		if g.group != "" {
			out = out.WithGroup(g.group)
		} else {
			out = out.WithAttrs(g.attrs)
		}
	}
	return out
}

func (h *ctxHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	return h.fork(groupOrAttrs{attrs: attrs}, h.next.WithAttrs(attrs), h.grouped)
}

func (h *ctxHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return h.fork(groupOrAttrs{group: name}, h.next.WithGroup(name), true)
}

// fork 派生一个新 handler。调用链必须无条件记全：分组可能在若干次 With 之后才出现，
// 那时先前挂的属性同样要参与重放，漏掉就会在慢路径上凭空丢字段。
// 每次都新建切片而不是 append 复用，避免两个 logger 从同一父节点派生时互相覆盖。
func (h *ctxHandler) fork(g groupOrAttrs, next slog.Handler, grouped bool) *ctxHandler {
	chain := make([]groupOrAttrs, len(h.chain), len(h.chain)+1)
	copy(chain, h.chain)
	return &ctxHandler{next: next, base: h.base, chain: append(chain, g), grouped: grouped}
}

// ctxAttrs 收齐 context 里携带的所有字段。
// 一次性收齐再 AddAttrs：slog.Record 只内联前 5 个属性，逐个添加会让底层切片反复扩容。
func ctxAttrs(ctx context.Context) []slog.Attr {
	if ctx == nil {
		return nil
	}
	s := scopeFrom(ctx)
	n := 0
	if s != nil {
		n = s.len()
	}
	f := traceExtractor.Load()
	if n == 0 && f == nil {
		return nil
	}

	attrs := make([]slog.Attr, 0, n+2)
	if f != nil {
		if traceID, spanID := (*f)(ctx); traceID != "" {
			attrs = append(attrs, slog.String("trace_id", traceID))
			if spanID != "" {
				attrs = append(attrs, slog.String("span_id", spanID))
			}
		}
	}
	if s != nil {
		s.each(func(k string, v any) { attrs = append(attrs, slog.Any(k, v)) })
	}
	return attrs
}
