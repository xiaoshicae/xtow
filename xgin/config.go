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
	"net"
	"strings"
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

	// ShutdownTimeout 优雅退出时等在途请求做完的上限。默认 10s，必须大于 0。
	//
	// 这是「HTTP 服务能占用的那一份」，不是整个退出流程的预算——后者是
	// xtow.WithStopTimeout（默认 15s），两者取更早的那个截止时间。
	// 所以这一项配得比总预算大没有意义，该调的是总预算。
	//
	// 总预算要小于部署环境给的终止宽限期（K8s 默认 30s）：两者相等意味着
	// 关闭还没走完进程就被 SIGKILL，等于没有优雅退出。
	//
	// 注意 0 不是「不限时」而是「一点都不等」：它会让 Shutdown 拿到一个
	// 已经过期的 context，在途请求当场被切断。所以这里拦住它。
	ShutdownTimeout time.Duration `yaml:"ShutdownTimeout"`

	// MaxMultipartMemory 解析 multipart 表单时在内存里留多少，单位字节。默认 8MB。
	//
	// 它不是「请求体上限」，是「超过多少才落盘」：超出的部分写进临时文件，
	// 不会被拒绝。实际代价是这个数的三倍左右——一次 60MB 的上传，
	// 配 32MB 时解析这一步让堆多占 96MB，配 8MB 是 24MB，配 1MB 是 3MB。
	// gin 自己默认 32MB，二十个并发上传就是两个 G。
	//
	// 框架不替业务定请求体上限，那得按接口来。要限的话在中间件里：
	//
	//	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 10<<20)
	MaxMultipartMemory int64 `yaml:"MaxMultipartMemory"`

	// TrustedProxies 信任哪些代理发来的 X-Forwarded-For / X-Real-IP。
	// 默认一个都不信，此时 ClientIP() 就是对端地址本身。
	//
	// gin 自己的默认是「全都信」，那意味着任何人发一个
	// X-Forwarded-For: 1.2.3.4 就能决定访问日志里的 client_ip 是什么——
	// 日志可以被伪造，建在这个字段上的限流和审计也一起失效。
	// 这种事不该靠使用者记得去关，所以这里默认关掉。
	//
	// 真的在负载均衡后面时，把它那一段网段写进来：
	//
	//	TrustedProxies: ["10.0.0.0/8"]
	TrustedProxies []string `yaml:"TrustedProxies"`

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
		Host:               "0.0.0.0",
		Port:               8080,
		ReadHeaderTimeout:  10 * time.Second,
		IdleTimeout:        60 * time.Second,
		MaxMultipartMemory: 8 << 20,
		ShutdownTimeout:    10 * time.Second,
		Mode:               "release",
	}
}

// validate 检查配置本身说不通的地方
func (c Config) validate() error {
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("Port must be within 1..65535, got=%d", c.Port)
	}
	// 只配一半的 TLS 是最危险的一种配错：服务会以明文起来，
	// 而配置文件看上去是配了证书的
	if (c.CertFile == "") != (c.KeyFile == "") {
		return fmt.Errorf("CertFile and KeyFile must both be set or both be empty")
	}
	// 0 在这里不是「不限时」而是「一点都不等」：Shutdown 会拿到一个已经过期的
	// context，在途请求当场被切断，而配置文件看上去只是没设上限
	if c.MaxMultipartMemory <= 0 {
		return fmt.Errorf("MaxMultipartMemory must be > 0, got=%d", c.MaxMultipartMemory)
	}
	if c.ShutdownTimeout <= 0 {
		return fmt.Errorf("ShutdownTimeout must be > 0 (0 is not unlimited, it is no wait at all), got=%v", c.ShutdownTimeout)
	}
	switch c.Mode {
	case "release", "debug", "test":
	default:
		return fmt.Errorf("unknown Mode=%q, supported: release / debug / test", c.Mode)
	}
	// 网段写错了就直接起不来。gin 那边的行为是解析到出错为止、把已经解出来的
	// 留下，于是前半段代理被信任、后半段被悄悄丢掉——日志里的 client_ip
	// 一半真一半假，是比起不来难查得多的状态
	for _, p := range c.TrustedProxies {
		if !isIPOrCIDR(p) {
			return fmt.Errorf("TrustedProxies contains an invalid address, want an IP or CIDR, got=%q", p)
		}
	}
	return nil
}

// isIPOrCIDR 判断一段是不是合法的 IP 或者网段，与 gin 接受的写法一致
func isIPOrCIDR(s string) bool {
	if strings.Contains(s, "/") {
		_, _, err := net.ParseCIDR(s)
		return err == nil
	}
	return net.ParseIP(s) != nil
}

// tlsEnabled 是否配了 TLS
func (c Config) tlsEnabled() bool { return c.CertFile != "" && c.KeyFile != "" }
