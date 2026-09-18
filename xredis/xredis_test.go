package xredis

import (
	"context"
	"fmt"
	"net"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/xiaoshicae/xtow/registry"
	"github.com/xiaoshicae/xtow/xmetric"
)

func liveCfg(f *fakeRedis) ClientConfig {
	c := DefaultClientConfig()
	c.Addr = f.addr()
	c.DialTimeout, c.ReadTimeout, c.WriteTimeout = 300*time.Millisecond, 300*time.Millisecond, 300*time.Millisecond
	return c
}

// deadAddr 一个没人监听的地址
func deadAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func TestNew_连得上就返回可用实例(t *testing.T) {
	f := newFakeRedis(t)
	client, closer, err := New(liveCfg(f))
	if err != nil {
		t.Fatalf("应当连得上：%v", err)
	}
	defer closer.Close()

	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Errorf("拿到的应是可用的原生 client：%v", err)
	}
	if !slices.Contains(f.seen(), "ping") {
		t.Errorf("建连时应当 Ping 一次确认连得上，实际收到的命令=%v", f.seen())
	}
}

func TestNew_启动时就验证连通性(t *testing.T) {
	// 地址写错、密码不对这类问题该在启动时暴露，而不是线上第一次读缓存才发现
	f := newFakeRedis(t)
	f.setFailPing(true)

	c := liveCfg(f)
	_, _, err := New(c)
	if err == nil {
		t.Fatal("Ping 失败时应当报错")
	}
	if !strings.Contains(err.Error(), c.Addr) {
		t.Errorf("错误里应有地址，否则排查不了连的是谁：%v", err)
	}
}

func TestNew_连不上时不漏连接池(t *testing.T) {
	c := DefaultClientConfig()
	c.Addr = deadAddr(t)
	c.DialTimeout, c.ReadTimeout = 50*time.Millisecond, 50*time.Millisecond
	c.Trace = false

	before := stabilize()
	for i := 0; i < 5; i++ {
		client, closer, err := New(c)
		if err == nil {
			closer.Close()
			t.Fatal("连不上时应当报错")
		}
		if client != nil || closer != nil {
			t.Error("失败时不该返回半成品")
		}
	}
	if after := settleTo(before); after > before+1 {
		t.Errorf("建连失败 5 次后协程数从 %d 涨到 %d，说明 client 没被关掉", before, after)
	}
}

func TestNew_密码不进日志(t *testing.T) {
	f := newFakeRedis(t)
	lines := capture(t)

	c := liveCfg(f)
	c.Password = "hunter2"
	_, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	closer.Close()

	for _, l := range lines() {
		blob := fmt.Sprint(l)
		if strings.Contains(blob, "hunter2") {
			t.Errorf("日志里出现了密码：%v", l)
		}
	}
	got := lines()
	if len(got) == 0 || got[0]["地址"] != c.Addr {
		t.Errorf("该写出地址，否则排查不了连的是谁，got=%v", got)
	}
}

func TestNew_配置有误时不建连(t *testing.T) {
	c := DefaultClientConfig()
	c.Addr = ""
	if _, _, err := New(c); err == nil {
		t.Fatal("Addr 为空应当报错")
	}
}

func TestNew_传下去的连接池参数生效(t *testing.T) {
	f := newFakeRedis(t)
	c := liveCfg(f)
	c.PoolSize, c.MinIdleConns = 7, 2

	client, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	if got := client.Options().PoolSize; got != 7 {
		t.Errorf("PoolSize 应传下去，got=%d", got)
	}
	if got := client.Options().MinIdleConns; got != 2 {
		t.Errorf("MinIdleConns 应传下去，got=%d", got)
	}
}

func TestPingTimeout(t *testing.T) {
	c := DefaultClientConfig()
	c.DialTimeout, c.ReadTimeout = time.Second, 2*time.Second
	if got := pingTimeout(c); got != 3*time.Second {
		t.Errorf("Ping 预算应为建连 + 读超时，got=%v", got)
	}
	c.DialTimeout, c.ReadTimeout = 0, 0
	if got := pingTimeout(c); got != fallbackPingTimeout {
		t.Errorf("推算不出时该用兜底值，got=%v", got)
	}
}

func TestPing_重试后仍失败(t *testing.T) {
	f := newFakeRedis(t)
	f.setFailPing(true)
	c := liveCfg(f)

	client := redis.NewClient(&redis.Options{Addr: c.Addr})
	defer client.Close()

	start := time.Now()
	if err := ping(client, c); err == nil {
		t.Fatal("Ping 一直失败时应当返回错误")
	}
	if elapsed := time.Since(start); elapsed < 2*pingInterval {
		t.Errorf("应当重试 %d 次，实际只用了 %v", pingAttempts, elapsed)
	}
	// 重试了几次，服务端就该收到几次 ping
	n := 0
	for _, cmd := range f.seen() {
		if cmd == "ping" {
			n++
		}
	}
	if n != pingAttempts {
		t.Errorf("服务端应收到 %d 次 ping，实际 %d 次", pingAttempts, n)
	}
}

// ---- 全局实例 ----

func withClients(t *testing.T, m map[string]*redis.Client) {
	t.Helper()
	mu.Lock()
	old := clients
	clients = m
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		clients = old
		mu.Unlock()
	})
}

func TestC_取不到就panic(t *testing.T) {
	withClients(t, map[string]*redis.Client{})
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

func TestC_panic信息列出已配置的实例(t *testing.T) {
	withClients(t, map[string]*redis.Client{"cache": {}, "session": {}})
	defer func() {
		msg := fmt.Sprint(recover())
		if !strings.Contains(msg, "cache") || !strings.Contains(msg, "session") {
			t.Errorf("应列出已配置的实例名，got=%v", msg)
		}
	}()
	C("typo")
}

func TestHasNames(t *testing.T) {
	withClients(t, map[string]*redis.Client{"session": {}, "cache": {}})
	if !Has("cache") || Has("nope") || Has() {
		t.Error("Has 应如实反映是否配过")
	}
	if got := Names(); len(got) != 2 || got[0] != "cache" {
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
	f := newFakeRedis(t)
	old := cfg
	t.Cleanup(func() { cfg = old })
	cfg = Config{Clients: map[string]ClientConfig{"a": liveCfg(f), "b": liveCfg(f)}}

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
	// Init 返回错误时框架拿不到 closer，已经建好的实例必须自己收拾——
	// 不收拾的话那些连接会一直挂到进程结束，而且 C() 还可能摸到它们
	f := newFakeRedis(t)
	old := cfg
	t.Cleanup(func() { cfg = old })

	bad := DefaultClientConfig()
	bad.Addr = deadAddr(t)
	bad.DialTimeout, bad.ReadTimeout, bad.MinIdleConns = 30*time.Millisecond, 30*time.Millisecond, 0
	cfg = Config{Clients: map[string]ClientConfig{"a": liveCfg(f), "z": bad}}

	_, err := initAll()
	if err == nil {
		t.Fatal("有实例连不上时应当报错")
	}
	if !strings.Contains(err.Error(), `"z"`) {
		t.Errorf("错误里应点名是哪个实例，got=%v", err)
	}
	if got := Names(); len(got) != 0 {
		t.Errorf("失败时不该发布任何实例，否则 C() 会摸到半成品，got=%v", got)
	}
	// 直接看假服务端：连接都断了，才说明先建好的那个真被关了
	if n := f.waitConns(0); n != 0 {
		t.Errorf("先建好的实例应当被回滚关闭，假服务端上还开着 %d 个连接", n)
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

func TestMetricEnabled(t *testing.T) {
	on, off := DefaultClientConfig(), DefaultClientConfig()
	off.Metric = false
	if metricEnabled(map[string]ClientConfig{"a": off}) {
		t.Error("都关了就不该注册")
	}
	if !metricEnabled(map[string]ClientConfig{"a": off, "b": on}) {
		t.Error("有一个开着就该注册")
	}
}

func TestPoolCollector(t *testing.T) {
	m, closer, err := xmetric.New(xmetric.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	stats := map[string]*redis.PoolStats{
		"cache":   {TotalConns: 5, IdleConns: 3, StaleConns: 1, Hits: 100, Misses: 7, Timeouts: 2},
		"nil 的实例": nil, // 不该让整次抓取炸掉
	}
	m.Registry.MustRegister(newPoolCollector("demo", nil, func() map[string]*redis.PoolStats { return stats }))

	w := httptest.NewRecorder()
	m.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	out := w.Body.String()

	for _, want := range []string{
		`demo_redis_pool_connections{name="cache"} 5`,
		`demo_redis_pool_connections_idle{name="cache"} 3`,
		`demo_redis_pool_connections_stale_total{name="cache"} 1`,
		`demo_redis_pool_hits_total{name="cache"} 100`,
		`demo_redis_pool_misses_total{name="cache"} 7`,
		`demo_redis_pool_timeouts_total{name="cache"} 2`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("导出里应有 %s\n实际=\n%s", want, out)
		}
	}
}

func TestPoolStats_读的是活着的实例(t *testing.T) {
	f := newFakeRedis(t)
	client, closer, err := New(liveCfg(f))
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	withClients(t, map[string]*redis.Client{"cache": client})

	got := poolStats()
	if len(got) != 1 || got["cache"] == nil {
		t.Errorf("应读到活着的实例，got=%v", got)
	}
}
