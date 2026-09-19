package xgorm

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/xiaoshicae/xtow/registry"
)

// TestMain 调短重试间隔：连不上的用例要跑满整轮重试，按一秒算一次就是几十秒
func TestMain(m *testing.M) {
	pingInterval = 10 * time.Millisecond
	os.Exit(m.Run())
}

// deadAddr 返回一个没人监听的地址：建连必定失败，且失败得很快
func deadAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close() // 立刻关掉，端口就没人监听了
	return addr
}

func TestNew_连不上时不漏协程(t *testing.T) {
	// New 失败之后不该留下活着的连接池：database/sql 的 opener 协程
	// 只在 Close 时退出，漏一个就是一个再也不会走的常驻协程。
	//
	// gorm v1.31 起会在自己的失败路径上把池子关掉，所以这条现在主要盯的是
	// gorm.Open 之后那几步——设连接池参数、Ping、挂链路——出错时的收尾。
	c := DefaultClientConfig()
	c.Driver = DriverMySQL
	c.DSN = "u:p@tcp(" + deadAddr(t) + ")/app"
	c.DialTimeout = 50 * time.Millisecond
	c.MySQL.ReadTimeout = 50 * time.Millisecond

	const rounds = 5
	before := stabilize()
	for i := 0; i < rounds; i++ {
		db, closer, err := New(context.Background(), c)
		if err == nil {
			closer.Close()
			t.Fatal("连不上时应当报错")
		}
		if db != nil || closer != nil {
			t.Error("失败时不该返回半成品")
		}
	}

	// 必须等协程真正退出再数：连接池关闭后它的 opener 协程是异步退出的，
	// 立刻去数会把「正在退出」当成「泄漏」，也会把真泄漏淹没在噪声里
	if after := settleTo(before); after > before+1 {
		t.Errorf("建连失败 %d 次后协程数从 %d 涨到 %d，说明连接池没被关掉", rounds, before, after)
	}
}

func TestNew_失败信息里有地址没有密码(t *testing.T) {
	addr := deadAddr(t)
	c := DefaultClientConfig()
	c.Driver = DriverMySQL
	c.DSN = "u:" + secret + "@tcp(" + addr + ")/app"
	c.DialTimeout = 50 * time.Millisecond
	c.MySQL.ReadTimeout = 50 * time.Millisecond

	_, _, err := New(context.Background(), c)
	if err == nil {
		t.Fatal("连不上时应当报错")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("错误信息里出现了密码：%v", err)
	}
	if !strings.Contains(err.Error(), addr) {
		t.Errorf("错误信息里应有地址，否则排查不了连的是谁：%v", err)
	}
}

func TestNew_配置有误时不建连(t *testing.T) {
	c := DefaultClientConfig() // 没有 DSN
	_, _, err := New(context.Background(), c)
	if err == nil {
		t.Fatal("DSN 为空应当报错")
	}
	if !strings.Contains(err.Error(), "DSN") {
		t.Errorf("错误该说清楚是哪儿的问题，got=%v", err)
	}
}

func TestPingTimeout(t *testing.T) {
	// 不能只用建连超时：Ping 是建连加一个往返，
	// 拿建连预算当整体预算，连接刚建成就会被判超时
	c := DefaultClientConfig()
	c.Driver = DriverMySQL
	c.DialTimeout = time.Second
	c.MySQL.ReadTimeout = 2 * time.Second
	if got := pingTimeout(c); got != 3*time.Second {
		t.Errorf("MySQL 的 Ping 预算应为建连 + 读超时，got=%v", got)
	}

	c.Driver = DriverPostgres
	if got := pingTimeout(c); got != time.Second {
		t.Errorf("PG 只用建连超时，got=%v", got)
	}

	c.DialTimeout = 0
	if got := pingTimeout(c); got != fallbackPingTimeout {
		t.Errorf("推算不出预算时该用兜底值，got=%v", got)
	}
}

func TestPing_重试后仍失败(t *testing.T) {
	// 重试要真的重试，也要在预算内结束——启动期卡死比连不上更难查
	pool, err := sql.Open("mysql", "u:p@tcp("+deadAddr(t)+")/app?timeout=30ms")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	c := DefaultClientConfig()
	c.Driver = DriverMySQL
	c.DialTimeout = 30 * time.Millisecond
	c.MySQL.ReadTimeout = 30 * time.Millisecond

	start := time.Now()
	if err := ping(context.Background(), pool, c); err == nil {
		t.Fatal("连不上时应当返回错误")
	}
	elapsed := time.Since(start)
	// 3 次尝试、间隔 1s，所以至少要过两个间隔；但不能拖过总预算太多
	if elapsed < 2*pingInterval {
		t.Errorf("应当重试 %d 次，实际只用了 %v", pingAttempts, elapsed)
	}
	if elapsed > 2*pingInterval+2*time.Second {
		t.Errorf("重试超出了预算，用了 %v", elapsed)
	}
}

// ---- 全局实例 ----

func withClients(t *testing.T, m map[string]*gorm.DB) {
	t.Helper()
	old := map[string]*gorm.DB{}
	for _, n := range reg.Names() {
		if v, ok := reg.Lookup(n); ok {
			old[n] = v
		}
	}
	reg.Publish(m)
	t.Cleanup(func() { reg.Publish(old) })
}

func TestC_取不到就panic(t *testing.T) {
	// 返回 nil 只是把同一个 panic 推迟到调用方第一次用它的时候，
	// 那里的栈里只剩 invalid memory address，看不出根因是配置没配
	withClients(t, map[string]*gorm.DB{})

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("取不到实例应当 panic")
		}
		msg := fmt.Sprint(r)
		if !strings.Contains(msg, ConfigKey) {
			t.Errorf("一个都没配时该提示去看配置块，got=%v", msg)
		}
	}()
	C()
}

func TestC_panic信息列出已配置的实例(t *testing.T) {
	// 名字写错和整块没配是两个不同的问题，列出实际配了哪些，两者一眼可分
	withClients(t, map[string]*gorm.DB{"main": {}, "report": {}})

	defer func() {
		msg := fmt.Sprint(recover())
		if !strings.Contains(msg, "main") || !strings.Contains(msg, "report") {
			t.Errorf("应列出已配置的实例名，got=%v", msg)
		}
	}()
	C("typo")
}

func TestHasNames(t *testing.T) {
	withClients(t, map[string]*gorm.DB{"report": {}, "main": {}})

	if !Has("main") || Has("nope") {
		t.Error("Has 应如实反映是否配过")
	}
	if got := Names(); len(got) != 2 || got[0] != "main" || got[1] != "report" {
		t.Errorf("Names 应按名字排序，got=%v", got)
	}
	if Has() {
		t.Error("没有 default 时 Has() 应为 false")
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
		t.Errorf("数据库要在服务对外之前就绪，got=%v", got.Stage)
	}
	if got.Config != &cfg {
		t.Error("登记的必须是包级配置变量本身")
	}
}

func TestInitAll_没配就什么都不做(t *testing.T) {
	old := cfg
	t.Cleanup(func() { cfg = old })
	cfg = DefaultConfig()

	closer, err := initAll(context.Background())
	if err != nil {
		t.Fatalf("没配 XGorm 不该报错：%v", err)
	}
	if err := closer.Close(); err != nil {
		t.Errorf("关闭不该报错：%v", err)
	}
	if len(Names()) != 0 {
		t.Errorf("不该建出实例，got=%v", Names())
	}
}

func TestInitAll_一个失败就全部回滚(t *testing.T) {
	// Init 返回错误时框架拿不到 closer，已经建好的必须自己收拾，否则漏连接池
	old := cfg
	t.Cleanup(func() { cfg = old })

	bad := DefaultClientConfig()
	bad.Driver, bad.DSN = DriverMySQL, "u:p@tcp("+deadAddr(t)+")/app"
	bad.DialTimeout, bad.MySQL.ReadTimeout = 30*time.Millisecond, 30*time.Millisecond
	cfg = Config{Clients: map[string]ClientConfig{"a": bad, "b": bad}}

	before := stabilize()
	_, err := initAll(context.Background())
	if err == nil {
		t.Fatal("连不上时应当报错")
	}
	if !strings.Contains(err.Error(), `"a"`) {
		t.Errorf("错误里应点名是哪个实例，got=%v", err)
	}
	if after := settleTo(before); after > before+1 {
		t.Errorf("回滚不干净，协程数从 %d 涨到 %d", before, after)
	}
}

func TestMetricEnabled(t *testing.T) {
	on, off := DefaultClientConfig(), DefaultClientConfig()
	off.Metric = false

	if metricEnabled(map[string]ClientConfig{"a": off}) {
		t.Error("都关了就不该注册")
	}
	if !metricEnabled(map[string]ClientConfig{"a": off, "b": on}) {
		t.Error("有一个开着就该注册——collector 是进程级的一个")
	}
}

func TestCWithCtx(t *testing.T) {
	// GORM 必须这样传 ctx，不像 go-redis 每个方法都收 ctx
	withClients(t, map[string]*gorm.DB{DefaultName: {Config: &gorm.Config{}, Statement: &gorm.Statement{}}})
	db := CWithCtx(context.Background())
	if db == nil {
		t.Fatal("应返回实例")
	}
}

func TestSettle_能看见泄漏的连接池(t *testing.T) {
	// 先验证这把尺子是准的：上一版读数没等协程退出、阈值又放到 +2，
	// 于是「不漏协程」那条测试怎么改都通过——一条永远不会失败的测试
	// 比没有测试更糟，它让人以为查过了。
	addr := deadAddr(t)
	before := stabilize()

	const leaked = 5
	pools := make([]*sql.DB, 0, leaked)
	for i := 0; i < leaked; i++ {
		pool, err := sql.Open("mysql", "u:p@tcp("+addr+")/app")
		if err != nil {
			t.Fatal(err)
		}
		pool.SetMaxIdleConns(1)
		// 碰一下让 opener 协程真正起来
		pool.PingContext(context.Background())
		pools = append(pools, pool)
	}

	if during := settleTo(before); during <= before {
		t.Fatalf("漏了 %d 个连接池却没看出协程增长（%d -> %d），这把尺子是坏的", leaked, before, during)
	}
	for _, p := range pools {
		p.Close()
	}
	if after := settleTo(before); after > before+1 {
		t.Errorf("全关掉之后应当回落，got %d -> %d", before, after)
	}
}

// stabilize 等协程数不再变化，用来取一个基准值
func stabilize() int {
	last := runtime.NumGoroutine()
	stable := 0
	for i := 0; i < 200; i++ {
		time.Sleep(50 * time.Millisecond)
		n := runtime.NumGoroutine()
		if n == last {
			if stable++; stable >= 3 {
				return n
			}
			continue
		}
		last, stable = n, 0
	}
	return last
}

// settleTo 等协程数回落到 target 附近，最多等 10 秒，超时返回实际值。
//
// 不能用「连续几次读数相同」当作稳定：后台协程是一批批退出的，
// 中间会有好几百毫秒纹丝不动，那时候读三次都一样，却离回落还远。
// 上一版就是这么误报的——它在半路上就宣布「稳定了，还剩 8 个」。
func settleTo(target int) int {
	for i := 0; i < 200; i++ {
		if n := runtime.NumGoroutine(); n <= target+1 {
			return n
		}
		time.Sleep(50 * time.Millisecond)
	}
	return runtime.NumGoroutine()
}
