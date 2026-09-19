package clickhouse

import (
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xiaoshicae/xtow/xgorm"
)

func cfg(dsn string, dial time.Duration) xgorm.ClientConfig {
	c := xgorm.DefaultClientConfig()
	c.Driver, c.DSN, c.DialTimeout = Driver, dsn, dial
	return c
}

func TestRegister_import进来就注册好了(t *testing.T) {
	// 使用者只写一行匿名 import，不该再调用任何函数
	if !slices.Contains(xgorm.Drivers(), Driver) {
		t.Fatalf("匿名 import 之后 clickhouse 就该在已注册列表里，got=%v", xgorm.Drivers())
	}
}

func TestResolve_注入建连超时(t *testing.T) {
	dsn, info, err := resolve(cfg("clickhouse://u:p@h:9000/analytics", 300*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	q := mustQuery(t, dsn)
	if q.Get(dialTimeoutKey) != "300ms" {
		t.Errorf("建连超时该注进 DSN，got=%q", q.Get(dialTimeoutKey))
	}
	if info.Driver != "clickhouse" || info.Addr != "h:9000" || info.DB != "analytics" {
		t.Errorf("连接信息不对，got=%+v", info)
	}
}

func TestResolve_DSN里写了的不覆盖(t *testing.T) {
	// 配置里的值只是默认值，使用者显式写进 DSN 的一律以他为准
	dsn, _, err := resolve(cfg("clickhouse://h:9000/db?dial_timeout=5s", time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if got := mustQuery(t, dsn).Get(dialTimeoutKey); got != "5s" {
		t.Errorf("DSN 里已有的不该被覆盖，got=%q", got)
	}
}

func TestResolve_超时为零就不注入(t *testing.T) {
	dsn, _, err := resolve(cfg("clickhouse://h:9000/db", 0))
	if err != nil {
		t.Fatal(err)
	}
	if mustQuery(t, dsn).Has(dialTimeoutKey) {
		t.Errorf("没配超时就不该凭空注一个，got=%s", dsn)
	}
}

func TestResolve_认不出的DSN原样透传(t *testing.T) {
	// 解不出来就别猜：猜错一个 DSN 的后果是连到一个使用者以为自己没在连的地方
	const raw = "h:9000"
	dsn, info, err := resolve(cfg(raw, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if dsn != raw {
		t.Errorf("该原样透传，got=%q", dsn)
	}
	if info.Driver != "clickhouse" || info.Addr != "" || info.DB != "" {
		t.Errorf("解不出来就只写驱动名，got=%+v", info)
	}
}

func TestResolve_各种scheme都认(t *testing.T) {
	for _, scheme := range []string{"clickhouse", "tcp", "http", "https"} {
		_, info, err := resolve(cfg(scheme+"://h:9000/db", time.Second))
		if err != nil {
			t.Fatalf("%s: %v", scheme, err)
		}
		if info.Addr != "h:9000" {
			t.Errorf("%s: 该解出地址，got=%+v", scheme, info)
		}
	}
}

func TestResolve_解析失败时错误里不带DSN(t *testing.T) {
	// url.Parse 的错误里带着整串 DSN，而错误会被记下来
	const secret = "hunter2"
	_, _, err := resolve(cfg("clickhouse://u:"+secret+"@h:9000/db\x7f\x00", time.Second))
	if err == nil {
		t.Fatal("非法 DSN 应当报错")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("错误信息里出现了凭证：%v", err)
	}
}

func mustQuery(t *testing.T, dsn string) url.Values {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}
