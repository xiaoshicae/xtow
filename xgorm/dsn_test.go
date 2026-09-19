package xgorm

import (
	"strings"
	"testing"
	"time"
)

// 这份测试最要紧的一条：凭证不能出现在任何错误或日志里。
// 上一版靠「把 DSN 打出去再脱敏」，那是个永远做不干净的活；
// 这一版根本不打印 DSN，所以这里要证明的是「真的没打」。

const secret = "hunter2"

func mysqlCfg(dsn string) ClientConfig {
	c := DefaultClientConfig()
	c.Driver, c.DSN = DriverMySQL, dsn
	return c
}

func pgCfg(dsn string) ClientConfig {
	c := DefaultClientConfig()
	c.Driver, c.DSN = DriverPostgres, dsn
	return c
}

func TestResolveDSN_MySQL注入超时(t *testing.T) {
	dsn, info, err := resolveDSN(mysqlCfg("u:" + secret + "@tcp(db.example.com:3306)/app"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"timeout=500ms", "readTimeout=3s", "writeTimeout=5s"} {
		if !strings.Contains(dsn, want) {
			t.Errorf("应注入 %s，got=%s", want, dsn)
		}
	}
	if info.Addr != "db.example.com:3306" || info.DB != "app" || info.Driver != "mysql" {
		t.Errorf("连接信息不对，got=%+v", info)
	}
}

func TestResolveDSN_MySQL不覆盖已写的超时(t *testing.T) {
	// 配置里的值只是默认值，DSN 里显式写了的以 DSN 为准
	dsn, _, err := resolveDSN(mysqlCfg("u:p@tcp(h:3306)/app?timeout=9s&readTimeout=8s&writeTimeout=7s"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"timeout=9s", "readTimeout=8s", "writeTimeout=7s"} {
		if !strings.Contains(dsn, want) {
			t.Errorf("不该覆盖 %s，got=%s", want, dsn)
		}
	}
}

func TestResolveDSN_连接信息里没有密码(t *testing.T) {
	// ConnInfo 是唯一进日志的东西，它必须不含凭证
	for _, c := range []ClientConfig{
		mysqlCfg("u:" + secret + "@tcp(h:3306)/app"),
		pgCfg("postgres://u:" + secret + "@h:5432/app"),
		pgCfg("host=h port=5432 dbname=app user=u password=" + secret),
		pgCfg("host=h dbname=app password='" + secret + " with space'"),
	} {
		_, info, err := resolveDSN(c)
		if err != nil {
			t.Fatal(err)
		}
		blob := info.Driver + info.Addr + info.DB
		if strings.Contains(blob, secret) {
			t.Errorf("连接信息里出现了密码：%+v", info)
		}
		if info.DB != "app" {
			t.Errorf("库名应解出来，got=%+v（DSN=%s）", info, c.DSN)
		}
	}
}

func TestResolveDSN_解析失败的错误里没有DSN(t *testing.T) {
	// 驱动的解析错误会把 DSN 片段带在错误信息里，不能原样往上传
	_, _, err := resolveDSN(mysqlCfg("u:" + secret + "@这不是个合法的DSN"))
	if err == nil {
		t.Fatal("非法 DSN 应当报错")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("错误信息里出现了密码：%v", err)
	}

	_, _, err = resolveDSN(pgCfg("postgres://u:" + secret + "@h:5432/app?x=%zz"))
	if err != nil && strings.Contains(err.Error(), secret) {
		t.Errorf("错误信息里出现了密码：%v", err)
	}
}

func TestResolveDSN_PG_URL形式(t *testing.T) {
	c := pgCfg("postgres://u:p@h:5432/app")
	c.Postgres.StatementTimeout = 2 * time.Second
	c.Postgres.LockTimeout = 1500 * time.Millisecond
	c.Postgres.IdleInTxTimeout = time.Minute

	dsn, info, err := resolveDSN(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"connect_timeout=1", // 500ms 向上取整为 1 秒，libpq 只收整数秒
		"statement_timeout=2000",
		"lock_timeout=1500",
		"idle_in_transaction_session_timeout=60000",
	} {
		if !strings.Contains(dsn, want) {
			t.Errorf("应注入 %s，got=%s", want, dsn)
		}
	}
	if info.Addr != "h:5432" || info.DB != "app" {
		t.Errorf("连接信息不对，got=%+v", info)
	}
}

func TestResolveDSN_PG_KV形式(t *testing.T) {
	c := pgCfg("host=h port=5432 dbname=app user=u")
	c.Postgres.StatementTimeout = time.Second

	dsn, info, err := resolveDSN(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dsn, "connect_timeout=1") || !strings.Contains(dsn, "statement_timeout=1000") {
		t.Errorf("应注入参数，got=%s", dsn)
	}
	if info.Addr != "h:5432" || info.DB != "app" {
		t.Errorf("连接信息不对，got=%+v", info)
	}
}

func TestResolveDSN_PG不覆盖已写的key(t *testing.T) {
	for _, dsn := range []string{
		"postgres://u:p@h:5432/app?connect_timeout=9",
		"host=h dbname=app connect_timeout=9",
	} {
		got, _, err := resolveDSN(pgCfg(dsn))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "connect_timeout=9") || strings.Contains(got, "connect_timeout=1") {
			t.Errorf("DSN 里写了的不该被覆盖，got=%s", got)
		}
	}
}

func TestResolveDSN_PG_Params优先(t *testing.T) {
	// Params 是使用者显式写的，比字段默认值更该作数
	c := pgCfg("host=h dbname=app")
	c.Postgres.StatementTimeout = time.Second
	c.Postgres.Params = map[string]string{"statement_timeout": "5000", "application_name": "svc"}

	dsn, _, err := resolveDSN(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dsn, "statement_timeout=5000") || strings.Contains(dsn, "statement_timeout=1000") {
		t.Errorf("Params 应覆盖字段值，got=%s", dsn)
	}
	if !strings.Contains(dsn, "application_name=svc") {
		t.Errorf("Params 里的其它 key 也该注入，got=%s", dsn)
	}
}

func TestQuoteKV(t *testing.T) {
	// libpq 的转义规则：含空格/单引号/反斜杠要加引号
	for _, c := range []struct{ in, want string }{
		{"simple", "simple"},
		{"", "''"},
		{"with space", "'with space'"},
		{"it's", `'it\'s'`},
		{`back\slash`, `'back\\slash'`},
	} {
		if got := quoteKV(c.in); got != c.want {
			t.Errorf("quoteKV(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestSecondsMillis(t *testing.T) {
	// connect_timeout 只收整数秒且最小为 1，向下取整会把 500ms 变成 0（不限制）
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{0, ""}, {-time.Second, ""},
		{time.Millisecond, "1"},
		{500 * time.Millisecond, "1"},
		{1500 * time.Millisecond, "2"},
		{3 * time.Second, "3"},
	} {
		if got := seconds(c.d); got != c.want {
			t.Errorf("seconds(%v)=%q want %q", c.d, got, c.want)
		}
	}
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{0, ""}, {time.Second, "1000"}, {1500 * time.Millisecond, "1500"},
	} {
		if got := millis(c.d); got != c.want {
			t.Errorf("millis(%v)=%q want %q", c.d, got, c.want)
		}
	}
}

func TestResolveDSN_不认识的驱动(t *testing.T) {
	c := DefaultClientConfig()
	c.Driver, c.DSN = "oracle", "x"
	if _, _, err := resolveDSN(c); err == nil {
		t.Fatal("不认识的驱动应当报错")
	}
}
