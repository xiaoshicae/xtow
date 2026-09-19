package xerror

import (
	"errors"
	"strings"
	"testing"
)

var errBase = errors.New("底层错误")

func TestErrorMessage(t *testing.T) {
	e := New("xgorm", "init", errBase)
	got := e.Error()
	// 前缀是框架名 xtow，曾经手抖写成了 xtwo —— 它出现在本包产出的每一条错误里
	for _, want := range []string{"xtow ", "xgorm", "init", "底层错误"} {
		if !strings.Contains(got, want) {
			t.Errorf("错误消息应包含 %q，got=%q", want, got)
		}
	}
}

func TestUnwrapSupportsErrorsIs(t *testing.T) {
	e := New("xgorm", "init", errBase)
	if !errors.Is(e, errBase) {
		t.Fatal("应能通过 errors.Is 找到被包装的原始错误")
	}
}

func TestIsWalksWholeChain(t *testing.T) {
	// 模块之间会互相包装：xgorm 的错误里裹着 config 的错误
	inner := New("config", "load", errBase)
	outer := New("xgorm", "init", inner)

	if !Is(outer, "xgorm") {
		t.Error("应认出最外层模块")
	}
	if !Is(outer, "config") {
		t.Error("应认出链条内层的模块 —— 只看最外层就会漏")
	}
	if Is(outer, "xredis") {
		t.Error("不该认出链条里没有的模块")
	}
}

func TestModuleTakesOutermost(t *testing.T) {
	outer := New("xgorm", "init", New("config", "load", errBase))
	if got := Module(outer); got != "xgorm" {
		t.Errorf("Module 应取最外层（谁最终报出这个错），got=%q", got)
	}
	if got := Module(errBase); got != "" {
		t.Errorf("非本包错误应返回空串，got=%q", got)
	}
}

func TestNewf(t *testing.T) {
	e := Newf("xhttp", "request", "超时 timeout=[%v]", "3s")
	if !strings.Contains(e.Error(), "timeout=[3s]") {
		t.Errorf("Newf 应格式化消息，got=%q", e.Error())
	}
}

func TestErrorMessage_框架自己的错不重复前缀(t *testing.T) {
	// 前缀的作用是让错误落进别人的日志时看得出是谁报的。
	// 模块名本来就是 xtow 的那种，再加一次就成了「xtow xtow init failed」
	if got := New("xtow", "init", errBase).Error(); strings.HasPrefix(got, "xtow xtow") {
		t.Errorf("前缀重复了：%s", got)
	}
	if got := New("xgorm", "init", errBase).Error(); !strings.HasPrefix(got, "xtow xgorm") {
		t.Errorf("其它模块该带前缀：%s", got)
	}
}
