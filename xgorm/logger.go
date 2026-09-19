package xgorm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// gormLogger 把 GORM 的日志接到标准库 slog 上
//
// 字段是结构化的而不是拼进消息里：SQL 里带引号和换行，塞进消息文本
// 会把一行日志撑成好几行，也没法按耗时或错误筛。
type gormLogger struct {
	slowThreshold  time.Duration
	ignoreNotFound bool
	level          logger.LogLevel
}

func newGormLogger(c ClientConfig) *gormLogger {
	return &gormLogger{
		slowThreshold:  c.SlowThreshold,
		ignoreNotFound: c.IgnoreNotFound,
		level:          logger.Info,
	}
}

// LogMode 返回副本，不改共享实例（GORM 的约定）
func (l *gormLogger) LogMode(level logger.LogLevel) logger.Interface {
	cp := *l
	cp.level = level
	return &cp
}

func (l *gormLogger) Info(ctx context.Context, msg string, args ...any) {
	if l.level >= logger.Info {
		slog.InfoContext(ctx, message(msg, args))
	}
}

func (l *gormLogger) Warn(ctx context.Context, msg string, args ...any) {
	if l.level >= logger.Warn {
		slog.WarnContext(ctx, message(msg, args))
	}
}

func (l *gormLogger) Error(ctx context.Context, msg string, args ...any) {
	if l.level >= logger.Error {
		slog.ErrorContext(ctx, message(msg, args))
	}
}

// Trace 每条 SQL 执行完都会被调用
func (l *gormLogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	if l.level <= logger.Silent {
		return
	}
	elapsed := time.Since(begin)

	switch {
	case err != nil && l.level >= logger.Error && !l.skipErr(err):
		sql, rows := fc()
		slog.ErrorContext(ctx, "SQL failed", attrs(sql, rows, elapsed, "error", err)...)

	case l.slowThreshold > 0 && elapsed > l.slowThreshold && l.level >= logger.Warn:
		sql, rows := fc()
		slog.WarnContext(ctx, "slow SQL", attrs(sql, rows, elapsed, "threshold", l.slowThreshold)...)

	case l.level >= logger.Info:
		sql, rows := fc()
		slog.InfoContext(ctx, "SQL", attrs(sql, rows, elapsed)...)
	}
}

// skipErr「没查到记录」通常是正常的业务分支，不是故障
func (l *gormLogger) skipErr(err error) bool {
	return l.ignoreNotFound && errors.Is(err, gorm.ErrRecordNotFound)
}

func attrs(sql string, rows int64, elapsed time.Duration, extra ...any) []any {
	out := make([]any, 0, 6+len(extra))
	out = append(out, "sql", sql, "elapsed", elapsed)
	if rows >= 0 {
		// -1 是 GORM 表示「行数未知」的约定，写成 -1 会被误读成真有 -1 行
		out = append(out, "rows_affected", rows)
	}
	return append(out, extra...)
}

// message 组装 GORM 传来的日志消息。
//
// GORM 的接口是 printf 风格的，但它也会传不带参数的纯文本，
// 那时候不能走 Sprintf——消息里的 % 会被当成占位符。
//
// 收切片而不是变参：变参会被 go vet 认成 printf 包装函数，
// 于是每个调用点都要求格式串是常量，而这里恰恰相反。
func message(msg string, args []any) string {
	if len(args) == 0 {
		return msg
	}
	return fmt.Sprintf(msg, args...)
}
