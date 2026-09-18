package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xiaoshicae/xtow/registry"
)

type demoConf struct {
	Addr    string        `yaml:"Addr"`
	Timeout time.Duration `yaml:"Timeout"`
	Retries int           `yaml:"Retries"`
	Enable  bool          `yaml:"Enable"`
}

func defaults() demoConf {
	return demoConf{Addr: "127.0.0.1:5432", Timeout: 5 * time.Second, Retries: 3, Enable: true}
}

// comps 造一个登记了 Demo 的组件列表，并返回它的配置指针
func comps(t *testing.T) ([]registry.Component, *demoConf) {
	t.Helper()
	c := defaults()
	return []registry.Component{{Key: "Demo", Config: &c}}, &c
}

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad_默认值在未写的字段上保留(t *testing.T) {
	list, c := comps(t)
	if err := Load(write(t, "Demo:\n  Addr: \"10.0.0.1:1\"\n"), list); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "10.0.0.1:1" {
		t.Errorf("写了的字段应被覆盖，got=%q", c.Addr)
	}
	if c.Timeout != 5*time.Second || c.Retries != 3 || !c.Enable {
		t.Errorf("没写的字段应保留默认值，got=%+v", *c)
	}
}

func TestLoad_显式零值能覆盖默认值(t *testing.T) {
	// 这正是 *bool 指针模式想解决的问题，预填默认值天然就有这个语义
	list, c := comps(t)
	if err := Load(write(t, "Demo:\n  Enable: false\n  Retries: 0\n  Addr: \"\"\n"), list); err != nil {
		t.Fatal(err)
	}
	if c.Enable || c.Retries != 0 || c.Addr != "" {
		t.Errorf("显式写的零值应覆盖默认值，got=%+v", *c)
	}
}

func TestLoad_Duration原生解析(t *testing.T) {
	for _, tc := range []struct {
		yaml string
		want time.Duration
	}{
		{"Demo:\n  Timeout: 30s\n", 30 * time.Second},
		{"Demo:\n  Timeout: \"1500ms\"\n", 1500 * time.Millisecond},
		{"Demo:\n  Timeout: 2m\n", 2 * time.Minute},
	} {
		list, c := comps(t)
		if err := Load(write(t, tc.yaml), list); err != nil {
			t.Fatalf("%q: %v", tc.yaml, err)
		}
		if c.Timeout != tc.want {
			t.Errorf("%q -> %v, want %v", tc.yaml, c.Timeout, tc.want)
		}
	}
}

func TestLoad_时长格式错误当场报错(t *testing.T) {
	list, _ := comps(t)
	err := Load(write(t, "Demo:\n  Timeout: 那么久\n"), list)
	if err == nil {
		t.Fatal("时长格式不对应该启动失败，而不是静默变成 0")
	}
}

func TestLoad_字段拼错是错误(t *testing.T) {
	list, _ := comps(t)
	err := Load(write(t, "Demo:\n  Adrr: \"x\"\n"), list)
	if err == nil {
		t.Fatal("拼错的字段应该报错，而不是被忽略")
	}
	if !strings.Contains(err.Error(), "Demo") {
		t.Errorf("错误里应指明是哪个块出的问题，got=%v", err)
	}
}

func TestLoad_没人认领的块是错误(t *testing.T) {
	list, _ := comps(t)
	err := Load(write(t, "Demo:\n  Addr: x\nXRedis:\n  Addr: y\n"), list)
	if err == nil {
		t.Fatal("配了但没有组件认领的块应该报错")
	}
	// 这个提示很重要：它同时覆盖「key 拼错」和「忘了 import 对应的包」
	if !strings.Contains(err.Error(), "XRedis") || !strings.Contains(err.Error(), "import") {
		t.Errorf("错误应指出是哪个块，并提示可能是忘了 import，got=%v", err)
	}
}

func TestLoad_占位符(t *testing.T) {
	t.Run("环境变量已设置", func(t *testing.T) {
		t.Setenv("XTOW_T_ADDR", "10.1.1.1:9")
		list, c := comps(t)
		if err := Load(write(t, "Demo:\n  Addr: \"${XTOW_T_ADDR}\"\n"), list); err != nil {
			t.Fatal(err)
		}
		if c.Addr != "10.1.1.1:9" {
			t.Errorf("got=%q", c.Addr)
		}
	})

	t.Run("未设置时用默认值", func(t *testing.T) {
		os.Unsetenv("XTOW_T_UNSET")
		list, c := comps(t)
		if err := Load(write(t, "Demo:\n  Addr: \"${XTOW_T_UNSET:1.2.3.4:5}\"\n"), list); err != nil {
			t.Fatal(err)
		}
		if c.Addr != "1.2.3.4:5" {
			t.Errorf("默认值里含冒号也应完整保留，got=%q", c.Addr)
		}
	})

	t.Run("必填未设置则启动失败", func(t *testing.T) {
		os.Unsetenv("XTOW_T_REQUIRED")
		list, _ := comps(t)
		err := Load(write(t, "Demo:\n  Addr: \"${XTOW_T_REQUIRED}\"\n"), list)
		if err == nil || !strings.Contains(err.Error(), "XTOW_T_REQUIRED") {
			t.Fatalf("必填占位符缺失应报错并指出变量名，got=%v", err)
		}
	})

	t.Run("显式空串覆盖默认值", func(t *testing.T) {
		t.Setenv("XTOW_T_EMPTY", "")
		list, c := comps(t)
		if err := Load(write(t, "Demo:\n  Addr: \"${XTOW_T_EMPTY:fallback}\"\n"), list); err != nil {
			t.Fatal(err)
		}
		if c.Addr != "" {
			t.Errorf("显式设为空串是有效取值，应覆盖默认值，got=%q", c.Addr)
		}
	})
}

func TestLoad_占位符填非字符串字段(t *testing.T) {
	// 文档把 ${VAR} 写成一条通用规则，那它就得对所有字段类型成立。
	// 解析时整个 ${PORT:8080} 是一段文本，标量因此被打上 !!str；
	// 替换之后不重新判定类型的话，这些字段全都以
	// "cannot unmarshal !!str into int" 失败——占位符沦为字符串字段专用。
	type demo struct {
		Retries int           `yaml:"Retries"`
		Enable  bool          `yaml:"Enable"`
		Ratio   float64       `yaml:"Ratio"`
		Timeout time.Duration `yaml:"Timeout"`
		Addr    string        `yaml:"Addr"`
	}

	t.Run("用默认值", func(t *testing.T) {
		os.Unsetenv("XTOW_T_N1")
		c := demo{}
		list := []registry.Component{{Key: "Demo", Config: &c}}
		body := "Demo:\n  Retries: ${XTOW_T_N1:7}\n  Enable: ${XTOW_T_N1:true}\n" +
			"  Ratio: ${XTOW_T_N1:0.25}\n  Timeout: ${XTOW_T_N1:90s}\n  Addr: ${XTOW_T_N1:h:1}\n"
		if err := Load(write(t, body), list); err != nil {
			t.Fatal(err)
		}
		want := demo{Retries: 7, Enable: true, Ratio: 0.25, Timeout: 90 * time.Second, Addr: "h:1"}
		if c != want {
			t.Errorf("got=%+v want=%+v", c, want)
		}
	})

	t.Run("取环境变量", func(t *testing.T) {
		t.Setenv("XTOW_T_N2", "42")
		c := demo{}
		list := []registry.Component{{Key: "Demo", Config: &c}}
		if err := Load(write(t, "Demo:\n  Retries: ${XTOW_T_N2}\n"), list); err != nil {
			t.Fatal(err)
		}
		if c.Retries != 42 {
			t.Errorf("Retries 应取自环境变量，got=%d", c.Retries)
		}
	})

	t.Run("裸数字进时长字段照样报错", func(t *testing.T) {
		// 重新判定类型不能顺手把这条保证放掉：${T:30} 和直接写 30 是一回事，
		// 写的人想要 30 秒，不是 30 纳秒
		c := demo{}
		list := []registry.Component{{Key: "Demo", Config: &c}}
		if err := Load(write(t, "Demo:\n  Timeout: ${XTOW_T_N3:30}\n"), list); err == nil {
			t.Fatal("占位符里的裸数字同样应当报错")
		}
	})

	t.Run("加了引号就仍按字符串处理", func(t *testing.T) {
		// 数字形态的密码、版本号指望这一条：显式引号是明确的意图，不该被重新判定
		t.Setenv("XTOW_T_N4", "0123456")
		c := demo{}
		list := []registry.Component{{Key: "Demo", Config: &c}}
		if err := Load(write(t, "Demo:\n  Addr: \"${XTOW_T_N4}\"\n"), list); err != nil {
			t.Fatal(err)
		}
		if c.Addr != "0123456" {
			t.Errorf("引号里的值应原样进字符串字段，got=%q", c.Addr)
		}
	})

	t.Run("不加引号的数字进字符串字段也不丢前导零", func(t *testing.T) {
		t.Setenv("XTOW_T_N5", "0123456")
		c := demo{}
		list := []registry.Component{{Key: "Demo", Config: &c}}
		if err := Load(write(t, "Demo:\n  Addr: ${XTOW_T_N5}\n"), list); err != nil {
			t.Fatal(err)
		}
		if c.Addr != "0123456" {
			t.Errorf("重新判定类型不该改变字符串字段读到的内容，got=%q", c.Addr)
		}
	})

	t.Run("空值不重新判定", func(t *testing.T) {
		// "" 重新判定会变成 null，把结构体里预填的默认值清成零值
		t.Setenv("XTOW_T_N6", "")
		c := demo{Retries: 3}
		list := []registry.Component{{Key: "Demo", Config: &c}}
		if err := Load(write(t, "Demo:\n  Addr: ${XTOW_T_N6}\n"), list); err != nil {
			t.Fatal(err)
		}
		if c.Addr != "" || c.Retries != 3 {
			t.Errorf("got=%+v", c)
		}
	})
}

func TestLoad_占位符在map和切片里也按新内容判定类型(t *testing.T) {
	// 展开是在整棵节点树上做的，map 的 value、切片元素、嵌套结构体都要对。
	// 尤其是 map[string]string：重新判定类型不该把 "123" 变成读不进 string 的东西
	os.Unsetenv("XTOW_T_C1")
	type demo struct {
		Labels  map[string]string `yaml:"Labels"`
		Names   []string          `yaml:"Names"`
		Buckets []float64         `yaml:"Buckets"`
		Nested  struct {
			Max int `yaml:"Max"`
		} `yaml:"Nested"`
	}

	body := "Demo:\n" +
		"  Labels:\n    env: ${XTOW_T_C1:dev}\n    ver: ${XTOW_T_C1:123}\n    on: ${XTOW_T_C1:true}\n" +
		"  Names:\n    - ${XTOW_T_C1:a}\n    - ${XTOW_T_C1:7}\n" +
		"  Buckets:\n    - ${XTOW_T_C1:0.1}\n    - ${XTOW_T_C1:1}\n" +
		"  Nested:\n    Max: ${XTOW_T_C1:9}\n"

	c := demo{}
	if err := Load(write(t, body), []registry.Component{{Key: "Demo", Config: &c}}); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"env": "dev", "ver": "123", "on": "true"}
	if !reflect.DeepEqual(c.Labels, want) {
		t.Errorf("map 的 string value 应原样是文本，got=%v", c.Labels)
	}
	if !reflect.DeepEqual(c.Names, []string{"a", "7"}) {
		t.Errorf("字符串切片，got=%v", c.Names)
	}
	if !reflect.DeepEqual(c.Buckets, []float64{0.1, 1}) {
		t.Errorf("数值切片，got=%v", c.Buckets)
	}
	if c.Nested.Max != 9 {
		t.Errorf("嵌套结构体，got=%d", c.Nested.Max)
	}
}

func TestLoad_占位符的值含特殊字符不破坏结构(t *testing.T) {
	// 在解析后的节点上展开，而不是对原始字节做文本替换 —— 否则这是条注入路径
	t.Setenv("XTOW_T_INJECT", "a: b\nEvil: true")
	list, c := comps(t)
	if err := Load(write(t, "Demo:\n  Addr: \"${XTOW_T_INJECT}\"\n"), list); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "a: b\nEvil: true" {
		t.Errorf("环境变量的值应原样进字段，不该被当成 YAML 解析，got=%q", c.Addr)
	}

	// 不加引号时标量会被重新判定类型，那一步同样不能让值逃出标量本身
	list2, c2 := comps(t)
	if err := Load(write(t, "Demo:\n  Addr: ${XTOW_T_INJECT}\n"), list2); err != nil {
		t.Fatal(err)
	}
	if c2.Addr != "a: b\nEvil: true" {
		t.Errorf("重新判定类型后仍应是一个标量，got=%q", c2.Addr)
	}
}

func TestLoad_空文件全部用默认值(t *testing.T) {
	list, c := comps(t)
	if err := Load(write(t, ""), list); err != nil {
		t.Fatal(err)
	}
	if *c != defaults() {
		t.Errorf("空文件应保持全部默认值，got=%+v", *c)
	}
}

func TestLoad_没配这一块时保持默认(t *testing.T) {
	list, c := comps(t)
	if err := Load(write(t, "Demo:\n"), list); err != nil {
		t.Fatal(err)
	}
	if *c != defaults() {
		t.Errorf("块为空应保持默认值，got=%+v", *c)
	}
}

func TestLoad_文件不存在(t *testing.T) {
	list, _ := comps(t)
	if err := Load(filepath.Join(t.TempDir(), "nope.yml"), list); err == nil {
		t.Fatal("文件不存在应报错")
	}
}

func TestLoad_时长字段(t *testing.T) {
	// 各模块的超时/周期都直接用 time.Duration，靠的是 yaml.v3 原生认识时长字符串。
	// 这是个横跨所有模块的假设，在这里钉住：改了依赖会先在这里炸，
	// 而不是等到某个模块的超时悄悄变成 0
	type demo struct {
		Timeout time.Duration `yaml:"Timeout"`
		Rotate  time.Duration `yaml:"Rotate"`
	}

	t.Run("认识时长字符串", func(t *testing.T) {
		c := demo{Rotate: 24 * time.Hour} // 预填的默认值
		list := []registry.Component{{Key: "Demo", Config: &c}}
		if err := Load(write(t, "Demo:\n  Timeout: 1h30m\n"), list); err != nil {
			t.Fatal(err)
		}
		if c.Timeout != 90*time.Minute {
			t.Errorf("Timeout 应为 90m，got=%v", c.Timeout)
		}
		if c.Rotate != 24*time.Hour {
			t.Errorf("没配的字段应保持默认值，got=%v", c.Rotate)
		}
	})

	t.Run("裸数字要报错", func(t *testing.T) {
		// 写 Timeout: 30 的人想要 30 秒，Go 的零值语义会给他 30 纳秒。
		// 启动就失败好过线上超时形同虚设
		c := demo{}
		list := []registry.Component{{Key: "Demo", Config: &c}}
		err := Load(write(t, "Demo:\n  Timeout: 30\n"), list)
		if err == nil {
			t.Fatal("裸数字应当报错，要求写明单位")
		}
		if !strings.Contains(err.Error(), "Demo") {
			t.Errorf("错误应指明是哪一块配置，got=%v", err)
		}
	})
}

func TestLoad_没人认领的顶层key要报错(t *testing.T) {
	// 多半是拼错了，或者忘了 import 对应的 contrib 包。
	// 静默忽略的话，使用者会盯着一份"明明配了"的文件查半天
	type demo struct {
		Addr string `yaml:"Addr"`
	}
	c := demo{}
	list := []registry.Component{{Key: "Demo", Config: &c}}

	err := Load(write(t, "Demo:\n  Addr: a\nXGrom:\n  DSN: x\n"), list)
	if err == nil {
		t.Fatal("拼错的顶层 key 应当报错")
	}
	if !strings.Contains(err.Error(), "XGrom") {
		t.Errorf("错误里要指出是哪个 key，got=%v", err)
	}
}

func TestLoad_默认值的覆盖语义(t *testing.T) {
	// 默认值预填在结构体里，所以「文件里写了会发生什么」必须钉死。
	// 两种容器的行为不一样，写默认值的人必须知道
	type demo struct {
		Buckets []float64         `yaml:"Buckets"`
		Labels  map[string]string `yaml:"Labels"`
	}
	fresh := func() demo {
		return demo{
			Buckets: []float64{1, 2, 3},
			Labels:  map[string]string{"pre": "filled"},
		}
	}

	t.Run("切片是整体替换", func(t *testing.T) {
		c := fresh()
		list := []registry.Component{{Key: "Demo", Config: &c}}
		if err := Load(write(t, "Demo:\n  Buckets: [0.1, 1]\n"), list); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(c.Buckets, []float64{0.1, 1}) {
			t.Errorf("配了就该整体换掉，不能和默认值混在一起，got=%v", c.Buckets)
		}
	})

	t.Run("map 是合并而不是替换", func(t *testing.T) {
		// 所以 map 类型的字段不要预填默认值：使用者删不掉预填的条目。
		// 这条钉在这里，免得哪天有人给 ConstLabels 之类加个「合理的默认」
		c := fresh()
		list := []registry.Component{{Key: "Demo", Config: &c}}
		if err := Load(write(t, "Demo:\n  Labels:\n    a: \"1\"\n"), list); err != nil {
			t.Fatal(err)
		}
		if c.Labels["pre"] != "filled" {
			t.Errorf("map 的默认值不会被覆盖掉，这正是不该给 map 预填默认值的原因，got=%v", c.Labels)
		}
	})

	t.Run("没配的字段保持默认", func(t *testing.T) {
		c := fresh()
		list := []registry.Component{{Key: "Demo", Config: &c}}
		if err := Load(write(t, "Demo: {}\n"), list); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(c.Buckets, []float64{1, 2, 3}) {
			t.Errorf("没配就该保持默认，got=%v", c.Buckets)
		}
	})
}
