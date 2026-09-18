package xapp

import (
	"testing"

	"github.com/xiaoshicae/xtow/registry"
)

func TestRegister_只认领配置不初始化(t *testing.T) {
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
	if got.Init != nil {
		t.Error("本包没有要初始化的东西，登记 Init 会让它出现在关闭序列里")
	}
	if got.Config != &cfg {
		t.Error("登记的必须是包级配置变量本身，否则框架解出来的配置读不到")
	}
}

func TestNameVersion(t *testing.T) {
	old := cfg
	t.Cleanup(func() { cfg = old })

	if Name() != "" || Version() != "" {
		t.Errorf("默认应为空，got=%q %q", Name(), Version())
	}
	cfg = Config{Name: "xone.demo.app", Version: "v1.2.0"}
	if Name() != "xone.demo.app" || Version() != "v1.2.0" {
		t.Errorf("读到的应是配置里的值，got=%q %q", Name(), Version())
	}
}
