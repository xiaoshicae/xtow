// Package xapp 提供应用自身的身份：名字和版本。
//
// 这些是多个组件共用的事实——链路要它做 service.name，指标要它做标签，
// 服务端要它做注册名。放在任何一个组件的配置里，另外几个就得重复配一遍，
// 然后总有一天它们会不一致。所以单独一块，谁都能读。
//
//	App:
//	  Name: xone.demo.app
//	  Version: v1.2.0
//
// 本包不初始化任何东西，只是在配置加载时认领 App 这一块。
package xapp

import "github.com/xiaoshicae/xtow/registry"

// ConfigKey 本模块在配置文件里的顶层 key
const ConfigKey = "App"

// Config 应用身份
type Config struct {
	// Name 应用名，建议全局唯一，如 team.system.app
	Name string `yaml:"Name"`

	// Version 应用版本，如 v1.2.0
	Version string `yaml:"Version"`
}

// DefaultConfig 默认值。名字留空——猜一个名字比没有名字更糟：
// 链路和指标上会出现一堆同名的 unknown，反而看不出是谁。
func DefaultConfig() Config { return Config{} }

var cfg = DefaultConfig()

// init 只认领配置，没有要初始化的东西
func init() {
	registry.Register(registry.Component{Key: ConfigKey, Config: &cfg})
}

// Name 返回应用名，未配置时为空字符串。
//
// 配置在所有组件初始化之前就已加载完毕，所以任何组件的 Init 里读到的都是最终值。
func Name() string { return cfg.Name }

// Version 返回应用版本，未配置时为空字符串。
func Version() string { return cfg.Version }
