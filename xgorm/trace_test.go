package xgorm

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// openLazy 造一个不连库的 *gorm.DB
//
// sql.Open 本身是懒的，但 mysql 方言在 Initialize 里会查一次 SELECT VERSION()
// 来判断服务端支持哪些特性，所以还要 SkipInitializeWithVersion 才真的不建连。
func openLazy(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(
		mysql.New(mysql.Config{DSN: "u:p@tcp(127.0.0.1:1)/app", SkipInitializeWithVersion: true}),
		&gorm.Config{DisableAutomaticPing: true},
	)
	if err != nil {
		t.Fatalf("建实例失败：%v", err)
	}
	t.Cleanup(func() { closePool(db) })
	return db
}

// recording 装一套独立的链路设施，返回取已结束 Span 的函数
func recording(t *testing.T) func() []sdktrace.ReadOnlySpan {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exp)),
	)
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(old); tp.Shutdown(context.Background()) })

	return func() []sdktrace.ReadOnlySpan {
		spans := make([]sdktrace.ReadOnlySpan, 0, len(exp.GetSpans()))
		for _, s := range exp.GetSpans().Snapshots() {
			spans = append(spans, s)
		}
		return spans
	}
}

func stmtDB(ctx context.Context) *gorm.DB {
	return &gorm.DB{Statement: &gorm.Statement{Context: ctx}}
}

func TestInstallTracing_六种操作都挂上(t *testing.T) {
	// 官方插件会把 ClickHouse 驱动编进来，所以回调是自己注册的；
	// 那就得自己保证一种都没漏——漏了的那种操作从此在链路里是隐形的
	db := openLazy(t)
	if err := installTracing(db, connInfo{Driver: "mysql"}); err != nil {
		t.Fatalf("注册失败：%v", err)
	}

	cb := db.Callback()
	for name, p := range map[string]interface{ Get(string) func(*gorm.DB) }{
		"create": cb.Create(), "query": cb.Query(), "update": cb.Update(),
		"delete": cb.Delete(), "row": cb.Row(), "raw": cb.Raw(),
	} {
		if p.Get("xgorm:trace:before") == nil {
			t.Errorf("%s 没挂上 before 回调", name)
		}
		if p.Get("xgorm:trace:after") == nil {
			t.Errorf("%s 没挂上 after 回调", name)
		}
	}
}

func TestSpan_带上连接信息与SQL(t *testing.T) {
	spans := recording(t)
	info := connInfo{Driver: "mysql", Addr: "h:3306", DB: "app"}

	db := stmtDB(context.Background())
	startSpan("query", info)(db)
	db.Statement.SQL.WriteString("SELECT * FROM users WHERE id = ?")
	db.RowsAffected = 3
	endSpan(db)

	got := spans()
	if len(got) != 1 {
		t.Fatalf("应产出一个 Span，got=%d", len(got))
	}
	s := got[0]
	if s.Name() != "gorm.query" {
		t.Errorf("Span 名不对，got=%s", s.Name())
	}
	attrs := map[string]string{}
	for _, kv := range s.Attributes() {
		attrs[string(kv.Key)] = kv.Value.Emit()
	}
	if attrs["db.system"] != "mysql" || attrs["server.address"] != "h:3306" || attrs["db.name"] != "app" {
		t.Errorf("连接信息不对，got=%v", attrs)
	}
	if attrs["db.statement"] != "SELECT * FROM users WHERE id = ?" {
		t.Errorf("SQL 不对，got=%v", attrs)
	}
	if attrs["db.rows_affected"] != "3" {
		t.Errorf("行数不对，got=%v", attrs)
	}
}

func TestSpan_不记参数值(t *testing.T) {
	// 参数里可能有手机号、身份证、令牌，记进链路就跟着采样一路送出去了
	spans := recording(t)
	db := stmtDB(context.Background())
	startSpan("query", connInfo{})(db)
	db.Statement.SQL.WriteString("SELECT * FROM users WHERE token = ?")
	db.Statement.Vars = []any{"hunter2"}
	endSpan(db)

	for _, kv := range spans()[0].Attributes() {
		if strings.Contains(kv.Value.Emit(), "hunter2") {
			t.Errorf("参数值不该进链路，属性 %s=%s", kv.Key, kv.Value.Emit())
		}
	}
}

func TestSpan_出错时标红(t *testing.T) {
	spans := recording(t)
	db := stmtDB(context.Background())
	startSpan("query", connInfo{})(db)
	db.Error = errors.New("连接断了")
	endSpan(db)

	s := spans()[0]
	if s.Status().Code != codes.Error {
		t.Errorf("出错应标成 Error，got=%v", s.Status())
	}
	if len(s.Events()) == 0 {
		t.Error("应把错误记成 Span 事件")
	}
}

func TestSpan_没查到记录不算错(t *testing.T) {
	// 「没查到」是正常的业务分支，标成错误会让链路里满屏红色
	spans := recording(t)
	db := stmtDB(context.Background())
	startSpan("query", connInfo{})(db)
	db.Error = gorm.ErrRecordNotFound
	endSpan(db)

	if s := spans()[0]; s.Status().Code == codes.Error {
		t.Errorf("没查到记录不该标成错误，got=%v", s.Status())
	}
}

func TestSpan_Statement为空时不炸(t *testing.T) {
	startSpan("query", connInfo{})(&gorm.DB{})
	endSpan(&gorm.DB{})
}

func TestLogConn_不打印凭证(t *testing.T) {
	// 建连日志是这个模块唯一会写出连接信息的地方
	lines := capture(t)
	c := DefaultClientConfig()
	c.Driver, c.DSN = DriverMySQL, "u:"+secret+"@tcp(h:3306)/app"
	_, info, err := resolveDSN(c)
	if err != nil {
		t.Fatal(err)
	}
	logConn(info, c)

	got := lines()
	if len(got) != 1 {
		t.Fatalf("应记一条，got=%v", got)
	}
	blob := strings.Join([]string{got[0]["msg"].(string), info.Addr, info.DB}, " ")
	for k, v := range got[0] {
		blob += k
		if s, ok := v.(string); ok {
			blob += s
		}
	}
	if strings.Contains(blob, secret) {
		t.Errorf("建连日志里出现了密码：%v", got[0])
	}
	if got[0]["地址"] != "h:3306" || got[0]["库"] != "app" {
		t.Errorf("该写出地址和库名，否则排查不了连的是谁，got=%v", got[0])
	}
}
