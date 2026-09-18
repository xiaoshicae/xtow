# 配置参考

一个文件，框架统一读，按顶层 key 分发给各组件。没配的块就是没启用——
除了 `XHttp`，它不连任何外部资源，不配也会给你一个可用的客户端。

配置文件位置：`--config=<path>` > `XTOW_CONFIG` > `conf/application.yml` 等约定路径。

## 通用规则

| 规则 | 说明 |
|---|---|
| 默认值 | 预填在结构体里，文件没写的字段保持不变。没有 `*bool` 指针，`Enable: false` 就是 false |
| 时间 | 直接写 `30s` / `1500ms` / `1h30m`。写裸数字会启动失败——写 `30` 的人想要 30 秒，Go 的零值语义会给他 30 纳秒 |
| 字段拼错 | **启动失败**，不是静默忽略 |
| 多配了没人认领的块 | **启动失败**，并提示可能是忘了 import 对应的包 |
| `${VAR}` | 必填，未设置则启动失败。凭证类配置都该写成这个形式 |
| `${VAR:default}` | 可选，未设置时用默认值 |
| 列表字段 | 文件里写了就整体替换，不会和默认值混在一起 |
| map 字段 | 文件里写的是**合并**进默认值，所以框架的 map 字段一律没有默认值 |

## App —— 应用身份

链路的 `service.name`、指标的默认标题都取自这里。放在一处，免得各配一遍再对不上。

```yaml
App:
  Name: xone.demo.app      # 建议 team.system.app，默认空
  Version: v1.2.0          # 默认空
```

## XLog —— 日志

装进标准库的 `slog.Default()`，业务代码直接用 `slog.InfoContext` 即可。

```yaml
XLog:
  Level: info              # debug / info / warn / error，默认 info
  Format: json             # json / text，默认 json
  AddSource: false         # 是否记代码位置，有开销，默认关
  Console: true            # 打到标准输出，由部署环境的采集组件收集，默认开
  File:
    Enable: false          # 默认关
    Path: /var/log/app     # 目录
    Name: app.log          # 实际文件是 app.log.20260918，另有同名符号链接指向当前文件
    RotateTime: 24h        # 轮转周期，按本地时区对齐，默认一天
    MaxAge: 168h           # 历史保留时长，<=0 表示不清理，默认 7 天
    Perm: "0644"           # 用字符串：YAML 里写 0644 会被当成十进制 644
```

## XTrace —— 链路

装好 OpenTelemetry 的全局 TracerProvider 和 Propagator，业务代码用原生的
`otel.Tracer("...")`。框架不内置任何上报 exporter（OTLP 一个就带进上百个构建依赖），
需要上报的服务自己 `xtrace.AddSpanProcessor(...)`。

```yaml
XTrace:
  Enable: true             # 默认开。关掉后 Span 是 noop，但 Header 透传照常生效
  Console: false           # 把 Span 打到标准输出，本地调试用，默认关
  SampleRatio: 1           # (0, 1]，>= 1 全采样。要关链路请用 Enable: false
  ShutdownTimeout: 5s      # 退出时等导出完成的上限，默认 5s
  ForwardHeaders:          # 向所有下游透传的 Header，默认无
    - X-Request-Id
  ForwardHeaderRules:      # 只发给匹配域名的 Header，默认无
    - Domains: ["api.internal.com", "*.trusted.com"]
      Headers: ["X-Internal-Token"]
```

同一个 header 同时出现在 `ForwardHeaders` 和 `ForwardHeaderRules` 里会**启动失败**：
一边说发给所有人、一边说只发给这些人，猜哪边都可能把内部标识发给第三方。

## XMetric —— 指标

```yaml
XMetric:
  Namespace: myapp         # 指标名前缀，默认无
  ConstLabels:             # 附加到所有指标上，默认无
    env: "${ENV:dev}"
  HTTPDurationBuckets: []  # 出入站 HTTP 耗时的桶（秒），默认 [0.001 … 10]
  HistogramBuckets: []     # 快捷方法建的业务直方图的桶，默认 prometheus.DefBuckets
  GoMetrics: true          # Go 运行时指标，默认开
  ProcessMetrics: true     # 进程指标，默认开
  LogErrorMetric: true     # Error 级别日志计入 log_errors_total，默认开（需配合 xlog）
```

## XGorm —— 数据库

两种写法。单实例：

```yaml
XGorm:
  Driver: postgres         # mysql / postgres，默认 postgres
  DSN: "${DB_DSN}"         # 必填
  DialTimeout: 500ms
  MaxOpenConns: 50
  MaxIdleConns: 50         # 配 0 就是一条空闲连接都不留
  MaxLifetime: 5m
  MaxIdleTime: 5m
  Log: false               # 把 SQL 接到 slog 上，默认关（一条 SQL 一行日志）
  SlowThreshold: 3s        # 超过就记 warn，需 Log 开启
  IgnoreNotFound: false    # 「没查到记录」是否不当错误
  Trace: true
  Metric: true
  MySQL:                   # 仅 Driver: mysql 生效
    ReadTimeout: 3s
    WriteTimeout: 5s
  Postgres:                # 仅 Driver: postgres 生效，下列值随建连发给服务端成为会话级 GUC
    StatementTimeout: 0s   # 默认不限制
    LockTimeout: 0s
    IdleInTxTimeout: 0s
    Params: {}             # 任意 PG 运行时参数，同名时以它为准
```

多实例：

```yaml
XGorm:
  Clients:
    default: {DSN: "${DB_DSN}"}            # C() 取的就是这个
    report:  {DSN: "${REPORT_DSN}", MaxOpenConns: 5}
```

两种写法不能混用。DSN 里已经写了的 timeout 之类的参数不会被配置覆盖——
配置里的值只是默认值。

## XRedis

同样支持单实例 / 多实例两种写法。

```yaml
XRedis:
  Addr: "127.0.0.1:6379"
  Username: ""
  Password: "${REDIS_PASSWORD}"
  DB: 0
  DialTimeout: 500ms
  ReadTimeout: 500ms
  WriteTimeout: 500ms
  PoolSize: 0              # 0 交给 go-redis（10 × GOMAXPROCS）
  MinIdleConns: 5
  MaxIdleConns: 0          # 0 不限制
  MaxActiveConns: 0
  PoolTimeout: 1s
  ConnMaxIdleTime: 5m
  ConnMaxLifetime: 5m
  MaxRetries: 0            # 0 交给 go-redis（3 次），-1 关闭
  MinRetryBackoff: 0s      # 同上，-1ns 关闭
  MaxRetryBackoff: 0s
  Trace: true
  Metric: true
```

## XCache —— 本地缓存

```yaml
XCache:
  NumCounters: 1000000     # 频率计数器个数，建议取预期条目数的 10 倍
  MaxCost: 100000          # 总成本上限。本包写入时 cost 固定为 1，所以等价于条目数
  BufferItems: 64
  DefaultTTL: 5m           # 包级 Set 用的过期时间
```

## XHttp —— 出站 HTTP

不配也能用，默认值就是一份合理的配置。

```yaml
XHttp:
  Timeout: 60s             # 配 0 是「永不超时」，不是「用默认值」
  DialTimeout: 30s
  DialKeepAlive: 30s
  MaxIdleConns: 100
  MaxIdleConnsPerHost: 10  # 标准库默认只有 2，对只调几个下游的服务太小
  IdleConnTimeout: 90s
  RetryCount: 0            # 默认不重试
  RetryWaitTime: 100ms
  RetryMaxWaitTime: 2s
  RetryOnlyIdempotent: true  # 默认只重试幂等方法，见下
  Trace: true
  Metric: true
```

`RetryOnlyIdempotent` 默认开着：传输层超时分不出「请求没到服务端」和
「服务端处理完了但响应丢了」，重发一个 POST 就可能变成重复下单。
确认接口幂等（比如带幂等键）之后再关掉它。

## XGin —— Web 服务

```yaml
XGin:
  Host: "0.0.0.0"
  Port: 8080
  Mode: release            # release / debug / test，默认 release
  UseH2C: false            # 非 TLS 下启用 HTTP/2。TLS 模式下 HTTP/2 本来就是自动的
  CertFile: ""             # 与 KeyFile 必须同时配或同时留空，只配一半会启动失败
  KeyFile: ""
  ReadHeaderTimeout: 10s   # 慢连接攻击的主要防线
  ReadTimeout: 0s          # 默认不限制：限制它会打断大文件上传
  WriteTimeout: 0s         # 默认不限制：限制它会打断 SSE、长轮询、大文件下载
  IdleTimeout: 60s
  ShutdownTimeout: 25s     # 要小于部署环境的终止宽限期（K8s 默认 30s）
```

中间件的开关不在配置里，在代码里：

```go
xgin.New(
    xgin.WithLog(true),
    xgin.WithSkipPaths("/health", "/internal/"),
    xgin.WithRequestBodyLog(false),   // 默认关，见下
)
```

请求/响应体日志默认关闭。打开它意味着每个请求都要缓存一份 body、逐字段脱敏，
而漏配一个字段名就是凭证明文落盘。打开前用
`middleware.AddSensitiveFields(...)` 把业务自己的敏感字段补上。

## XGinSwagger

单独一个 module，UI 资源不进普通服务。

```yaml
XGinSwagger:
  Host: api.example.com    # 文档里显示的地址
  BasePath: /api/v1
  Title: ""                # 默认取 App.Name
  Description: ""
  Schemes: ["https", "http"]
  URLPrefix: ""            # UI 挂载路径前缀，默认挂在 /swagger/*any
```

## XFlow —— 流程编排

```yaml
XFlow:
  Monitor: true            # 关掉之后 Execute 一次监控回调都不走
  RollbackTimeout: 30s     # 回滚全部步骤的总预算，必须大于 0
```

回滚不沿用调用方的 context，否则请求一超时补偿必然全部失败——而补偿最需要
执行的时机恰恰就是那时候。它由 `RollbackTimeout` 单独限时。
