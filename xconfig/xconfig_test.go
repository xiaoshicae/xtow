package xconfig

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type item struct {
	A string `yaml:"A"`
	B int    `yaml:"B"`
}

func defItem() item { return item{B: 42} }

// UnmarshalYAML 就是本包文档里推荐的写法，顺带当例子测一遍
func (i *item) UnmarshalYAML(n *yaml.Node) error {
	*i = defItem()
	type raw item
	return DecodeStrict(n, (*raw)(i))
}

func decode(t *testing.T, src string, v any) error {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(src), &n); err != nil {
		t.Fatal(err)
	}
	return DecodeStrict(n.Content[0], v)
}

func TestDecodeStrict_未知字段是错误(t *testing.T) {
	var got item
	err := decode(t, "A: x\nZZZ: 1\n", &got)
	if err == nil {
		t.Fatal("拼错的字段应当报错")
	}
	if !strings.Contains(err.Error(), "ZZZ") {
		t.Errorf("错误里要点名，got=%v", err)
	}
}

func TestDecodeStrict_集合元素也保留严格检查(t *testing.T) {
	// 这是本包存在的理由：元素类型自己写 UnmarshalYAML 铺默认值时，
	// 如果图省事用 node.Decode，严格检查就在集合里悄悄失效了
	var got map[string]item
	if err := decode(t, "a:\n  A: x\n  ZZZ: 1\n", &got); err == nil {
		t.Fatal("集合元素里的字段拼错也应当报错")
	}
}

func TestDecodeStrict_默认值铺得上(t *testing.T) {
	var got map[string]item
	if err := decode(t, "a:\n  A: x\n", &got); err != nil {
		t.Fatal(err)
	}
	if got["a"].A != "x" || got["a"].B != 42 {
		t.Errorf("没写的字段应保持默认，got=%+v", got["a"])
	}
}

func TestHasKey(t *testing.T) {
	var n yaml.Node
	if err := yaml.Unmarshal([]byte("A: 1\nClients:\n  x: 1\n"), &n); err != nil {
		t.Fatal(err)
	}
	doc := n.Content[0]

	if !HasKey(doc, "Clients") || !HasKey(doc, "A") {
		t.Error("有的 key 应当认得出来")
	}
	if HasKey(doc, "Nope") {
		t.Error("没有的 key 不该认成有")
	}
	if HasKey(nil, "A") {
		t.Error("nil 节点不该 panic，也不该说有")
	}

	var scalar yaml.Node
	yaml.Unmarshal([]byte("just-a-string\n"), &scalar)
	if HasKey(scalar.Content[0], "A") {
		t.Error("不是 mapping 的节点不该说有 key")
	}
}

type client struct {
	Addr string `yaml:"Addr"`
	Max  int    `yaml:"Max"`
}

func defClient() client { return client{Addr: "127.0.0.1", Max: 50} }

func (c *client) UnmarshalYAML(n *yaml.Node) error {
	*c = defClient()
	type raw client
	return DecodeStrict(n, (*raw)(c))
}

func clients(t *testing.T, src string) (map[string]client, error) {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(src), &n); err != nil {
		t.Fatal(err)
	}
	return DecodeClients(n.Content[0], defClient)
}

func TestDecodeClients_单实例写法(t *testing.T) {
	got, err := clients(t, "Addr: 10.0.0.1\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("应解出一个实例，got=%v", got)
	}
	c, ok := got[DefaultClientName]
	if !ok {
		t.Fatalf("单实例写法应规整成名为 %s 的实例，got=%v", DefaultClientName, got)
	}
	if c.Addr != "10.0.0.1" || c.Max != 50 {
		t.Errorf("写了的生效、没写的保持默认，got=%+v", c)
	}
}

func TestDecodeClients_多实例写法(t *testing.T) {
	got, err := clients(t, "Clients:\n  default: {Addr: a}\n  report: {Addr: b, Max: 5}\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("应解出两个实例，got=%v", got)
	}
	// 一个实例覆盖了的字段不该影响另一个
	if got["default"].Max != 50 || got["report"].Max != 5 {
		t.Errorf("默认值应逐个实例生效，got=%v", got)
	}
}

func TestDecodeClients_两种写法不能混用(t *testing.T) {
	// 混着写时「default 到底是哪个」没有不让人意外的答案
	_, err := clients(t, "Addr: a\nClients:\n  x: {Addr: b}\n")
	if err == nil {
		t.Fatal("混用两种写法应当报错")
	}
	if !strings.Contains(err.Error(), "cannot mix") {
		t.Errorf("错误该说清楚为什么，got=%v", err)
	}
	// 还要点名是哪个字段没了归属，否则一个几十行的配置块看不出改哪里
	if !strings.Contains(err.Error(), "Addr") {
		t.Errorf("错误该指出是哪个字段，got=%v", err)
	}
}

func TestDecodeClients_实例里拼错不该被报成混用(t *testing.T) {
	// 「不能混用」这句话曾经是套在任何一个解码错误上的：实例里一个字段拼错
	// （Clients.x.Adrr）报的也是它。那份配置根本没混用，使用者会照着这句话
	// 去改一个没问题的地方，真正的拼写错误反而被这句提示盖住了。
	cases := map[string]string{
		"实例里字段拼错": "Clients:\n  x: {Adrr: a}\n",
		"实例里类型不对": "Clients:\n  x: {Max: abc}\n",
	}
	for name, src := range cases {
		_, err := clients(t, src)
		if err == nil {
			t.Fatalf("%s：应当报错", name)
		}
		if strings.Contains(err.Error(), "cannot mix") {
			t.Errorf("%s：这不是混用，不该这么报，got=%v", name, err)
		}
	}
}

func TestDecodeClients_空的Clients要报错(t *testing.T) {
	if _, err := clients(t, "Clients: {}\n"); err == nil {
		t.Fatal("写了 Clients 却是空的，应当报错")
	}
}

func TestDecodeClients_拼写错误两种写法都要拦住(t *testing.T) {
	// 集合元素走的是自定义解码器，严格检查很容易在那里悄悄失效
	if _, err := clients(t, "Adrr: a\n"); err == nil {
		t.Error("单实例写法里拼错应当报错")
	}
	if _, err := clients(t, "Clients:\n  x: {Adrr: a}\n"); err == nil {
		t.Error("多实例写法里拼错应当报错")
	}
}
