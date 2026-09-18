package xgin

// Option 调整内置中间件的开关与参数。
//
// 没有单独的 options 子包：xgin.New(xgin.WithLog(false)) 比
// xgin.New(options.EnableLogMiddleware(false)) 短，也少一个包要 import。
type Option func(*settings)

type settings struct {
	log        bool
	trace      bool
	metric     bool
	zhTrans    bool
	logBody    bool
	respBody   bool
	skipPaths  []string
	metricPath string
}

func defaultSettings() settings {
	return settings{
		log:        true,
		trace:      true,
		metric:     true,
		metricPath: "/metrics",
	}
}

// WithLog 是否启用访问日志中间件。默认启用。
func WithLog(on bool) Option { return func(s *settings) { s.log = on } }

// WithTrace 是否启用链路中间件。默认启用。
func WithTrace(on bool) Option { return func(s *settings) { s.trace = on } }

// WithMetric 是否启用指标中间件。默认启用。
//
// 启用时会自动注册 /metrics 路由，不需要自己挂。
func WithMetric(on bool) Option { return func(s *settings) { s.metric = on } }

// WithMetricPath 改 /metrics 的路径。默认 /metrics。
func WithMetricPath(p string) Option { return func(s *settings) { s.metricPath = p } }

// WithSkipPaths 指定不记访问日志的路径。
//
// 以 / 结尾的按前缀匹配，其余精确匹配。/metrics 会自动加进来，不用自己写。
func WithSkipPaths(paths ...string) Option {
	return func(s *settings) { s.skipPaths = append(s.skipPaths, paths...) }
}

// WithRequestBodyLog 是否把请求体记进访问日志。默认不记。
//
// 记 body 要缓存整个请求体并对每个字段做脱敏，代价和风险都不小。
// 打开前先确认敏感字段配全了（middleware.AddSensitiveFields）。
func WithRequestBodyLog(on bool) Option { return func(s *settings) { s.logBody = on } }

// WithResponseBodyLog 是否把响应体记进访问日志。默认不记。
func WithResponseBodyLog(on bool) Option { return func(s *settings) { s.respBody = on } }

// WithZHTranslations 是否把 validator 的报错翻成中文。默认不翻。
func WithZHTranslations(on bool) Option { return func(s *settings) { s.zhTrans = on } }
