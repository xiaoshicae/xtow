package xflow

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

type noop struct{ n string }

func (p noop) Name() string                         { return p.n }
func (p noop) Dependency() Dependency               { return Strong }
func (p noop) Process(context.Context, *int) error  { return nil }
func (p noop) Rollback(context.Context, *int) error { return nil }

func quiet(b *testing.B) {
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	b.Cleanup(func() { slog.SetDefault(old) })
}

func BenchmarkExecute_五步全成功_开监控(b *testing.B) {
	quiet(b)
	f := New("下单", noop{"1"}, noop{"2"}, noop{"3"}, noop{"4"}, noop{"5"})
	d := 0
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.Execute(context.Background(), &d)
	}
}

func BenchmarkExecute_五步全成功_关监控(b *testing.B) {
	quiet(b)
	old := cfg.Monitor
	cfg.Monitor = false
	b.Cleanup(func() { cfg.Monitor = old })
	f := New("下单", noop{"1"}, noop{"2"}, noop{"3"}, noop{"4"}, noop{"5"})
	d := 0
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.Execute(context.Background(), &d)
	}
}
