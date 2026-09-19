#!/bin/sh
# 用一个全新的外部工程验证「发布出去的版本装得上、跑得起来」。
#
#   ./verify.sh v0.2.0
#
# 必须在 git push --tags 之后跑。CI 跑的是工作区里的代码，模块之间还靠
# replace 互指，证明不了一个真正的使用者 go get 之后会发生什么——
# require 的版本对不对、tag 打全了没有、去掉 replace 还编不编得过，
# 全都只有这一步能回答。
set -e

VERSION="$1"
[ -n "$VERSION" ] || { echo "用法：./verify.sh v0.1.0"; exit 1; }

MOD=github.com/xiaoshicae/xtow
DIR=$(mktemp -d)
trap 'rm -rf "$DIR"' EXIT

echo "== 在 $DIR 里建一个干净的消费者工程 =="
cd "$DIR"
cat > go.mod <<MOD_EOF
module verify

go 1.25
MOD_EOF

# 一个真实使用者会写的样子：起服务、连库、打点，全部走公开 API
cat > main.go <<'GO_EOF'
package main

import (
	"github.com/gin-gonic/gin"

	"github.com/xiaoshicae/xtow"
	"github.com/xiaoshicae/xtow/xgin"
	_ "github.com/xiaoshicae/xtow/xgorm"
	_ "github.com/xiaoshicae/xtow/xgorm/clickhouse"
	_ "github.com/xiaoshicae/xtow/xhttp"
	"github.com/xiaoshicae/xtow/xmetric"
	_ "github.com/xiaoshicae/xtow/xredis"
	_ "github.com/xiaoshicae/xtow/xtrace"
)

func main() {
	xtow.MustRun(xgin.New().WithRoutes(func(e *gin.Engine) {
		e.GET("/ping", func(c *gin.Context) {
			xmetric.CounterInc("ping_total")
			c.String(200, "pong")
		})
	}))
}
GO_EOF

echo "== go get 各模块的 $VERSION =="
for m in "" /xtrace /xmetric /xcache /xgorm /xgorm/clickhouse /xredis /xhttp /xgin /xginswagger; do
  echo "  $MOD$m@$VERSION"
  GOFLAGS=-mod=mod go get "$MOD$m@$VERSION" >/dev/null
done

echo "== 编译 =="
go mod tidy >/dev/null
go build -o verify . 
echo "  ✓ 装得上、编得过"

echo "== 起一遍：启动 → 请求 → 退出 =="
./verify --config=/dev/null >out.txt 2>&1 &
pid=$!
trap 'kill -9 "$pid" 2>/dev/null; rm -rf "$DIR"' EXIT

i=0
until curl -sf http://127.0.0.1:8080/ping >/dev/null 2>&1; do
  i=$((i + 1))
  [ "$i" -lt 50 ] || { echo "✗ 服务没起来"; cat out.txt; exit 1; }
  sleep 0.2
done
echo "  ✓ /ping 通了"

kill -TERM "$pid"
i=0
while kill -0 "$pid" 2>/dev/null; do
  i=$((i + 1))
  [ "$i" -lt 100 ] || { echo "✗ 收到 SIGTERM 之后没退出"; cat out.txt; exit 1; }
  sleep 0.2
done
echo "  ✓ 收到 SIGTERM 之后干净退出"

grep -q "closing" out.txt || { echo "✗ 没看到组件逆序关闭的日志"; cat out.txt; exit 1; }
echo "  ✓ 组件被逆序关闭"
echo
echo "✓ $VERSION 验证通过"
