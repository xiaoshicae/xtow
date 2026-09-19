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

// listConf 带一个列表字段，用来验证「列表整体替换、不逐元素合并」
type listConf struct {
	Addr    string        `yaml:"Addr"`
	Timeout time.Duration `yaml:"Timeout"`
	Headers []string      `yaml:"Headers"`
	Nested  struct {
		A string `yaml:"A"`
		B string `yaml:"B"`
	} `yaml:"Nested"`
}

func listComps(t *testing.T) ([]registry.Component, *listConf) {
	t.Helper()
	c := listConf{Addr: "default:1", Timeout: time.Second}
	return []registry.Component{{Key: "Demo", Config: &c}}, &c
}

// files 在同一个临时目录里写好一组文件，返回其中 base 的路径
func files(t *testing.T, base string, m map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range m {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, base)
}

// withProfileEnv 设置 XTOW_PROFILE，退出时还原
func withProfileEnv(t *testing.T, v string) {
	t.Helper()
	t.Setenv(ProfileEnvKey, v)
}

func TestLoad_profile文件压过base(t *testing.T) {
	withProfileEnv(t, "prod")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "Demo:\n  Addr: base:1\n  Timeout: 1s\n",
		"application-prod.yml": "Demo:\n  Addr: prod:1\n",
	})

	list, c := listComps(t)
	if err := Load(base, list); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "prod:1" {
		t.Errorf("profile 文件该压过 base，Addr=%q", c.Addr)
	}
	if c.Timeout != time.Second {
		t.Errorf("profile 没写的字段该保留 base 的值，Timeout=%v", c.Timeout)
	}
}

func TestLoad_靠后的profile压过靠前的(t *testing.T) {
	withProfileEnv(t, "a, b")
	base := files(t, "application.yml", map[string]string{
		"application.yml":   "Demo:\n  Addr: base:1\n",
		"application-a.yml": "Demo:\n  Addr: a:1\n  Timeout: 2s\n",
		"application-b.yml": "Demo:\n  Addr: b:1\n",
	})

	list, c := listComps(t)
	if err := Load(base, list); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "b:1" {
		t.Errorf("靠后的 profile 该压过靠前的，Addr=%q", c.Addr)
	}
	if c.Timeout != 2*time.Second {
		t.Errorf("a 写了而 b 没写的字段该保留 a 的，Timeout=%v", c.Timeout)
	}
}

func TestLoad_列表整体替换不逐元素合并(t *testing.T) {
	// 逐元素合并的话 [A,B] 叠上 [C] 会变成 [C,B]——使用者以为换掉了整张表，
	// 实际只换掉第一项，剩下那项来自另一个文件。Spring 也是整体替换
	withProfileEnv(t, "prod")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "Demo:\n  Headers: [A, B]\n",
		"application-prod.yml": "Demo:\n  Headers: [C]\n",
	})

	list, c := listComps(t)
	if err := Load(base, list); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.Headers, []string{"C"}) {
		t.Errorf("列表该被整体替换，got=%v want=[C]", c.Headers)
	}
}

func TestLoad_map递归合并(t *testing.T) {
	withProfileEnv(t, "prod")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "Demo:\n  Nested:\n    A: base-a\n    B: base-b\n",
		"application-prod.yml": "Demo:\n  Nested:\n    A: prod-a\n",
	})

	list, c := listComps(t)
	if err := Load(base, list); err != nil {
		t.Fatal(err)
	}
	if c.Nested.A != "prod-a" || c.Nested.B != "base-b" {
		t.Errorf("map 该递归合并，A=%q B=%q", c.Nested.A, c.Nested.B)
	}
}

func TestLoad_profile文件不存在直接失败(t *testing.T) {
	// 与 Spring 不同：那边静默跳过。点名要了某个 profile 文件却不在，
	// 几乎总是名字写错了，静默跳过的结果是一份谁都没看过的配置以默认值起来
	withProfileEnv(t, "typo")
	base := files(t, "application.yml", map[string]string{
		"application.yml": "Demo:\n  Addr: base:1\n",
	})

	list, _ := listComps(t)
	err := Load(base, list)
	if err == nil {
		t.Fatal("profile 文件不存在该报错")
	}
	if !strings.Contains(err.Error(), "application-typo.yml") {
		t.Errorf("错误里该指出是哪个文件，got=%v", err)
	}
}

func TestLoad_import进来的压过引它的(t *testing.T) {
	// 与 Spring 一致：import 相当于插在声明它的那份文档正下方，下面的压过上面的
	base := files(t, "application.yml", map[string]string{
		"application.yml": "Import: shared.yml\nDemo:\n  Addr: base:1\n  Timeout: 1s\n",
		"shared.yml":      "Demo:\n  Addr: shared:1\n",
	})

	list, c := listComps(t)
	if err := Load(base, list); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "shared:1" {
		t.Errorf("import 进来的该压过引它的文件，Addr=%q", c.Addr)
	}
	if c.Timeout != time.Second {
		t.Errorf("import 没写的字段该保留，Timeout=%v", c.Timeout)
	}
}

func TestLoad_profile压过base的import(t *testing.T) {
	// 顺序：base < base 的 import < profile 文件 < profile 的 import
	withProfileEnv(t, "prod")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "Import: shared.yml\nDemo:\n  Addr: base:1\n",
		"shared.yml":           "Demo:\n  Addr: shared:1\n",
		"application-prod.yml": "Demo:\n  Addr: prod:1\n",
	})

	list, c := listComps(t)
	if err := Load(base, list); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "prod:1" {
		t.Errorf("profile 文件该压过 base 引进来的，Addr=%q", c.Addr)
	}
}

func TestLoad_import多个按顺序生效(t *testing.T) {
	base := files(t, "application.yml", map[string]string{
		"application.yml": "Import:\n  - a.yml\n  - b.yml\nDemo:\n  Addr: base:1\n",
		"a.yml":           "Demo:\n  Addr: a:1\n  Timeout: 3s\n",
		"b.yml":           "Demo:\n  Addr: b:1\n",
	})

	list, c := listComps(t)
	if err := Load(base, list); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "b:1" {
		t.Errorf("靠后的 import 该压过靠前的，Addr=%q", c.Addr)
	}
	if c.Timeout != 3*time.Second {
		t.Errorf("a 写了而 b 没写的字段该保留 a 的，Timeout=%v", c.Timeout)
	}
}

func TestLoad_import的相对路径按引它的文件解析(t *testing.T) {
	// 按进程工作目录解析的话，配置目录整个搬个位置里面的引用就失效了
	base := files(t, "conf/application.yml", map[string]string{
		"conf/application.yml": "Import: parts/db.yml\nDemo:\n  Addr: base:1\n",
		"conf/parts/db.yml":    "Demo:\n  Addr: db:1\n",
	})

	list, c := listComps(t)
	if err := Load(base, list); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "db:1" {
		t.Errorf("相对路径该按引它的文件所在目录解析，Addr=%q", c.Addr)
	}
}

func TestLoad_import文件不存在是错误(t *testing.T) {
	base := files(t, "application.yml", map[string]string{
		"application.yml": "Import: missing.yml\nDemo:\n  Addr: base:1\n",
	})

	list, _ := listComps(t)
	if err := Load(base, list); err == nil {
		t.Fatal("import 的文件不存在该报错")
	}
}

func TestLoad_optional的import可以不存在(t *testing.T) {
	base := files(t, "application.yml", map[string]string{
		"application.yml": "Import: optional:local.yml\nDemo:\n  Addr: base:1\n",
	})

	list, c := listComps(t)
	if err := Load(base, list); err != nil {
		t.Fatalf("optional: 的文件不存在不该报错：%v", err)
	}
	if c.Addr != "base:1" {
		t.Errorf("Addr=%q", c.Addr)
	}
}

func TestLoad_import成环不会转不出来(t *testing.T) {
	base := files(t, "application.yml", map[string]string{
		"application.yml": "Import: a.yml\nDemo:\n  Addr: base:1\n",
		"a.yml":           "Import: application.yml\nDemo:\n  Addr: a:1\n",
	})

	list, c := listComps(t)
	// 同一个文件只算一次，所以环会自己断掉而不是无限递归
	if err := Load(base, list); err != nil {
		t.Fatalf("成环该被同一文件只读一次挡住：%v", err)
	}
	if c.Addr != "a:1" {
		t.Errorf("Addr=%q", c.Addr)
	}
}

func TestLoad_Profiles只能写在base里(t *testing.T) {
	base := files(t, "application.yml", map[string]string{
		"application.yml": "Import: shared.yml\nDemo:\n  Addr: base:1\n",
		"shared.yml":      "Profiles:\n  Active: [prod]\nDemo:\n  Addr: shared:1\n",
	})

	list, _ := listComps(t)
	err := Load(base, list)
	if err == nil {
		t.Fatal("被引进来的文件再去激活 profile 该报错")
	}
	if !strings.Contains(err.Error(), ProfilesKey) {
		t.Errorf("错误里该提到是哪个 key，got=%v", err)
	}
}

func TestLoad_base里的Profiles生效(t *testing.T) {
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "Profiles:\n  Active: [prod]\nDemo:\n  Addr: base:1\n",
		"application-prod.yml": "Demo:\n  Addr: prod:1\n",
	})

	list, c := listComps(t)
	if err := Load(base, list); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "prod:1" {
		t.Errorf("配置文件里声明的 profile 该生效，Addr=%q", c.Addr)
	}
}

func TestLoad_环境变量压过文件里的Profiles(t *testing.T) {
	withProfileEnv(t, "dev")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "Profiles:\n  Active: [prod]\nDemo:\n  Addr: base:1\n",
		"application-dev.yml":  "Demo:\n  Addr: dev:1\n",
		"application-prod.yml": "Demo:\n  Addr: prod:1\n",
	})

	list, c := listComps(t)
	if err := Load(base, list); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "dev:1" {
		t.Errorf("环境变量该压过文件里声明的 profile，Addr=%q", c.Addr)
	}
}

func TestLoad_Import和Profiles不算没人认领的块(t *testing.T) {
	base := files(t, "application.yml", map[string]string{
		"application.yml": "Profiles:\n  Active: []\nImport: optional:x.yml\nDemo:\n  Addr: base:1\n",
	})

	list, _ := listComps(t)
	if err := Load(base, list); err != nil {
		t.Fatalf("这两个是保留 key，不该被当成没人认领：%v", err)
	}
}

func TestLoad_被profile覆盖掉的必填占位符不再要求设置(t *testing.T) {
	// 占位符在全部合并完之后才展开。逐个文件展开的话，
	// base 里那个 ${SECRET} 即便已经被 prod 换掉了，也还是会要求必须设置
	withProfileEnv(t, "prod")
	base := files(t, "application.yml", map[string]string{
		"application.yml":      "Demo:\n  Addr: \"${SECRET}\"\n",
		"application-prod.yml": "Demo:\n  Addr: prod:1\n",
	})

	list, c := listComps(t)
	if err := Load(base, list); err != nil {
		t.Fatalf("已经被覆盖掉的占位符不该再要求设置：%v", err)
	}
	if c.Addr != "prod:1" {
		t.Errorf("Addr=%q", c.Addr)
	}
}

func TestLoad_import路径里的占位符会展开(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "shared.yml"), []byte("Demo:\n  Addr: shared:1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	basePath := filepath.Join(dir, "application.yml")
	if err := os.WriteFile(basePath, []byte("Import: \"${CONF_DIR}/shared.yml\"\nDemo:\n  Addr: base:1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONF_DIR", dir)

	list, c := listComps(t)
	if err := Load(basePath, list); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "shared:1" {
		t.Errorf("import 路径里的占位符该展开，Addr=%q", c.Addr)
	}
}
