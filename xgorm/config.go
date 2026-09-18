// Package xgorm 按配置装好 GORM，使用者拿到的是原生的 *gorm.DB。
//
//	err := xgorm.CWithCtx(ctx).First(&u, id).Error
//
// 支持 MySQL 与 PostgreSQL，连接池、超时、慢查询日志、链路、连接池指标
// 全部由配置决定，业务代码里没有任何初始化。
package xgorm

import (
	"fmt"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/xiaoshicae/xtow/xconfig"
)

// ConfigKey 本模块在配置文件里的顶层 key
const ConfigKey = "XGorm"

// DefaultName C() 不带参数时取的那个 client 的名字
const DefaultName = "default"

// Driver 数据库驱动
type Driver string

const (
	DriverMySQL    Driver = "mysql"
	DriverPostgres Driver = "postgres"
)

// Config 本模块的配置。两种写法：
//
//	XGorm:                  # 单实例，直接写字段，名字就是 default
//	  DSN: "${DB_DSN}"
//
//	XGorm:                  # 多实例，按名字写
//	  Clients:
//	    default: {DSN: "${DB_DSN}"}
//	    report:  {DSN: "${REPORT_DSN}", MaxOpenConns: 5}
type Config struct {
	// Clients 按名字组织的实例。单实例写法会被规整成一个名为 default 的实例。
	Clients map[string]ClientConfig
}

// ClientConfig 一个数据库实例的配置
type ClientConfig struct {
	// Driver 驱动：mysql / postgres。默认 postgres。
	Driver Driver `yaml:"Driver"`

	// DSN 连接串，必填。
	//
	// 建议写成 "${DB_DSN}"：凭证不该进版本库，漏配时启动就失败。
	DSN string `yaml:"DSN"`

	// DialTimeout 建连超时。默认 500ms。
	//
	// MySQL 注入 DSN 的 timeout，PostgreSQL 注入 connect_timeout（向上取整为秒）。
	DialTimeout time.Duration `yaml:"DialTimeout"`

	// MySQL 仅在 Driver 为 mysql 时生效
	MySQL MySQLConfig `yaml:"MySQL"`

	// Postgres 仅在 Driver 为 postgres 时生效
	Postgres PostgresConfig `yaml:"Postgres"`

	// MaxOpenConns 最大连接数。默认 50。
	MaxOpenConns int `yaml:"MaxOpenConns"`

	// MaxIdleConns 最大空闲连接数。默认 50。
	//
	// 配 0 是「一条空闲连接都不留」，每次查询都要重新建连，通常不是你想要的。
	// 比 MaxOpenConns 大也没关系，database/sql 会自己压到 MaxOpenConns。
	MaxIdleConns int `yaml:"MaxIdleConns"`

	// MaxLifetime 连接最长存活时间。默认 5m。
	MaxLifetime time.Duration `yaml:"MaxLifetime"`

	// MaxIdleTime 空闲连接最长存活时间。默认 5m。
	MaxIdleTime time.Duration `yaml:"MaxIdleTime"`

	// Log 是否把 GORM 的 SQL 日志接到 slog 上。默认关闭。
	Log bool `yaml:"Log"`

	// SlowThreshold 超过这个耗时的 SQL 记一条 warn 日志。默认 3s，需 Log 开启。
	SlowThreshold time.Duration `yaml:"SlowThreshold"`

	// IgnoreNotFound 是否不把「没查到记录」当错误记日志。默认 false。
	IgnoreNotFound bool `yaml:"IgnoreNotFound"`

	// Trace 是否挂 OpenTelemetry 插件。默认开启。
	//
	// 没装链路时它产出的是 noop Span，代价可以忽略，所以默认就开着。
	Trace bool `yaml:"Trace"`

	// Metric 是否导出连接池指标（连接数、等待次数、等待时长等）。默认开启。
	//
	// 指标在被抓取时才读 sql.DB.Stats()，不额外占协程。
	Metric bool `yaml:"Metric"`
}

// MySQLConfig MySQL 特有的配置
type MySQLConfig struct {
	// ReadTimeout 读超时，对应 DSN 的 readTimeout。默认 3s。
	ReadTimeout time.Duration `yaml:"ReadTimeout"`

	// WriteTimeout 写超时，对应 DSN 的 writeTimeout。默认 5s。
	WriteTimeout time.Duration `yaml:"WriteTimeout"`
}

// PostgresConfig PostgreSQL 特有的配置
//
// 这些值在建连时随 startup message 发给服务端，成为会话级 GUC。
// DSN 里已经显式写了同名 key 的话，以 DSN 为准。
type PostgresConfig struct {
	// StatementTimeout 单条 SQL 最长执行时间。默认不限制。
	StatementTimeout time.Duration `yaml:"StatementTimeout"`

	// LockTimeout 等锁的最长时间。默认不限制。
	LockTimeout time.Duration `yaml:"LockTimeout"`

	// IdleInTxTimeout 事务中空闲多久就断连。默认不限制。
	IdleInTxTimeout time.Duration `yaml:"IdleInTxTimeout"`

	// Params 其它任意 PG 运行时参数，原样拼进 DSN。
	Params map[string]string `yaml:"Params"`
}

// DefaultClientConfig 单个实例的全部默认值集中在这里
func DefaultClientConfig() ClientConfig {
	return ClientConfig{
		Driver:        DriverPostgres,
		DialTimeout:   500 * time.Millisecond,
		MySQL:         MySQLConfig{ReadTimeout: 3 * time.Second, WriteTimeout: 5 * time.Second},
		MaxOpenConns:  50,
		MaxIdleConns:  50,
		MaxLifetime:   5 * time.Minute,
		MaxIdleTime:   5 * time.Minute,
		SlowThreshold: 3 * time.Second,
		Trace:         true,
		Metric:        true,
	}
}

// DefaultConfig 默认没有任何实例——没配 XGorm 就不该连任何数据库
func DefaultConfig() Config { return Config{} }

// UnmarshalYAML 支持单实例和多实例两种写法
//
// 看有没有 Clients 决定按哪种解。两种混着写直接报错：
// 那时候「default 到底是哪个」没有一个不让人意外的答案。
func (c *Config) UnmarshalYAML(n *yaml.Node) error {
	if !xconfig.HasKey(n, "Clients") {
		single := DefaultClientConfig()
		if err := xconfig.DecodeStrict(n, &single); err != nil {
			return err
		}
		c.Clients = map[string]ClientConfig{DefaultName: single}
		return nil
	}

	var multi struct {
		Clients map[string]ClientConfig `yaml:"Clients"`
	}
	if err := xconfig.DecodeStrict(n, &multi); err != nil {
		return fmt.Errorf("%w（单实例和多实例两种写法不能混用：写了 Clients 就把所有字段都放进去）", err)
	}
	if len(multi.Clients) == 0 {
		return fmt.Errorf("Clients 是空的：要么写上实例，要么整块删掉")
	}
	c.Clients = multi.Clients
	return nil
}

// UnmarshalYAML 先铺默认值再解，这样文件里没写的字段保持默认
//
// map 的 value 是从零值开始解的，框架没法替它预填，只能由元素类型自己来。
func (c *ClientConfig) UnmarshalYAML(n *yaml.Node) error {
	*c = DefaultClientConfig()
	type raw ClientConfig // 换个类型，否则这里递归调用自己
	return xconfig.DecodeStrict(n, (*raw)(c))
}

// validate 检查配置本身说不通的地方，在建连之前就失败
func (c ClientConfig) validate() error {
	if c.DSN == "" {
		return fmt.Errorf("DSN 不能为空")
	}
	switch c.Driver {
	case DriverMySQL, DriverPostgres:
	default:
		return fmt.Errorf("不认识的 Driver=%q，支持 mysql / postgres", c.Driver)
	}
	if c.MaxOpenConns <= 0 {
		return fmt.Errorf("MaxOpenConns 必须大于 0，got=%d", c.MaxOpenConns)
	}
	return nil
}
