module github.com/xiaoshicae/xtow/xredis

go 1.25.0

require (
	github.com/prometheus/client_golang v1.24.1
	github.com/redis/go-redis/extra/redisotel/v9 v9.22.0
	github.com/redis/go-redis/v9 v9.22.0
	github.com/xiaoshicae/xtow v0.0.0
	github.com/xiaoshicae/xtow/xmetric v0.0.0
	go.yaml.in/yaml/v3 v3.0.4
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.70.1 // indirect
	github.com/prometheus/procfs v0.21.1 // indirect
	github.com/redis/go-redis/extra/rediscmd/v9 v9.22.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel v1.43.0 // indirect
	go.opentelemetry.io/otel/metric v1.43.0 // indirect
	go.opentelemetry.io/otel/trace v1.43.0 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

// 都还没打 tag，本地开发与 CI 都走仓库内的相对路径
replace (
	github.com/xiaoshicae/xtow => ../
	github.com/xiaoshicae/xtow/xmetric => ../xmetric
)
