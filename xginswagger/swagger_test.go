package xginswagger

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/swaggo/swag"

	"github.com/xiaoshicae/xtow/internal/config"
	"github.com/xiaoshicae/xtow/registry"
)

func init() { gin.SetMode(gin.TestMode) }

// loadAll 走真实的配置加载路径，把 YAML 灌进各组件，测试结束后还原
//
// 各组件的配置是包级变量，测试之间会互相污染，所以先存一份再改
func loadAll(t *testing.T, yml string) {
	t.Helper()
	list := registry.Snapshot()

	saved := make([]reflect.Value, len(list))
	for i, c := range list {
		cur := reflect.ValueOf(c.Config).Elem()
		saved[i] = reflect.New(cur.Type()).Elem()
		saved[i].Set(cur)
	}
	t.Cleanup(func() {
		for i, c := range list {
			reflect.ValueOf(c.Config).Elem().Set(saved[i])
		}
	})

	path := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(path, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := config.Load(path, list); err != nil {
		t.Fatalf("加载失败：%v", err)
	}
}

func withConfig(t *testing.T, c Config) {
	t.Helper()
	old := cfg
	t.Cleanup(func() { cfg = old })
	cfg = c
}

func TestRegister_挂上UI(t *testing.T) {
	withConfig(t, DefaultConfig())
	e := gin.New()
	Register(e, nil)

	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest("GET", "/swagger/index.html", nil))
	if w.Code != http.StatusOK {
		t.Errorf("Swagger UI 应当可访问，got=%d", w.Code)
	}
}

func TestRegister_可以加前缀(t *testing.T) {
	c := DefaultConfig()
	c.URLPrefix = "/internal"
	withConfig(t, c)

	e := gin.New()
	Register(e, nil)

	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest("GET", "/internal/swagger/index.html", nil))
	if w.Code != http.StatusOK {
		t.Errorf("应挂在配置的前缀下，got=%d", w.Code)
	}
	if !strings.HasPrefix(URL(), "/internal/") {
		t.Errorf("URL 应带上前缀，got=%s", URL())
	}
}

func TestRegister_nil引擎不炸(t *testing.T) {
	withConfig(t, DefaultConfig())
	Register(nil, nil)
}

func TestFill_只覆盖配了的字段(t *testing.T) {
	// 空的配置块不该把注解里写好的文档标题清成空白
	withConfig(t, Config{})
	info := &swag.Spec{
		Title:       "注解里的标题",
		Description: "注解里的描述",
		BasePath:    "/v1",
		Host:        "annotated.example.com",
		Schemes:     []string{"http"},
	}
	fill(info)

	if info.Description != "注解里的描述" || info.BasePath != "/v1" || info.Host != "annotated.example.com" {
		t.Errorf("没配的字段应保留注解里的值，got=%+v", info)
	}
	if len(info.Schemes) != 1 || info.Schemes[0] != "http" {
		t.Errorf("没配 Schemes 就该保留注解里的，got=%v", info.Schemes)
	}
}

func TestFill_配了就覆盖(t *testing.T) {
	withConfig(t, Config{
		Host: "api.example.com", BasePath: "/api/v2",
		Title: "配置里的标题", Description: "配置里的描述",
		Schemes: []string{"https"},
	})
	info := &swag.Spec{Title: "注解里的标题", Host: "old"}
	fill(info)

	if info.Host != "api.example.com" || info.BasePath != "/api/v2" {
		t.Errorf("配了就该覆盖，got=%+v", info)
	}
	if info.Title != "配置里的标题" || info.Description != "配置里的描述" {
		t.Errorf("标题和描述应被覆盖，got=%+v", info)
	}
	if len(info.Schemes) != 1 || info.Schemes[0] != "https" {
		t.Errorf("Schemes 应被覆盖，got=%v", info.Schemes)
	}
}

func TestFill_标题与版本默认跟App走(t *testing.T) {
	// 同一个事实配两遍迟早会不一致
	loadAll(t, "App:\n  Name: xone.demo.app\n  Version: v2.3.4\nXGinSwagger:\n  BasePath: /api\n")

	info := &swag.Spec{}
	fill(info)
	if info.Title != "xone.demo.app" {
		t.Errorf("没配标题时应取 App.Name，got=%q", info.Title)
	}
	if info.Version != "v2.3.4" {
		t.Errorf("版本应取 App.Version，got=%q", info.Version)
	}
	if info.BasePath != "/api" {
		t.Errorf("BasePath 应生效，got=%q", info.BasePath)
	}
}

func TestRegister_登记内容与框架对得上(t *testing.T) {
	var got *registry.Component
	for _, c := range registry.Snapshot() {
		if c.Key == ConfigKey {
			got = &c
			break
		}
	}
	if got == nil {
		t.Fatalf("没有以 %s 登记", ConfigKey)
	}
	if got.Config != &cfg {
		t.Error("登记的必须是包级配置变量本身")
	}
	if got.Init != nil {
		t.Error("本包没有要初始化的东西，登记 Init 会让它出现在关闭序列里")
	}
}

func TestConfig_默认协议(t *testing.T) {
	c := DefaultConfig()
	if len(c.Schemes) != 2 || c.Schemes[0] != "https" {
		t.Errorf("默认应为 https 优先，got=%v", c.Schemes)
	}
}
