package xlog

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xiaoshicae/xtow/registry"
)

// fileCfg 造一份只写文件的配置，返回配置和日志文件路径
func fileCfg(t *testing.T) (Config, string) {
	t.Helper()
	dir := t.TempDir()
	c := DefaultConfig()
	c.Console = false
	c.File = FileConfig{Enable: true, Path: dir, Name: "app.log",
		RotateTime: 24 * time.Hour, MaxAge: 7 * 24 * time.Hour, Perm: "0644"}
	return c, filepath.Join(dir, "app.log")
}

// readLines 读日志文件里的每一行 JSON
func readLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读日志文件失败: %v", err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("日志不是合法 JSON: %q", line)
		}
		out = append(out, m)
	}
	return out
}

func TestNew_级别解析(t *testing.T) {
	for _, s := range []string{"debug", "info", "warn", "warning", "error", "INFO", " info ", ""} {
		c := DefaultConfig()
		c.Console = false
		c.Level = s
		if _, _, err := New(c); err != nil {
			t.Errorf("级别 %q 应被接受，got=%v", s, err)
		}
	}
}

func TestNew_级别写错当场报错(t *testing.T) {
	c := DefaultConfig()
	c.Level = "verbose"
	_, _, err := New(c)
	if err == nil {
		t.Fatal("不认识的级别应该报错，而不是悄悄退回 info")
	}
	if !strings.Contains(err.Error(), "verbose") {
		t.Errorf("错误里应回显写错的值，got=%v", err)
	}
}

func TestNew_格式写错当场报错(t *testing.T) {
	c := DefaultConfig()
	c.Format = "xml"
	if _, _, err := New(c); err == nil {
		t.Fatal("不认识的格式应该报错")
	}
}

func TestNew_失败时不留下已经打开的日志文件(t *testing.T) {
	// 格式校验曾经排在打开文件之后：New 返回错误，可日志文件已经建好、
	// fd 也开着，而调用方手上没有 Closer 可关 —— 那个 fd 和它的符号链接
	// 就一直留在那里。配置项应当全部校验完再动文件。
	c, _ := fileCfg(t)
	c.Format = "xml"
	if _, _, err := New(c); err == nil {
		t.Fatal("不认识的格式应该报错")
	}

	entries, err := os.ReadDir(c.File.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) > 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("New 失败不该留下任何文件，got=%v", names)
	}
}

func TestNew_权限写错当场报错(t *testing.T) {
	c, _ := fileCfg(t)
	c.File.Perm = "rw-r--r--"
	if _, _, err := New(c); err == nil {
		t.Fatal("权限格式不对应该报错")
	}
}

func TestNew_全部输出都关掉也能正常工作(t *testing.T) {
	c := DefaultConfig()
	c.Console = false
	c.File.Enable = false

	l, closer, err := New(c)
	if err != nil {
		t.Fatalf("「我就是不要日志」是合理选择，不该让服务起不来: %v", err)
	}
	l.Info("这条会被丢弃")
	if err := closer.Close(); err != nil {
		t.Errorf("Close 不该出错: %v", err)
	}
}

func TestNew_Closer永不为nil(t *testing.T) {
	c := DefaultConfig()
	c.Console = true
	c.File.Enable = false
	_, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	if closer == nil {
		t.Fatal("即便没有文件输出，Closer 也不该是 nil —— 调用方不必判空")
	}
	_ = closer.Close()
}

func TestNew_写文件并按级别过滤(t *testing.T) {
	c, path := fileCfg(t)
	c.Level = "warn"

	l, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	l.Debug("debug 不该出现")
	l.Info("info 不该出现")
	l.Warn("warn 应该出现")
	l.Error("error 应该出现")
	closer.Close()

	lines := readLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("级别 warn 应只放过 2 条，got=%d: %v", len(lines), lines)
	}
	if lines[0]["level"] != "WARN" || lines[1]["level"] != "ERROR" {
		t.Errorf("放过的应是 WARN 和 ERROR，got=%v", lines)
	}
}

func TestNew_文本格式(t *testing.T) {
	c, path := fileCfg(t)
	c.Format = FormatText

	l, closer, _ := New(c)
	l.Info("你好", "k", "v")
	closer.Close()

	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "k=v") {
		t.Errorf("text 格式应输出 k=v，got=%q", string(b))
	}
}

// ---- ctx 作用域 ----

func TestScope_字段进日志(t *testing.T) {
	c, path := fileCfg(t)
	l, closer, _ := New(c)

	ctx := CtxWithScope(context.Background())
	AddKV(ctx, "user_id", 42)
	AddKVs(ctx, map[string]any{"req_id": "abc", "path": "/api"})
	l.InfoContext(ctx, "处理完成")
	closer.Close()

	got := readLines(t, path)[0]
	if got["user_id"] != float64(42) || got["req_id"] != "abc" || got["path"] != "/api" {
		t.Errorf("作用域里的字段都应出现在日志里，got=%v", got)
	}
}

func TestScope_深层调用栈写入对持有者可见(t *testing.T) {
	// 这是「存指针而不是存值」的意义：业务函数在调用栈深处拿不到 *gin.Context，
	// 没机会把新 context 回传，但它写的字段必须能被入口处的访问日志看到
	c, path := fileCfg(t)
	l, closer, _ := New(c)

	ctx := CtxWithScope(context.Background())
	func(ctx context.Context) { // 深层业务函数，只拿到 ctx
		AddKV(ctx, "从深处写的", true)
	}(ctx)
	l.InfoContext(ctx, "入口处的访问日志") // 入口处用的还是原来那个 ctx
	closer.Close()

	if got := readLines(t, path)[0]; got["从深处写的"] != true {
		t.Errorf("深层写入应对持有同一 ctx 的地方可见，got=%v", got)
	}
}

func TestScope_重复开启不清空已有字段(t *testing.T) {
	ctx := CtxWithScope(context.Background())
	AddKV(ctx, "a", 1)

	ctx2 := CtxWithScope(ctx) // 比如中间件被注册了两次
	AddKV(ctx2, "b", 2)

	n := 0
	scopeFrom(ctx).each(func(k string, v any) { n++ })
	if n != 2 {
		t.Errorf("重复开启应幂等，两个字段都在，got=%d", n)
	}
}

func TestScope_没有作用域时丢弃并计数(t *testing.T) {
	before := DroppedKVCount()
	AddKV(context.Background(), "k", "v")
	AddKVs(context.Background(), map[string]any{"a": 1, "b": 2})

	if got := DroppedKVCount() - before; got != 3 {
		t.Errorf("没有作用域的写入应被计数，便于排查「字段没出现在日志里」，got=%d want=3", got)
	}
}

func TestScope_nil_ctx不崩(t *testing.T) {
	//lint:ignore SA1012 故意传 nil 验证不 panic
	AddKV(nil, "k", "v")
	if ctx := CtxWithScope(nil); ctx == nil {
		t.Error("CtxWithScope(nil) 应返回一个可用的 context")
	}
}

// ---- trace 提取器 ----

func TestTraceExtractor(t *testing.T) {
	t.Cleanup(func() { SetTraceExtractor(nil) })

	c, path := fileCfg(t)
	l, closer, _ := New(c)

	SetTraceExtractor(func(ctx context.Context) (string, string) {
		return "trace-1", "span-1"
	})
	l.InfoContext(context.Background(), "带链路的日志")
	closer.Close()

	got := readLines(t, path)[0]
	if got["trace_id"] != "trace-1" || got["span_id"] != "span-1" {
		t.Errorf("注入的链路标识应出现在日志里，got=%v", got)
	}
}

func TestTraceExtractor_未注入时不产生字段(t *testing.T) {
	SetTraceExtractor(nil)

	c, path := fileCfg(t)
	l, closer, _ := New(c)
	l.InfoContext(context.Background(), "没有链路")
	closer.Close()

	if got := readLines(t, path)[0]; got["trace_id"] != nil {
		t.Errorf("未注入提取器时不该有 trace 字段，got=%v", got)
	}
}

func TestTraceExtractor_空traceID不产生字段(t *testing.T) {
	t.Cleanup(func() { SetTraceExtractor(nil) })
	SetTraceExtractor(func(context.Context) (string, string) { return "", "" })

	c, path := fileCfg(t)
	l, closer, _ := New(c)
	l.InfoContext(context.Background(), "链路未采样")
	closer.Close()

	if got := readLines(t, path)[0]; got["trace_id"] != nil {
		t.Errorf("提取不到时不该写空字段，got=%v", got)
	}
}

func TestHandler_分组不吞掉ctx字段(t *testing.T) {
	// slog 的 With 走 WithAttrs、WithGroup 会给后续属性加前缀。
	// 用户划的分组是给业务字段用的，trace_id 是整条记录的身份，必须留在顶层。
	t.Cleanup(func() { SetTraceExtractor(nil) })
	SetTraceExtractor(func(context.Context) (string, string) { return "t1", "s1" })

	c, path := fileCfg(t)
	l, closer, _ := New(c)
	ctx := CtxWithScope(context.Background())
	AddKV(ctx, "uid", "u9")
	l.With("固定字段", "x").WithGroup("g").InfoContext(ctx, "msg", "业务字段", "y")
	closer.Close()

	got := readLines(t, path)[0]
	if got["trace_id"] != "t1" || got["span_id"] != "s1" {
		t.Errorf("链路字段应在顶层，got=%v", got)
	}
	if got["uid"] != "u9" {
		t.Errorf("scope 字段应在顶层，got=%v", got)
	}
	if got["固定字段"] != "x" {
		t.Errorf("With 挂的属性在分组之前，应留在顶层，got=%v", got)
	}
	g, ok := got["g"].(map[string]any)
	if !ok || g["业务字段"] != "y" {
		t.Errorf("分组之后的业务字段才该落进分组，got=%v", got)
	}
	if _, dup := g["trace_id"]; dup {
		t.Errorf("链路字段不该同时出现在分组里，got=%v", got)
	}
}

func TestHandler_嵌套分组(t *testing.T) {
	// 重放调用链要保持原顺序，否则嵌套分组会串位
	t.Cleanup(func() { SetTraceExtractor(nil) })
	SetTraceExtractor(func(context.Context) (string, string) { return "t1", "" })

	c, path := fileCfg(t)
	l, closer, _ := New(c)
	l.WithGroup("a").With("内层", 1).WithGroup("b").InfoContext(context.Background(), "msg", "叶子", 2)
	closer.Close()

	got := readLines(t, path)[0]
	if got["trace_id"] != "t1" {
		t.Errorf("链路字段应在顶层，got=%v", got)
	}
	a, ok := got["a"].(map[string]any)
	if !ok {
		t.Fatalf("缺少 a 分组，got=%v", got)
	}
	if a["内层"] != float64(1) {
		t.Errorf("a.内层 应为 1，got=%v", got)
	}
	b, ok := a["b"].(map[string]any)
	if !ok || b["叶子"] != float64(2) {
		t.Errorf("a.b.叶子 应为 2，got=%v", got)
	}
}

func TestHandler_无分组时走快路径(t *testing.T) {
	// 没开过分组时属性本来就在顶层，不该退化成每条记录重建 handler
	h := newCtxHandler(slog.NewJSONHandler(io.Discard, nil))
	w := h.WithAttrs([]slog.Attr{slog.String("a", "1")}).(*ctxHandler)
	if w.grouped {
		t.Error("只调用 WithAttrs 不该触发重放")
	}
	if same := h.WithAttrs(nil); same != slog.Handler(h) {
		t.Error("空属性应原样返回")
	}
	if same := h.WithGroup(""); same != slog.Handler(h) {
		t.Error("空分组名应原样返回")
	}
}

func TestDefaultConfig(t *testing.T) {
	c := DefaultConfig()
	if c.Level != "info" || c.Format != FormatJSON {
		t.Errorf("默认应为 info + json，got=%+v", c)
	}
	if !c.Console {
		t.Error("控制台默认应开启")
	}
	if c.File.Enable {
		t.Error("文件输出默认应关闭")
	}
	if c.File.RotateTime != 24*time.Hour || c.File.MaxAge != 7*24*time.Hour {
		t.Errorf("轮转默认应为一天、保留七天，got=%+v", c.File)
	}
}

func TestHandler_兄弟派生互不影响(t *testing.T) {
	// 两个分支从同一个父 logger 派生，调用链不能共享底层数组，
	// 否则后派生的会把先派生的最后一节覆盖掉
	t.Cleanup(func() { SetTraceExtractor(nil) })
	SetTraceExtractor(func(context.Context) (string, string) { return "t1", "" })

	c, path := fileCfg(t)
	l, closer, _ := New(c)
	parent := l.With("共同", "p")
	a := parent.WithGroup("a")
	b := parent.WithGroup("b")
	a.InfoContext(context.Background(), "msg", "x", 1)
	b.InfoContext(context.Background(), "msg", "x", 2)
	closer.Close()

	lines := readLines(t, path)
	for i, want := range []string{"a", "b"} {
		got := lines[i]
		if got["共同"] != "p" || got["trace_id"] != "t1" {
			t.Errorf("第 %d 行顶层字段不对，got=%v", i, got)
		}
		g, ok := got[want].(map[string]any)
		if !ok || g["x"] != float64(i+1) {
			t.Errorf("第 %d 行应落进 %s 分组，got=%v", i, want, got)
		}
	}
}

func TestRegister_登记内容与框架对得上(t *testing.T) {
	// 这是 xlog 和框架之间唯一的一根线：key 写错、stage 挂错，
	// 表现是「配置不生效」或「日志比别的组件晚就绪」，别处都测不出来
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
	if got.Stage != registry.StageLog {
		t.Errorf("日志必须在 StageLog 就绪，否则后面组件初始化时打的日志会丢，got=%v", got.Stage)
	}
	if got.Config != &cfg {
		t.Error("登记的必须是包级配置变量本身，否则框架解出来的配置写不回来")
	}
	if got.Init == nil {
		t.Fatal("Init 不能为空")
	}

	// 跑一遍真实的初始化：它要把 slog 的全局默认 logger 换掉
	old := slog.Default()
	oldCfg := cfg
	t.Cleanup(func() { slog.SetDefault(old); cfg = oldCfg })

	dir := t.TempDir()
	cfg = DefaultConfig()
	cfg.Console = false
	cfg.File = FileConfig{Enable: true, Path: dir, Name: "app.log", RotateTime: time.Hour, MaxAge: time.Hour}

	closer, err := got.Init()
	if err != nil {
		t.Fatalf("初始化失败：%v", err)
	}
	slog.Default().Info("经全局默认 logger")
	if err := closer.Close(); err != nil {
		t.Errorf("关闭失败：%v", err)
	}

	files, _ := filepath.Glob(filepath.Join(dir, "app.log.*"))
	if len(files) == 0 {
		t.Fatal("初始化后 slog.Default() 应写到配置的文件里")
	}
	if b, _ := os.ReadFile(files[0]); !strings.Contains(string(b), "经全局默认 logger") {
		t.Errorf("文件内容不对，got=%q", b)
	}
}

func TestTraceIDs(t *testing.T) {
	// 给不想依赖 OpenTelemetry、又需要链路标识的包用（xmetric 拿它做 exemplar）
	t.Cleanup(func() { SetTraceExtractor(nil) })

	if id, sp := TraceIDs(context.Background()); id != "" || sp != "" {
		t.Errorf("没注入提取器时应返回空，got=%q %q", id, sp)
	}
	SetTraceExtractor(func(context.Context) (string, string) { return "t1", "s1" })
	if id, sp := TraceIDs(context.Background()); id != "t1" || sp != "s1" {
		t.Errorf("应返回提取器给的值，got=%q %q", id, sp)
	}
	if id, sp := TraceIDs(nil); id != "" || sp != "" { //nolint:staticcheck // 故意传 nil
		t.Errorf("nil ctx 不该 panic，got=%q %q", id, sp)
	}
}

func TestAddObserver(t *testing.T) {
	// xmetric 靠它统计错误日志，而不必反过来让 xlog 认识 Prometheus
	old := observers.Load()
	t.Cleanup(func() { observers.Store(old) })
	observers.Store(nil)

	var seen []string
	AddObserver(func(_ context.Context, r slog.Record) { seen = append(seen, r.Level.String()+":"+r.Message) })
	AddObserver(nil) // 忽略，不该炸

	c, _ := fileCfg(t)
	c.Level = "warn"
	l, closer, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	l.Info("被级别挡掉")
	l.Warn("警告")
	l.Error("错误")
	closer.Close()

	want := []string{"WARN:警告", "ERROR:错误"}
	if !reflect.DeepEqual(seen, want) {
		t.Errorf("观察者只该收到实际写出的日志，got=%v want=%v", seen, want)
	}
}

func TestAddObserver_panic不打断日志(t *testing.T) {
	old := observers.Load()
	t.Cleanup(func() { observers.Store(old) })
	observers.Store(nil)

	AddObserver(func(context.Context, slog.Record) { panic("炸了") })
	var reached bool
	AddObserver(func(context.Context, slog.Record) { reached = true })

	c, path := fileCfg(t)
	l, closer, _ := New(c)
	l.Error("出事了")
	closer.Close()

	if !reached {
		t.Error("一个观察者 panic 不该影响后面的观察者")
	}
	if got := readLines(t, path)[0]; got["msg"] != "出事了" {
		t.Errorf("观察者 panic 了，日志本身还得写出去，got=%v", got)
	}
}
