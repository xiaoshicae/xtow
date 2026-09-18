module github.com/xiaoshicae/xtow/xcache

go 1.25.0

require (
	github.com/dgraph-io/ristretto/v2 v2.4.2
	github.com/xiaoshicae/xtow v0.0.0
	go.yaml.in/yaml/v3 v3.0.4
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	golang.org/x/sys v0.36.0 // indirect
)

// 还没打 tag，本地开发与 CI 都走仓库内的相对路径
replace github.com/xiaoshicae/xtow => ../
