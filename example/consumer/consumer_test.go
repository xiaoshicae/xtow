package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xiaoshicae/xtow"
	"github.com/xiaoshicae/xtow/registry"
)

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func testSettings(workers int, timeout time.Duration) func() Settings {
	return func() Settings { return Settings{Workers: workers, Timeout: timeout} }
}

func TestConsumer_在途消息做完之后Start才返回(t *testing.T) {
	// 框架是在 Start 返回之后才关数据库和缓存的。提前返回的话，
	// 还在处理的消息会摸到已经关掉的连接池
	q := newMemQueue(4)
	q.publish("m1", nil)

	entered := make(chan struct{})
	var finished atomic.Bool

	c := &Consumer{
		q:    q,
		conf: testSettings(1, time.Second),
		handle: func(context.Context, Message) error {
			close(entered)
			time.Sleep(200 * time.Millisecond)
			finished.Store(true)
			return nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Start(ctx) }()

	<-entered
	cancel() // 退出信号

	select {
	case <-done:
		if !finished.Load() {
			t.Error("Start 在消息还没处理完的时候就返回了")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Start 没返回")
	}
}

func TestConsumer_在途消息拿到的ctx没有被取消(t *testing.T) {
	// 沿用已取消的 ctx 的话，这条消息里每一次写库、每一次调下游、
	// 连最后那次 Ack 都会一进去就被拒绝——消息没做完，队列也没收到确认
	q := newMemQueue(4)
	q.publish("m1", nil)

	entered := make(chan struct{})
	var errAfterCancel error

	c := &Consumer{
		q:    q,
		conf: testSettings(1, time.Second),
		handle: func(ctx context.Context, _ Message) error {
			close(entered)
			time.Sleep(150 * time.Millisecond) // 这中间退出信号到了
			errAfterCancel = ctx.Err()         // 处理逻辑此刻看到的 ctx 状态
			return nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Start(ctx) }()

	<-entered
	cancel()
	<-done

	if errAfterCancel != nil {
		t.Errorf("在途消息的 ctx 被取消了（%v），这条消息的写库和 Ack 都会当场失败", errAfterCancel)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.acked) != 1 {
		t.Errorf("在途消息该被 Ack，acked=%v nacked=%v", q.acked, q.nacked)
	}
}

func TestConsumer_Stop不关队列(t *testing.T) {
	// 框架先调 Stop、再等 Start 返回，此刻 worker 还在处理在途消息。
	// 多数客户端 Close 时会顺带提交 offset，提前提交等于把还没处理完的
	// 消息标记成已消费——退出时静默丢消息
	q := newMemQueue(4)
	c := &Consumer{q: q, conf: testSettings(1, time.Second),
		handle: func(context.Context, Message) error { return nil }}

	if err := c.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if q.closed.Load() {
		t.Error("Stop 把队列关了，而此时 worker 可能还在处理在途消息")
	}
}

func TestConsumer_一条毒消息不会拖垮worker(t *testing.T) {
	q := newMemQueue(8)
	q.publish("bad", nil)
	q.publish("good", nil)

	var handled atomic.Int32
	c := &Consumer{
		q:    q,
		conf: testSettings(1, time.Second),
		handle: func(_ context.Context, m Message) error {
			handled.Add(1)
			if m.ID == "bad" {
				panic("deliberate panic in a message handler")
			}
			return nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	slog.SetDefault(quiet())
	_ = c.Start(ctx)

	if handled.Load() != 2 {
		t.Errorf("毒消息之后 worker 该继续消费，实际只处理了 %d 条", handled.Load())
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.nacked) != 1 || q.nacked[0] != "bad" {
		t.Errorf("panic 的那条该被 Nack，nacked=%v", q.nacked)
	}
	if len(q.acked) != 1 || q.acked[0] != "good" {
		t.Errorf("正常的那条该被 Ack，acked=%v", q.acked)
	}
}

func TestConsumer_失败的消息被Nack(t *testing.T) {
	q := newMemQueue(4)
	q.publish("m1", nil)

	c := &Consumer{q: q, conf: testSettings(1, time.Second),
		handle: func(context.Context, Message) error { return errors.New("downstream is down") }}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	slog.SetDefault(quiet())
	_ = c.Start(ctx)

	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.nacked) != 1 {
		t.Errorf("处理失败的消息该被 Nack，nacked=%v acked=%v", q.nacked, q.acked)
	}
}

// TestRun_组件在消费者收工之后才关 是这个目录里唯一走完整框架的测试：
// 上面几条证明 Consumer 自己的行为，这一条证明它和 xtow.Run 接得上
func TestRun_组件在消费者收工之后才关(t *testing.T) {
	q := newMemQueue(4)
	q.publish("m1", nil)

	var mu sync.Mutex
	var closedAt, drainedAt time.Time

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

	// registry.Register 是公开的：业务自己的组件就是这样接进框架的
	registry.Register(comp)

	var once sync.Once
	entered := make(chan struct{})
	c := &Consumer{
		q:    q,
		conf: testSettings(1, time.Second),
		handle: func(context.Context, Message) error {
			once.Do(func() { close(entered) })
			time.Sleep(150 * time.Millisecond)
			mu.Lock()
			defer mu.Unlock()
			drainedAt = time.Now()
			return nil
		},
	}

	go func() {
		<-entered
		p, _ := os.FindProcess(os.Getpid())
		_ = p.Signal(os.Interrupt)
	}()

	if err := xtow.Run(c, xtow.WithLogger(quiet())); err != nil {
		t.Fatalf("Run 失败：%v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if drainedAt.IsZero() || closedAt.IsZero() {
		t.Fatal("两边都该跑到")
	}
	if closedAt.Before(drainedAt) {
		t.Errorf("组件在消息还没处理完的时候就被关了：关闭=%v 消息处理完=%v", closedAt, drainedAt)
	}
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }
