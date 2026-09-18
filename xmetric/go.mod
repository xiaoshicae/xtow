module github.com/xiaoshicae/xtow/xmetric

go 1.25.0

require github.com/xiaoshicae/xtow v0.0.0

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/prometheus/client_golang v1.24.1
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.70.1 // indirect
	github.com/prometheus/procfs v0.21.1 // indirect
	golang.org/x/sys v0.47.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

// 核心还没打 tag，本地开发与 CI 都走仓库内的相对路径。
// 消费者的构建会忽略依赖里的 replace，所以这行不影响使用者。
// 打第一个 tag 之后改成真实版本号。
replace github.com/xiaoshicae/xtow => ../
