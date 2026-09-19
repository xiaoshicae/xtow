// Package xtrace 按配置装好 OpenTelemetry 的全局 TracerProvider 与 Propagator。
//
// 使用者拿到的是原生的 OpenTelemetry：业务代码直接用 otel.Tracer("...") 开 Span，
// 不需要认识本包。本包只有两件事是自己的：
//
//   - AddSpanProcessor —— 挂上报用的 exporter（框架不内置任何一个）
//   - HeaderPropagator —— 按配置透传自定义 Header，如 X-Request-Id
package xtrace

import (
	"fmt"
	"time"
)

// ConfigKey 本模块在配置文件里的顶层 key
const ConfigKey = "XTrace"

// ForwardHeaderRule 按域名透传的 Header 规则。
// 仅当目标域名匹配 Domains 中任一模式时，才透传对应的 Headers。
type ForwardHeaderRule struct {
	// Domains 域名模式，支持精确匹配和通配前缀（*.example.com）
	Domains []string `yaml:"Domains"`

	// Headers 该规则下要透传的 Header
	Headers []string `yaml:"Headers"`
}

// Config 链路配置
type Config struct {
	// Enable 是否开启链路。默认开启。
	//
	// 关掉之后 Span 仍然能创建（是个 noop），TraceID 不再生成，
	// 但 Header 透传照常生效——那是两件事。
	Enable bool `yaml:"Enable"`

	// Console 是否把 Span 打到标准输出，仅用于本地调试。默认关闭。
	Console bool `yaml:"Console"`

	// SampleRatio 采样率，>= 1 全采样。默认 1。
	//
	// 写 0 是「新链路一条都不采」，但 Span 照常创建、TraceID 照常生成和透传——
	// 想让下游拿得到 TraceID、本地又不落 Span 时就这么配。
	// 要连 Span 都不产生（noop provider）请用 Enable: false，那是另一件事。
	SampleRatio float64 `yaml:"SampleRatio"`

	// ShutdownTimeout 关闭时等待 Span 导出完成的上限。默认 5s，必须大于 0。
	//
	// 必须有上限：导出端不可达时 Shutdown 会一直阻塞，
	// 没有 deadline 就是退出时挂死。
	//
	// 同样地，0 不是「不限时」而是「一点都不等」：Shutdown 拿到的是一个
	// 已经过期的 context，缓冲区里还没发出去的 Span 会被直接丢掉。
	ShutdownTimeout time.Duration `yaml:"ShutdownTimeout"`

	// ForwardHeaders 向所有域名透传的 Header。默认无。
	ForwardHeaders []string `yaml:"ForwardHeaders"`

	// ForwardHeaderRules 按域名透传的 Header 规则。默认无。
	//
	// 用于不该外泄的内部标识：只发给自己人，不发给第三方。
	ForwardHeaderRules []ForwardHeaderRule `yaml:"ForwardHeaderRules"`
}

// DefaultConfig 全部默认值集中在这里。
func DefaultConfig() Config {
	return Config{
		Enable:          true,
		SampleRatio:     1,
		ShutdownTimeout: 5 * time.Second,
	}
}

// validate 检查配置本身说不通的地方
func (c Config) validate() error {
	if c.ShutdownTimeout <= 0 {
		return fmt.Errorf("xtrace: invalid config: ShutdownTimeout must be > 0 "+
			"(0 is not unlimited, it is no wait at all, and buffered spans get dropped), got=%v", c.ShutdownTimeout)
	}
	return nil
}

// forwardEnabled 是否配置了 Header 透传
func (c Config) forwardEnabled() bool {
	return len(c.ForwardHeaders) > 0 || len(c.ForwardHeaderRules) > 0
}
