// Package xconfig 提供集成包写配置解码时需要的那一点东西。
//
// 框架读配置的规矩是「默认值预填在结构体里、未知字段是错误」。
// 结构体字段上这两条都自动成立，但**集合装不下默认值**：
// map 的 value 和切片元素都是从零值开始解的，框架不知道该拿什么去填。
//
// 解法是给元素类型写一个 UnmarshalYAML，先铺默认值再解：
//
//	func (c *ClientConfig) UnmarshalYAML(n *yaml.Node) error {
//		*c = DefaultClientConfig()
//		type raw ClientConfig // 换个类型，否则这里会递归调用自己
//		return xconfig.DecodeStrict(n, (*raw)(c))
//	}
//
// 这里必须用 DecodeStrict 而不是 n.Decode：后者不带严格检查，
// 于是「字段拼错就启动失败」这条保证会在集合里悄悄失效。
package xconfig

import (
	"bytes"

	"go.yaml.in/yaml/v3"
)

// DecodeStrict 把一个 YAML 节点解进 v，认不出的字段是错误。
func DecodeStrict(node *yaml.Node, v any) error {
	b, err := yaml.Marshal(node)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	return dec.Decode(v)
}

// HasKey 报告一个 mapping 节点里有没有这个 key。
//
// 用于按配置的形状分派——比如同一个块既支持单实例也支持多实例时，
// 看有没有 Clients 决定按哪种解。
func HasKey(node *yaml.Node, key string) bool {
	if node == nil || node.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return true
		}
	}
	return false
}
