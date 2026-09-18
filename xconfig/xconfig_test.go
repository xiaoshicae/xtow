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
