package xmetric

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// collector 缓存的类型前缀，确保同名不同类型的指标各占一个 key
const (
	kindCounter   = "c"
	kindGauge     = "g"
	kindHistogram = "h"
)

var (
	collectors sync.Map   // map[string]prometheus.Collector
	createMu   sync.Mutex // 保证「创建 + 注册 + 入缓存」是一步
)

// Tag 指标标签键值对
type Tag struct {
	Name  string
	Value string
}

// T 创建标签
func T(name, value string) Tag { return Tag{Name: name, Value: value} }

// CounterInc 计数器 +1
func CounterInc(name string, tags ...Tag) {
	names, values := parseTags(tags)
	counterOf(name, names).WithLabelValues(values...).Inc()
}

// CounterAdd 计数器 +v（v 必须 >= 0）
func CounterAdd(name string, v float64, tags ...Tag) {
	names, values := parseTags(tags)
	counterOf(name, names).WithLabelValues(values...).Add(v)
}

// GaugeSet 设置仪表盘值
func GaugeSet(name string, v float64, tags ...Tag) {
	names, values := parseTags(tags)
	gaugeOf(name, names).WithLabelValues(values...).Set(v)
}

// GaugeInc 仪表盘 +1
func GaugeInc(name string, tags ...Tag) {
	names, values := parseTags(tags)
	gaugeOf(name, names).WithLabelValues(values...).Inc()
}

// GaugeDec 仪表盘 -1
func GaugeDec(name string, tags ...Tag) {
	names, values := parseTags(tags)
	gaugeOf(name, names).WithLabelValues(values...).Dec()
}

// HistogramObserve 直方图观测，单位秒
func HistogramObserve(name string, v float64, tags ...Tag) {
	names, values := parseTags(tags)
	histogramOf(name, names).WithLabelValues(values...).Observe(v)
}

// parseTags 取出标签名和值，按名字排序，让不同书写顺序命中同一个缓存
func parseTags(tags []Tag) (names, values []string) {
	if len(tags) == 0 {
		return nil, nil
	}
	// 先拷贝再排序，不动调用方的切片
	sorted := slices.Clone(tags)
	slices.SortStableFunc(sorted, func(a, b Tag) int { return cmp.Compare(a.Name, b.Name) })

	names = make([]string, len(sorted))
	values = make([]string, len(sorted))
	for i, t := range sorted {
		names[i], values[i] = t.Name, t.Value
	}
	return names, values
}

// cacheKey 拼出 collector 缓存键，一次分配完成
func cacheKey(kind, name string, labelNames []string) string {
	size := len(kind) + 1 + len(name) + 1
	for i, l := range labelNames {
		if i > 0 {
			size++
		}
		size += len(l)
	}

	var b strings.Builder
	b.Grow(size)
	b.WriteString(kind)
	b.WriteByte(':')
	b.WriteString(name)
	b.WriteByte(':')
	for i, l := range labelNames {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(l)
	}
	return b.String()
}

// collectorOf 按 key 复用 collector，未命中时由 build 创建并注册。
//
// 注册回来的类型与预期不符，说明这个指标名已经被注册成别的类型了
// （比如先 Counter 后 Gauge）。此时本实例不在 registry 里，
// 记录的值永远导不出去——必须说出来，否则是一次完全静默的数据丢失。
func collectorOf[T prometheus.Collector](key, name string, build func() T) T {
	if v, ok := collectors.Load(key); ok {
		if typed, ok := v.(T); ok {
			return typed
		}
	}

	createMu.Lock()
	defer createMu.Unlock()
	if v, ok := collectors.Load(key); ok {
		if typed, ok := v.(T); ok {
			return typed
		}
	}

	c := build()
	registered, err := Register(c)
	switch typed, ok := registered.(T); {
	case err != nil:
		// 只能记一笔：打点是运行期调用，这里没有「让启动失败」这个选项
		slog.Error("xmetric 指标注册失败，通过它记录的值不会被导出", "指标", name, "错误", err)
	case ok:
		c = typed
	default:
		logNameConflict(name, registered)
	}

	collectors.Store(key, c)
	return c
}

func counterOf(name string, labelNames []string) *prometheus.CounterVec {
	return collectorOf(cacheKey(kindCounter, name, labelNames), name, func() *prometheus.CounterVec {
		return prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace:   Namespace(),
			Name:        name,
			Help:        name,
			ConstLabels: ConstLabels(),
		}, labelNames)
	})
}

func gaugeOf(name string, labelNames []string) *prometheus.GaugeVec {
	return collectorOf(cacheKey(kindGauge, name, labelNames), name, func() *prometheus.GaugeVec {
		return prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace:   Namespace(),
			Name:        name,
			Help:        name,
			ConstLabels: ConstLabels(),
		}, labelNames)
	})
}

func histogramOf(name string, labelNames []string) *prometheus.HistogramVec {
	return collectorOf(cacheKey(kindHistogram, name, labelNames), name, func() *prometheus.HistogramVec {
		return prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace:   Namespace(),
			Name:        name,
			Help:        name,
			Buckets:     slices.Clone(active().cfg.HistogramBuckets),
			ConstLabels: ConstLabels(),
		}, labelNames)
	})
}

// asAlreadyRegistered 抽出来是为了让 xmetric.go 不必再 import errors
func asAlreadyRegistered(err error, target *prometheus.AlreadyRegisteredError) bool {
	return errors.As(err, target)
}

// clearCollectors 清空缓存，返回清掉的数量
func clearCollectors() int {
	n := 0
	collectors.Range(func(k, _ any) bool {
		collectors.Delete(k)
		n++
		return true
	})
	return n
}

// logNameConflict 同一个指标名被注册成了不同类型
func logNameConflict(name string, registered prometheus.Collector) {
	slog.Error("xmetric 指标名冲突，通过它记录的值不会被导出",
		"指标", name, "已注册的类型", fmt.Sprintf("%T", registered))
}
