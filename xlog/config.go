// Package xlog 基于标准库 log/slog 提供配置驱动的日志：级别、格式、控制台与文件轮转。
//
// 使用者拿到的是原生的 *slog.Logger，业务代码直接用标准库的
// slog.Info / slog.InfoContext 即可，不需要认识本包。
package xlog

import (
	"time"
)

// ConfigKey 本模块在配置文件里的顶层 key
const ConfigKey = "XLog"

// 日志格式
const (
	FormatJSON = "json"
	FormatText = "text"
)

// Config 日志配置
type Config struct {
	// Level 日志级别：debug / info / warn / error
	Level string `yaml:"Level"`

	// Format 输出格式：json / text
	Format string `yaml:"Format"`

	// AddSource 是否记录打日志的代码位置。有开销，默认关闭。
	AddSource bool `yaml:"AddSource"`

	// Console 是否打到标准输出，由部署环境的日志采集组件收集。默认开启。
	Console bool `yaml:"Console"`

	// File 文件输出
	File FileConfig `yaml:"File"`
}

// FileConfig 文件输出配置
type FileConfig struct {
	// Enable 是否写文件。默认关闭。
	Enable bool `yaml:"Enable"`

	// Path 日志目录
	Path string `yaml:"Path"`

	// Name 日志文件名。实际文件是 {Name}.{时间后缀}，
	// 另有一个指向当前文件的同名符号链接，便于 tail 跟随。
	Name string `yaml:"Name"`

	// RotateTime 轮转周期。默认一天，按本地时区对齐。
	RotateTime time.Duration `yaml:"RotateTime"`

	// MaxAge 历史文件保留时长，<=0 表示不清理。默认 7 天。
	MaxAge time.Duration `yaml:"MaxAge"`

	// Perm 日志文件权限，八进制字符串。默认 "0644"。
	//
	// 用字符串而不是数字：YAML 里写 0644 会被当成十进制 644 解析，
	// 那是 --w----r-T，几乎肯定不是使用者想要的。
	Perm string `yaml:"Perm"`
}

// DefaultConfig 全部默认值集中在这里。
//
// 文件里没写的字段保持这些值，写了的覆盖——包括显式写成零值（false / 0 / ""）。
func DefaultConfig() Config {
	return Config{
		Level:   "info",
		Format:  FormatJSON,
		Console: true,
		File: FileConfig{
			Name:       "app.log",
			RotateTime: 24 * time.Hour,
			MaxAge:     7 * 24 * time.Hour,
			Perm:       "0644",
		},
	}
}
