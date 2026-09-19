package xredis

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
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
	client, closer, err := New(context.Background(), liveCfg(f))
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
	_, _, err := New(context.Background(), c)
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
		client, closer, err := New(context.Background(), c)
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
	_, closer, err := New(context.Background(), c)
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
	if len(got) == 0 || got[0]["addr"] != c.Addr {
		t.Errorf("该写出地址，否则排查不了连的是谁，got=%v", got)
	}
}

func TestNew_配置有误时不建连(t *testing.T) {
	c := DefaultClientConfig()
	c.Addr = ""
	if _, _, err := New(context.Background(), c); err == nil {
		t.Fatal("Addr 为空应当报错")
	}
}

func TestNew_传下去的连接池参数生效(t *testing.T) {
	f := newFakeRedis(t)
	c := liveCfg(f)
	c.PoolSize, c.MinIdleConns = 7, 2

	client, closer, err := New(context.Background(), c)
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

func TestNew_退出信号到达时当场放弃建连(t *testing.T) {
	// 地址不通时这里要走满一轮 Ping 重试（默认 3 次 × 间隔）。
	// 启动到一半收到 SIGTERM，就该立刻放弃，而不是让进程卡在一个
	// 注定连不上的库上，把退出时间拖满整轮重试
	f := newFakeRedis(t)
	f.setFailPing(true)
	c := liveCfg(f)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 模拟建连之前就收到了退出信号

	start := time.Now()
	_, _, err := New(ctx, c)
	if err == nil {
		t.Fatal("ctx 已取消时不该建连成功")
	}
	if elapsed := time.Since(start); elapsed > pingInterval {
		t.Errorf("应当当场放弃而不是走完整轮重试，耗时=%v", elapsed)
	}
}

func TestPing_重试后仍失败(t *testing.T) {
	f := newFakeRedis(t)
	f.setFailPing(true)
	c := liveCfg(f)

	client := redis.NewClient(&redis.Options{Addr: c.Addr})
	defer client.Close()

	start := time.Now()
	if err := ping(context.Background(), client, c); err == nil {
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
	old := map[string]*redis.Client{}
	for _, n := range reg.Names() {
		if v, ok := reg.Lookup(n); ok {
			old[n] = v
		}
	}
	reg.Publish(m)
	t.Cleanup(func() { reg.Publish(old) })
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
	// 配置直接传进去，不用换包级变量再记得换回来：
	// initAll 的签名如实说明它需要一份配置
	f := newFakeRedis(t)
	c := Config{Clients: map[string]ClientConfig{"a": liveCfg(f), "b": liveCfg(f)}}

	closer, err := initAll(context.Background(), c)
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

	_, err := initAll(context.Background(), cfg)
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

	closer, err := initAll(context.Background(), cfg)
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
	client, closer, err := New(context.Background(), liveCfg(f))
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

func TestNew_命令遵守调用方的deadline(t *testing.T) {
	// go-redis 默认不让请求的 context 管住 socket 读写：不开
	// ContextTimeoutEnabled 的话，每个命令用的是 ReadTimeout 这组固定值，
	// 调用方给的 deadline 只是摆设——一个 200ms 超时的请求照样会在一个
	// 慢 Redis 上等满 ReadTimeout，上游的超时预算和级联保护跟着一起失效
	f := newFakeRedis(t)
	f.setStall("get") // 收下 GET 但永不回复

	c := liveCfg(f)
	c.ReadTimeout = 5 * time.Second // 比 ctx 的预算大得多
	c.MaxRetries = -1               // 重试会掩盖掉这件事
	client, closer, err := New(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	err = client.Get(ctx, "k").Err()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("该超时的")
	}
	// go-redis 把 ctx 的 deadline 设到 socket 上，所以报上来的是
	// os.ErrDeadlineExceeded（i/o timeout）而不是 context.DeadlineExceeded。
	// 调用方要判超时得认这个，或者干脆判 ctx.Err()
	if !errors.Is(err, os.ErrDeadlineExceeded) && !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("该是超时错误，got=%v (%T)", err, err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("ctx 给了 200ms，实际等了 %v —— deadline 没管住 socket 读写", elapsed.Round(10*time.Millisecond))
	}
}
