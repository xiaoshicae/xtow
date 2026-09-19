package xhttp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-resty/resty/v2"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/xiaoshicae/xtow/registry"
	"github.com/xiaoshicae/xtow/xerror"
	"github.com/xiaoshicae/xtow/xmetric"
	"github.com/xiaoshicae/xtow/xtrace"
)

// fallbackTimeout 初始化之前或关闭之后，兜底 client 的超时
//
// 零值超时是「永不超时」而不是「有个默认值」：对端不响应时请求会一直挂着。
// 最难受的是关闭阶段——某个组件在关闭时发一个这样的请求，
// 整个进程的退出流程就被卡在那里了。
const fallbackTimeout = 30 * time.Second

// New 按配置建一个 HTTP 客户端，不碰本包的全局实例。
//
// 一个例外：cfg.Metric 开着时（默认开着）耗时直方图要注册到 xmetric
// 的全局 Registry —— 指标本来就只有一份，注册到别处就导不出去。
// 不想碰它就把 cfg.Metric 关掉。
//
// 返回的 io.Closer 释放连接池里的空闲连接。
func New(cfg Config) (*resty.Client, io.Closer, error) {
	if err := cfg.validate(); err != nil {
		return nil, nil, xerror.Newf("xhttp", "config", "invalid config: %w", err)
	}

	// 自己抓住连接池那一层，不指望 http.Client.CloseIdleConnections 找得到它。
	//
	// 那个方法是靠类型断言往下找的：链路开着时中间隔着 otelhttp.Transport，
	// 而它没有实现这个方法，断言到那里就断了——整条调用变成空操作，
	// 而链路默认是开着的。自己持有，关的时候直接关它。
	pool := tunedTransport(cfg)
	client := resty.NewWithClient(&http.Client{
		Transport: traced(cfg, pool),
		Timeout:   cfg.Timeout,
	})

	if cfg.RetryCount > 0 {
		client.SetRetryCount(cfg.RetryCount).
			SetRetryWaitTime(cfg.RetryWaitTime).
			SetRetryMaxWaitTime(cfg.RetryMaxWaitTime)
		if cfg.RetryOnlyIdempotent {
			client.AddRetryCondition(retryOnlyIdempotent)
		}
	}

	if cfg.Metric {
		// 用 RegisterAs 的返回值：重复注册时它给的是已有那个实例，
		// 记到新建的那个上会永远导不出去
		hist, err := xmetric.RegisterAs(newDurationHistogram())
		if err != nil {
			return nil, nil, xerror.New("xhttp", "register", err)
		}
		installMetrics(client, hist)
	}

	return client, &clientCloser{pool: pool}, nil
}

// traced 在连接池外面包上链路那两层
//
//	client → xtrace.Transport → otelhttp.Transport → 调好参数的 http.Transport
//
// xtrace.Transport 把目标 host 写进 ctx，按域名透传 Header 的规则才能生效。
// otelhttp 无论链路是否采样都会调用全局 Propagator 注入，所以不需要
// 「链路关了就自己注入」的第二种包装——这一点由本包的测试钉住。
func traced(cfg Config, pool http.RoundTripper) http.RoundTripper {
	if !cfg.Trace {
		return pool
	}
	return &xtrace.Transport{
		Next: otelhttp.NewTransport(pool, otelhttp.WithSpanNameFormatter(spanName)),
	}
}

// tunedTransport 从 DefaultTransport 克隆再改，保留 TLS、HTTP/2、代理等默认设置
func tunedTransport(cfg Config) http.RoundTripper {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		slog.Warn("xhttp cannot tune the connection pool", "default_transport_type", fmt.Sprintf("%T", http.DefaultTransport))
		return http.DefaultTransport
	}

	t = t.Clone()
	t.MaxIdleConns = cfg.MaxIdleConns
	t.MaxIdleConnsPerHost = cfg.MaxIdleConnsPerHost
	t.IdleConnTimeout = cfg.IdleConnTimeout
	t.DialContext = (&net.Dialer{Timeout: cfg.DialTimeout, KeepAlive: cfg.DialKeepAlive}).DialContext
	return t
}

// spanName Span 命名为「方法 路径」，不带 query——query 里常有 id 和令牌，
// 放进 Span 名会把基数撑爆，也会把敏感值带出去
func spanName(_ string, r *http.Request) string {
	return r.Method + " " + r.URL.Path
}

// idempotentMethods 可以安全重试的方法（RFC 9110 的幂等方法）
var idempotentMethods = map[string]struct{}{
	http.MethodGet: {}, http.MethodHead: {}, http.MethodOptions: {},
	http.MethodTrace: {}, http.MethodPut: {}, http.MethodDelete: {},
}

// retryOnlyIdempotent 只让幂等方法重试
//
// resty 默认「传输层出错就重试」，不看方法。但超时分不出
// 「请求没到服务端」和「服务端处理完了但响应丢了」，重发一个 POST
// 就可能变成重复下单。
func retryOnlyIdempotent(resp *resty.Response, err error) bool {
	if err == nil {
		return false // 拿到响应就不重试，与 resty 的默认条件一致
	}
	method := ""
	if resp != nil && resp.Request != nil {
		method = strings.ToUpper(resp.Request.Method)
	}
	if _, ok := idempotentMethods[method]; ok {
		return true
	}
	slog.Debug("xhttp skipped retrying a non-idempotent method",
		"method", method, "to_allow_set", ConfigKey+".RetryOnlyIdempotent=false")
	return false
}

// clientCloser 持有连接池本身，而不是外面那个 http.Client
type clientCloser struct{ pool http.RoundTripper }

func (c *clientCloser) Close() error {
	if p, ok := c.pool.(interface{ CloseIdleConnections() }); ok {
		p.CloseIdleConnections()
	}
	return nil
}

func errorsAs(err error, target any) bool { return errors.As(err, target) }

// ---- 全局实例 ----

var (
	mu      sync.RWMutex
	current = fallbackClient()
)

// fallbackClient 初始化之前或关闭之后用的兜底 client，带超时
func fallbackClient() *resty.Client { return resty.New().SetTimeout(fallbackTimeout) }

// C 取 resty client。
//
// 不像 xgorm / xredis 那样取不到就 panic：HTTP 客户端不连任何外部资源，
// 没配也能用，所以这里任何时候都返回一个可用的实例。
// 初始化之前拿到的是带 30s 超时的兜底实例。
func C() *resty.Client {
	mu.RLock()
	defer mu.RUnlock()
	return current
}

// R 开一个绑定了 ctx 的请求，链路和超时才能传到下游。
//
//	resp, err := xhttp.R(ctx).SetResult(&out).Get(url)
func R(ctx context.Context) *resty.Request { return C().R().SetContext(ctx) }

// RawClient 取底层的原生 *http.Client。
//
// 用于需要自己处理响应体的场景，比如 SSE 这类流式请求——
// resty 会把响应整个读进内存，那对流式接口是不对的。
//
// 初始化之前返回兜底实例内部的那个，而不是 http.DefaultClient：
// 后者的超时是 0，请求可以永久挂住。
//
// 直接从 C() 取而不是另存一份：两份关联状态要同步维护，
// 而 resty 的 GetClient 返回的就是它一直在用的那个，取值一致、生命周期一致。
func RawClient() *http.Client { return C().GetClient() }

// ---- 登记 ----

var cfg = DefaultConfig()

// init 只登记，不初始化。真正的初始化由框架在 StageClient 执行。
func init() {
	registry.Register(registry.Component{
		Key:    ConfigKey,
		Stage:  registry.StageClient,
		Config: &cfg,
		Init:   func(ctx context.Context) (io.Closer, error) { return initClient(ctx, cfg) },
	})
}

func initClient(_ context.Context, c Config) (io.Closer, error) {
	client, closer, err := New(c)
	if err != nil {
		return nil, err
	}

	mu.Lock()
	current = client
	mu.Unlock()

	slog.Info("xhttp ready", "timeout", c.Timeout, "max_idle_conns_per_host", c.MaxIdleConnsPerHost, "retries", c.RetryCount)
	return &resetCloser{inner: closer}, nil
}

// resetCloser 关闭时把全局实例换回兜底的那个
//
// 换回去而不是置空：关闭阶段仍可能有组件发请求，让它带着超时失败，
// 好过在一个 nil 上空指针，或者永远挂着。
type resetCloser struct{ inner io.Closer }

func (c *resetCloser) Close() error {
	mu.Lock()
	current = fallbackClient()
	mu.Unlock()
	return c.inner.Close()
}
