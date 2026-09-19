// 一个消息队列消费者服务：没有 HTTP 端口，只有一直在消费的 worker。
//
//	cd example/consumer && go run . --config=application.yml
//
// 与 ../（Web 服务）的区别只有一处：交给 xtow.Run 的 Runnable 不是 xgin，
// 是这里的 Consumer。配置、初始化、逆序关闭全都一模一样。
package main

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/xiaoshicae/xtow"
	"github.com/xiaoshicae/xtow/example/consumer/conf"
	"github.com/xiaoshicae/xtow/xcache"

	// 匿名 import 就是全部「装配」。想用数据库就加上 xgorm，
	// 想用 Redis 就加上 xredis——Runnable 那边一个字都不用改
	_ "github.com/xiaoshicae/xtow/xcache"
	_ "github.com/xiaoshicae/xtow/xlog"
	_ "github.com/xiaoshicae/xtow/xmetric"
	_ "github.com/xiaoshicae/xtow/xtrace"
)

func main() {
	q := newMemQueue(64)
	go feed(q) // 真实项目里没有这一行，消息是别人发进来的

	xtow.MustRun(&Consumer{q: q, handle: handle, conf: settings})
}

// settings 把业务配置转成 Consumer 要的那几个数。
//
// 它是个函数而不是一个值：配置由 xtow.Run 加载，main 里构造 Consumer 的
// 那一刻还没读过配置文件，此时取值会拿到一份默认值
func settings() Settings {
	c := conf.C()
	return Settings{Workers: c.Workers, Timeout: c.MessageTimeout}
}

// handle 处理一条消息。业务代码只认识标准库 slog 和原生 client，
// 不认识本框架的包；日志会自动带上 trace_id（上游透传了的话）
func handle(ctx context.Context, m Message) error {
	slog.InfoContext(ctx, "handling message", "message_id", m.ID, "topic", conf.C().Topic)

	// 拿到的是原生的 ristretto 缓存。换成 xgorm.C()（原生 *gorm.DB）
	// 或者 xredis.C()（原生 *redis.Client）是一样的
	xcache.Set("last_message", m.ID)

	time.Sleep(20 * time.Millisecond) // 假装在干活
	return nil
}

// feed 每 100ms 发一条消息进来，纯粹为了让这个例子有东西可消费
func feed(q *memQueue) {
	for i := 1; ; i++ {
		q.publish("msg-"+strconv.Itoa(i), []byte("payload"))
		time.Sleep(100 * time.Millisecond)
	}
}
