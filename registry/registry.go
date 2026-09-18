// Package registry 是 contrib 包唯一需要认识的东西。
//
// 它零第三方依赖、不读配置、不启动任何东西——只是一块登记板。
// 单独拆出来是为了让「contrib 不会反向依赖框架核心」在编译层面成立：
// contrib 只能 import 这里，import 不到 scaffold 本体。
package registry

import (
	"io"
	"sync"
)

// Stage 初始化档位。用固定几档而不是任意数字：
// 数字需要全局协调（谁该填 30、谁该填 50），档位不需要——
// 同一档内的组件本来就互不依赖，顺序无所谓。
type Stage int

const (
	// StageLog 日志：最先起、最后关，这样其余组件的启停日志都写得出去
	StageLog Stage = iota

	// StageTelemetry 链路与指标：要在任何客户端之前就绪，
	// 客户端发出的 Span 才挂得上、打的指标才收得到
	StageTelemetry

	// StageClient 数据库、缓存、HTTP 客户端等被服务依赖的东西
	StageClient

	// StageServer 对外服务：最后起、最先关
	StageServer
)

// Component 一个可被托管的组件。
type Component struct {
	// Key 配置文件里对应的顶层 key，如 "XGorm"。留空表示不需要配置。
	Key string
	// Stage 属于哪一档。
	Stage Stage
	// Config 指向已填好默认值的配置结构体，框架把 Key 那一段解进去。
	Config any
	// Init 配置就绪后调用。返回的 Closer 会在退出时被逆序关闭。
	Init func() (io.Closer, error)
}

var (
	mu   sync.Mutex
	list []Component
)

// Register 登记一个组件。只是记下来，不做任何初始化——
// 所以 Go 的 init 执行顺序对最终结果没有影响，真正的顺序由 Stage 决定。
func Register(c Component) {
	mu.Lock()
	defer mu.Unlock()
	list = append(list, c)
}

// Snapshot 取出当前登记的全部组件，供框架初始化时使用。
func Snapshot() []Component {
	mu.Lock()
	defer mu.Unlock()
	out := make([]Component, len(list))
	copy(out, list)
	return out
}
