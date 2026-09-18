module github.com/xiaoshicae/xtow/xtrace

go 1.25.0

require (
	github.com/xiaoshicae/xtow v0.0.0
	go.opentelemetry.io/contrib/propagators/b3 v1.46.0
	go.opentelemetry.io/otel v1.46.0
	go.opentelemetry.io/otel/exporters/stdout/stdouttrace v1.46.0
	go.opentelemetry.io/otel/sdk v1.46.0
	go.opentelemetry.io/otel/trace v1.46.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel/metric v1.46.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/sys v0.47.0 // indirect
)

// 核心还没打 tag，本地开发与 CI 都走仓库内的相对路径。
// 消费者的构建会忽略依赖里的 replace，所以这行不影响使用者。
// 打第一个 tag 之后改成真实版本号。
replace github.com/xiaoshicae/xtow => ../
