// Package xflow 把一串步骤编排成一个流程，强依赖失败时自动逆序回滚。
//
//	flow := xflow.New("下单",
//		&扣券{}, &扣库存{}, &扣款{}, &发通知{},
//	)
//	result := flow.Execute(ctx, data)
//	if !result.Success() { ... }
//
// 每个步骤自己声明是强依赖还是弱依赖：强依赖失败就中断并回滚，
// 弱依赖失败只记一笔继续往下走。
//
// 本包零第三方依赖，所以留在核心模块里。
package xflow

import (
	"context"
	"fmt"
	"runtime/debug"
	"time"
)

// Dependency 步骤的依赖强弱
type Dependency int

const (
	// Strong 强依赖：失败就中断流程并回滚
	Strong Dependency = iota
	// Weak 弱依赖：失败只记一笔，继续往下走
	Weak
)

func (d Dependency) String() string {
	switch d {
	case Strong:
		return "strong"
	case Weak:
		return "weak"
	default:
		return "unknown"
	}
}

// Processor 流程里的一步。
//
// 类型参数 T 是贯穿整个流程的共享数据，建议用指针（如 *OrderData）：
// 入参、出参、各步骤之间的中间结果都放进这一个结构体，
// 于是每一步的方法签名里只出现业务自己的类型，不必重复框架的泛型。
//
// Process 和 Rollback 要写在同一个类型上：谁做的事，谁负责撤销。
// 拆开写迟早会出现「加了一步正向逻辑，忘了配套的补偿」。
type Processor[T any] interface {
	// Name 步骤名，用于日志和错误信息
	Name() string

	// Dependency 强依赖还是弱依赖
	Dependency() Dependency

	// Process 正向逻辑
	Process(ctx context.Context, data T) error

	// Rollback 补偿逻辑，强依赖失败时逆序调用已成功的步骤。
	//
	// 两件事要留意：
	//   - 不能假设 Process 完全成功。弱依赖失败之后仍会被回滚，
	//     所以这里要做幂等处理。
	//   - 用的 ctx 已经剥掉了原来的取消和超时（见 Execute 的说明）。
	Rollback(ctx context.Context, data T) error
}

// StepError 某一步出的错
type StepError struct {
	Processor  string
	Dependency Dependency
	Err        error
}

func (e *StepError) Error() string {
	return fmt.Sprintf("step %q (%s dependency) failed: %v", e.Processor, e.Dependency, e.Err)
}

func (e *StepError) Unwrap() error { return e.Err }

// Result 一次执行的结果。
//
// 业务数据不在这里——它在调用方传进 Execute 的那个 T 里。
// 流程失败时，此前各步写进去的内容依然保留，便于排查和人工补偿。
type Result struct {
	// Err 致命错误：强依赖失败，或者 ctx 被取消。nil 表示流程走完了。
	Err error

	// Skipped 弱依赖失败的记录，不影响流程继续
	Skipped []*StepError

	// RollbackErrors 回滚过程中出的错。
	//
	// 这个列表非空意味着有资源没能补偿回来，需要人工介入。
	RollbackErrors []*StepError

	// Rolled 是否触发了回滚
	Rolled bool
}

// Success 流程是否走完了（没有强依赖失败）
func (r *Result) Success() bool { return r.Err == nil }

func (r *Result) String() string {
	if r.Err == nil {
		if len(r.Skipped) > 0 {
			return fmt.Sprintf("flow succeeded, %d weak step(s) skipped after failing", len(r.Skipped))
		}
		return "flow succeeded"
	}
	msg := fmt.Sprintf("flow failed: %v", r.Err)
	if r.Rolled {
		msg += ", rolled back"
	}
	if n := len(r.RollbackErrors); n > 0 {
		msg += fmt.Sprintf(", %d step(s) failed to roll back", n)
	}
	return msg
}

// Flow 一个编排好的流程。
//
// 构建之后字段不再变化，可以并发 Execute。
type Flow[T any] struct {
	name  string
	steps []Processor[T]
}

// New 按传入顺序构建流程。
//
// 传 nil 步骤直接 panic：它只会在执行到那一步时炸成空指针，
// 而那时错误早就脱离了构建现场，越早暴露越好。
func New[T any](name string, steps ...Processor[T]) *Flow[T] {
	for i, p := range steps {
		if p == nil {
			panic(fmt.Sprintf("xflow: step %d of flow %q is nil", i, name))
		}
	}
	return &Flow[T]{name: name, steps: steps}
}

// Name 流程名
func (f *Flow[T]) Name() string { return f.name }

// Execute 按顺序执行各步骤，data 贯穿全程供各步读写。
//
// 强依赖失败或 ctx 被取消时中断并逆序回滚已执行的步骤；
// 弱依赖失败记进 Skipped 后继续，但同样会被纳入回滚范围——
// 它可能已经产生了副作用，只是后面没走下去而已。
func (f *Flow[T]) Execute(ctx context.Context, data T) *Result {
	if ctx == nil {
		ctx = context.Background()
	}

	m := activeMonitor()
	res := &Result{}
	start := time.Now()

	// 用 defer 而不是在每个出口各写一遍：出口有三个（取消、失败、正常走完），
	// 以后再加一个分支时漏掉通知，表现是监控上这次执行凭空消失，
	// 而流程本身照常返回——不会有人发现
	defer func() { f.notifyFlow(ctx, m, res, start) }()

	done := make([]Processor[T], 0, len(f.steps))

	for _, p := range f.steps {
		// 调用方已经不等了，就不再启动新步骤；但已经做完的仍要回滚
		if err := ctx.Err(); err != nil {
			res.Err = fmt.Errorf("xflow: flow %q canceled before step %q: %w", f.name, p.Name(), err)
			f.rollback(ctx, data, done, res, m)
			return res
		}

		stepStart := time.Now()
		err := safeProcess(ctx, p, data)
		f.notifyStep(ctx, m, false, p, err, stepStart)

		if err == nil {
			done = append(done, p)
			continue
		}

		se := &StepError{Processor: p.Name(), Dependency: p.Dependency(), Err: err}

		if p.Dependency() == Weak {
			res.Skipped = append(res.Skipped, se)
			done = append(done, p) // 失败的弱依赖也可能留下了副作用，同样要回滚

			// 弱依赖失败只记一笔继续走——除非它是被取消带下水的。
			//
			// 取消只在每步开始前查一次的话，最后一步撞上取消就查不到了：
			// 它的 context.Canceled 走进这个「跳过」分支，循环随即结束，
			// 于是一个被取消的流程报成了 Success，还一步都没回滚。
			if ctx.Err() == nil {
				continue
			}
			res.Err = fmt.Errorf("xflow: flow %q canceled while running step %q: %w",
				f.name, p.Name(), ctx.Err())
		} else {
			// 强依赖失败：这一步没成，不纳入回滚范围
			res.Err = se
		}

		f.rollback(ctx, data, done, res, m)
		return res
	}
	return res
}

// rollback 逆序回滚已执行的步骤。
//
// 回滚用的 ctx 剥掉了原来的取消和超时：补偿逻辑（退款、还库存、解冻额度）
// 最需要执行的时机恰恰是请求超时之后，沿用已经取消的 ctx 会让每个补偿调用
// 一进去就被拒绝，资源就真的漏掉了。
// 剥掉之后由 RollbackTimeout 单独限时，免得补偿无限期挂住退出流程。
func (f *Flow[T]) rollback(ctx context.Context, data T, done []Processor[T], res *Result, m Monitor) {
	res.Rolled = true

	rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.RollbackTimeout)
	defer cancel()

	for i := len(done) - 1; i >= 0; i-- {
		p := done[i]

		// 预算耗尽也要把剩下的逐个记下来：调用方得知道还有哪些资源悬着
		if err := rbCtx.Err(); err != nil {
			res.RollbackErrors = append(res.RollbackErrors, &StepError{
				Processor:  p.Name(),
				Dependency: p.Dependency(),
				Err:        fmt.Errorf("rollback budget exhausted, this step never ran: %w", err),
			})
			continue
		}

		stepStart := time.Now()
		err := safeRollback(rbCtx, p, data)
		f.notifyStep(rbCtx, m, true, p, err, stepStart)

		if err != nil {
			res.RollbackErrors = append(res.RollbackErrors, &StepError{
				Processor: p.Name(), Dependency: p.Dependency(), Err: err,
			})
		}
	}
}

// safeProcess 执行 Process 并隔离 panic
//
// 一步炸了不该把整个进程打穿：它应该变成一个普通的步骤失败，
// 好让前面几步有机会被回滚。
func safeProcess[T any](ctx context.Context, p Processor[T], data T) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("xflow: step %q panicked: %v\n%s", p.Name(), r, debug.Stack())
		}
	}()
	return p.Process(ctx, data)
}

// safeRollback 执行 Rollback 并隔离 panic
//
// 尤其重要：一步补偿炸了不该拦住其余步骤的补偿。
func safeRollback[T any](ctx context.Context, p Processor[T], data T) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("xflow: rollback of step %q panicked: %v\n%s", p.Name(), r, debug.Stack())
		}
	}()
	return p.Rollback(ctx, data)
}
