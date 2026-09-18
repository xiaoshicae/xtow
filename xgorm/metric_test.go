package xgorm

import (
	"database/sql"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/xiaoshicae/xtow/xmetric"
)

func TestPoolCollector(t *testing.T) {
	// 连接池状态是瞬时量，所以是抓取时才读，而不是定时推进 Gauge
	m, closer, err := xmetric.New(xmetric.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	stats := map[string]sql.DBStats{
		"main":   {OpenConnections: 7, InUse: 3, Idle: 4, MaxOpenConnections: 50, WaitCount: 2, WaitDuration: 1500 * time.Millisecond},
		"report": {OpenConnections: 1, MaxLifetimeClosed: 9},
	}
	m.Registry.MustRegister(newPoolCollector("demo", nil, func() map[string]sql.DBStats { return stats }))

	w := httptest.NewRecorder()
	m.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	out := w.Body.String()

	for _, want := range []string{
		`demo_db_connections_open{name="main"} 7`,
		`demo_db_connections_in_use{name="main"} 3`,
		`demo_db_connections_idle{name="main"} 4`,
		`demo_db_connections_max_open{name="main"} 50`,
		`demo_db_connections_wait_total{name="main"} 2`,
		`demo_db_connections_wait_duration_seconds_total{name="main"} 1.5`,
		`demo_db_connections_closed_max_lifetime_total{name="report"} 9`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("导出里应有 %s\n实际=\n%s", want, out)
		}
	}
}

func TestPoolCollector_按实例分标签(t *testing.T) {
	// 多实例时必须分得开，否则连接数是几个池子加起来的，看不出是谁满了
	m, closer, _ := xmetric.New(xmetric.Config{})
	defer closer.Close()

	m.Registry.MustRegister(newPoolCollector("", nil, func() map[string]sql.DBStats {
		return map[string]sql.DBStats{"a": {OpenConnections: 1}, "b": {OpenConnections: 2}}
	}))

	w := httptest.NewRecorder()
	m.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	out := w.Body.String()
	if !strings.Contains(out, `db_connections_open{name="a"} 1`) || !strings.Contains(out, `db_connections_open{name="b"} 2`) {
		t.Errorf("每个实例该是独立的时间序列\n实际=\n%s", out)
	}
}

func TestPoolStats_读的是活着的实例(t *testing.T) {
	withClients(t, nil)
	if got := poolStats(); len(got) != 0 {
		t.Errorf("没有实例时应为空，got=%v", got)
	}
}

func TestPoolCollector_带上常量标签(t *testing.T) {
	// 自建指标要和框架内置指标带上同样的环境/集群标签，否则看板上对不起来
	m, closer, _ := xmetric.New(xmetric.Config{})
	defer closer.Close()

	m.Registry.MustRegister(newPoolCollector("", prometheus.Labels{"env": "prod"},
		func() map[string]sql.DBStats { return map[string]sql.DBStats{"a": {OpenConnections: 1}} }))

	w := httptest.NewRecorder()
	m.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if out := w.Body.String(); !strings.Contains(out, `db_connections_open{env="prod",name="a"} 1`) {
		t.Errorf("常量标签应带上\n实际=\n%s", out)
	}
}
