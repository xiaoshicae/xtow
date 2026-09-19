package xflow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiaoshicae/xtow/internal/config"
	"github.com/xiaoshicae/xtow/registry"
)

// TestMain 把流程日志丢掉：这些用例本来就会刷一屏
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

// data 流程里贯穿全程的共享数据
type data struct {
	mu   sync.Mutex
	done []string // 正向执行过的步骤
	back []string // 回滚过的步骤
}

func (d *data) record(list *[]string, name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	*list = append(*list, name)
}

func (d *data) doneList() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.done...)
}

func (d *data) backList() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.back...)
}

// step 一个可编排的测试步骤
type step struct {
	name     string
	dep      Dependency
	err      error // Process 返回它
	rbErr    error // Rollback 返回它
	panics   bool
	rbPanics bool
	delay    time.Duration
	rbDelay  time.Duration
}

func (s *step) Name() string           { return s.name }
func (s *step) Dependency() Dependency { return s.dep }

func (s *step) Process(ctx context.Context, d *data) error {
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	if s.panics {
		panic("步骤炸了：" + s.name)
	}
	if s.err != nil {
		return s.err // done 只记成功的，断言里才好读
	}
	d.record(&d.done, s.name)
	return nil
}

func (s *step) Rollback(ctx context.Context, d *data) error {
	if s.rbDelay > 0 {
		time.Sleep(s.rbDelay)
	}
	if s.rbPanics {
		panic("回滚炸了：" + s.name)
	}
	d.record(&d.back, s.name)
	return s.rbErr
}

func ok(name string) *step   { return &step{name: name, dep: Strong} }
func weak(name string) *step { return &step{name: name, dep: Weak} }

func failing(name string, dep Dependency) *step {
	return &step{name: name, dep: dep, err: errors.New(name + " 失败")}
}

// withConfig 换一份配置，测试结束还原
func withConfig(t *testing.T, mutate func(*Config)) {
	t.Helper()
	old := cfg
	t.Cleanup(func() { cfg = old })
	cfg = DefaultConfig()
	if mutate != nil {
		mutate(&cfg)
	}
}

func TestExecute_全部成功(t *testing.T) {
	withConfig(t, nil)
	d := &data{}
	res := New("下单", ok("扣券"), ok("扣库存"), ok("扣款")).Execute(context.Background(), d)

	if !res.Success() {
		t.Fatalf("应当成功：%v", res)
	}
	if res.Rolled {
		t.Error("成功的流程不该回滚")
	}
	if got := d.doneList(); len(got) != 3 || got[0] != "扣券" || got[2] != "扣款" {
		t.Errorf("应按顺序执行，got=%v", got)
	}
	if len(d.backList()) != 0 {
		t.Errorf("不该回滚，got=%v", d.backList())
	}
}

func TestExecute_强依赖失败逆序回滚(t *testing.T) {
	// 「谁做的事，谁负责撤销」，而且撤销顺序必须和执行顺序相反
	withConfig(t, nil)
	d := &data{}
	res := New("下单",
		ok("扣券"), ok("扣库存"), failing("扣款", Strong), ok("发通知"),
	).Execute(context.Background(), d)

	if res.Success() {
		t.Fatal("强依赖失败时流程应当失败")
	}
	if !res.Rolled {
		t.Error("应当触发回滚")
	}
	if got := d.doneList(); len(got) != 2 {
		t.Errorf("失败之后的步骤不该执行，got=%v", got)
	}
	if got := d.backList(); len(got) != 2 || got[0] != "扣库存" || got[1] != "扣券" {
		t.Errorf("应逆序回滚，got=%v", got)
	}

	var se *StepError
	if !errors.As(res.Err, &se) || se.Processor != "扣款" {
		t.Errorf("错误里应点名是哪一步，got=%v", res.Err)
	}
}

func TestExecute_弱依赖失败继续执行(t *testing.T) {
	withConfig(t, nil)
	d := &data{}
	res := New("下单", ok("扣券"), failing("发通知", Weak), ok("写日志")).Execute(context.Background(), d)

	if !res.Success() {
		t.Fatalf("弱依赖失败不该中断流程：%v", res)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Processor != "发通知" {
		t.Errorf("应记下跳过的错误，got=%v", res.Skipped)
	}
	if got := d.doneList(); len(got) != 2 || got[1] != "写日志" {
		t.Errorf("后面的步骤应继续执行，got=%v", got)
	}
}

func TestExecute_失败的弱依赖也要回滚(t *testing.T) {
	// 它可能已经产生了副作用，只是后面没走下去而已
	withConfig(t, nil)
	d := &data{}
	New("下单", ok("扣券"), failing("发通知", Weak), failing("扣款", Strong)).
		Execute(context.Background(), d)

	got := d.backList()
	if len(got) != 2 || got[0] != "发通知" || got[1] != "扣券" {
		t.Errorf("失败的弱依赖也该纳入回滚范围，got=%v", got)
	}
}

func TestExecute_ctx取消后不再启动新步骤但照常回滚(t *testing.T) {
	withConfig(t, nil)
	d := &data{}
	ctx, cancel := context.WithCancel(context.Background())

	cancelStep := &step{name: "取消", dep: Strong}
	res := New("下单", ok("扣券"), &cancelHook{step: cancelStep, cancel: cancel}, ok("扣款")).
		Execute(ctx, d)

	if res.Success() {
		t.Fatal("ctx 取消后流程应当失败")
	}
	if !errors.Is(res.Err, context.Canceled) {
		t.Errorf("错误里应能看出是取消，got=%v", res.Err)
	}
	if got := d.doneList(); len(got) != 2 {
		t.Errorf("取消之后不该再启动新步骤，got=%v", got)
	}
	if got := d.backList(); len(got) != 2 {
		t.Errorf("已完成的部分照常回滚，got=%v", got)
	}
}

// cancelHook 执行时顺手取消 ctx
type cancelHook struct {
	*step
	cancel context.CancelFunc
}

func (c *cancelHook) Process(ctx context.Context, d *data) error {
	err := c.step.Process(ctx, d)
	c.cancel()
	return err
}

func TestRollback_用剥了取消的ctx(t *testing.T) {
	// 补偿逻辑最需要执行的时机恰恰是请求超时之后。
	// 沿用已取消的 ctx，每个补偿调用一进去就被拒绝，资源就真的漏掉了
	withConfig(t, nil)
	d := &data{}

	var rbCtxErr error
	checker := &ctxChecker{step: &step{name: "检查", dep: Strong}, got: &rbCtxErr}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 一开始就取消

	New("下单", checker, ok("后面这步不会执行")).Execute(ctx, d)

	if rbCtxErr != nil {
		t.Errorf("回滚用的 ctx 不该是已取消的，got=%v", rbCtxErr)
	}
}

// ctxChecker 在回滚时记下 ctx 的状态
type ctxChecker struct {
	*step
	got *error
}

func (c *ctxChecker) Rollback(ctx context.Context, d *data) error {
	*c.got = ctx.Err()
	return c.step.Rollback(ctx, d)
}

func TestRollback_ctx里的值保留(t *testing.T) {
	// 剥的是取消和超时，不是 value：补偿逻辑常常要用到 ctx 里的租户、链路标识
	withConfig(t, nil)
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "租户A")

	var seen any
	checker := &valueChecker{step: &step{name: "检查", dep: Strong}, key: key{}, got: &seen}
	New("下单", checker, failing("扣款", Strong)).Execute(ctx, &data{})

	if seen != "租户A" {
		t.Errorf("回滚时应还能读到 ctx 里的值，got=%v", seen)
	}
}

type valueChecker struct {
	*step
	key any
	got *any
}

func (c *valueChecker) Rollback(ctx context.Context, d *data) error {
	*c.got = ctx.Value(c.key)
	return c.step.Rollback(ctx, d)
}

func TestRollback_预算耗尽时把剩下的记下来(t *testing.T) {
	// 调用方得知道还有哪些资源悬着，否则只能人工翻日志猜
	withConfig(t, func(c *Config) { c.RollbackTimeout = 30 * time.Millisecond })
	d := &data{}

	slow := &step{name: "慢补偿", dep: Strong, rbDelay: 60 * time.Millisecond}
	res := New("下单", ok("第一步"), ok("第二步"), slow, failing("扣款", Strong)).
		Execute(context.Background(), d)

	if len(res.RollbackErrors) == 0 {
		t.Fatal("预算耗尽时应把没补偿的记下来")
	}
	var names []string
	for _, e := range res.RollbackErrors {
		names = append(names, e.Processor)
	}
	if !strings.Contains(strings.Join(names, ","), "第一步") {
		t.Errorf("没轮到的步骤也该记下来，got=%v", names)
	}
	if !strings.Contains(res.String(), "failed to roll back") {
		t.Errorf("结果摘要里该看得出回滚没做完，got=%s", res.String())
	}
}

func TestRollback_一步失败不拦住其余步骤(t *testing.T) {
	withConfig(t, nil)
	d := &data{}
	bad := &step{name: "补偿会失败", dep: Strong, rbErr: errors.New("补偿失败")}

	res := New("下单", ok("第一步"), bad, failing("扣款", Strong)).Execute(context.Background(), d)

	if len(res.RollbackErrors) != 1 {
		t.Errorf("应记下那一步的回滚错误，got=%v", res.RollbackErrors)
	}
	// 两步的补偿都跑过了：一步失败不该拦住另一步
	if got := d.backList(); len(got) != 2 {
		t.Errorf("其余步骤的回滚应照常进行，got=%v", got)
	}
}

func TestExecute_步骤panic变成普通失败(t *testing.T) {
	// 一步炸了不该把整个进程打穿，前面几步还得有机会回滚
	withConfig(t, nil)
	d := &data{}
	res := New("下单", ok("扣券"), &step{name: "炸", dep: Strong, panics: true}).
		Execute(context.Background(), d)

	if res.Success() {
		t.Fatal("panic 应当变成流程失败")
	}
	if !strings.Contains(res.Err.Error(), "panic") {
		t.Errorf("错误里应看得出是 panic，got=%v", res.Err)
	}
	if got := d.backList(); len(got) != 1 || got[0] != "扣券" {
		t.Errorf("前面的步骤仍应回滚，got=%v", got)
	}
}

func TestRollback_panic不拦住其余补偿(t *testing.T) {
	withConfig(t, nil)
	d := &data{}
	res := New("下单",
		ok("第一步"),
		&step{name: "补偿会炸", dep: Strong, rbPanics: true},
		failing("扣款", Strong),
	).Execute(context.Background(), d)

	if len(res.RollbackErrors) != 1 {
		t.Errorf("应记下那一步的 panic，got=%v", res.RollbackErrors)
	}
	if got := d.backList(); len(got) != 1 || got[0] != "第一步" {
		t.Errorf("其余补偿应照常进行，got=%v", got)
	}
}

func TestNew_nil步骤直接panic(t *testing.T) {
	// 它只会在执行到那一步时炸成空指针，那时错误早已脱离构建现场
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("传 nil 步骤应当 panic")
		}
		if !strings.Contains(fmt.Sprint(r), "step 1") {
			t.Errorf("应指出是第几步，got=%v", r)
		}
	}()
	New[*data]("下单", ok("第一步"), nil)
}

func TestFlow_可以并发执行(t *testing.T) {
	// 构建之后字段不再变化
	withConfig(t, nil)
	flow := New("下单", ok("扣券"), weak("发通知"), ok("扣款"))

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if res := flow.Execute(context.Background(), &data{}); !res.Success() {
				t.Errorf("并发执行应当都成功：%v", res)
			}
		}()
	}
	wg.Wait()
}

func TestExecute_nil_ctx不炸(t *testing.T) {
	withConfig(t, nil)
	//nolint:staticcheck // 故意传 nil
	if res := New("下单", ok("第一步")).Execute(nil, &data{}); !res.Success() {
		t.Errorf("nil ctx 应当兜住，got=%v", res)
	}
}

func TestDependencyString(t *testing.T) {
	if Strong.String() != "strong" || Weak.String() != "weak" || Dependency(9).String() != "unknown" {
		t.Error("依赖类型的文案不对")
	}
}

func TestResultString(t *testing.T) {
	for _, c := range []struct {
		res  *Result
		want string
	}{
		{&Result{}, "flow succeeded"},
		{&Result{Skipped: []*StepError{{}}}, "1 weak step(s) skipped"},
		{&Result{Err: errors.New("炸了")}, "flow failed"},
		{&Result{Err: errors.New("炸了"), Rolled: true}, "rolled back"},
		{&Result{Err: errors.New("炸了"), Rolled: true, RollbackErrors: []*StepError{{}}}, "1 step(s) failed to roll back"},
	} {
		if got := c.res.String(); !strings.Contains(got, c.want) {
			t.Errorf("摘要里应含 %q，got=%q", c.want, got)
		}
	}
}

func TestStepError(t *testing.T) {
	cause := errors.New("根因")
	e := &StepError{Processor: "扣款", Dependency: Strong, Err: cause}
	if !strings.Contains(e.Error(), "扣款") || !strings.Contains(e.Error(), "strong") {
		t.Errorf("错误信息应带上步骤名和依赖类型，got=%s", e.Error())
	}
	if !errors.Is(e, cause) {
		t.Error("应当能 errors.Is 出根因")
	}
}

func TestFlowName(t *testing.T) {
	if got := New[*data]("下单").Name(); got != "下单" {
		t.Errorf("Name 不对，got=%q", got)
	}
}

// ---- 配置与登记 ----

func TestConfig_从文件加载(t *testing.T) {
	withConfig(t, nil)
	path := filepath.Join(t.TempDir(), "application.yml")
	os.WriteFile(path, []byte("XFlow:\n  Monitor: false\n  RollbackTimeout: 5s\n"), 0o644)

	if err := config.Load(path, []registry.Component{{Key: ConfigKey, Config: &cfg}}); err != nil {
		t.Fatal(err)
	}
	if cfg.Monitor || cfg.RollbackTimeout != 5*time.Second {
		t.Errorf("配置没生效，got=%+v", cfg)
	}
}

func TestValidate_回滚预算为零要失败(t *testing.T) {
	// 配成 0 会让每次回滚一进去就判超时、所有补偿被跳过，
	// 而流程本身看起来一切正常——这种配错必须在启动时拦住
	c := DefaultConfig()
	c.RollbackTimeout = 0
	if err := c.validate(); err == nil {
		t.Fatal("回滚预算为 0 应当报错")
	}
	if err := DefaultConfig().validate(); err != nil {
		t.Errorf("默认配置应当合法：%v", err)
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
	if got.Config != &cfg {
		t.Error("登记的必须是包级配置变量本身")
	}
	if got.Init == nil {
		t.Fatal("应当登记 Init 用于启动时校验")
	}

	withConfig(t, nil)
	closer, err := got.Init(context.Background())
	if err != nil {
		t.Errorf("默认配置不该校验失败：%v", err)
	}
	if closer != nil {
		t.Error("本模块没有要关的资源，不该返回 Closer")
	}

	cfg.RollbackTimeout = 0
	if _, err := got.Init(context.Background()); err == nil {
		t.Error("非法配置应当让启动失败")
	}
}

// panicMonitor 每个回调都炸，用来确认监控实现的故障不会打断业务流程
type panicMonitor struct{}

func (panicMonitor) OnStep(context.Context, *StepEvent) { panic("监控的 OnStep 炸了") }
func (panicMonitor) OnFlow(context.Context, *FlowEvent) { panic("监控的 OnFlow 炸了") }

func TestMonitor_回调panic被隔离(t *testing.T) {
	// 观测出问题只该丢一次观测，不该把业务流程打断。
	// 隔离是用 defer recoverNotify() 做的——recover 必须由被 defer 的那个
	// 函数直接调用才生效，包一层就失效了，所以这条要钉住
	withConfig(t, nil)
	SetMonitor(panicMonitor{})
	t.Cleanup(func() { SetMonitor(slogMonitor{}) })

	d := &data{}
	res := New("下单", ok("扣券"), ok("扣款")).Execute(context.Background(), d)

	if !res.Success() {
		t.Fatalf("监控炸了不该让流程失败：%v", res)
	}
	if got := d.doneList(); len(got) != 2 {
		t.Errorf("每一步都该照常执行，got=%v", got)
	}
}

func TestMonitor_回滚时回调panic也被隔离(t *testing.T) {
	withConfig(t, nil)
	SetMonitor(panicMonitor{})
	t.Cleanup(func() { SetMonitor(slogMonitor{}) })

	d := &data{}
	res := New("下单", ok("扣券"), failing("扣款", Strong)).Execute(context.Background(), d)

	if res.Success() {
		t.Fatal("强依赖失败时流程应当失败")
	}
	if got := d.backList(); len(got) != 1 || got[0] != "扣券" {
		t.Errorf("监控炸了不该拦住回滚，got=%v", got)
	}
}

func TestSlogMonitor_级别调到debug后逐步日志还在(t *testing.T) {
	// 成功的步骤记 debug，而默认级别是 info，所以那一行平时不拼也不写。
	// 但「需要逐步排查时把级别调到 debug」是这个设计给出的承诺——
	// 省开销的那个提前返回不能顺手把承诺也省掉
	withConfig(t, nil)
	var buf strings.Builder
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	New("下单", ok("扣券"), ok("扣款")).Execute(context.Background(), &data{})

	got := buf.String()
	for _, want := range []string{"xflow step process done", "扣券", "扣款"} {
		if !strings.Contains(got, want) {
			t.Errorf("debug 级别下该看得到每一步，缺 %q\n实际=\n%s", want, got)
		}
	}
}

func TestSlogMonitor_默认级别下不写逐步日志(t *testing.T) {
	// 一个五步的流程每次执行会产出六行，默认级别下全打出来日志里就只剩流程编排了
	withConfig(t, nil)
	var buf strings.Builder
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(old) })

	New("下单", ok("扣券")).Execute(context.Background(), &data{})

	if strings.Contains(buf.String(), "xflow step") {
		t.Errorf("默认级别下不该有逐步日志\n实际=\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "xflow flow done") {
		t.Errorf("流程结果任何时候都该看得到\n实际=\n%s", buf.String())
	}
}

// cancelingStep 在自己执行期间取消父 ctx，并把取消如实返回
type cancelingStep struct {
	name   string
	dep    Dependency
	cancel context.CancelFunc
}

func (s *cancelingStep) Name() string           { return s.name }
func (s *cancelingStep) Dependency() Dependency { return s.dep }
func (s *cancelingStep) Process(ctx context.Context, d *data) error {
	s.cancel()
	return ctx.Err()
}
func (s *cancelingStep) Rollback(_ context.Context, d *data) error {
	d.record(&d.back, s.name)
	return nil
}

func TestExecute_最后一步被取消不能报成功(t *testing.T) {
	// 取消只在每步开始前查一次的话，最后一步撞上取消就查不到了：
	// 它的 context.Canceled 走进「弱依赖跳过」分支，循环随即结束，
	// 于是一个被取消的流程报成了 Success，还一步都没回滚
	withConfig(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	d := &data{}

	res := New("下单", ok("扣券"), &cancelingStep{name: "发通知", dep: Weak, cancel: cancel}).
		Execute(ctx, d)

	if res.Success() {
		t.Fatalf("流程被取消了，不该报成功：%v", res)
	}
	if !errors.Is(res.Err, context.Canceled) {
		t.Errorf("该如实说是被取消，got=%v", res.Err)
	}
	if !res.Rolled {
		t.Error("取消之后已执行的步骤要回滚")
	}
	if got := d.backList(); len(got) != 2 {
		t.Errorf("失败的弱依赖也在回滚范围内，got=%v", got)
	}
}

func TestExecute_中间一步弱依赖被取消也中断(t *testing.T) {
	withConfig(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	d := &data{}

	res := New("下单",
		ok("扣券"),
		&cancelingStep{name: "发通知", dep: Weak, cancel: cancel},
		ok("不该跑到"),
	).Execute(ctx, d)

	if res.Success() {
		t.Fatal("流程被取消了，不该报成功")
	}
	if got := d.doneList(); strings.Contains(strings.Join(got, ","), "不该跑到") {
		t.Errorf("取消之后不该再往下走，got=%v", got)
	}
}
