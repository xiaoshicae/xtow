package xlog

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

func benchLogger(b *testing.B) *slog.Logger {
	h := slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo})
	return slog.New(newCtxHandler(h))
}

func BenchmarkHandle_裸(b *testing.B) {
	l := benchLogger(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.InfoContext(ctx, "请求完成", "状态", 200)
	}
}

func BenchmarkHandle_装了链路提取器但ctx里没有span(b *testing.B) {
	SetTraceExtractor(func(context.Context) (string, string) { return "", "" })
	b.Cleanup(func() { SetTraceExtractor(nil) })
	l := benchLogger(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.InfoContext(ctx, "请求完成", "状态", 200)
	}
}

func BenchmarkHandle_有链路有作用域(b *testing.B) {
	SetTraceExtractor(func(context.Context) (string, string) {
		return "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	})
	b.Cleanup(func() { SetTraceExtractor(nil) })
	l := benchLogger(b)
	ctx := CtxWithScope(context.Background())
	AddKV(ctx, "uid", 12345)
	AddKV(ctx, "route", "/order")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.InfoContext(ctx, "请求完成", "状态", 200)
	}
}

func BenchmarkHandle_开过分组的慢路径(b *testing.B) {
	SetTraceExtractor(func(context.Context) (string, string) {
		return "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	})
	b.Cleanup(func() { SetTraceExtractor(nil) })
	l := benchLogger(b).With("svc", "demo").WithGroup("http")
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.InfoContext(ctx, "请求完成", "状态", 200)
	}
}
