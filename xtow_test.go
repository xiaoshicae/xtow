package xtow

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/xiaoshicae/xtow/registry"
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
		Init: func() (io.Closer, error) {
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
			Init: func() (io.Closer, error) { return &closer{name: "stuck", r: r, err: closeErr}, nil },
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
			Init: func() (io.Closer, error) { panic("初始化炸了") },
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
		Init: func() (io.Closer, error) {
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
