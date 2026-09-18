package xutil

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileExist(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	os.WriteFile(f, []byte("x"), 0o600)

	if !FileExist(f) {
		t.Error("存在的文件应返回 true")
	}
	if FileExist(dir) {
		t.Error("目录不是文件，应返回 false")
	}
	if FileExist(filepath.Join(dir, "nope")) {
		t.Error("不存在的路径应返回 false")
	}
}

func TestDirExist(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	os.WriteFile(f, []byte("x"), 0o600)

	if !DirExist(dir) {
		t.Error("存在的目录应返回 true")
	}
	if DirExist(f) {
		t.Error("文件不是目录，应返回 false")
	}
}

func TestToPtr(t *testing.T) {
	p := ToPtr(42)
	if p == nil || *p != 42 {
		t.Fatalf("ToPtr(42) 应指向 42，got=%v", p)
	}
}

func TestGetOrDefault(t *testing.T) {
	cases := []struct{ v, def, want string }{
		{"", "d", "d"},
		{"v", "d", "v"},
	}
	for _, c := range cases {
		if got := GetOrDefault(c.v, c.def); got != c.want {
			t.Errorf("GetOrDefault(%q,%q)=%q want %q", c.v, c.def, got, c.want)
		}
	}
	if got := GetOrDefault(0, 5); got != 5 {
		t.Errorf("零值应返回默认值，got=%d", got)
	}
}

func TestRetry_首次成功不重试(t *testing.T) {
	n := 0
	err := Retry(3, time.Second, time.Millisecond, func(context.Context) error {
		n++
		return nil
	})
	if err != nil || n != 1 {
		t.Errorf("首次成功就该返回，n=%d err=%v", n, err)
	}
}

func TestRetry_失败后重试(t *testing.T) {
	n := 0
	err := Retry(3, time.Second, time.Millisecond, func(context.Context) error {
		n++
		if n < 3 {
			return errors.New("还不行")
		}
		return nil
	})
	if err != nil || n != 3 {
		t.Errorf("应重试到成功，n=%d err=%v", n, err)
	}
}

func TestRetry_耗尽后返回最后一次的错误(t *testing.T) {
	last := errors.New("第三次也不行")
	n := 0
	err := Retry(3, time.Second, time.Millisecond, func(context.Context) error {
		n++
		if n == 3 {
			return last
		}
		return errors.New("不行")
	})
	if !errors.Is(err, last) {
		t.Errorf("应返回最后一次的错误，got=%v", err)
	}
	if n != 3 {
		t.Errorf("应当尝试 3 次，got=%d", n)
	}
}

func TestRetry_每次单独限时(t *testing.T) {
	// 一次卡住不该把整轮预算吃光
	var deadlines int
	err := Retry(3, 20*time.Millisecond, time.Millisecond, func(ctx context.Context) error {
		<-ctx.Done()
		deadlines++
		return ctx.Err()
	})
	if err == nil {
		t.Fatal("每次都超时应当返回错误")
	}
	if deadlines != 3 {
		t.Errorf("每次都该有自己的 deadline，got=%d 次", deadlines)
	}
}

func TestRetry_总预算兜住整轮(t *testing.T) {
	// 带总预算是为了让启动期收到的退出信号能及时生效：
	// 不可中断的重试会让进程必须等满整轮才肯退出
	start := time.Now()
	Retry(5, 10*time.Millisecond, 10*time.Millisecond, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	// 预算 = 5×10ms + 4×10ms = 90ms，宽松一点留出调度余量
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("整轮应在总预算内结束，用了 %v", elapsed)
	}
}

func TestRetry_次数小于一也至少跑一次(t *testing.T) {
	n := 0
	Retry(0, time.Second, time.Millisecond, func(context.Context) error { n++; return nil })
	if n != 1 {
		t.Errorf("至少该跑一次，got=%d", n)
	}
}
