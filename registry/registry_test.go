package registry

import (
	"io"
	"testing"
)

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

func TestRegisterAndSnapshot(t *testing.T) {
	// 登记板是包级状态，测试之间要还原
	t.Cleanup(func() { mu.Lock(); list = nil; mu.Unlock() })

	Register(Component{Key: "A", Stage: StageClient})
	Register(Component{Key: "B", Stage: StageLog})

	got := Snapshot()
	if len(got) != 2 || got[0].Key != "A" || got[1].Key != "B" {
		t.Fatalf("Snapshot 应按登记顺序返回全部组件，got=%v", got)
	}
}

func TestSnapshotIsCopy(t *testing.T) {
	t.Cleanup(func() { mu.Lock(); list = nil; mu.Unlock() })

	Register(Component{Key: "A"})
	got := Snapshot()
	got[0].Key = "改了"

	if Snapshot()[0].Key != "A" {
		t.Fatal("Snapshot 返回的应是副本，改它不该影响登记板")
	}
}

func TestStageOrder(t *testing.T) {
	// 档位的相对顺序是框架的承诺，写死在测试里防止有人随手调整枚举
	if !(StageLog < StageTrace && StageTrace < StageClient && StageClient < StageServer) {
		t.Fatal("档位顺序必须是 Log < Trace < Client < Server")
	}
}

func TestComponentAcceptsCloser(t *testing.T) {
	var _ func() (io.Closer, error) = func() (io.Closer, error) { return nopCloser{}, nil }
}
