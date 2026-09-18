// Package xginswagger 把 Swagger UI 挂到 Gin 上。
//
//	gx := xgin.New().WithRoutes(func(e *gin.Engine) {
//		xginswagger.Register(e, docs.SwaggerInfo)
//		registerBusinessRoutes(e)
//	})
//
// 单独一个 module，不并进 xgin：Swagger UI 会把整套前端资源编进二进制，
// 实测比只用 gin 多 26 个模块。文档是开发期的事，不该让每个线上服务都背上。
package xginswagger

import (
	"github.com/gin-gonic/gin"
	swaggerfiles "github.com/swaggo/files"
	ginswagger "github.com/swaggo/gin-swagger"
	"github.com/swaggo/swag"

	"github.com/xiaoshicae/xtow/registry"
	"github.com/xiaoshicae/xtow/xapp"
)

// ConfigKey 本模块在配置文件里的顶层 key
const ConfigKey = "XGinSwagger"

// route Swagger UI 的路由，*any 是 gin 的通配段，Swagger UI 要用它加载各种资源
const route = "/swagger/*any"

// Config Swagger 文档的元信息
//
// 这些值写在配置里而不是代码注释里，是因为它们随环境变：
// 联调环境和生产环境的 Host 不一样，而文档注解是编译进去的。
type Config struct {
	// Host 文档里显示的服务地址，如 api.example.com。默认取自请求。
	Host string `yaml:"Host"`

	// BasePath 所有接口的公共前缀，如 /api/v1。
	BasePath string `yaml:"BasePath"`

	// Title 文档标题。默认取 App.Name。
	Title string `yaml:"Title"`

	// Description 文档描述。
	Description string `yaml:"Description"`

	// Schemes 支持的协议。默认 ["https", "http"]。
	Schemes []string `yaml:"Schemes"`

	// URLPrefix Swagger UI 挂载路径的前缀，如 /internal。
	// 默认挂在 /swagger/*any。
	URLPrefix string `yaml:"URLPrefix"`
}

// DefaultConfig 全部默认值集中在这里
func DefaultConfig() Config {
	return Config{Schemes: []string{"https", "http"}}
}

var cfg = DefaultConfig()

// init 只认领配置，没有要初始化的东西
func init() {
	registry.Register(registry.Component{Key: ConfigKey, Config: &cfg})
}

// Register 把 Swagger UI 挂到 engine 上，并用配置填好文档元信息。
//
// info 通常是 swaggo 生成的 docs.SwaggerInfo。传 nil 表示只挂 UI 不填元信息。
func Register(e *gin.Engine, info *swag.Spec) {
	if e == nil {
		return
	}
	if info != nil {
		fill(info)
	}
	e.GET(cfg.URLPrefix+route, ginswagger.WrapHandler(swaggerfiles.Handler))
}

// URL 返回 Swagger UI 的实际访问路径，方便打日志或做跳转
func URL() string { return cfg.URLPrefix + "/swagger/index.html" }

// fill 把配置写进 swaggo 生成的元信息
//
// 只覆盖配了的字段：没配就保留注解里写的那份，
// 否则一个空的配置块会把文档标题清成空白。
func fill(info *swag.Spec) {
	if cfg.Host != "" {
		info.Host = cfg.Host
	}
	if cfg.BasePath != "" {
		info.BasePath = cfg.BasePath
	}
	if cfg.Description != "" {
		info.Description = cfg.Description
	}
	if len(cfg.Schemes) > 0 {
		info.Schemes = cfg.Schemes
	}

	// 标题和版本默认跟 App 走：同一个事实配两遍迟早会不一致
	switch {
	case cfg.Title != "":
		info.Title = cfg.Title
	case xapp.Name() != "":
		info.Title = xapp.Name()
	}
	if v := xapp.Version(); v != "" {
		info.Version = v
	}
}
