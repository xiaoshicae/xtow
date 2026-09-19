package xclient

import "testing"

func BenchmarkRegistry_Get(b *testing.B) {
	r := NewRegistry[string]("xdemo", "XDemo")
	r.Publish(map[string]string{"default": "A", "report": "B"})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = r.Get()
	}
}

func BenchmarkRegistry_Get具名(b *testing.B) {
	r := NewRegistry[string]("xdemo", "XDemo")
	r.Publish(map[string]string{"default": "A", "report": "B"})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = r.Get("report")
	}
}
