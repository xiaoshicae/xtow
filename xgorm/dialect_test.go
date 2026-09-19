package xgorm

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

// withDialect 临时注册一个方言，测试结束后摘掉
func withDialect(t *testing.T, d Dialect) {
	t.Helper()
	RegisterDialect(d)
	t.Cleanup(func() {
		dialectMu.Lock()
		delete(dialects, d.Name)
		dialectMu.Unlock()
	})
}

func TestDrivers_内置两个(t *testing.T) {
	got := Drivers()
	if !slices.Contains(got, DriverMySQL) || !slices.Contains(got, DriverPostgres) {
		t.Errorf("mysql 和 postgres 应当内置，got=%v", got)
	}
	if !slices.IsSorted(got) {
		t.Errorf("应按字母序返回，否则错误信息每次都不一样，got=%v", got)
	}
}

func TestRegisterDialect_注册之后驱动就能用了(t *testing.T) {
	withDialect(t, Dialect{
		Name: "demo",
		Open: func(string) gorm.Dialector { return nil },
		Resolve: func(c ClientConfig) (string, ConnInfo, error) {
			return c.DSN + "?tuned=1", ConnInfo{Driver: "demo", Addr: "h:1", DB: "d"}, nil
		},
	})

	c := DefaultClientConfig()
	c.Driver, c.DSN = "demo", "demo://h:1/d"
	if err := c.validate(); err != nil {
		t.Fatalf("注册过的驱动应当通过校验：%v", err)
	}

	dsn, info, err := resolveDSN(c)
	if err != nil {
		t.Fatal(err)
	}
	if dsn != "demo://h:1/d?tuned=1" {
		t.Errorf("该走方言自己的 Resolve，got=%q", dsn)
	}
	if info.Driver != "demo" {
		t.Errorf("连接信息该来自方言，got=%+v", info)
	}
}

func TestRegisterDialect_没有Resolve时DSN原样用(t *testing.T) {
	// 对一个只想先跑起来的驱动，超时写进 DSN 里一样有效
	withDialect(t, Dialect{Name: "bare", Open: func(string) gorm.Dialector { return nil }})

	c := DefaultClientConfig()
	c.Driver, c.DSN = "bare", "bare://h:1/d"
	dsn, info, err := resolveDSN(c)
	if err != nil {
		t.Fatal(err)
	}
	if dsn != c.DSN {
		t.Errorf("该原样用，got=%q", dsn)
	}
	if info.Driver != "bare" || info.Addr != "" {
		t.Errorf("解不出来就只写驱动名，got=%+v", info)
	}
}

func TestRegisterDialect_重名直接panic(t *testing.T) {
	// 两个 Dialect 抢同一个名字时，选哪个都可能让服务连到一个
	// 它以为自己没在连的地方。这是 import 期的问题，不该留到运行时
	defer func() {
		if recover() == nil {
			t.Error("重名注册应当 panic")
		}
	}()
	RegisterDialect(Dialect{Name: DriverMySQL, Open: func(string) gorm.Dialector { return nil }})
}

func TestRegisterDialect_缺字段直接panic(t *testing.T) {
	for _, c := range []struct {
		name string
		d    Dialect
	}{
		{"没名字", Dialect{Open: func(string) gorm.Dialector { return nil }}},
		{"没 Open", Dialect{Name: "noopen"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("应当 panic")
				}
			}()
			RegisterDialect(c.d)
		})
	}
}

func TestValidate_没注册的驱动报错时列出已注册的(t *testing.T) {
	// 只说「不认识」帮助有限：名字拼错和忘了 import 对应的 module
	// 是两个不同的问题，把实际注册了哪些列出来，两者一眼可分
	c := DefaultClientConfig()
	c.Driver, c.DSN = "clickhouse", "clickhouse://h:9000/db"
	err := c.validate()
	if err == nil {
		t.Fatal("没注册的驱动应当报错")
	}
	for _, want := range []string{"clickhouse", "mysql", "postgres", "import"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误里该出现 %q，got=%v", want, err)
		}
	}
}

// stubDialector 一个只会在 Initialize 里失败的 Dialector，
// 用来确认 New 真的用了注册进来的那个 Open
type stubDialector struct{ err error }

func (d stubDialector) Name() string                    { return "stub" }
func (d stubDialector) Initialize(*gorm.DB) error       { return d.err }
func (d stubDialector) Migrator(*gorm.DB) gorm.Migrator { return nil }
func (d stubDialector) DataTypeOf(*schema.Field) string { return "" }
func (d stubDialector) DefaultValueOf(*schema.Field) clause.Expression {
	return clause.Expr{}
}
func (d stubDialector) BindVarTo(clause.Writer, *gorm.Statement, any) {}
func (d stubDialector) QuoteTo(clause.Writer, string)                 {}
func (d stubDialector) Explain(sql string, _ ...any) string           { return sql }

func TestNew_用的是注册进来的那个Open(t *testing.T) {
	// 回归用例。曾经 New 里还留着一个写死 mysql / postgres 的旧函数，
	// 于是注册表看着是对的（Drivers() 里有它、校验也过了），
	// 建连却悄悄走了 postgres —— 连到了一个使用者以为自己没在连的地方
	sentinel := errors.New("stub dialector was used")
	withDialect(t, Dialect{
		Name: "stub",
		Open: func(string) gorm.Dialector { return stubDialector{err: sentinel} },
	})

	c := DefaultClientConfig()
	c.Driver, c.DSN = "stub", "stub://h:1/d"
	_, _, err := New(context.Background(), c)
	if !errors.Is(err, sentinel) {
		t.Fatalf("New 该用注册进来的 Open，got=%v", err)
	}
}
