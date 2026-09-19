package xgorm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaoshicae/xtow/internal/config"
	"github.com/xiaoshicae/xtow/registry"
)

// load 走真实的配置加载路径，把 YAML 解进本包的配置变量
func load(t *testing.T, yml string) Config {
	t.Helper()
	c := DefaultConfig()
	path := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(path, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := config.Load(path, []registry.Component{{Key: ConfigKey, Config: &c}}); err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	return c
}

func loadErr(t *testing.T, yml string) error {
	t.Helper()
	c := DefaultConfig()
	path := filepath.Join(t.TempDir(), "application.yml")
	os.WriteFile(path, []byte(yml), 0o644)
	return config.Load(path, []registry.Component{{Key: ConfigKey, Config: &c}})
}

func TestConfig_单实例写法(t *testing.T) {
	c := load(t, "XGorm:\n  Driver: mysql\n  DSN: u:p@tcp(h:3306)/app\n")
	if len(c.Clients) != 1 {
		t.Fatalf("应解出一个实例，got=%v", c.Clients)
	}
	got, ok := c.Clients[DefaultName]
	if !ok {
		t.Fatalf("单实例写法应规整成名为 %s 的实例，got=%v", DefaultName, c.Clients)
	}
	if got.Driver != DriverMySQL || got.DSN != "u:p@tcp(h:3306)/app" {
		t.Errorf("配置不对，got=%+v", got)
	}
	// 没写的字段应保持默认
	if got.MaxOpenConns != 50 || got.MaxLifetime != 5*time.Minute || !got.Trace {
		t.Errorf("没写的字段应保持默认，got=%+v", got)
	}
}

func TestConfig_多实例写法(t *testing.T) {
	c := load(t, `
XGorm:
  Clients:
    default:
      DSN: host=h1 dbname=main
    report:
      DSN: host=h2 dbname=report
      MaxOpenConns: 5
      Trace: false
`)
	if len(c.Clients) != 2 {
		t.Fatalf("应解出两个实例，got=%v", c.Clients)
	}
	if c.Clients["default"].MaxOpenConns != 50 {
		t.Errorf("没写的字段应保持默认，got=%+v", c.Clients["default"])
	}
	r := c.Clients["report"]
	if r.MaxOpenConns != 5 || r.Trace {
		t.Errorf("写了的字段应生效，got=%+v", r)
	}
	// 关键：同一个 map 里，一个实例覆盖了的字段不该影响另一个
	if c.Clients["default"].Trace != true {
		t.Error("一个实例关掉 Trace 不该影响另一个")
	}
}

func TestConfig_集合元素里的拼写错误也要失败(t *testing.T) {
	// 集合元素走的是自定义解码器，严格检查很容易在那里悄悄失效
	err := loadErr(t, "XGorm:\n  Clients:\n    default:\n      DSN: x\n      MaxOpenConn: 5\n")
	if err == nil {
		t.Fatal("实例里的字段拼错应当启动失败")
	}
	if !strings.Contains(err.Error(), "MaxOpenConn") {
		t.Errorf("错误里要点名拼错的字段，got=%v", err)
	}
}

func TestConfig_顶层拼写错误也要失败(t *testing.T) {
	err := loadErr(t, "XGorm:\n  DSN: x\n  MaxOpenConn: 5\n")
	if err == nil {
		t.Fatal("字段拼错应当启动失败")
	}
}

func TestConfig_两种写法不能混用(t *testing.T) {
	// 混着写时「default 到底是哪个」没有不让人意外的答案，所以直接失败
	err := loadErr(t, "XGorm:\n  DSN: x\n  Clients:\n    a:\n      DSN: y\n")
	if err == nil {
		t.Fatal("混用两种写法应当失败")
	}
	if !strings.Contains(err.Error(), "cannot mix") {
		t.Errorf("错误信息该说清楚为什么，got=%v", err)
	}
}

func TestConfig_空的Clients要失败(t *testing.T) {
	if err := loadErr(t, "XGorm:\n  Clients: {}\n"); err == nil {
		t.Fatal("写了 Clients 却是空的，应当失败")
	}
}

func TestConfig_没配就没有实例(t *testing.T) {
	c := load(t, "# 整个文件里没有 XGorm 这一块\n")
	if len(c.Clients) != 0 {
		t.Errorf("没配 XGorm 就不该连任何数据库，got=%v", c.Clients)
	}
}

func TestConfig_时长与环境变量(t *testing.T) {
	t.Setenv("TEST_DB_DSN", "host=h dbname=app")
	c := load(t, "XGorm:\n  DSN: \"${TEST_DB_DSN}\"\n  DialTimeout: 2s\n  SlowThreshold: 100ms\n")
	got := c.Clients[DefaultName]
	if got.DSN != "host=h dbname=app" {
		t.Errorf("环境变量应展开，got=%+v", got)
	}
	if got.DialTimeout != 2*time.Second || got.SlowThreshold != 100*time.Millisecond {
		t.Errorf("时长应解析成 Duration，got=%+v", got)
	}
}

func TestConfig_MaxIdleConns配零就是零(t *testing.T) {
	// 预填默认值的全部意义就在这里：显式写的零值不会被「没配」的逻辑吃掉
	c := load(t, "XGorm:\n  DSN: x\n  MaxIdleConns: 0\n")
	if got := c.Clients[DefaultName].MaxIdleConns; got != 0 {
		t.Errorf("显式配 0 就该是 0，got=%d", got)
	}
}

func TestConfig_布尔开关配false就是false(t *testing.T) {
	c := load(t, "XGorm:\n  DSN: x\n  Trace: false\n  Metric: false\n")
	got := c.Clients[DefaultName]
	if got.Trace || got.Metric {
		t.Errorf("显式关掉就该是关的，got=%+v", got)
	}
}

func TestValidate(t *testing.T) {
	ok := DefaultClientConfig()
	ok.DSN = "host=h"
	if err := ok.validate(); err != nil {
		t.Errorf("合法配置不该报错：%v", err)
	}

	for name, mutate := range map[string]func(*ClientConfig){
		"DSN 为空":           func(c *ClientConfig) { c.DSN = "" },
		"驱动不认识":            func(c *ClientConfig) { c.Driver = "oracle" },
		"MaxOpenConns 为 0": func(c *ClientConfig) { c.MaxOpenConns = 0 },
	} {
		c := ok
		mutate(&c)
		if err := c.validate(); err == nil {
			t.Errorf("%s 应当报错", name)
		}
	}
}
