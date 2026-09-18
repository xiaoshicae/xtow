// Package config 把配置文件读成各组件自己的结构体，读完就结束。
//
// 三条规矩：默认值预填在结构体里；未知字段是错误；${VAR} 未设置是错误。
package config

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/xiaoshicae/xtow/registry"
)

var placeholder = regexp.MustCompile(`\$\{([^}:]+)(?::([^}]*))?\}`)

// Load 读一次配置文件，把每个 Component 声明的那一段解进它自己的结构体。
//
// 组件的 Config 指针里已经是默认值，文件里没写的字段保持不变——
// 所以不需要指针字段来区分「没配」和「配成零值」。
func Load(path string, list []registry.Component) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取配置 %s 失败: %w", path, err)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return fmt.Errorf("解析配置 %s 失败: %w", path, err)
	}

	var missing []string
	expand(&root, &missing)
	if len(missing) > 0 {
		return fmt.Errorf("环境变量未设置: %s", strings.Join(missing, ", "))
	}

	sections, err := topLevel(&root)
	if err != nil {
		return err
	}

	claimed := map[string]bool{}
	for _, c := range list {
		if c.Key == "" || c.Config == nil {
			continue
		}
		claimed[c.Key] = true
		node, ok := sections[c.Key]
		if !ok || isEmptyNode(node) {
			continue // 没配这一块，或者写了个空块，都保持默认值
		}
		if err := decodeStrict(node, c.Config); err != nil {
			return fmt.Errorf("配置 %s 有误: %w", c.Key, err)
		}
	}

	// 没有任何组件认领的顶层 key —— 多半是拼错了，或者忘了 import 对应的 contrib
	for key := range sections {
		if !claimed[key] {
			return fmt.Errorf("配置里的 %q 没有任何组件认领：检查拼写，或确认是否 import 了对应的 contrib 包", key)
		}
	}
	return nil
}

func topLevel(root *yaml.Node) (map[string]*yaml.Node, error) {
	out := map[string]*yaml.Node{}
	if root.Kind == 0 || len(root.Content) == 0 {
		return out, nil // 空文件
	}
	doc := root.Content[0]
	if doc.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("配置文件顶层必须是一个 mapping")
	}
	for i := 0; i+1 < len(doc.Content); i += 2 {
		out[doc.Content[i].Value] = doc.Content[i+1]
	}
	return out, nil
}

// isEmptyNode 判断一个配置块是否没有任何内容
//
// `Demo:` 后面什么都不写，解析出来是一个 null 标量；`Demo: {}` 是一个空 mapping。
// 两种都该等同于「没配这一块」——直接交给解码器的话，前者会得到一个
// 意义不明的 EOF 错误，后者虽然能过但没必要走一趟。
func isEmptyNode(n *yaml.Node) bool {
	if n == nil {
		return true
	}
	if n.Tag == "!!null" {
		return true
	}
	return (n.Kind == yaml.MappingNode || n.Kind == yaml.SequenceNode) && len(n.Content) == 0
}

// decodeStrict 严格解码：认不出的字段是错误，不是忽略
func decodeStrict(node *yaml.Node, target any) error {
	b, err := yaml.Marshal(node)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	return dec.Decode(target)
}

// expand 在解析后的节点上展开占位符，不在原始字节上做文本替换：
// 环境变量的值里若含冒号或换行，文本替换会改变 YAML 结构。
func expand(n *yaml.Node, missing *[]string) {
	if n.Kind == yaml.ScalarNode && strings.Contains(n.Value, "${") {
		n.Value = placeholder.ReplaceAllStringFunc(n.Value, func(m string) string {
			idx := placeholder.FindStringSubmatchIndex(m)
			name := m[idx[2]:idx[3]]
			if v, ok := os.LookupEnv(name); ok {
				return v
			}
			if idx[4] >= 0 {
				return m[idx[4]:idx[5]]
			}
			*missing = append(*missing, name)
			return m
		})
	}
	for _, c := range n.Content {
		expand(c, missing)
	}
}
