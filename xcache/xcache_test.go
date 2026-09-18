package xcache

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaoshicae/xtow/internal/config"
	"github.com/xiaoshicae/xtow/registry"
)

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

// withInstances 装一套实例，测试结束后还原
func withInstances(t *testing.T, cfgs map[string]ClientConfig) {
	t.Helper()
	built := map[string]instance{}
	for name, c := range cfgs {
		cache, closer, err := New(c)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { closer.Close() })
		built[name] = instance{cache: cache, ttl: c.DefaultTTL}
	}
	old := map[string]instance{}
	for _, n := range reg.Names() {
		if v, ok := reg.Lookup(n); ok {
			old[n] = v
		}
	}
	reg.Publish(built)
	t.Cleanup(func() { reg.Publish(old) })
}

func TestConfig_单实例写法(t *testing.T) {
	c := load(t, "XCache:\n  MaxCost: 500\n")
	got, ok := c.Clients[DefaultName]
	if !ok {
		t.Fatalf("单实例写法应规整成名为 %s 的实例，got=%v", DefaultName, c.Clients)
	}
	if got.MaxCost != 500 {
		t.Errorf("写了的字段应生效，got=%+v", got)
	}
	if got.NumCounters != 1_000_000 || got.DefaultTTL != 5*time.Minute {
		t.Errorf("没写的字段应保持默认，got=%+v", got)
	}
}

func TestConfig_多实例写法(t *testing.T) {
	c := load(t, "XCache:\n  Clients:\n    default: {MaxCost: 100}\n    session: {MaxCost: 200, DefaultTTL: 30m}\n")
	if len(c.Clients) != 2 {
		t.Fatalf("应解出两个实例，got=%v", c.Clients)
	}
	if c.Clients["session"].DefaultTTL != 30*time.Minute {
		t.Errorf("写了的字段应生效，got=%+v", c.Clients["session"])
	}
	if c.Clients["default"].DefaultTTL != 5*time.Minute {
		t.Errorf("一个实例改了 TTL 不该影响另一个，got=%+v", c.Clients["default"])
	}
}

func TestConfig_拼写错误要失败(t *testing.T) {
	if err := loadErr(t, "XCache:\n  MaxCoat: 1\n"); err == nil {
		t.Fatal("字段拼错应当启动失败")
	}
	if err := loadErr(t, "XCache:\n  Clients:\n    a: {MaxCoat: 1}\n"); err == nil {
		t.Fatal("实例里的字段拼错也应当启动失败")
	}
}

func TestConfig_两种写法不能混用(t *testing.T) {
	if err := loadErr(t, "XCache:\n  MaxCost: 1\n  Clients:\n    a: {MaxCost: 2}\n"); err == nil {
		t.Fatal("混用两种写法应当失败")
	}
}

func TestConfig_没配就没有实例(t *testing.T) {
	if c := load(t, "# 没有 XCache 这一块\n"); len(c.Clients) != 0 {
		t.Errorf("没配就不该建缓存，got=%v", c.Clients)
	}
}

func TestValidate(t *testing.T) {
	ok := DefaultClientConfig()
	if err := ok.validate(); err != nil {
		t.Errorf("默认配置应当合法：%v", err)
	}
	for name, mutate := range map[string]func(*ClientConfig){
		"NumCounters 为 0": func(c *ClientConfig) { c.NumCounters = 0 },
		"MaxCost 为 0":     func(c *ClientConfig) { c.MaxCost = 0 },
		"BufferItems 为 0": func(c *ClientConfig) { c.BufferItems = 0 },
	} {
		c := ok
		mutate(&c)
		if err := c.validate(); err == nil {
			t.Errorf("%s 应当报错", name)
		}
	}
}

func TestNew_拿到的是原生缓存(t *testing.T) {
	cache, closer, err := New(DefaultClientConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	cache.SetWithTTL("k", 42, 1, time.Minute)
	cache.Wait() // 写入走环形缓冲异步生效，要确定性地读到就得先等
	if v, ok := cache.Get("k"); !ok || v != 42 {
		t.Errorf("应读到刚写的值，got=%v ok=%v", v, ok)
	}
}

func TestSet_用配置里的默认TTL(t *testing.T) {
	// 包级 Set 的全部价值就在这里：不用每次都把 cost 和 TTL 写一遍
	withInstances(t, map[string]ClientConfig{DefaultName: func() ClientConfig {
		c := DefaultClientConfig()
		c.DefaultTTL = 90 * time.Second
		return c
	}()})

	Set("k", "v")
	C().Wait()

	ttl, ok := C().GetTTL("k")
	if !ok {
		t.Fatal("应能读到刚写的键")
	}
	if ttl > 90*time.Second || ttl < 80*time.Second {
		t.Errorf("应当用配置里的 TTL，got=%v", ttl)
	}
	if got := DefaultTTL(); got != 90*time.Second {
		t.Errorf("DefaultTTL 应返回配置值，got=%v", got)
	}
}

func TestGetSetDel(t *testing.T) {
	withInstances(t, map[string]ClientConfig{DefaultName: DefaultClientConfig()})

	Set("k", "v")
	C().Wait()
	if v, ok := Get("k"); !ok || v != "v" {
		t.Errorf("应读到刚写的值，got=%v ok=%v", v, ok)
	}

	Del("k")
	C().Wait()
	if _, ok := Get("k"); ok {
		t.Error("删掉之后不该还读得到")
	}
}

func TestSetWithTTL(t *testing.T) {
	withInstances(t, map[string]ClientConfig{DefaultName: DefaultClientConfig()})

	SetWithTTL("k", "v", 10*time.Second)
	C().Wait()
	ttl, ok := C().GetTTL("k")
	if !ok || ttl > 10*time.Second {
		t.Errorf("应当用传进去的 TTL，got=%v ok=%v", ttl, ok)
	}
}

func TestC_取不到就panic(t *testing.T) {
	withInstances(t, nil)
	defer func() {
		msg := fmt.Sprint(recover())
		if msg == "<nil>" {
			t.Fatal("取不到实例应当 panic")
		}
		if !strings.Contains(msg, ConfigKey) {
			t.Errorf("一个都没配时该提示去看配置块，got=%v", msg)
		}
	}()
	C()
}

func TestSet_没配默认实例时panic(t *testing.T) {
	// 包级 Set 走的是另一条取实例的路径，别漏了这层保护
	withInstances(t, map[string]ClientConfig{"session": DefaultClientConfig()})
	defer func() {
		msg := fmt.Sprint(recover())
		if !strings.Contains(msg, "session") {
			t.Errorf("应列出已配置的实例名，got=%v", msg)
		}
	}()
	Set("k", "v")
}

func TestHasNames(t *testing.T) {
	withInstances(t, map[string]ClientConfig{"session": DefaultClientConfig(), "page": DefaultClientConfig()})
	if !Has("page") || Has("nope") || Has() {
		t.Error("Has 应如实反映是否配过")
	}
	if got := Names(); len(got) != 2 || got[0] != "page" {
		t.Errorf("Names 应按名字排序，got=%v", got)
	}
}

func TestRegister_登记内容与框架对得上(t *testing.T) {
	var got *registry.Component
	for _, c := range registry.Snapshot() {
		if c.Key == ConfigKey {
			got = &c
			break
		}
	}
	if got == nil {
		t.Fatalf("没有以 %s 登记", ConfigKey)
	}
	if got.Stage != registry.StageClient {
		t.Errorf("缓存要在服务对外之前就绪，got=%v", got.Stage)
	}
	if got.Config != &cfg {
		t.Error("登记的必须是包级配置变量本身")
	}
}

func TestInitAll_建起来又关干净(t *testing.T) {
	old := cfg
	t.Cleanup(func() { cfg = old })
	cfg = Config{Clients: map[string]ClientConfig{"a": DefaultClientConfig(), "b": DefaultClientConfig()}}

	closer, err := initAll()
	if err != nil {
		t.Fatalf("应当建得起来：%v", err)
	}
	if got := Names(); len(got) != 2 {
		t.Fatalf("应有两个实例，got=%v", got)
	}
	if err := closer.Close(); err != nil {
		t.Errorf("关闭不该报错：%v", err)
	}
	if got := Names(); len(got) != 0 {
		t.Errorf("关闭后应清空，否则 C() 会返回已关闭的实例，got=%v", got)
	}
}

func TestInitAll_一个失败就全部回滚(t *testing.T) {
	old := cfg
	t.Cleanup(func() { cfg = old })

	bad := DefaultClientConfig()
	bad.MaxCost = -1
	cfg = Config{Clients: map[string]ClientConfig{"a": DefaultClientConfig(), "z": bad}}

	if _, err := initAll(); err == nil {
		t.Fatal("配置非法时应当报错")
	} else if !strings.Contains(err.Error(), `"z"`) {
		t.Errorf("错误里应点名是哪个实例，got=%v", err)
	}
	if got := Names(); len(got) != 0 {
		t.Errorf("失败时不该发布任何实例，got=%v", got)
	}
}

func TestInitAll_没配就什么都不做(t *testing.T) {
	old := cfg
	t.Cleanup(func() { cfg = old })
	cfg = DefaultConfig()

	closer, err := initAll()
	if err != nil {
		t.Fatalf("没配不该报错：%v", err)
	}
	closer.Close()
	if len(Names()) != 0 {
		t.Errorf("不该建出实例，got=%v", Names())
	}
}
