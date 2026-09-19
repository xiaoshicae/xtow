package xtow

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/xiaoshicae/xtow/registry"
	"github.com/xiaoshicae/xtow/xerror"
)

// ---- 测试替身：不用 mock，都是普通类型 ----

type recorder struct {
	mu  sync.Mutex
	seq []string
}

func (r *recorder) add(s string) { r.mu.Lock(); r.seq = append(r.seq, s); r.mu.Unlock() }
func (r *recorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.seq, " → ")
}

type closer struct {
	name string
	r    *recorder
	err  error
}

func (c *closer) Close() error { c.r.add("close:" + c.name); return c.err }

// comp 造一个登记项：初始化时记一笔，返回的 Closer 关闭时记一笔
func comp(key string, stage registry.Stage, r *recorder, initErr error) registry.Component {
	return registry.Component{
		Key:   key,
		Stage: stage,
		Init: func(context.Context) (io.Closer, error) {
			r.add("init:" + key)
			if initErr != nil {
				return nil, initErr
			}
			return &closer{name: key, r: r}, nil
		},
	}
}

type server struct {
	r       *recorder
	stopped chan struct{}
	startEr error
}

func newServer(r *recorder) *server { return &server{r: r, stopped: make(chan struct{})} }

func (s *server) Start(context.Context) error {
	s.r.add("start:server")
	if s.startEr != nil {
		return s.startEr
	}
	<-s.stopped
	return nil
}

func (s *server) Stop(context.Context) error {
	s.r.add("stop:server")
	close(s.stopped)
	return nil
}

func emptyConf(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "application.yml")
	os.WriteFile(p, []byte(""), 0o600)
	return p
}

// ---- 用例 ----

func TestRun_停止预算为零直接失败(t *testing.T) {
	// 0 在这里不是「不限时」而是「一点都不等」：Stop 拿到的是一个已经过期的
	// context，服务当场被切断，后面每个组件的关闭也都在超时状态下跑。
	// 这种配错必须在做任何事之前就拦住
	started := false
	r := &lateRunnable{
		start: func(context.Context) error { started = true; return nil },
		stop:  func(context.Context) error { return nil },
	}
	err := Run(r, withComponents(), WithLogger(quietLogger()), WithStopTimeout(0))
	if err == nil {
		t.Fatal("停止预算为 0 应当直接报错")
	}
	if started {
		t.Error("报错之前不该已经把服务启动起来")
	}
}

func TestRun_逆序关闭(t *testing.T) {
	r := &recorder{}
	go func() { time.Sleep(120 * time.Millisecond); syscall.Kill(syscall.Getpid(), syscall.SIGTERM) }()

	err := Run(newServer(r),
		WithConfigPath(emptyConf(t)),
		withComponents(comp("a", registry.StageClient, r, nil), comp("b", registry.StageClient, r, nil)))
	if err != nil {
		t.Fatalf("正常退出不该有错误: %v", err)
	}

	want := "init:a → init:b → start:server → stop:server → close:b → close:a"
	if got := r.String(); got != want {
		t.Errorf("关闭顺序不对\n got=%s\nwant=%s", got, want)
	}
}

func TestRun_Stage决定顺序而非登记顺序(t *testing.T) {
	// 这是整套设计的核心主张：init() 的执行顺序（也就是登记顺序）不影响结果
	r := &recorder{}
	go func() { time.Sleep(120 * time.Millisecond); syscall.Kill(syscall.Getpid(), syscall.SIGTERM) }()

	// 故意按「反的」顺序登记
	err := Run(newServer(r),
		WithConfigPath(emptyConf(t)),
		withComponents(
			comp("client", registry.StageClient, r, nil),
			comp("trace", registry.StageTelemetry, r, nil),
			comp("log", registry.StageLog, r, nil),
		))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(r.String(), "init:log → init:trace → init:client") {
		t.Errorf("应按 Stage 升序初始化，与登记顺序无关，got=%s", r.String())
	}
	if !strings.HasSuffix(r.String(), "close:client → close:trace → close:log") {
		t.Errorf("关闭应是初始化的严格逆序，got=%s", r.String())
	}
}

func TestRun_同档内保持登记顺序(t *testing.T) {
	r := &recorder{}
	go func() { time.Sleep(120 * time.Millisecond); syscall.Kill(syscall.Getpid(), syscall.SIGTERM) }()

	_ = Run(newServer(r), WithConfigPath(emptyConf(t)),
		withComponents(
			comp("first", registry.StageClient, r, nil),
			comp("second", registry.StageClient, r, nil),
		))
	if !strings.HasPrefix(r.String(), "init:first → init:second") {
		t.Errorf("同一档内应保持登记顺序（稳定排序），got=%s", r.String())
	}
}

func TestRun_组件初始化失败时回滚已初始化的部分(t *testing.T) {
	r := &recorder{}
	boom := errors.New("连不上")

	err := Run(newServer(r), WithConfigPath(emptyConf(t)),
		withComponents(
			comp("ok", registry.StageClient, r, nil),
			comp("bad", registry.StageClient, r, boom),
		))

	if !errors.Is(err, boom) {
		t.Fatalf("应返回初始化失败的原始错误，got=%v", err)
	}
	if got := r.String(); got != "init:ok → init:bad → close:ok" {
		t.Errorf("失败时应逆序关闭已初始化的部分，且不启动 server，got=%s", got)
	}
}

func TestRun_服务启动失败(t *testing.T) {
	r := &recorder{}
	boom := errors.New("端口被占用")
	s := newServer(r)
	s.startEr = boom

	err := Run(s, WithConfigPath(emptyConf(t)), withComponents(comp("a", registry.StageClient, r, nil)))
	if !errors.Is(err, boom) {
		t.Fatalf("应返回服务启动错误，got=%v", err)
	}
	if !strings.Contains(r.String(), "close:a") {
		t.Errorf("服务起不来时组件也要被关掉，got=%s", r.String())
	}
}

func TestRun_关闭出错会被汇总而不是吞掉(t *testing.T) {
	r := &recorder{}
	closeErr := errors.New("关不掉")
	go func() { time.Sleep(120 * time.Millisecond); syscall.Kill(syscall.Getpid(), syscall.SIGTERM) }()

	err := Run(newServer(r), WithConfigPath(emptyConf(t)),
		withComponents(registry.Component{
			Key:  "stuck",
			Init: func(context.Context) (io.Closer, error) { return &closer{name: "stuck", r: r, err: closeErr}, nil },
		}))

	if !errors.Is(err, closeErr) {
		t.Fatalf("关闭错误应被返回，got=%v", err)
	}
}

func TestRun_组件panic被隔离(t *testing.T) {
	r := &recorder{}
	err := Run(newServer(r), WithConfigPath(emptyConf(t)),
		withComponents(registry.Component{
			Key:  "panicky",
			Init: func(context.Context) (io.Closer, error) { panic("初始化炸了") },
		}))

	if err == nil || !strings.Contains(err.Error(), "panic") {
		t.Fatalf("组件 panic 应被转成错误而不是打穿进程，got=%v", err)
	}
}

func TestRun_显式指定的配置文件不存在是错误(t *testing.T) {
	r := &recorder{}
	err := Run(newServer(r), WithConfigPath(filepath.Join(t.TempDir(), "nope.yml")))
	if err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("点名要的配置文件找不到应当场失败，got=%v", err)
	}
}

func TestRun_找不到配置文件时用默认值正常启动(t *testing.T) {
	r := &recorder{}
	chdir(t, t.TempDir()) // 约定路径下什么都没有
	os.Args = []string{"svc"}
	go func() { time.Sleep(120 * time.Millisecond); syscall.Kill(syscall.Getpid(), syscall.SIGTERM) }()

	if err := Run(newServer(r), withComponents(comp("a", registry.StageClient, r, nil))); err != nil {
		t.Fatalf("没有配置文件应该只告警、用默认值起，got=%v", err)
	}
	if !strings.Contains(r.String(), "init:a") {
		t.Errorf("组件仍应被初始化，got=%s", r.String())
	}
}

// chdir 切换工作目录，测试结束后切回。见 internal/config 里同名助手的说明：
// testing.T.Chdir 要 Go 1.24，核心模块的下限不为一个测试助手上抬。
func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
}

func TestRun_等服务真正退出再关组件(t *testing.T) {
	// 回归用例。Stop 返回不等于服务已经停干净——Stop 只负责「让它停」，
	// 等不等在处理的请求做完是各实现自己的事。不等就往下关的话，
	// 还在跑的请求会摸到已经关掉的数据库和缓存。
	var closedAt, startReturnedAt time.Time
	var mu sync.Mutex

	comp := registry.Component{
		Key: "Probe", Stage: registry.StageClient,
		Init: func(context.Context) (io.Closer, error) {
			return closerFunc(func() error {
				mu.Lock()
				defer mu.Unlock()
				closedAt = time.Now()
				return nil
			}), nil
		},
	}

	// Stop 只发个信号就返回，Start 还要再跑一会儿才退出
	stop := make(chan struct{})
	r := &lateRunnable{
		start: func(context.Context) error {
			<-stop
			time.Sleep(50 * time.Millisecond)
			mu.Lock()
			defer mu.Unlock()
			startReturnedAt = time.Now()
			return nil
		},
		stop: func(context.Context) error { close(stop); return nil },
	}

	go func() { time.Sleep(50 * time.Millisecond); syscallSelfInterrupt(t) }()
	if err := Run(r, withComponents(comp), WithLogger(quietLogger())); err != nil {
		t.Fatalf("Run 失败：%v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if startReturnedAt.IsZero() || closedAt.IsZero() {
		t.Fatal("两边都该跑到")
	}
	if closedAt.Before(startReturnedAt) {
		t.Errorf("组件在服务退出之前就被关了：关闭=%v 服务退出=%v", closedAt, startReturnedAt)
	}
}

func TestRun_服务退出时的错误不会被丢掉(t *testing.T) {
	// 走信号分支时 Start 的返回值此前从没被读过
	wantErr := errors.New("监听挂了")
	stop := make(chan struct{})
	r := &lateRunnable{
		start: func(context.Context) error { <-stop; return wantErr },
		stop:  func(context.Context) error { close(stop); return nil },
	}

	go func() { time.Sleep(50 * time.Millisecond); syscallSelfInterrupt(t) }()
	err := Run(r, withComponents(), WithLogger(quietLogger()))
	if !errors.Is(err, wantErr) {
		t.Errorf("服务退出时的错误应当被带出来，got=%v", err)
	}
}

type lateRunnable struct {
	start func(context.Context) error
	stop  func(context.Context) error
}

func (l *lateRunnable) Start(ctx context.Context) error { return l.start(ctx) }
func (l *lateRunnable) Stop(ctx context.Context) error  { return l.stop(ctx) }

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// syscallSelfInterrupt 给自己发一个 SIGINT，模拟收到退出信号
func syscallSelfInterrupt(t *testing.T) {
	t.Helper()
	p, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Error(err)
		return
	}
	if err := p.Signal(syscall.SIGINT); err != nil {
		t.Error(err)
	}
}

func TestRun_服务自己退出时立刻返回(t *testing.T) {
	// 回归用例。服务自己退出时，上面那次 select 已经把 runErr 取走了，
	// 再取一次就是白等满整个停止预算——一个只会表现为「慢」的 bug。
	r := &lateRunnable{
		start: func(context.Context) error { return nil },
		stop:  func(context.Context) error { return nil },
	}

	start := time.Now()
	if err := Run(r, withComponents(), WithLogger(quietLogger()), WithStopTimeout(5*time.Second)); err != nil {
		t.Fatalf("Run 失败：%v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("服务自己退出时应当立刻返回，实际用了 %v", elapsed)
	}
}

// ---- 启动期间收到退出信号 ----

func TestRun_初始化期间收到信号就不启动服务(t *testing.T) {
	// 信号接管必须早于初始化。装在初始化之后的话，连库、连 Redis、Ping 重试
	// 那几秒里 SIGTERM 走的是系统默认处置——进程当场暴毙，已经建好的资源
	// 一个都来不及注销（注册中心里那条记录、那把分布式锁只能等超时过期）
	r := &recorder{}
	started := false

	slow := registry.Component{
		Key:   "慢组件",
		Stage: registry.StageClient,
		Init: func(ctx context.Context) (io.Closer, error) {
			r.add("init:慢组件")
			syscallSelfInterrupt(t) // 正在初始化时收到退出信号
			select {
			case <-ctx.Done(): // 组件自己把 ctx 传下去了，于是当场就能放弃
				r.add("慢组件被打断")
			case <-time.After(3 * time.Second):
				t.Error("组件的 ctx 没有被取消")
			}
			return &closer{name: "慢组件", r: r}, nil
		},
	}
	after := comp("后面的组件", registry.StageServer, r, nil)

	srv := &lateRunnable{
		start: func(context.Context) error { started = true; return nil },
		stop:  func(context.Context) error { return nil },
	}

	if err := Run(srv, withComponents(slow, after), WithLogger(quietLogger())); err != nil {
		t.Fatalf("按信号退出不是故障，不该报错：%v", err)
	}
	if started {
		t.Error("初始化期间就收到了退出信号，不该再把服务起起来")
	}
	got := r.String()
	if !strings.Contains(got, "慢组件被打断") {
		t.Errorf("组件应当能从 ctx 感知到退出信号，got=%s", got)
	}
	if strings.Contains(got, "init:后面的组件") {
		t.Errorf("收到退出信号后不该再初始化剩余组件，got=%s", got)
	}
	if !strings.Contains(got, "close:慢组件") {
		t.Errorf("已经建好的组件仍要被逆序关干净，got=%s", got)
	}
}

func TestRun_初始化被信号打断而失败时不算故障(t *testing.T) {
	// 被取消的建连必然失败。把它当故障报上去的话，每次滚动更新撞上
	// 这个窗口都会在面板上留一条「启动失败」，而它其实是按要求退出
	r := &recorder{}
	broken := registry.Component{
		Key:   "连不上的库",
		Stage: registry.StageClient,
		Init: func(ctx context.Context) (io.Closer, error) {
			syscallSelfInterrupt(t)
			<-ctx.Done()
			return nil, ctx.Err() // 建连被取消，如实返回错误
		},
	}
	before := comp("先起来的", registry.StageLog, r, nil)

	srv := &lateRunnable{
		start: func(context.Context) error { t.Error("不该启动服务"); return nil },
		stop:  func(context.Context) error { return nil },
	}

	if err := Run(srv, withComponents(before, broken), WithLogger(quietLogger())); err != nil {
		t.Fatalf("按信号退出不该以错误收场：%v", err)
	}
	if got := r.String(); !strings.Contains(got, "close:先起来的") {
		t.Errorf("先起来的组件仍要被关掉，got=%s", got)
	}
}

// stuckChildEnv 置位时，测试进程扮演「卡在初始化里的子进程」
const stuckChildEnv = "XTOW_TEST_STUCK_CHILD"

func TestRun_卡住时第二个信号能立即终止(t *testing.T) {
	if os.Getenv(stuckChildEnv) == "1" {
		runStuckChild()
		return
	}

	// 注册信号处理这件事本身，取消了系统原本的「收到就死」。
	// 于是框架一旦卡在某个不看 ctx 的第三方调用里（这里用 time.Sleep 模拟），
	// 信号就只是往一个没人看的 ctx 里送——进程变成只有 kill -9 收得掉，
	// K8s 得等满整个终止宽限期。第一个信号之后把默认处置还回去，
	// 第二个信号才有地方可去。
	// 用 os.Executable 而不是 os.Args[0]：定位配置文件的测试会改写 os.Args，
	// 跑在它后面时 os.Args[0] 已经是那个测试编的假程序名了
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run=^TestRun_卡住时第二个信号能立即终止$")
	cmd.Env = append(os.Environ(), stuckChildEnv+"=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	waitFor(t, stdout, "STUCK")

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // 让接管协程把默认处置还回去
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("子进程该被信号终止，got=%v", err)
		}
		st, ok := ee.Sys().(syscall.WaitStatus)
		if !ok || !st.Signaled() || st.Signal() != syscall.SIGTERM {
			t.Errorf("该以「被 SIGTERM 终止」收场（退出码 143），got=%v", ee)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("第二个信号没能终止卡住的子进程——只剩 kill -9 这一条路了")
	}
}

// runStuckChild 起一个卡在初始化里、且不看 ctx 的进程
func runStuckChild() {
	stuck := registry.Component{
		Key:   "卡住的组件",
		Stage: registry.StageClient,
		Init: func(context.Context) (io.Closer, error) {
			fmt.Println("STUCK")
			os.Stdout.Sync()
			time.Sleep(60 * time.Second) // 模拟没有超时的第三方建连
			return nil, nil
		},
	}
	srv := &lateRunnable{
		start: func(ctx context.Context) error { <-ctx.Done(); return nil },
		stop:  func(context.Context) error { return nil },
	}
	_ = Run(srv, withComponents(stuck), WithLogger(quietLogger()))
}

// waitFor 读子进程的输出直到出现 marker
func waitFor(t *testing.T, r io.Reader, marker string) {
	t.Helper()
	found := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			if strings.Contains(sc.Text(), marker) {
				close(found)
				return
			}
		}
	}()
	select {
	case <-found:
	case <-time.After(10 * time.Second):
		t.Fatalf("没等到子进程输出 %q", marker)
	}
}

// ---- 停止预算 ----

func TestShutdown_关不掉的组件不拖住其余组件(t *testing.T) {
	// WithStopTimeout 说的是「所有组件共享的停止预算」，而 io.Closer.Close()
	// 没有 ctx。不看着它就等于没有上限——一个连接池关不掉，整个进程就陪着它
	// 挂到部署环境来 SIGKILL 为止
	r := &recorder{}
	stuck := registry.Component{
		Key: "关不掉的", Stage: registry.StageClient,
		Init: func(context.Context) (io.Closer, error) {
			return closerFunc(func() error { select {} }), nil
		},
	}
	after := comp("先关的", registry.StageServer, r, nil)

	srv := &lateRunnable{
		start: func(context.Context) error { return nil },
		stop:  func(context.Context) error { return nil },
	}

	start := time.Now()
	err := Run(srv, withComponents(stuck, after), WithLogger(quietLogger()),
		WithStopTimeout(300*time.Millisecond))
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Fatalf("卡住的 Close 把整个退出流程拖住了，耗时=%v", elapsed)
	}
	if err == nil || !strings.Contains(err.Error(), "关不掉的") {
		t.Errorf("该如实报告是哪个组件没关掉，got=%v", err)
	}
	// 卡住的那个排在后面关，它之前的仍要被关掉
	if got := r.String(); !strings.Contains(got, "close:先关的") {
		t.Errorf("卡住的组件不该拦住其余组件，got=%s", got)
	}
}

func TestShutdown_初始化失败时的关闭也有预算(t *testing.T) {
	// 这一支还没有 stopCtx，但已经建好的那几个照样可能关不掉
	stuck := registry.Component{
		Key: "关不掉的", Stage: registry.StageLog,
		Init: func(context.Context) (io.Closer, error) {
			return closerFunc(func() error { select {} }), nil
		},
	}
	boom := registry.Component{
		Key: "起不来的", Stage: registry.StageClient,
		Init: func(context.Context) (io.Closer, error) { return nil, errors.New("起不来") },
	}

	srv := &lateRunnable{
		start: func(context.Context) error { t.Error("不该启动服务"); return nil },
		stop:  func(context.Context) error { return nil },
	}

	start := time.Now()
	err := Run(srv, withComponents(stuck, boom), WithLogger(quietLogger()),
		WithStopTimeout(300*time.Millisecond))
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("启动失败后的关闭没有预算，耗时=%v", elapsed)
	}
	if err == nil || !strings.Contains(err.Error(), "起不来") {
		t.Errorf("初始化失败的原因该带出来，got=%v", err)
	}
}

func TestRun_出错时能问出是谁报的(t *testing.T) {
	// 统一成 xerror 的全部意义就在这里：调用方拿到一个错误，
	// 既能问「最终是谁报的」，也能问「链里牵扯到谁」，
	// 而不必去匹配错误消息里的字符串前缀
	boom := xerror.Newf("xgorm", "connect", "cannot reach %s: %w", "127.0.0.1:5432", errors.New("connection refused"))
	r := &recorder{}

	err := Run(newServer(r), WithConfigPath(emptyConf(t)), WithLogger(quietLogger()),
		withComponents(comp("XGorm", registry.StageClient, r, boom)))

	if err == nil {
		t.Fatal("初始化失败该返回错误")
	}
	// 最外层是框架：是它最终把这个错误交出来的
	if got := xerror.Module(err); got != "xtow" {
		t.Errorf("最外层该是 xtow，got=%q", got)
	}
	// 但根因仍然问得出来
	if !xerror.Is(err, "xgorm") {
		t.Errorf("该能问出根因在 xgorm，err=%v", err)
	}
	if xerror.Is(err, "xredis") {
		t.Errorf("不该认成没参与的模块，err=%v", err)
	}
	// 原始错误链也没断
	if !errors.Is(err, boom) {
		t.Errorf("原始错误该还在链上，err=%v", err)
	}
}
