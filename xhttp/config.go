// Package xhttp 按配置装好出站 HTTP 客户端，使用者拿到的是原生的 *resty.Client。
//
//	resp, err := xhttp.R(ctx).SetResult(&out).Get(url)
//
// 链路、指标、连接池、重试策略都由配置决定，业务代码里没有初始化。
package xhttp

import (
	"fmt"
	"time"
)

// ConfigKey 本模块在配置文件里的顶层 key
const ConfigKey = "XHttp"

// Config 出站 HTTP 客户端配置
//
// 与 xgorm / xredis 不同，这里没有多实例：HTTP 客户端不连任何外部资源，
// 不配也能用，配一份公共的连接池和超时就够了。需要第二套参数的场景
// 直接用 New 自己建一个。
type Config struct {
	// Timeout 单次尝试的超时。默认 60s。
	//
	// 配 0 是「永不超时」，不是「用个默认值」：对端不响应时请求会一直挂着。
	//
	// 注意它管的是一次尝试，不是一次逻辑请求：开了 RetryCount 之后，
	// 最坏情况是 (RetryCount+1) × Timeout 再加上几次退避等待
	// （实测 Timeout=300ms、RetryCount=3 时整整 1.24s）。
	// 要给整个逻辑请求封顶，用调用方的 ctx——重试的退避和每次尝试都听它的：
	//
	//	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	//	defer cancel()
	//	resp, err := xhttp.C().R().SetContext(ctx).Get(url)
	Timeout time.Duration `yaml:"Timeout"`

	// DialTimeout 建立 TCP 连接的超时。默认 30s。
	DialTimeout time.Duration `yaml:"DialTimeout"`

	// DialKeepAlive TCP keep-alive 探测间隔。默认 30s。
	DialKeepAlive time.Duration `yaml:"DialKeepAlive"`

	// MaxIdleConns 全局最大空闲连接数。默认 100。
	MaxIdleConns int `yaml:"MaxIdleConns"`

	// MaxIdleConnsPerHost 每个 host 的最大空闲连接数。默认 10。
	//
	// 标准库的默认值是 2，对只调几个下游的服务来说太小，
	// 会让连接反复重建，尾延迟里全是握手。
	MaxIdleConnsPerHost int `yaml:"MaxIdleConnsPerHost"`

	// IdleConnTimeout 空闲连接多久后关闭。默认 90s。
	IdleConnTimeout time.Duration `yaml:"IdleConnTimeout"`

	// RetryCount 重试次数。默认 0，即不重试。
	RetryCount int `yaml:"RetryCount"`

	// RetryWaitTime 重试的起始等待时间。默认 100ms。
	RetryWaitTime time.Duration `yaml:"RetryWaitTime"`

	// RetryMaxWaitTime 重试等待时间的上限。默认 2s。
	RetryMaxWaitTime time.Duration `yaml:"RetryMaxWaitTime"`

	// RetryOnlyIdempotent 是否只重试幂等方法。默认开启。
	//
	// 传输层超时分不出「请求没到服务端」和「服务端处理完了但响应丢了」，
	// 重发一个 POST 就可能变成重复下单、重复扣款。
	// 确认接口幂等（比如带幂等键）之后再关掉它。
	RetryOnlyIdempotent bool `yaml:"RetryOnlyIdempotent"`

	// Trace 是否为出站请求开 Span 并透传链路 Header。默认开启。
	Trace bool `yaml:"Trace"`

	// Metric 是否导出出站请求的指标（按方法、目标、状态码分）。默认开启。
	Metric bool `yaml:"Metric"`
}

// DefaultConfig 全部默认值集中在这里
func DefaultConfig() Config {
	return Config{
		Timeout:             60 * time.Second,
		DialTimeout:         30 * time.Second,
		DialKeepAlive:       30 * time.Second,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		RetryWaitTime:       100 * time.Millisecond,
		RetryMaxWaitTime:    2 * time.Second,
		RetryOnlyIdempotent: true,
		Trace:               true,
		Metric:              true,
	}
}

// validate 检查配置本身说不通的地方
func (c Config) validate() error {
	if c.RetryCount < 0 {
		return fmt.Errorf("RetryCount must not be negative, got=%d", c.RetryCount)
	}
	if c.MaxIdleConns < 0 || c.MaxIdleConnsPerHost < 0 {
		return fmt.Errorf("connection counts must not be negative, MaxIdleConns=%d MaxIdleConnsPerHost=%d", c.MaxIdleConns, c.MaxIdleConnsPerHost)
	}
	return nil
}
