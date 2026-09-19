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
| 占位符的类型 | 按替换后的内容判定，`Port: ${PORT:8080}` 进的是 int 字段。加了引号就固定按字符串处理，数字形态的密码用 `"${PW}"` |
| 超时写 0 | **不是「不限时」而是「一点都不等」**。`XGin.ShutdownTimeout`、`XTrace.ShutdownTimeout`、`XFlow.RollbackTimeout` 写 0 直接启动失败 |
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
  SampleRatio: 1           # >= 1 全采样；0 是「不采样但照常生成透传 TraceID」
                           # 要连 Span 都不产生请用 Enable: false，那是另一件事
  ShutdownTimeout: 5s      # 退出时等导出完成的上限，默认 5s，必须 > 0
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
  Driver: postgres         # mysql / postgres 内置，默认 postgres；其余驱动见下
  DSN: "${DB_DSN}"         # 必填
  DialTimeout: 500ms       # 也是 MySQL 那一小段不可取消的建连的上限，见下
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

MySQL 有一处已知限制：GORM 的 MySQL Dialector 在初始化时会查一次
`SELECT VERSION()`，驱动那行写死了 `context.Background()`，所以这一段
建连不受退出信号控制，上限是 `DialTimeout`（默认 500ms）。
不关掉它是因为 GORM 靠这个版本号决定 MariaDB / MySQL 5.7 上的一堆行为
（改索引、改列、`FOR SHARE`、`RETURNING`），为省这半秒换一组静默的行为变化
不划算。在意的话把 `DialTimeout` 调小。PostgreSQL 与 ClickHouse 没有这个问题。

### 其它驱动

mysql 和 postgres 内置，其余驱动住在自己的 module 里，匿名 import 一行就注册好：

```go
import (
	"github.com/xiaoshicae/xtow/xgorm"
	_ "github.com/xiaoshicae/xtow/xgorm/clickhouse"
)
```

```yaml
XGorm:
  Driver: clickhouse
  DSN: "${CH_DSN}"         # clickhouse://user:pass@host:9000/db
  DialTimeout: 500ms       # 注入 DSN 的 dial_timeout，DSN 里已写的不覆盖
```

拿到的仍然是原生的 `*gorm.DB`，配置项和多实例写法都一样。

为什么不直接放进 xgorm：实测一个只 import xgorm 的应用模块图是 65 个，
加上 ClickHouse 驱动变成 146 个（编译包 140 → 183）。多出来的大头是 Docker 和
testcontainers —— `clickhouse-go` 用它们跑集成测试，而 `go.mod` 分不出
「只测试用」。Go 的 MVS 按模块图把版本要求强加给使用者，不用它的人不该付这个钱。

驱动名写错或忘了 import 时启动会失败，错误里列出当前注册了哪些；
也可以用 `xgorm.Drivers()` 自己查。

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
  TrustedProxies: []       # 信任哪些代理发来的 X-Forwarded-For / X-Real-IP，默认一个都不信
                           # gin 自己的默认是「全都信」，那样任何人发一个
                           # X-Forwarded-For 就能决定访问日志里的 client_ip 是什么，
                           # 建在这个字段上的限流和审计跟着一起失效。
                           # 真在负载均衡后面时写它那一段网段：["10.0.0.0/8"]
                           # 写错的网段会直接启动失败，不会只生效一半
  ShutdownTimeout: 10s     # HTTP 服务能占的那一份，必须 > 0
                           # 到点仍有在途请求时会强制断连：Shutdown 只返回错误、
                           # 不动那些连接，不补这一刀的话 handler 会在框架关掉
                           # 数据库之后继续访问它
                           # 整个退出流程的总预算是 xtow.WithStopTimeout（默认 15s），
                           # 两者取更早的那个截止时间，所以这项配得比总预算大没有意义
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

---

## 写一个自己的集成

第三方集成只需要认识 `registry`，外加两个可选的辅助包。

最小形态（单实例、无外部资源）：

```go
const ConfigKey = "XMine"

type Config struct {
    Addr string `yaml:"Addr"`
}

func DefaultConfig() Config { return Config{Addr: "127.0.0.1:1234"} }

var cfg = DefaultConfig()

func New(c Config) (*Client, io.Closer, error) { /* 纯构造，不碰全局 */ }

func init() {
    registry.Register(registry.Component{
        Key:    ConfigKey,
        Stage:  registry.StageClient,
        Config: &cfg,
        Init:   func() (io.Closer, error) { /* 建实例、发布到全局 */ },
    })
}
```

要支持「单实例 / 多实例两种写法」就再加两样：

```go
// 配置分派
func (c *Config) UnmarshalYAML(n *yaml.Node) error {
    clients, err := xconfig.DecodeClients(n, DefaultClientConfig)
    if err != nil {
        return err
    }
    c.Clients = clients
    return nil
}

// 集合元素自己铺默认值：map 的 value 从零值开始解，框架替不了它。
// 这里必须用 xconfig.DecodeStrict 而不是 n.Decode——后者会丢掉严格检查，
// 于是「字段拼错就启动失败」在集合里悄悄失效
func (c *ClientConfig) UnmarshalYAML(n *yaml.Node) error {
    *c = DefaultClientConfig()
    type raw ClientConfig // 换个类型，否则这里递归调用自己
    return xconfig.DecodeStrict(n, (*raw)(c))
}

// 具名实例注册表
var reg = xclient.NewRegistry[*Client]("xmine", ConfigKey)

func C(name ...string) *Client { return reg.Get(name...) }
func Has(name ...string) bool  { return reg.Has(name...) }
func Names() []string          { return reg.Names() }

func initAll() (io.Closer, error) { return xclient.Build(reg, cfg.Clients, New) }
```

两条硬性要求（`check.sh` 会查）：

- 必须导出纯构造器 `New`——零装配是默认路径，不能是唯一路径
- 不许 import 根包，只能 import `registry`：依赖是单向的
