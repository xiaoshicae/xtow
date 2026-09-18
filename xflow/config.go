package xflow

import (
	"fmt"
	"io"
	"time"

	"github.com/xiaoshicae/xtow/registry"
)

// ConfigKey 本模块在配置文件里的顶层 key
const ConfigKey = "XFlow"

// Config 流程编排配置
type Config struct {
	// Monitor 是否开启监控回调。默认开启。
	//
	// 关掉之后 Execute 一次监控回调都不走，是真正的零开销。
	Monitor bool `yaml:"Monitor"`

	// RollbackTimeout 回滚全部步骤的总预算。默认 30s。
	//
	// 回滚不沿用调用方的 context（否则请求一超时，补偿必然全部失败），
	// 改由这一项单独限时，免得补偿逻辑无限期挂住退出流程。
	RollbackTimeout time.Duration `yaml:"RollbackTimeout"`
}

// DefaultConfig 全部默认值集中在这里
func DefaultConfig() Config {
	return Config{Monitor: true, RollbackTimeout: 30 * time.Second}
}

func (c Config) validate() error {
	if c.RollbackTimeout <= 0 {
		return fmt.Errorf("RollbackTimeout 必须大于 0，got=%v", c.RollbackTimeout)
	}
	return nil
}

var cfg = DefaultConfig()

// init 登记配置，并在启动时校验一次。
//
// 本模块没有要初始化的资源，Init 只做校验：RollbackTimeout 配成 0
// 会让每次回滚一进去就判超时、所有补偿被跳过，而流程本身看起来一切正常。
// 这种配错必须在启动时就拦住。
func init() {
	registry.Register(registry.Component{
		Key:    ConfigKey,
		Config: &cfg,
		Init: func() (io.Closer, error) {
			if err := cfg.validate(); err != nil {
				return nil, fmt.Errorf("xflow: 配置有误: %w", err)
			}
			return nil, nil // 没有要关的东西
		},
	})
}
