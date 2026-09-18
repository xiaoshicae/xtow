package xlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestWriter 造一个时钟可控的轮转写入器
func newTestWriter(t *testing.T, rotate, maxAge time.Duration, now *time.Time) (*rotateWriter, string) {
	t.Helper()
	base := filepath.Join(t.TempDir(), "app.log")
	w, err := newRotateWriter(base, maxAge, rotate, 0)
	if err != nil {
		t.Fatalf("创建轮转写入器失败：%v", err)
	}
	t.Cleanup(func() { w.Close() })
	w.clock = func() time.Time { return *now }
	// 构造时用的是真实时间，重新按测试时钟切一次，保证后续文件名可预期
	if err := w.rotateTo(w.filenameFor(*now)); err != nil {
		t.Fatalf("按测试时钟切换失败：%v", err)
	}
	return w, base
}

func TestRotateLayoutFor(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{24 * time.Hour, rotateLayoutDay},
		{48 * time.Hour, rotateLayoutDay},
		{time.Hour, rotateLayoutHour},
		{6 * time.Hour, rotateLayoutHour},
		{time.Minute, rotateLayoutMinute},
		{0, rotateLayoutMinute},
	}
	for _, c := range cases {
		if got := rotateLayoutFor(c.d); got != c.want {
			t.Errorf("周期 %v 应选 %s，got=%s", c.d, c.want, got)
		}
	}
}

func TestRotateWriter_跨周期切文件(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 30, 0, 0, time.Local)
	w, base := newTestWriter(t, time.Minute, 0, &now)

	w.Write([]byte("第一分钟\n"))
	now = now.Add(time.Minute)
	w.Write([]byte("第二分钟\n"))

	first := base + ".202609181030"
	second := base + ".202609181031"
	if got := readFile(t, first); got != "第一分钟\n" {
		t.Errorf("%s 内容不对，got=%q", first, got)
	}
	if got := readFile(t, second); got != "第二分钟\n" {
		t.Errorf("%s 内容不对，got=%q", second, got)
	}
}

func TestRotateWriter_符号链接跟随当前文件(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 30, 0, 0, time.Local)
	w, base := newTestWriter(t, time.Minute, 0, &now)

	if got, err := os.Readlink(base); err != nil || got != "app.log.202609181030" {
		t.Fatalf("链接应指向当前文件，got=%q err=%v", got, err)
	}
	now = now.Add(time.Minute)
	w.Write([]byte("x\n"))
	if got, _ := os.Readlink(base); got != "app.log.202609181031" {
		t.Errorf("轮转后链接应改指新文件，got=%q", got)
	}
	// 用链接名读到的就是新文件的内容
	if got := readFile(t, base); got != "x\n" {
		t.Errorf("经链接读到的内容不对，got=%q", got)
	}
}

func TestRotateWriter_Purge清理过期文件(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 30, 0, 0, time.Local)
	w, base := newTestWriter(t, time.Minute, 5*time.Minute, &now)

	stale := base + ".202609181010" // 20 分钟前，早该清
	fresh := base + ".202609181029" // 上一分钟，还在保留期内
	current := base + ".202609181030"
	for _, p := range []string{stale, fresh} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	w.purge(current)

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("过期文件应被删除：%s", stale)
	}
	for _, p := range []string{fresh, current, base} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("不该删的被删了：%s (%v)", p, err)
		}
	}
}

func TestRotateWriter_Purge不删符号链接(t *testing.T) {
	// 替换链接期间会短暂存在 base.tmp，它匹配 base.* 但不能当历史文件删
	now := time.Date(2026, 9, 18, 10, 30, 0, 0, time.Local)
	w, base := newTestWriter(t, time.Minute, time.Minute, &now)

	tmp := base + ".tmp"
	if err := os.Symlink("app.log.202609181030", tmp); err != nil {
		t.Skipf("当前环境不支持符号链接：%v", err)
	}
	w.purge(base + ".202609181030")

	if _, err := os.Lstat(tmp); err != nil {
		t.Errorf("临时链接不该被删：%v", err)
	}
}

func TestRotateWriter_Purge关闭时不清理(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 30, 0, 0, time.Local)
	w, base := newTestWriter(t, time.Minute, 0, &now) // maxAge <= 0

	stale := base + ".202601010000"
	os.WriteFile(stale, []byte("x"), 0o644)
	w.purge(base + ".202609181030")

	if _, err := os.Stat(stale); err != nil {
		t.Errorf("maxAge<=0 表示不清理，但文件没了：%v", err)
	}
}

func TestRotateWriter_Expired按文件名而非mtime(t *testing.T) {
	// 备份恢复、rsync、镜像分层都会重写 mtime，按它判断会误删或永不清
	now := time.Date(2026, 9, 18, 10, 30, 0, 0, time.Local)
	w, base := newTestWriter(t, time.Minute, 5*time.Minute, &now)

	old := base + ".202609181000"
	os.WriteFile(old, []byte("x"), 0o644)
	os.Chtimes(old, now, now) // mtime 是刚刚，文件名说是半小时前

	w.purge(base + ".202609181030")
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("文件名已过期就该删，不该被新 mtime 救回来")
	}
}

func TestRotateWriter_Expired文件名解析不了时退回mtime(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 30, 0, 0, time.Local)
	w, base := newTestWriter(t, time.Minute, 5*time.Minute, &now)

	junk := base + ".不是时间"
	os.WriteFile(junk, []byte("x"), 0o644)
	os.Chtimes(junk, now.Add(-time.Hour), now.Add(-time.Hour))

	w.purge(base + ".202609181030")
	if _, err := os.Stat(junk); !os.IsNotExist(err) {
		t.Error("文件名解析不了时应按 mtime 判断，这个已经过期了")
	}
}

func TestRotateWriter_Expired边界(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 30, 0, 0, time.Local)
	w, base := newTestWriter(t, time.Minute, 5*time.Minute, &now)
	cutoff := now.Add(-5 * time.Minute) // 10:25

	// 10:24 这一分钟的文件，周期在 10:25 结束，正好等于 cutoff —— 算过期
	if !w.expired(base+".202609181024", nil, cutoff) {
		t.Error("周期终点等于 cutoff 应判为过期")
	}
	// 10:25 这一分钟的文件，周期到 10:26 才结束 —— 不算过期
	if w.expired(base+".202609181025", nil, cutoff) {
		t.Error("周期终点晚于 cutoff 不该判为过期")
	}
}

func TestRotateWriter_轮转失败时降级写旧文件(t *testing.T) {
	// 轮转失败直接返错的话，从此每条日志都失败，而 Close 时才暴露出来，
	// 现象就是日志某天突然断了却没有任何报错
	now := time.Date(2026, 9, 18, 10, 30, 0, 0, time.Local)
	w, base := newTestWriter(t, time.Minute, 0, &now)

	// 在下一周期的文件名上放一个目录，让 OpenFile 失败
	if err := os.Mkdir(base+".202609181031", 0o755); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)

	n, err := w.Write([]byte("降级\n"))
	if err != nil || n == 0 {
		t.Fatalf("轮转失败不该让写入失败，n=%d err=%v", n, err)
	}
	if got := readFile(t, base+".202609181030"); got != "降级\n" {
		t.Errorf("应继续写旧文件，got=%q", got)
	}
}

func TestRotateWriter_轮转失败告警降噪(t *testing.T) {
	// 周期到了之后每条日志都会重试轮转，不限流会把告警刷爆
	now := time.Date(2026, 9, 18, 10, 30, 0, 0, time.Local)
	w, _ := newTestWriter(t, time.Minute, 0, &now)

	w.reportRotateFailure(errFake{})
	first := w.lastRotateErrAt
	if first.IsZero() {
		t.Fatal("首次失败应记录时刻")
	}

	now = now.Add(rotateErrLogInterval / 2)
	w.reportRotateFailure(errFake{})
	if !w.lastRotateErrAt.Equal(first) {
		t.Error("间隔内的重复失败应被压掉")
	}

	now = now.Add(rotateErrLogInterval)
	w.reportRotateFailure(errFake{})
	if w.lastRotateErrAt.Equal(first) {
		t.Error("超过间隔后应重新告警")
	}
}

func TestRotateWriter_Close后拒绝写入(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 30, 0, 0, time.Local)
	w, _ := newTestWriter(t, time.Minute, 0, &now)

	if err := w.Close(); err != nil {
		t.Fatalf("Close 失败：%v", err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("重复 Close 应安全，got=%v", err)
	}
	if _, err := w.Write([]byte("x")); err != os.ErrClosed {
		t.Errorf("关闭后写入应返回 ErrClosed，got=%v", err)
	}
}

func TestRotateWriter_打不开文件时构造失败(t *testing.T) {
	// 权限、路径问题要在初始化阶段暴露，而不是等到第一条日志
	_, err := newRotateWriter(filepath.Join(t.TempDir(), "没有这个目录", "app.log"), 0, time.Hour, 0)
	if err == nil {
		t.Fatal("路径不存在时应构造失败")
	}
	if !strings.Contains(err.Error(), "xlog") {
		t.Errorf("错误应带模块名，got=%v", err)
	}
}

func TestTruncateInLocation(t *testing.T) {
	// 按天轮转必须对齐本地零点。time.Truncate 以 UTC 零点为基准，
	// 在 UTC+8 直接用会把切割点放在本地早上 8 点
	loc := time.FixedZone("UTC+8", 8*3600)
	got := truncateInLocation(time.Date(2026, 9, 18, 3, 15, 0, 0, loc), 24*time.Hour)
	want := time.Date(2026, 9, 18, 0, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Errorf("按天截断应落在本地零点，got=%v want=%v", got, want)
	}

	utc := time.Date(2026, 9, 18, 3, 15, 0, 0, time.UTC)
	if g := truncateInLocation(utc, time.Hour); !g.Equal(time.Date(2026, 9, 18, 3, 0, 0, 0, time.UTC)) {
		t.Errorf("UTC 按小时截断出错，got=%v", g)
	}
	if g := truncateInLocation(utc, 0); !g.Equal(utc) {
		t.Errorf("周期 <=0 应原样返回，got=%v", g)
	}
}

func TestWarnf写到stderr(t *testing.T) {
	// 轮转器是日志系统的底层写入器，自身故障不能再走日志系统，否则成环
	old := os.Stderr
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = f
	warnf("出事了 file=[%s]", "a.log")
	os.Stderr = old
	f.Close()

	if got := readFile(t, f.Name()); !strings.Contains(got, "xlog rotate: 出事了 file=[a.log]") {
		t.Errorf("stderr 内容不对，got=%q", got)
	}
}

type errFake struct{}

func (errFake) Error() string { return "fake" }

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读 %s 失败：%v", p, err)
	}
	return string(b)
}
