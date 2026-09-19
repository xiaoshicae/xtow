// Package conf 演示业务自己的配置块怎么接进框架。
//
// 框架对没人认领的顶层 key 是直接启动失败的（多半是拼错了，或者忘了
// import 对应的包），所以配置文件里写了 MyApp，就得有人认领它。
// 认领只要一次 registry.Register，不需要 Init——本包没有任何东西要初始化。
//
// 单独一个包而不是写在 main 里：认领配置这件事不依赖框架本体，
// 拆开之后测试可以只 import 它，不必把 Run 一起拖进来。
package conf

import (
	"time"

	"github.com/xiaoshicae/xtow/registry"
)

// Config 业务自己的配置
type Config struct {
	// Topic 订阅哪个主题
	Topic string `yaml:"Topic"`

	// Workers 并发处理消息的协程数。默认 4
	Workers int `yaml:"Workers"`

	// MessageTimeout 单条消息的处理上限。默认 5s。
	// 必须小于 xtow.WithStopTimeout（默认 15s），见 Consumer.timeout
	MessageTimeout time.Duration `yaml:"MessageTimeout"`
}

// DefaultConfig 默认值预填在结构体里，文件里没写的字段保持不变
func DefaultConfig() Config {
	return Config{
		Topic:          "orders",
		Workers:        4,
		MessageTimeout: 5 * time.Second,
	}
}

var cfg = DefaultConfig()

// C 返回解好的配置。要在 xtow.Run 开始之后读，否则拿到的是默认值。
func C() Config { return cfg }

// init 只认领配置，没有要初始化的东西
func init() {
	registry.Register(registry.Component{Key: "MyApp", Config: &cfg})
}
