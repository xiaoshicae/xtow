package config

import (
	"os"
	"path/filepath"
	"testing"
)

// withArgs 临时替换命令行参数
func withArgs(t *testing.T, args ...string) {
	t.Helper()
	old := os.Args
	os.Args = append([]string{"svc"}, args...)
	t.Cleanup(func() { os.Args = old })
}

func TestLocate_启动参数优先(t *testing.T) {
	withArgs(t, "--config=/a/b.yml")
	t.Setenv(EnvKey, "/from/env.yml")

	if got := Locate(); got != "/a/b.yml" {
		t.Errorf("启动参数应优先于环境变量，got=%q", got)
	}
}

func TestLocate_启动参数两种写法(t *testing.T) {
	t.Run("等号", func(t *testing.T) {
		withArgs(t, "--config=/a.yml")
		if got := Locate(); got != "/a.yml" {
			t.Errorf("got=%q", got)
		}
	})
	t.Run("空格", func(t *testing.T) {
		withArgs(t, "--config", "/b.yml")
		if got := Locate(); got != "/b.yml" {
			t.Errorf("got=%q", got)
		}
	})
	t.Run("单横线", func(t *testing.T) {
		withArgs(t, "-config=/c.yml")
		if got := Locate(); got != "/c.yml" {
			t.Errorf("got=%q", got)
		}
	})
}

func TestLocate_不吃掉别人的参数(t *testing.T) {
	// 使用者的程序有自己的命令行参数，框架不该误读
	withArgs(t, "--port", "8080", "--configx=/x.yml", "--verbose")
	t.Setenv(EnvKey, "")

	if got := Locate(); got != "" && got != SearchPaths[0] {
		t.Errorf("不该把别的参数当成配置路径，got=%q", got)
	}
}

func TestLocate_环境变量次之(t *testing.T) {
	withArgs(t)
	t.Setenv(EnvKey, "/from/env.yml")

	if got := Locate(); got != "/from/env.yml" {
		t.Errorf("got=%q", got)
	}
}

func TestLocate_按约定路径查找(t *testing.T) {
	withArgs(t)
	t.Setenv(EnvKey, "")

	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "conf"), 0o755)
	os.WriteFile(filepath.Join(dir, "conf", "application.yml"), []byte("{}"), 0o600)
	chdir(t, dir)

	if got := Locate(); got != "conf/application.yml" {
		t.Errorf("应命中约定路径，got=%q", got)
	}
}

func TestLocate_都没有时返回空(t *testing.T) {
	withArgs(t)
	t.Setenv(EnvKey, "")
	chdir(t, t.TempDir())

	if got := Locate(); got != "" {
		t.Errorf("找不到时应返回空串，由调用方决定怎么办，got=%q", got)
	}
}

// chdir 切换工作目录，测试结束后切回。
//
// 不用 testing.T.Chdir：它要 Go 1.24，而核心模块的 go 指令定的是所有使用者的
// 语言版本下限——为一个测试助手把下限抬两个版本不值得。
// 代价是没有它对 t.Parallel 的检查，所以用到它的测试不要并行。
func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
}
