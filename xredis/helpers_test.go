package xredis

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

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

func TestSettle_能看见泄漏的协程(t *testing.T) {
	// 先验证这把尺子是准的——一条永远不会失败的测试比没有测试更糟，
	// 它让人以为查过了
	before := stabilize()
	stop := make(chan struct{})
	for i := 0; i < 5; i++ {
		go func() { <-stop }()
	}
	if during := settleTo(before); during <= before {
		t.Fatalf("漏了 5 个协程却没看出增长（%d -> %d），这把尺子是坏的", before, during)
	}
	close(stop)
	if after := settleTo(before); after > before+1 {
		t.Errorf("协程退出后应当回落，got %d -> %d", before, after)
	}
}

// TestMain 调短重试间隔，并把 go-redis 的内部日志接到丢弃里。
// 连不上的用例本来就会刷一屏「failed to dial」，那是预期内的噪声。
func TestMain(m *testing.M) {
	pingInterval = 10 * time.Millisecond
	redis.SetLogger(discardLogger{})
	os.Exit(m.Run())
}

type discardLogger struct{}

func (discardLogger) Printf(context.Context, string, ...any) {}
