package xutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileExist(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	os.WriteFile(f, []byte("x"), 0o600)

	if !FileExist(f) {
		t.Error("存在的文件应返回 true")
	}
	if FileExist(dir) {
		t.Error("目录不是文件，应返回 false")
	}
	if FileExist(filepath.Join(dir, "nope")) {
		t.Error("不存在的路径应返回 false")
	}
}

func TestDirExist(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	os.WriteFile(f, []byte("x"), 0o600)

	if !DirExist(dir) {
		t.Error("存在的目录应返回 true")
	}
	if DirExist(f) {
		t.Error("文件不是目录，应返回 false")
	}
}

func TestToPtr(t *testing.T) {
	p := ToPtr(42)
	if p == nil || *p != 42 {
		t.Fatalf("ToPtr(42) 应指向 42，got=%v", p)
	}
}

func TestGetOrDefault(t *testing.T) {
	cases := []struct{ v, def, want string }{
		{"", "d", "d"},
		{"v", "d", "v"},
	}
	for _, c := range cases {
		if got := GetOrDefault(c.v, c.def); got != c.want {
			t.Errorf("GetOrDefault(%q,%q)=%q want %q", c.v, c.def, got, c.want)
		}
	}
	if got := GetOrDefault(0, 5); got != 5 {
		t.Errorf("零值应返回默认值，got=%d", got)
	}
}
