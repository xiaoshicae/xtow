// Package xredis 按配置装好 go-redis，使用者拿到的是原生的 *redis.Client。
//
//	v, err := xredis.C().Get(ctx, "key").Result()
//
// 不提供 CWithCtx：go-redis 的每个方法本来就收 ctx，再包一层没有意义。
package xredis

import (
	"fmt"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/xiaoshicae/xtow/xclient"
	"github.com/xiaoshicae/xtow/xconfig"
)

// ConfigKey 本模块在配置文件里的顶层 key
const ConfigKey = "XRedis"

// DefaultName C() 不带参数时取的那个实例的名字
const DefaultName = xclient.DefaultName

// Config 本模块的配置。两种写法：
//
//	XRedis:                 # 单实例
//	  Addr: "127.0.0.1:6379"
//
//	XRedis:                 # 多实例
//	  Clients:
//	    default: {Addr: "127.0.0.1:6379"}
//	    cache:   {Addr: "127.0.0.1:6380", DB: 1}
type Config struct {
	// Clients 按名字组织的实例。单实例写法会被规整成一个名为 default 的实例。
	Clients map[string]ClientConfig
}

// ClientConfig 一个 Redis 实例的配置
type ClientConfig struct {
	// Addr 服务器地址。默认 127.0.0.1:6379。
	Addr string `yaml:"Addr"`

	// Username ACL 用户名（Redis 6.0+）。默认无。
	Username string `yaml:"Username"`

	// Password 认证密码。默认无。
	//
	// 建议写成 "${REDIS_PASSWORD}"：凭证不该进版本库，漏配时启动就失败。
	// 本模块不会把它写进任何日志。
	Password string `yaml:"Password"`

	// DB 数据库编号。默认 0。
	DB int `yaml:"DB"`

	// DialTimeout 建连超时。默认 500ms。
	DialTimeout time.Duration `yaml:"DialTimeout"`

	// ReadTimeout 读超时。默认 500ms。
	ReadTimeout time.Duration `yaml:"ReadTimeout"`

	// WriteTimeout 写超时。默认 500ms。
	WriteTimeout time.Duration `yaml:"WriteTimeout"`

	// PoolSize 连接池大小。默认 0，交给 go-redis（10 × GOMAXPROCS）。
	PoolSize int `yaml:"PoolSize"`

	// MinIdleConns 最小空闲连接数，预热连接避免冷启动抖动。默认 5。
	MinIdleConns int `yaml:"MinIdleConns"`

	// MaxIdleConns 最大空闲连接数。默认 0，即不限制。
	MaxIdleConns int `yaml:"MaxIdleConns"`

	// MaxActiveConns 最大活跃连接数。默认 0，即不限制。
	MaxActiveConns int `yaml:"MaxActiveConns"`

	// PoolTimeout 池子没有空闲连接时的等待上限。默认 1s。
	PoolTimeout time.Duration `yaml:"PoolTimeout"`

	// ConnMaxIdleTime 空闲连接最长存活时间。默认 5m。
	ConnMaxIdleTime time.Duration `yaml:"ConnMaxIdleTime"`

	// ConnMaxLifetime 连接最长存活时间。默认 5m。
	//
	// 定期换连接，服务端扩缩容后流量才会重新摊开。
	ConnMaxLifetime time.Duration `yaml:"ConnMaxLifetime"`

	// MaxRetries 最大重试次数。默认 0，交给 go-redis（3 次）；配 -1 关闭重试。
	MaxRetries int `yaml:"MaxRetries"`

	// MinRetryBackoff 最小重试退避。默认 0，交给 go-redis（8ms）；配 -1 关闭退避。
	MinRetryBackoff time.Duration `yaml:"MinRetryBackoff"`

	// MaxRetryBackoff 最大重试退避。默认 0，交给 go-redis（512ms）；配 -1 关闭退避。
	MaxRetryBackoff time.Duration `yaml:"MaxRetryBackoff"`

	// Trace 是否挂 OpenTelemetry 钩子。默认开启。
	//
	// 没装链路时它产出的是 noop Span，代价可以忽略，所以默认就开着。
	Trace bool `yaml:"Trace"`

	// Metric 是否导出连接池指标。默认开启。
	//
	// 指标在被抓取时才读 PoolStats()，不额外占协程。
	Metric bool `yaml:"Metric"`
}

// DefaultClientConfig 单个实例的全部默认值集中在这里
func DefaultClientConfig() ClientConfig {
	return ClientConfig{
		Addr:            "127.0.0.1:6379",
		DialTimeout:     500 * time.Millisecond,
		ReadTimeout:     500 * time.Millisecond,
		WriteTimeout:    500 * time.Millisecond,
		MinIdleConns:    5,
		PoolTimeout:     time.Second,
		ConnMaxIdleTime: 5 * time.Minute,
		ConnMaxLifetime: 5 * time.Minute,
		Trace:           true,
		Metric:          true,
	}
}

// DefaultConfig 默认没有任何实例——没配 XRedis 就不该连任何 Redis
func DefaultConfig() Config { return Config{} }

// UnmarshalYAML 支持单实例和多实例两种写法，解码规则见 xconfig.DecodeClients
func (c *Config) UnmarshalYAML(n *yaml.Node) error {
	clients, err := xconfig.DecodeClients(n, DefaultClientConfig)
	if err != nil {
		return err
	}
	c.Clients = clients
	return nil
}

// UnmarshalYAML 先铺默认值再解，map 的 value 是从零值开始的，框架替不了它
func (c *ClientConfig) UnmarshalYAML(n *yaml.Node) error {
	*c = DefaultClientConfig()
	type raw ClientConfig // 换个类型，否则这里递归调用自己
	return xconfig.DecodeStrict(n, (*raw)(c))
}

// validate 检查配置本身说不通的地方，在建连之前就失败
func (c ClientConfig) validate() error {
	if c.Addr == "" {
		return fmt.Errorf("Addr 不能为空")
	}
	if c.DB < 0 {
		return fmt.Errorf("DB 不能为负数，got=%d", c.DB)
	}
	return nil
}
