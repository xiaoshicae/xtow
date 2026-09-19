package xclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

type fake struct{ name string }

type recordCloser struct {
	name   string
	closed *[]string
	err    error
}

func (c *recordCloser) Close() error {
	*c.closed = append(*c.closed, c.name)
	return c.err
}

func TestGet_取不到就panic并列出已有的(t *testing.T) {
	// 名字写错和整块没配是两个不同的问题，列出实际配了哪些，两者一眼可分
	r := NewRegistry[*fake]("xdemo", "XDemo")

	func() {
		defer func() {
			msg := fmt.Sprint(recover())
			if !strings.Contains(msg, "XDemo") {
				t.Errorf("一个都没配时该提示去看配置块，got=%v", msg)
			}
		}()
		r.Get()
	}()

	r.Publish(map[string]*fake{"main": {}, "report": {}})
	defer func() {
		msg := fmt.Sprint(recover())
		if !strings.Contains(msg, "main") || !strings.Contains(msg, "report") {
			t.Errorf("应列出已配置的实例名，got=%v", msg)
		}
	}()
	r.Get("typo")
}

func TestLookupHasNames(t *testing.T) {
	r := NewRegistry[*fake]("xdemo", "XDemo")
	r.Publish(map[string]*fake{"report": {name: "r"}, "main": {name: "m"}})

	if v, ok := r.Lookup("main"); !ok || v.name != "m" {
		t.Errorf("应取到 main，got=%v ok=%v", v, ok)
	}
	if _, ok := r.Lookup("nope"); ok {
		t.Error("取不到时应返回 false")
	}
	if !r.Has("main") || r.Has("nope") || r.Has() {
		t.Error("Has 应如实反映是否配过")
	}
	if got := r.Names(); len(got) != 2 || got[0] != "main" {
		t.Errorf("Names 应按名字排序，got=%v", got)
	}
}

func TestGet_不带参数取default(t *testing.T) {
	r := NewRegistry[*fake]("xdemo", "XDemo")
	r.Publish(map[string]*fake{DefaultName: {name: "d"}})
	if r.Get().name != "d" {
		t.Error("不带参数应取 default")
	}
}

func TestBuild_全部成功(t *testing.T) {
	r := NewRegistry[string]("xdemo", "XDemo")
	var closed []string

	closer, err := Build(context.Background(), r, map[string]string{"a": "A", "b": "B"},
		func(_ context.Context, c string) (string, io.Closer, error) {
			return c, &recordCloser{name: c, closed: &closed}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Names(); len(got) != 2 {
		t.Fatalf("应建出两个，got=%v", got)
	}

	if err := closer.Close(); err != nil {
		t.Errorf("关闭不该报错：%v", err)
	}
	// 逆序关
	if len(closed) != 2 || closed[0] != "B" || closed[1] != "A" {
		t.Errorf("应逆序关闭，got=%v", closed)
	}
	// 关完必须摘干净，否则 C() 会返回已经关掉的实例
	if got := r.Names(); len(got) != 0 {
		t.Errorf("关闭后应清空，got=%v", got)
	}
}

func TestBuild_一个失败就全部回滚(t *testing.T) {
	// 组件 Init 返回错误时框架拿不到 Closer，已建好的必须自己收拾
	r := NewRegistry[string]("xdemo", "XDemo")
	var closed []string

	_, err := Build(context.Background(), r, map[string]string{"a": "A", "z": "Z"},
		func(_ context.Context, c string) (string, io.Closer, error) {
			if c == "Z" {
				return "", nil, errors.New("建不起来")
			}
			return c, &recordCloser{name: c, closed: &closed}, nil
		})
	if err == nil {
		t.Fatal("应当报错")
	}
	if !strings.Contains(err.Error(), `"z"`) {
		t.Errorf("错误里应点名是哪个实例，got=%v", err)
	}
	if len(closed) != 1 || closed[0] != "A" {
		t.Errorf("已建好的应被关掉，got=%v", closed)
	}
	if got := r.Names(); len(got) != 0 {
		t.Errorf("失败时不该发布任何实例，got=%v", got)
	}
}

func TestBuild_关闭错误会带出来但不中断(t *testing.T) {
	r := NewRegistry[string]("xdemo", "XDemo")
	var closed []string
	boom := errors.New("关不掉")

	closer, err := Build(context.Background(), r, map[string]string{"a": "A", "b": "B"},
		func(_ context.Context, c string) (string, io.Closer, error) {
			return c, &recordCloser{name: c, closed: &closed, err: boom}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if err := closer.Close(); !errors.Is(err, boom) {
		t.Errorf("关闭错误应带出来，got=%v", err)
	}
	if len(closed) != 2 {
		t.Errorf("一个关不掉不该拦住另一个，got=%v", closed)
	}
}

func TestBuild_空配置(t *testing.T) {
	r := NewRegistry[string]("xdemo", "XDemo")
	closer, err := Build(context.Background(), r, map[string]string{}, func(context.Context, string) (string, io.Closer, error) {
		t.Fatal("不该被调用")
		return "", nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := closer.Close(); err != nil {
		t.Errorf("空的也该关得掉：%v", err)
	}
}

func TestBuild_nil_Closer不炸(t *testing.T) {
	// 有的实例可能没有要关的东西
	r := NewRegistry[string]("xdemo", "XDemo")
	closer, err := Build(context.Background(), r, map[string]string{"a": "A"},
		func(_ context.Context, c string) (string, io.Closer, error) { return c, nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := closer.Close(); err != nil {
		t.Errorf("nil Closer 应被跳过，got=%v", err)
	}
}

func TestBuild_收到退出信号就不再建剩下的实例(t *testing.T) {
	// 配了五个库、第一个就要重试到超时的话，收到退出信号应当就此打住，
	// 而不是把剩下四个也挨个试一遍——那几次注定失败的重试会拖满退出时间
	r := NewRegistry[string]("xdemo", "XDemo")
	ctx, cancel := context.WithCancel(context.Background())
	var built, closed []string

	_, err := Build(ctx, r, map[string]string{"a": "A", "b": "B", "c": "C"},
		func(_ context.Context, c string) (string, io.Closer, error) {
			built = append(built, c)
			cancel() // 建第一个的时候收到退出信号
			return c, &recordCloser{name: c, closed: &closed}, nil
		})

	if err == nil {
		t.Fatal("收到退出信号应当中止并报错")
	}
	if len(built) != 1 {
		t.Errorf("取消之后不该再建，got=%v", built)
	}
	if len(closed) != 1 || closed[0] != "A" {
		t.Errorf("已经建好的要关掉，否则漏一个连接池，got=%v", closed)
	}
	if r.Names() != nil && len(r.Names()) != 0 {
		t.Errorf("中途失败不该把半成品发布出去，got=%v", r.Names())
	}
}
