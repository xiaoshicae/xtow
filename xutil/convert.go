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
// 带总预算是为了让启动期收到的退出信号能及时生效：不可中断的重试会让进程
// 必须等满整轮才肯退出。
//
// 返回最后一次的错误；一次都没成功且预算先耗尽时，返回的是耗尽前那次的错误。
func Retry(attempts int, timeout, interval time.Duration, fn func(context.Context) error) error {
	if attempts < 1 {
		attempts = 1
	}
	budget := timeout*time.Duration(attempts) + interval*time.Duration(attempts-1)
	ctx, cancel := context.WithTimeout(context.Background(), budget)
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
