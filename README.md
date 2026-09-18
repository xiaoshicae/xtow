# xtow

Go 三方库集成脚手架：**统一管理配置、屏蔽初始化过程、业务拿到的是原生 client**。

```go
package main

import (
	"github.com/xiaoshicae/xtow"

	_ "github.com/xiaoshicae/xtow/xlog"    // 想要哪个组件就 import 哪个
	_ "github.com/xiaoshicae/xtow/xtrace"
	_ "github.com/xiaoshicae/xtow/xmetric"
)

func main() {
	xtow.MustRun(&server{})
}
```

```go
// 业务代码里 —— 用的都是原生 API，不认识本框架的包
slog.InfoContext(ctx, "收到请求")          // 标准库；trace_id 自动带上
ctx, span := otel.Tracer("app").Start(ctx, "干活")
defer xmetric.Timer("handle")()
```

```yaml
# conf/application.yml
App:
  Name: xone.demo.app
XLog:
  Level: info
XMetric:
  Namespace: demo
  ConstLabels:
    env: "${ENV:dev}"
```

没有 `Manage`、没有 `Serve`、没有适配器类型、没有初始化样板。

`example/` 是一个可以直接跑的完整示例：

```bash
go run ./example --config=example/application.yml
```

---

## 三条设计原则

### 一、`init()` 只登记，不初始化

Go 的 `init()` 执行顺序是「拓扑序 + 包路径字典序」，使用者控制不了。所以问题从来不是
`import _` 本身，而是**很多框架把「登记」和「初始化」合成了一件事**，于是那个不可控的
顺序就变成了初始化顺序。

xtow 把它们拆开：`init()` 只把「我是谁、我要哪段配置、怎么初始化我」记进一个列表，
真正的初始化由框架在 `Run()` 里按 **Stage 档位**执行。**Go 的 init 顺序完全不影响结果。**

档位只有四档，不用数字：数字需要全局协调（谁填 30、谁填 50），档位不需要——
同一档里的组件本来就互不依赖。

```
StageLog → StageTelemetry → StageClient → StageServer
```

关闭是严格逆序。

### 二、每个集成是独立的 Go module

这一条直接关系到使用体验。Go 的 MVS 会把**整个模块图**里的版本要求强加给使用者——
哪怕他一个包都没 import。实测：一个只 import 了零依赖包 `xone/xerror` 的应用，
自己写死 `gin v1.9.1`，最终被顶到了 `v1.12.0`，还被定死了 gorm、redis、otel 的版本。

所以：

```
github.com/xiaoshicae/xtow           核心，2 个模块，Go 1.22
├── xapp  xlog  xconfig              零依赖，所以留在核心里
├── xtrace                           独立 module，23 个模块
├── xmetric                          独立 module，35 个模块
├── xcache                           独立 module，11 个模块
├── xhttp                            独立 module，54 个模块
├── xredis                           独立 module，55 个模块
├── xgorm                            独立 module，62 个模块
├── xgin                             独立 module，83 个模块
└── xginswagger                      独立 module，82 个模块
```

`xflow`（流程编排）和 `xlog` 一样零第三方依赖，所以也留在核心里。

**你不用的集成，它的依赖不会进你的模块图**，它要求的 Go 版本也不会。
除核心外的集成都因为上游而需要 Go 1.25，核心留在 1.22。
每个集成也能独立升大版本，不会因为某一个要改 API 就逼着整个框架升级。

零依赖的集成（如 `xlog`）留在核心模块里：分模块是为了把依赖挡在使用者之外，
没有依赖可挡就不必多一个模块。

同一条线也用在模块内部：Swagger UI 要把整套前端资源编进二进制，比只用 gin
多 26 个模块，所以它是 `xginswagger` 而不是 `xgin` 的一部分——
文档是开发期的事，不该让每个线上服务都背着。

注意**测试依赖也算数**：`go mod tidy` 会把它记成直接依赖，一样进使用者的模块图。
实测给 `xtrace` 加一个只用于测试的 `otelhttp`，使用者的模块图从 23 涨到 27。

### 三、每个集成必须导出纯构造器

```go
func New(cfg Config) (*T, io.Closer, error)   // 不碰全局、不读文件、不依赖框架
```

零装配是默认路径，`New()` 保证它**不是唯一路径**：
测试直接调它拿一个干净实例（不需要 mock），需要两套配置时也有出路。

这一条写进了 CI 检查。

### 三个模块共用的那一份

`xgorm` / `xredis` / `xcache` 是同一个形状：配置里可以写一个实例也可以按名字写
好几个，运行时 `C()` 按名字取，启动时挨个建、有一个建不起来就把已建好的全关掉。

这些语义本该处处一致——「名字找不到时说什么」「两种写法混用怎么办」都是一次决定。
分散在三个模块里就是三份会各自漂移的实现，所以收进了核心：

| 共用的东西 | 在哪 |
|---|---|
| 具名实例注册表、建实例、逆序关闭 | `xclient.Registry` / `xclient.Build` |
| 单实例 / 多实例两种写法的解码 | `xconfig.DecodeClients` |
| 带总预算的重试（建连验证用） | `xutil.Retry` |
| 注册指标并断回具体类型 | `xmetric.RegisterAs` |

### 模块之间怎么互相扩展

下层不认识上层。需要上层能力时，下层持有一个函数类型的扩展点，上层注入：

| 扩展点 | 注入方 | 作用 |
|---|---|---|
| `xlog.SetTraceExtractor` | xtrace | 日志自动带上 `trace_id`，而 xlog 不依赖 OpenTelemetry |
| `xlog.AddObserver` | xmetric | 统计错误日志条数，而 xlog 不依赖 Prometheus |

不用「在 `slog.Default()` 外面包一层」的办法：`slog.SetDefault` 会把标准库
`log` 包的输出也接到新 handler 上，链条一旦绕回 slog 自带的 handler 就成环，
卡死在 `log` 包那把不可重入的锁上。让下层自己持有扩展点就没有这个问题。

---

## 配置

一个文件，框架统一读，按顶层 key 分发给各组件。

| 行为 | 说明 |
|---|---|
| 默认值 | 预填在结构体里，文件没写的字段保持不变。**不用 `*bool` 指针** |
| 集合里的默认值 | map / 切片的元素从零值开始解，要默认值就给元素类型写 `UnmarshalYAML`，里面用 `xconfig.DecodeStrict`（**不能用 `node.Decode`，它会丢掉严格检查**） |
| 多实例 | `XGorm.DSN` 是单实例写法，`XGorm.Clients.<名字>` 是多实例写法，两者不能混用 |
| 时间 | 直接用 `time.Duration`，YAML 里写 `30s` / `1500ms` |
| 字段拼错 | **启动失败**，不是静默忽略 |
| 配了但没人认领的块 | **启动失败**，并提示可能是忘了 import 对应的包 |
| `${VAR}` | 必填，未设置则启动失败 |
| `${VAR:default}` | 可选，未设置时用默认值 |

占位符在**解析后的节点上**展开，不是对原始字节做文本替换——否则环境变量的值里
含冒号或换行就会改变 YAML 结构，那是一条注入路径。

配置文件位置：`--config=<path>` > `XTOW_CONFIG` > `conf/application.yml` 等约定路径。
显式指定的文件找不到是错误；约定路径一个都没命中则只告警，全用默认值起。

---

## 仓库结构

```
xtow/
├── xtow.go              根包：Run / MustRun / Option
├── registry/            登记板，零依赖。集成包唯一需要认识的东西
├── internal/config/     配置加载
├── xerror/  xutil/      零第三方依赖
├── xapp/                应用身份（名字、版本），只认领配置不初始化
├── xconfig/             配置解码辅助：严格解码、单/多实例分派
├── xclient/             「一组按名字组织的实例」——xgorm/xredis/xcache 共用
├── xlog/                日志，基于 log/slog，零第三方依赖
├── xtrace/              链路，基于 OpenTelemetry（独立 module）
├── xmetric/             指标，基于 Prometheus（独立 module）
├── xgorm/               数据库，基于 GORM（独立 module）
├── xredis/              Redis，基于 go-redis（独立 module）
├── xcache/              本地缓存，基于 ristretto（独立 module）
├── xhttp/               出站 HTTP，基于 resty（独立 module）
├── xgin/                Web 服务，基于 Gin，内置四个中间件（独立 module）
├── xginswagger/         Swagger UI（独立 module，UI 资源不进普通服务）
├── xflow/               流程编排 + 自动回滚，零第三方依赖
├── docs/config.md       全部配置项参考
├── example/             可直接跑的示例，同时是唯一的跨模块集成测试
├── check.sh             把设计约束编译成检查
└── test.sh              跑全仓库测试（go test ./... 不跨模块边界）
```

`check.sh` 在 CI 里跑：核心模块图不超过 3 个模块、核心不依赖任何集成模块、
registry 与基础包零第三方依赖、`init()` 只出现在集成包里、根包公开 API 不超过 15 个、
集成包必须导出 `New` 且不许 import 根包、**每个配置字段都写进了 `docs/config.md`**。

`release.sh` 打 tag：多模块仓库每个 module 有自己的 tag，
发布前要把开发用的 `replace` 换成真实版本号，且必须按依赖顺序发。
默认只打印要做什么，加 `--apply` 才真的改。

---

## 状态

| 波次 | 内容 | 状态 |
|---|---|---|
| 0 | 地基：registry / config / 根包 / check.sh | 完成 |
| 1 | xapp / xlog / xtrace / xmetric | 完成 |
| 2 | xgorm / xredis / xcache / xhttp | 完成 |
| 3 | xgin 及中间件、xginswagger | 完成 |
| 4 | xflow、文档、CI、发布脚本 | 完成 |
| — | 打 v0.1.0 tag | 待人工确认 |

各模块都还没打 tag，子模块用 `replace` 指向仓库内的相对路径。
