package xgorm

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// capture 把 slog 默认 logger 换成写进 buffer 的，返回取解析结果的函数
func capture(t *testing.T) func() []map[string]any {
	t.Helper()
	var buf strings.Builder
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	return func() []map[string]any {
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}
			m := map[string]any{}
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatalf("日志不是 JSON：%v，内容=%q", err, line)
			}
			out = append(out, m)
		}
		return out
	}
}

func traceOnce(l logger.Interface, err error, elapsed time.Duration) {
	begin := time.Now().Add(-elapsed)
	l.Trace(context.Background(), begin, func() (string, int64) {
		return "SELECT * FROM users WHERE id = ?", 1
	}, err)
}

func TestLogger_SQL走结构化字段(t *testing.T) {
	// SQL 里带引号和换行，塞进消息文本会把一行日志撑成好几行，也没法按耗时筛
	lines := capture(t)
	c := DefaultClientConfig()
	traceOnce(newGormLogger(c), nil, time.Millisecond)

	got := lines()
	if len(got) != 1 {
		t.Fatalf("应记一条，got=%v", got)
	}
	if got[0]["sql"] != "SELECT * FROM users WHERE id = ?" {
		t.Errorf("SQL 应是独立字段，got=%v", got[0])
	}
	if got[0]["elapsed"] == nil || got[0]["rows_affected"] != float64(1) {
		t.Errorf("耗时和行数也该是字段，got=%v", got[0])
	}
}

func TestLogger_慢查询记warn(t *testing.T) {
	lines := capture(t)
	c := DefaultClientConfig()
	c.SlowThreshold = 10 * time.Millisecond
	traceOnce(newGormLogger(c), nil, time.Second)

	got := lines()
	if len(got) != 1 || got[0]["level"] != "WARN" {
		t.Fatalf("超过阈值应记 warn，got=%v", got)
	}
	if got[0]["threshold"] == nil {
		t.Errorf("该带上阈值，否则看不出为什么算慢，got=%v", got[0])
	}
}

func TestLogger_出错记error(t *testing.T) {
	lines := capture(t)
	traceOnce(newGormLogger(DefaultClientConfig()), errors.New("连接断了"), time.Millisecond)

	got := lines()
	if len(got) != 1 || got[0]["level"] != "ERROR" {
		t.Fatalf("出错应记 error，got=%v", got)
	}
	if got[0]["error"] != "连接断了" {
		t.Errorf("该带上错误，got=%v", got[0])
	}
}

func TestLogger_没查到记录可以不当错误(t *testing.T) {
	// 「没查到」通常是正常的业务分支，默认还是记下来，配了才忽略
	for _, c := range []struct {
		ignore    bool
		wantLevel any
	}{
		{false, "ERROR"},
		{true, "INFO"}, // 不当错误，退回普通 SQL 日志
	} {
		lines := capture(t)
		cfg := DefaultClientConfig()
		cfg.IgnoreNotFound = c.ignore
		traceOnce(newGormLogger(cfg), gorm.ErrRecordNotFound, time.Millisecond)

		got := lines()
		if len(got) != 1 || got[0]["level"] != c.wantLevel {
			t.Errorf("IgnoreNotFound=%v 时应记 %v，got=%v", c.ignore, c.wantLevel, got)
		}
	}
}

func TestLogger_行数未知时不写字段(t *testing.T) {
	// GORM 用 -1 表示「行数未知」，写成 -1 会被误读成真有 -1 行
	lines := capture(t)
	l := newGormLogger(DefaultClientConfig())
	l.Trace(context.Background(), time.Now(), func() (string, int64) { return "SELECT 1", -1 }, nil)

	got := lines()
	if len(got) != 1 {
		t.Fatalf("应记一条，got=%v", got)
	}
	if _, has := got[0]["rows_affected"]; has {
		t.Errorf("行数未知时不该写这个字段，got=%v", got[0])
	}
}

func TestLogger_Silent时什么都不记(t *testing.T) {
	lines := capture(t)
	l := newGormLogger(DefaultClientConfig()).LogMode(logger.Silent)
	traceOnce(l, errors.New("出错了"), time.Second)

	if got := lines(); len(got) != 0 {
		t.Errorf("Silent 时不该有任何日志，got=%v", got)
	}
}

func TestLogger_LogMode返回副本(t *testing.T) {
	// GORM 的约定：LogMode 返回新实例，不能改共享的那个
	l := newGormLogger(DefaultClientConfig())
	other := l.LogMode(logger.Silent).(*gormLogger)
	if l.level == logger.Silent {
		t.Error("不该改动原实例")
	}
	if other.level != logger.Silent {
		t.Error("副本应带上新级别")
	}
}

func TestLogger_InfoWarnError(t *testing.T) {
	lines := capture(t)
	l := newGormLogger(DefaultClientConfig())
	l.Info(context.Background(), "普通消息")
	l.Warn(context.Background(), "警告 %d", 1)
	l.Error(context.Background(), "error")

	got := lines()
	if len(got) != 3 {
		t.Fatalf("应记三条，got=%v", got)
	}
	if got[1]["msg"] != "警告 1" {
		t.Errorf("有参数时才格式化，got=%v", got[1])
	}
}

func TestMessage_没参数时不当格式串(t *testing.T) {
	// GORM 也会传不带参数的纯文本，消息里的 % 不该被当成占位符
	if got := message("100% 命中", nil); got != "100% 命中" {
		t.Errorf("没有参数时应原样返回，got=%q", got)
	}
	if got := message("命中 %d%%", []any{50}); got != "命中 50%" {
		t.Errorf("有参数时才格式化，got=%q", got)
	}
}
