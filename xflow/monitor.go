package xflow

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// StepEvent 一步执行完的事件，Process 和 Rollback 共用
type StepEvent struct {
	Flow       string
	Processor  string
	Dependency Dependency
	Rollback   bool // true 表示这是一次回滚
	Err        error
	Duration   time.Duration
}

// FlowEvent 整个流程执行完的事件，Duration 含回滚耗时
type FlowEvent struct {
	Flow     string
	Result   *Result
	Duration time.Duration
}

// Monitor 观测流程执行。
//
// 两个回调都被 panic 隔离：监控实现出错只丢一次观测，不会打断业务流程。
type Monitor interface {
	OnStep(ctx context.Context, e *StepEvent)
	OnFlow(ctx context.Context, e *FlowEvent)
}

// monitor 当前生效的实现，原子读写：Execute 是热点路径，不该为它加锁
var monitor atomic.Pointer[Monitor]

// SetMonitor 换掉默认的监控实现。传 nil 表示关掉监控。
func SetMonitor(m Monitor) {
	if m == nil {
		monitor.Store(nil)
		return
	}
	monitor.Store(&m)
}

func init() { SetMonitor(slogMonitor{}) }

// activeMonitor 本次执行用哪个 Monitor，关掉时返回 nil（零开销）
func activeMonitor() Monitor {
	if !cfg.Monitor {
		return nil
	}
	m := monitor.Load()
	if m == nil {
		return nil
	}
	return *m
}

func (f *Flow[T]) notifyStep(ctx context.Context, m Monitor, rollback bool, p Processor[T], err error, start time.Time) {
	if m == nil {
		return
	}
	e := &StepEvent{
		Flow: f.name, Processor: p.Name(), Dependency: p.Dependency(),
		Rollback: rollback, Err: err, Duration: time.Since(start),
	}
	safeNotify(func() { m.OnStep(ctx, e) })
}

func (f *Flow[T]) notifyFlow(ctx context.Context, m Monitor, res *Result, start time.Time) {
	if m == nil {
		return
	}
	e := &FlowEvent{Flow: f.name, Result: res, Duration: time.Since(start)}
	safeNotify(func() { m.OnFlow(ctx, e) })
}

// safeNotify 隔离监控实现的 panic
func safeNotify(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("xflow 监控实现 panic，已隔离", "错误", r)
		}
	}()
	fn()
}

// slogMonitor 默认实现，写到标准库 slog。
//
// 成功的步骤记 debug 而不是 info：一个五步的流程每次执行会产出六行，
// 默认级别下把它们全打出来，日志里就只剩流程编排了。
// 需要逐步排查时把级别调到 debug；失败的步骤和流程结果任何时候都看得到。
type slogMonitor struct{}

func (slogMonitor) OnStep(ctx context.Context, e *StepEvent) {
	action := "执行"
	if e.Rollback {
		action = "回滚"
	}
	attrs := []any{"流程", e.Flow, "步骤", e.Processor, "依赖", e.Dependency.String(), "耗时", e.Duration}

	if e.Err != nil {
		slog.WarnContext(ctx, "xflow 步骤"+action+"失败", append(attrs, "错误", e.Err)...)
		return
	}
	slog.DebugContext(ctx, "xflow 步骤"+action+"完成", attrs...)
}

func (slogMonitor) OnFlow(ctx context.Context, e *FlowEvent) {
	attrs := []any{"流程", e.Flow, "耗时", e.Duration, "结果", e.Result.String()}

	// 回滚有失败意味着有资源没补偿回来，需要人工介入——这一条必须醒目
	if len(e.Result.RollbackErrors) > 0 {
		slog.ErrorContext(ctx, "xflow 回滚未能全部完成，可能有资源悬着",
			append(attrs, "未补偿步骤数", len(e.Result.RollbackErrors))...)
		return
	}
	if !e.Result.Success() {
		slog.WarnContext(ctx, "xflow 流程失败", attrs...)
		return
	}
	slog.InfoContext(ctx, "xflow 流程完成", attrs...)
}
