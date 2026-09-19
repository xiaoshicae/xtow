// Package config 把配置文件读成各组件自己的结构体，读完就结束。
//
// 三条规矩：默认值预填在结构体里；未知字段是错误；${VAR} 未设置是错误。
package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/xiaoshicae/xtow/registry"
	"github.com/xiaoshicae/xtow/xconfig"
)

var placeholder = regexp.MustCompile(`\$\{([^}:]+)(?::([^}]*))?\}`)

// Load 读一次配置文件，把每个 Component 声明的那一段解进它自己的结构体。
//
// 组件的 Config 指针里已经是默认值，文件里没写的字段保持不变——
// 所以不需要指针字段来区分「没配」和「配成零值」。
func Load(path string, list []registry.Component) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config %s: %w", path, err)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return fmt.Errorf("parse config %s: %w", path, err)
	}

	var missing []string
	expand(&root, &missing)
	if len(missing) > 0 {
		return fmt.Errorf("environment variables not set: %s", strings.Join(missing, ", "))
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
			return fmt.Errorf("invalid config %s: %w", c.Key, err)
		}
	}

	// 没有任何组件认领的顶层 key —— 多半是拼错了，或者忘了 import 对应的 contrib
	for key := range sections {
		if !claimed[key] {
			return fmt.Errorf("config key %q is not claimed by any component: check the spelling, or whether the matching contrib package is imported", key)
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
		return nil, fmt.Errorf("top level of the config file must be a mapping")
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

// decodeStrict 严格解码：认不出的字段是错误，不是忽略。
//
// 与集成包用的是同一个实现：集合元素的默认值要靠元素自己的 UnmarshalYAML 铺，
// 那里必须能拿到同样的严格检查，否则「拼错就失败」在集合里会悄悄失效。
func decodeStrict(node *yaml.Node, target any) error {
	return xconfig.DecodeStrict(node, target)
}

// expand 在解析后的节点上展开占位符，不在原始字节上做文本替换：
// 环境变量的值里若含冒号或换行，文本替换会改变 YAML 结构。
func expand(n *yaml.Node, missing *[]string) {
	if n.Kind == yaml.ScalarNode && strings.Contains(n.Value, "${") {
		before := n.Value
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
		if n.Value != before {
			retag(n)
		}
	}
	for _, c := range n.Content {
		expand(c, missing)
	}
}

// retag 让替换过的标量按新内容重新判定类型。
//
// 解析的时候整个 ${PORT:8080} 是一段文本，所以这个标量被打上了 !!str。
// 替换之后它的内容是 8080，标签却还留在 !!str 上，于是
//
//	Port: ${PORT:8080}
//
// 会以「cannot unmarshal !!str into int」失败——占位符因此只能用在字符串字段上，
// 而文档里它是一条通用规则。清掉标签，让 yaml 按替换后的内容重新判定即可。
//
// 这不会让环境变量的值改变 YAML 结构：重新判定的对象仍是这一个标量，
// 序列化时 yaml 会按内容自己选引号，值里的冒号、换行、星号都留在标量内部。
//
// 两种情况保持原样：
//   - 使用者显式加了引号或写了标签（Style 非 0），那是明确的「按字符串处理」，
//     数字形态的密码、版本号都指望它；
//   - 替换结果为空，重新判定会变成 null，把结构体里预填的默认值清成零值。
func retag(n *yaml.Node) {
	if n.Style != 0 || n.Value == "" {
		return
	}
	n.Tag = ""
}
