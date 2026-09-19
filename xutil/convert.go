package xutil

import (
	"context"
	"time"
)

// ToPtr 取值的地址。用于需要区分「没设置」和「设置成零值」的少数场景。
//
// 配置结构体不该用它：默认值预填进结构体就有同样的语义，见 internal/config。
func ToPtr[T any](v T) *T { return &v }

// GetOrDefault v 为零值时返回 defaultV
func GetOrDefault[T comparable](v, defaultV T) T {
	var zero T
	if v == zero {
		return defaultV
	}
	return v
}

// Retry 反复调用 fn 直到成功，每次单独限时，两次之间隔 interval。
//
// 整轮有一个总预算（attempts × timeout + 间隔之和），到了就不再重试。
//
// parent 被取消时整轮立即中止，剩下的尝试和等待都不再进行。
// 启动期的建连重试靠这一条：收到退出信号时进程不必等满整轮才肯退出。
// 传 nil 等同于 context.Background()。
//
// 返回最后一次的错误；一次都没成功且预算先耗尽时，返回的是耗尽前那次的错误。
func Retry(parent context.Context, attempts int, timeout, interval time.Duration, fn func(context.Context) error) error {
	if attempts < 1 {
		attempts = 1
	}
	if parent == nil {
		parent = context.Background()
	}
	// 进来时就已经取消的话，一次都不用试。少了这一句，
	// 启动到一半收到退出信号时，每个连不上的实例还要再发一次注定失败的网络请求
	if err := parent.Err(); err != nil {
		return err
	}

	budget := timeout*time.Duration(attempts) + interval*time.Duration(attempts-1)
	ctx, cancel := context.WithTimeout(parent, budget)
	defer cancel()

	var last error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return last
			case <-time.After(interval):
			}
		}
		attemptCtx, attemptCancel := context.WithTimeout(ctx, timeout)
		last = fn(attemptCtx)
		attemptCancel()
		if last == nil {
			return nil
		}
	}
	return last
}
