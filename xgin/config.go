// Package xgin 按配置装好 Gin，使用者拿到的是原生的 *gin.Engine。
//
//	func main() {
//		gx := xgin.New().WithRoutes(registerRoutes)
//		xtow.MustRun(gx)
//	}
//
// 内置日志、链路、指标、panic 恢复四个中间件，顺序和开关都由框架管；
// 监听地址、超时、TLS 由配置决定，业务代码里没有初始化。
package xgin

import (
	"fmt"
	"time"
)

// ConfigKey 本模块在配置文件里的顶层 key
const ConfigKey = "XGin"

// Config 服务端配置
type Config struct {
	// Host 监听地址。默认 0.0.0.0。
	Host string `yaml:"Host"`

	// Port 监听端口。默认 8080。
	Port int `yaml:"Port"`

	// UseH2C 是否在非 TLS 下启用 HTTP/2。默认关闭。
	//
	// TLS 模式下 HTTP/2 本来就是自动的，不需要这一项。
	UseH2C bool `yaml:"UseH2C"`

	// CertFile TLS 证书路径。与 KeyFile 必须同时配或同时不配。
	CertFile string `yaml:"CertFile"`

	// KeyFile TLS 私钥路径。
	KeyFile string `yaml:"KeyFile"`

	// ReadHeaderTimeout 读请求头的超时。默认 10s。
	//
	// 这是慢连接攻击的主要防线：不限制的话，慢客户端可以一直占着连接不放，
	// 连接数打满之后服务整体不可用。
	ReadHeaderTimeout time.Duration `yaml:"ReadHeaderTimeout"`

	// ReadTimeout 读完整个请求（含 body）的超时。默认不限制。
	//
	// 默认不限制是因为它会打断大文件上传。按业务上限配一个值更好，
	// 但那个值只有业务自己知道。
	ReadTimeout time.Duration `yaml:"ReadTimeout"`

	// WriteTimeout 写响应的超时。默认不限制。
	//
	// 默认不限制是因为它会打断 SSE、长轮询和大文件下载。
	WriteTimeout time.Duration `yaml:"WriteTimeout"`

	// IdleTimeout keep-alive 连接的空闲超时。默认 60s。
	IdleTimeout time.Duration `yaml:"IdleTimeout"`

	// ShutdownTimeout 优雅退出的等待上限。默认 25s。
	//
	// 要小于部署环境给的终止宽限期（K8s 默认 30s）：两者相等意味着
	// Shutdown 还没走完进程就被 SIGKILL，等于没有优雅退出。
	ShutdownTimeout time.Duration `yaml:"ShutdownTimeout"`

	// Mode Gin 的运行模式：release / debug / test。默认 release。
	//
	// 默认 release 而不是跟随 GIN_MODE：debug 模式会打印每一条路由、
	// 每个请求多打一行日志，还会在响应里带上调试信息。
	// 线上忘了设环境变量的代价比本地少一行提示大得多。
	Mode string `yaml:"Mode"`
}

// DefaultConfig 全部默认值集中在这里
func DefaultConfig() Config {
	return Config{
		Host:              "0.0.0.0",
		Port:              8080,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		ShutdownTimeout:   25 * time.Second,
		Mode:              "release",
	}
}

// validate 检查配置本身说不通的地方
func (c Config) validate() error {
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("Port 必须在 1..65535 之间，got=%d", c.Port)
	}
	// 只配一半的 TLS 是最危险的一种配错：服务会以明文起来，
	// 而配置文件看上去是配了证书的
	if (c.CertFile == "") != (c.KeyFile == "") {
		return fmt.Errorf("CertFile 和 KeyFile 必须同时配置或同时留空")
	}
	switch c.Mode {
	case "release", "debug", "test":
	default:
		return fmt.Errorf("不认识的 Mode=%q，支持 release / debug / test", c.Mode)
	}
	return nil
}

// tlsEnabled 是否配了 TLS
func (c Config) tlsEnabled() bool { return c.CertFile != "" && c.KeyFile != "" }
