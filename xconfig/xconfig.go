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
	"fmt"

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

// DecodeClients 解一个「既支持单实例也支持多实例」的配置块。
//
//	XGorm:                  # 单实例，直接写字段，名字就是 default
//	  DSN: "${DB_DSN}"
//
//	XGorm:                  # 多实例，按名字写
//	  Clients:
//	    default: {DSN: "${DB_DSN}"}
//	    report:  {DSN: "${REPORT_DSN}"}
//
// 看有没有 Clients 决定按哪种解。两种混着写直接报错：那时候
// 「default 到底是哪个」没有一个不让人意外的答案。
//
// defaults 提供单个实例的默认值。多实例那一支靠元素类型自己的
// UnmarshalYAML 铺默认值（见本包开头的说明）。
func DecodeClients[C any](n *yaml.Node, defaults func() C) (map[string]C, error) {
	if !HasKey(n, clientsKey) {
		single := defaults()
		if err := DecodeStrict(n, &single); err != nil {
			return nil, err
		}
		return map[string]C{DefaultClientName: single}, nil
	}

	var multi struct {
		Clients map[string]C `yaml:"Clients"`
	}
	if err := DecodeStrict(n, &multi); err != nil {
		return nil, fmt.Errorf("%w（单实例和多实例两种写法不能混用：写了 %s 就把所有字段都放进去）",
			err, clientsKey)
	}
	if len(multi.Clients) == 0 {
		return nil, fmt.Errorf("%s 是空的：要么写上实例，要么整块删掉", clientsKey)
	}
	return multi.Clients, nil
}

const (
	clientsKey = "Clients"

	// DefaultClientName 单实例写法被规整成的名字。
	// 与 xclient.DefaultName 一致；这里再写一遍是为了不让核心的配置包
	// 反过来依赖 xclient。
	DefaultClientName = "default"
)
