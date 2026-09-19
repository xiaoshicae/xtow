package xgorm

import (
	"errors"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"gorm.io/gorm"
)

// 这里自己注册 GORM 回调，而不是用 gorm.io/plugin/opentelemetry/tracing。
//
// 那个官方插件 import 了 gorm.io/driver/clickhouse，于是每个用它的二进制
// 都要编进整套 ClickHouse 客户端：实测模块图 159 个、编译包 372 个。
// 为了在 MySQL 上打一条 Span，这个价钱太贵了。
//
// GORM 的回调接口本身很简单，自己接上只要下面这些代码，依赖只多 otel。

const tracerName = "github.com/xiaoshicae/xtow/xgorm"

// registrar 是 *callback.Register 的方法值。
//
// GORM 的 processor / callback 都是非导出类型，外部没法给它们声明接口或变量，
// 但方法值可以拿出来存——于是这张表还是写得成表。
type registrar func(name string, fn func(*gorm.DB)) error

// installTracing 给实例挂上链路回调，每种操作前后各一个
func installTracing(db *gorm.DB, info ConnInfo) error {
	cb := db.Callback()
	pairs := []struct {
		op            string
		before, after registrar
	}{
		{"create", cb.Create().Before("gorm:create").Register, cb.Create().After("gorm:create").Register},
		{"query", cb.Query().Before("gorm:query").Register, cb.Query().After("gorm:query").Register},
		{"update", cb.Update().Before("gorm:update").Register, cb.Update().After("gorm:update").Register},
		{"delete", cb.Delete().Before("gorm:delete").Register, cb.Delete().After("gorm:delete").Register},
		{"row", cb.Row().Before("gorm:row").Register, cb.Row().After("gorm:row").Register},
		{"raw", cb.Raw().Before("gorm:raw").Register, cb.Raw().After("gorm:raw").Register},
	}

	var errs []error
	for _, p := range pairs {
		errs = append(errs,
			p.before("xgorm:trace:before", startSpan(p.op, info)),
			p.after("xgorm:trace:after", endSpan),
		)
	}
	return errors.Join(errs...)
}

// startSpan 开一个 Span 并把它塞回 Statement 的 context
func startSpan(op string, info ConnInfo) func(*gorm.DB) {
	return func(db *gorm.DB) {
		if db.Statement == nil {
			return
		}
		ctx, _ := otel.Tracer(tracerName).Start(db.Statement.Context, "gorm."+op,
			trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(
				attribute.String("db.system", info.Driver),
				attribute.String("db.name", info.DB),
				attribute.String("server.address", info.Addr),
				attribute.String("db.operation", op),
			),
		)
		db.Statement.Context = ctx
	}
}

// endSpan 补上 SQL 与结果，然后结束 Span
func endSpan(db *gorm.DB) {
	if db.Statement == nil {
		return
	}
	span := trace.SpanFromContext(db.Statement.Context)
	if !span.IsRecording() {
		// 没采样时连 SQL 字符串都不用取
		span.End()
		return
	}
	defer span.End()

	// 记的是带占位符的 SQL，不记 Statement.Vars——参数里可能有个人信息或凭证。
	// 行数取 db.RowsAffected 而不是 db.Statement.RowsAffected：后者是从
	// Statement 内嵌的 *DB 提升上来的，指的是另一个实例，这里没有理由绕过去。
	span.SetAttributes(
		attribute.String("db.statement", db.Statement.SQL.String()),
		attribute.Int64("db.rows_affected", db.RowsAffected),
	)

	// 「没查到记录」是正常的业务分支，标成错误会让链路里满屏红色
	if db.Error != nil && !errors.Is(db.Error, gorm.ErrRecordNotFound) {
		span.RecordError(db.Error)
		span.SetStatus(codes.Error, db.Error.Error())
	}
}
