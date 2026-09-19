package config

import (
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/xiaoshicae/xtow/xerror"
)

// 两个保留的顶层 key。它们由加载器自己消费，不会分发给任何组件，
// 所以「没人认领的 key 直接失败」那条规矩对它们不适用。
const (
	// ProfilesKey 声明激活哪些 profile，对应 Spring 的 spring.profiles.active
	ProfilesKey = "Profiles"
	// ImportKey 引入别的配置文件，对应 Spring 的 spring.config.import
	ImportKey = "Import"
)

// optionalPrefix 带这个前缀的 import 文件不存在时跳过，不报错
const optionalPrefix = "optional:"

// maxImportDepth import 的嵌套上限，防止配置写出一条看不见的深链
const maxImportDepth = 16

// loaded 一个加载好的配置文件
type loaded struct {
	path string
	node *yaml.Node // 文档节点
}

// loadAll 按优先级从低到高列出所有要合并的文件。
//
// 顺序与 Spring 一致：
//
//	application.yml  <  它 import 的  <  application-prod.yml  <  prod import 的
//
// 也就是说 import 进来的会压过引它的那个文件（Spring 的说法是
// 「import 相当于插在声明它的那份文档正下方」，而下面的压过上面的），
// profile 文件又压过不带 profile 的那份。
func loadAll(base string, profiles []string) ([]loaded, error) {
	seen := map[string]bool{} // 同一个文件只 import 一次，与 Spring 一致
	var out []loaded

	add := func(path string, required bool) error {
		files, err := withImports(path, required, seen, 0)
		if err != nil {
			return err
		}
		out = append(out, files...)
		return nil
	}

	if err := add(base, true); err != nil {
		return nil, err
	}
	for _, p := range profiles {
		// profile 文件不存在直接失败，与 Spring 不同：那边是静默跳过。
		// 这里点名要了某个 profile，文件却不在，几乎总是名字写错了——
		// 静默跳过的结果是一份谁都没看过的配置悄悄以默认值起来
		if err := add(profilePath(base, p), true); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// withImports 读一个文件，并把它 import 的文件排在它后面（于是压过它）
func withImports(path string, required bool, seen map[string]bool, depth int) ([]loaded, error) {
	if depth > maxImportDepth {
		return nil, xerror.Newf("xconfig", "config",
			"import nested more than %d levels deep at %s, check for a cycle", maxImportDepth, path)
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if seen[abs] {
		return nil, nil // 同一个文件只算一次
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if !required && os.IsNotExist(err) {
			return nil, nil
		}
		return nil, xerror.Newf("xconfig", "config", "read config %s: %w", path, err)
	}
	seen[abs] = true

	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, xerror.Newf("xconfig", "config", "parse config %s: %w", path, err)
	}

	imports, err := importsOf(&doc, path)
	if err != nil {
		return nil, err
	}

	out := []loaded{{path: path, node: &doc}}
	for _, spec := range imports {
		target, optional := strings.CutPrefix(spec, optionalPrefix)
		// 相对路径按「引它的那个文件所在目录」解析，而不是进程的工作目录：
		// 配置目录整个搬个位置，里面的相对引用不该跟着失效
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		nested, err := withImports(target, !optional, seen, depth+1)
		if err != nil {
			return nil, err
		}
		out = append(out, nested...)
	}
	return out, nil
}

// importsOf 取出并移除文档里的 Import 块。
//
// 路径上的 ${VAR} 在这里就展开：要先知道读哪个文件，才谈得上合并。
// 其余字段的展开留到全部合并完之后——base 里一个必填的 ${VAR}
// 如果被 profile 文件覆盖掉了，就不该再要求它必须设置。
func importsOf(doc *yaml.Node, path string) ([]string, error) {
	node := takeTopLevel(doc, ImportKey)
	if node == nil {
		return nil, nil
	}

	var missing []string
	expand(node, &missing)
	if len(missing) > 0 {
		return nil, xerror.Newf("xconfig", "config",
			"environment variables not set in %s of %s: %s", ImportKey, path, strings.Join(missing, ", "))
	}

	switch node.Kind {
	case yaml.ScalarNode:
		if node.Tag == "!!null" || node.Value == "" {
			return nil, nil
		}
		return []string{node.Value}, nil
	case yaml.SequenceNode:
		out := make([]string, 0, len(node.Content))
		for _, c := range node.Content {
			if c.Kind != yaml.ScalarNode {
				return nil, xerror.Newf("xconfig", "config",
					"%s in %s must be a path or a list of paths", ImportKey, path)
			}
			out = append(out, c.Value)
		}
		return out, nil
	default:
		return nil, xerror.Newf("xconfig", "config",
			"%s in %s must be a path or a list of paths", ImportKey, path)
	}
}

// takeTopLevel 取出顶层的某个 key 并把它从文档里摘掉，没有则返回 nil
func takeTopLevel(doc *yaml.Node, key string) *yaml.Node {
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil
	}
	m := doc.Content[0]
	if m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value != key {
			continue
		}
		val := m.Content[i+1]
		m.Content = append(m.Content[:i], m.Content[i+2:]...)
		return val
	}
	return nil
}

// profilePath 由 application.yml 推出 application-prod.yml，目录和扩展名都跟着原文件
func profilePath(base, profile string) string {
	ext := filepath.Ext(base)
	return strings.TrimSuffix(base, ext) + "-" + profile + ext
}
