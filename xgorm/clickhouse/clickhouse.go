// Package clickhouse 给 xgorm 加上 ClickHouse 驱动。
//
// 匿名 import 即可，不需要写任何代码：
//
//	import (
//		"github.com/xiaoshicae/xtow/xgorm"
//		_ "github.com/xiaoshicae/xtow/xgorm/clickhouse"
//	)
//
// 然后配置里写 Driver: clickhouse，拿到的还是原生的 *gorm.DB：
//
//	XGorm:
//	  Driver: clickhouse
//	  DSN: "${CH_DSN}"     # clickhouse://user:pass@host:9000/db
//
// 为什么是独立的 module：实测一个只 import xgorm 的应用模块图是 65 个，
// 加上这个包变成 733 个，而编译包只从 140 涨到 183——模块图涨得比实际
// 编进去的代码多得多，而 Go 的 MVS 正是按模块图把版本要求强加给使用者的。
// 不用 ClickHouse 的人不该为它付这个钱。
package clickhouse

import (
	"fmt"
	"net/url"
	"strings"

	"gorm.io/driver/clickhouse"
	"gorm.io/gorm"

	"github.com/xiaoshicae/xtow/xgorm"
)

// Driver 配置里 Driver 那一项要写的值
const Driver xgorm.Driver = "clickhouse"

// dialTimeoutKey ClickHouse DSN 里建连超时对应的 query 参数
const dialTimeoutKey = "dial_timeout"

// init 只注册，不初始化。真正建连由 xgorm 在框架的 StageClient 里做。
func init() {
	xgorm.RegisterDialect(xgorm.Dialect{
		Name:    Driver,
		Open:    func(dsn string) gorm.Dialector { return clickhouse.Open(dsn) },
		Resolve: resolve,
	})
}

// resolve 把配置里的建连超时注入 DSN，并解出可安全记录的连接信息
//
// 只认 URL 形式（clickhouse://user:pass@host:9000/db?k=v）。其余写法
// （驱动也接受的 host:port 裸地址等）原样透传：解不出来就别猜，
// 猜错一个 DSN 的后果是连到一个使用者以为自己没在连的地方。
func resolve(c xgorm.ClientConfig) (string, xgorm.ConnInfo, error) {
	if !isURL(c.DSN) {
		return c.DSN, xgorm.ConnInfo{Driver: string(Driver)}, nil
	}

	u, err := url.Parse(c.DSN)
	if err != nil {
		// 不回传原始错误：url.Parse 的错误里带着整串 DSN，而错误会被记下来
		return "", xgorm.ConnInfo{}, fmt.Errorf(
			"failed to parse DSN, check the format of %s (details omitted to keep credentials out of logs)",
			xgorm.ConfigKey)
	}

	// 使用者在 DSN 里显式写了的，一律不覆盖——配置里的值只是默认值
	if v := dialTimeout(c); v != "" {
		q := u.Query()
		if !q.Has(dialTimeoutKey) {
			q.Set(dialTimeoutKey, v)
			u.RawQuery = q.Encode()
		}
	}

	return u.String(), xgorm.ConnInfo{
		Driver: string(Driver),
		Addr:   u.Host,
		DB:     strings.TrimPrefix(u.Path, "/"),
	}, nil
}

// isURL 判断是不是 URL 形式的 DSN
//
// 驱动认这几种 scheme，别的形态（裸 host:port）走透传
func isURL(dsn string) bool {
	for _, p := range []string{"clickhouse://", "tcp://", "http://", "https://"} {
		if strings.HasPrefix(dsn, p) {
			return true
		}
	}
	return false
}

// dialTimeout 把建连超时写成 ClickHouse 认的时长字符串，<=0 表示不注入
func dialTimeout(c xgorm.ClientConfig) string {
	if c.DialTimeout <= 0 {
		return ""
	}
	return c.DialTimeout.String()
}
