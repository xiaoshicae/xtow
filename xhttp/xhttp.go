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
	"github.com/xiaoshicae/xtow/xmetric"
	"github.com/xiaoshicae/xtow/xtrace"
)

// fallbackTimeout 初始化之前或关闭之后，兜底 client 的超时
//
// 零值超时是「永不超时」而不是「有个默认值」：对端不响应时请求会一直挂着。
// 最难受的是关闭阶段——某个组件在关闭时发一个这样的请求，
// 整个进程的退出流程就被卡在那里了。
const fallbackTimeout = 30 * time.Second

// New 按配置建一个 HTTP 客户端，不触碰任何全局变量。
//
// 返回的 io.Closer 释放连接池里的空闲连接。
func New(cfg Config) (*resty.Client, io.Closer, error) {
	if err := cfg.validate(); err != nil {
		return nil, nil, fmt.Errorf("xhttp: 配置有误: %w", err)
	}

	raw := &http.Client{Transport: buildTransport(cfg), Timeout: cfg.Timeout}
	client := resty.NewWithClient(raw)

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
			return nil, nil, fmt.Errorf("xhttp: %w", err)
		}
		installMetrics(client, hist)
	}

	return client, &clientCloser{raw: raw}, nil
}

// buildTransport 组装出站的 Transport 链
//
//	client → xtrace.Transport → otelhttp.Transport → 调好参数的 http.Transport
//
// xtrace.Transport 把目标 host 写进 ctx，按域名透传 Header 的规则才能生效。
// otelhttp 无论链路是否采样都会调用全局 Propagator 注入，所以不需要
// 「链路关了就自己注入」的第二种包装——这一点由本包的测试钉住。
func buildTransport(cfg Config) http.RoundTripper {
	base := tunedTransport(cfg)
	if !cfg.Trace {
		return base
	}
	return &xtrace.Transport{
		Next: otelhttp.NewTransport(base, otelhttp.WithSpanNameFormatter(spanName)),
	}
}

// tunedTransport 从 DefaultTransport 克隆再改，保留 TLS、HTTP/2、代理等默认设置
func tunedTransport(cfg Config) http.RoundTripper {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		slog.Warn("xhttp 无法调整连接池参数", "http.DefaultTransport 的类型", fmt.Sprintf("%T", http.DefaultTransport))
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
	slog.Debug("xhttp 跳过非幂等方法的重试",
		"方法", method, "要允许就配", ConfigKey+".RetryOnlyIdempotent=false")
	return false
}

type clientCloser struct{ raw *http.Client }

func (c *clientCloser) Close() error {
	c.raw.CloseIdleConnections()
	return nil
}

func errorsAs(err error, target any) bool { return errors.As(err, target) }

// ---- 全局实例 ----

var (
	mu       sync.RWMutex
	current  = fallbackClient()
	rawOwned *http.Client
)

// fallbackClient 初始化之前或关闭之后用的兜底 client，带超时
func fallbackClient() *resty.Client { return resty.New().SetTimeout(fallbackTimeout) }

// fallbackRaw 兜底的原生 client。共用一个而不是每次新建：
// 每次新建意味着每次请求都要重新握手，连接池形同虚设。
var fallbackRaw = &http.Client{Timeout: fallbackTimeout}

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
// 初始化之前返回一个带兜底超时的实例，而不是 http.DefaultClient：
// 后者的超时是 0，请求可以永久挂住。
func RawClient() *http.Client {
	mu.RLock()
	defer mu.RUnlock()
	if rawOwned != nil {
		return rawOwned
	}
	return fallbackRaw
}

// ---- 登记 ----

var cfg = DefaultConfig()

// init 只登记，不初始化。真正的初始化由框架在 StageClient 执行。
func init() {
	registry.Register(registry.Component{
		Key:    ConfigKey,
		Stage:  registry.StageClient,
		Config: &cfg,
		Init:   initClient,
	})
}

func initClient() (io.Closer, error) {
	client, closer, err := New(cfg)
	if err != nil {
		return nil, err
	}

	mu.Lock()
	current = client
	rawOwned = client.GetClient()
	mu.Unlock()

	slog.Info("xhttp 就绪", "超时", cfg.Timeout, "每主机空闲连接", cfg.MaxIdleConnsPerHost, "重试", cfg.RetryCount)
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
	rawOwned = nil
	mu.Unlock()
	return c.inner.Close()
}
