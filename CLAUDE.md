# xtow 项目约定

xtow 是一个 Go 三方库集成框架：统一读配置、按阶段初始化、逆序关闭，
使用者拿到的是**原生 client**（`*gorm.DB`、`*redis.Client`、`*gin.Engine`）。

## 语言

| 位置 | 语言 |
|---|---|
| 对话、代码注释、README 与 docs | 简体中文 |
| **error / panic 的消息** | **英文** |
| **日志的 message 与字段名** | **英文** |
| commit message | 英文 |
| 测试函数名、基准函数名 | 中文（描述场景，便于定位） |

这是一个给别人用的库。**它产出的错误和日志会落进使用者的系统里**——
进他们的告警、他们的日志检索、他们的 issue。中文字段名还会变成 JSON 的 key，
让日志平台的索引和看板直接对不上。注释是写给读这份代码的人的，那是另一回事。

```go
// 连接池满了就等着，不新建连接 —— 注释用中文
return fmt.Errorf("xgorm: connect to %s failed: %w", addr, err)   // 错误用英文
slog.Info("xgorm ready", "driver", info.Driver, "addr", info.Addr) // 日志用英文
```

## 优雅优先于快

**不做过度优化。** 代码要保持优雅、整洁、好读——这比省下几十纳秒重要得多。

只有同时满足以下两条才动手优化：

1. **量过**。有基准测试给出改前改后的数字，不接受"看着像是慢"。
2. **改完的代码不比改前复杂**。最好是更简单——把一次多余的分配去掉、
   把白干的活挪到条件后面，这类改动往往同时让代码更清楚。

为了性能引入缓存层、对象池、手写序列化、`unsafe`，一律先说明为什么
没有别的办法。已有的基准测试在 `*_test.go` 里，改热点代码前后各跑一次：

```bash
go test -run=NONE -bench=. -benchtime=100000x ./xlog/ ./xflow/ ./xgin/middleware/
```

## 错误一律用 xerror

模块对外返回的每一个错误都是 `*xerror.Error`，带上模块名和操作名：

```go
return xerror.Newf("xgorm", "connect", "cannot reach %s: %w", info.Addr, err)
return xerror.New("xgorm", "init", err)
```

调用方因此永远可以问「这是谁报的」：

```go
xerror.Is(err, "xconfig")   // 整条链里有没有 xconfig 的错误
xerror.Module(err)          // 最外层是谁报的
```

四条规矩：

1. **底层错误一律用 `%w`，不用 `%v`。** `%v` 把错误变成一段文本，
   `errors.Is` / `errors.As` 到此为止 —— 调用方再也判断不了根因是什么。
2. **一个模块边界一个 xerror，不是一层一个。** 内部的中间错误（比如
   `Config.validate()` 返回的那些）保持普通 error，由边界那一层包一次。
   每层都包的话文本会套成
   `xtow xgin config failed, err=[xtow xgin validate failed, err=[...]]`，
   信息没多，噪声翻倍。
3. **消息里不再重复模块名。** 外框已经有了，再写一遍就是
   `xtow xgorm init failed, err=[xgorm: ...]`。
4. **op 从这组词里选**，不要每处现编：

   | op | 用在 |
   |---|---|
   | `config` | 配置不合法、解码失败 |
   | `init` | 组件初始化（框架调的那次） |
   | `new` | 构造实例 |
   | `connect` | 建连、探测 |
   | `close` | 关闭、释放 |
   | `register` | 注册指标、注册方言 |
   | `start` / `stop` | 服务启停 |
   | `execute` | 跑一次业务流程（xflow） |

## 第三方库的默认值一律要量过

这个仓库被外部 review 挑出来的问题里，**大半是同一个毛病**：接了一个库，
用了它的默认行为，没量过它到底是什么行为，然后按自己以为的那个写进文档。

已经踩过的（每一条都是真的量出来才发现的）：

| 库 | 以为的 | 实际的 |
|---|---|---|
| gin | `TrustedProxies` 默认安全 | 默认 `0.0.0.0/0`，谁发 `X-Forwarded-For` 谁就是 `client_ip` |
| gin | `MaxMultipartMemory` 是请求体上限 | 是落盘阈值，堆开销约为它的三倍；32MB 默认 = 每请求 96MB |
| gorm | 不给 Logger 就是不打日志 | 补上它自己的默认：带 ANSI 颜色写 `os.Stdout` |
| gorm | `gorm.Open` 只装配 | 会自己 ping 一次，用的是它自己的 context |
| go-redis | 命令听调用方的 deadline | 默认不听，只认 `ReadTimeout`；实测 200ms 的预算等满 5s |
| ristretto | `MaxCost` 就是容量 | 每条另加 56 字节内部开销，配 2000 实际存 35 条 |
| resty | `Timeout` 管一次请求 | 管一次尝试；配 300ms + 3 次重试实测跑 1.24s |
| otelhttp | `CloseIdleConnections` 能传下去 | 它没实现，整条调用变成空操作 |
| net/http | `Shutdown` 超时会断开连接 | 只返回错误，在途连接照跑 |

所以接一个新库、或者升级一个库的时候：

1. **写进文档的每一句行为描述，先用一段代码量出来**，别照抄它的 README。
2. **我们没显式设的字段就是我们接受了它的默认值**——列一遍这些字段，
   逐个问「它的默认值是什么，我知道吗」。
3. 量出来的数字写进注释和 `docs/config.md`。后来的人不必再量一次，
   升级依赖之后数字对不上也能立刻看出来。

`./mutate.sh` 是这件事的兜底：把每条承诺对应的代码改坏，看有没有测试会失败。
活下来的变异 = 一条没有牙齿的承诺。改完安全或生命周期相关的代码跑一次。

**重构挪动了代码之后，对应的变异要跟着挪。** 变异模式失效（改不动任何东西）
和「改坏了没人发现」一样严重：那条承诺这一轮根本没被检查。脚本会把这种情况
单独报出来并且整轮失败——它自己就这样烂过两次，`applyConfig` 挪走
`SetTrustedProxies`、xerror 改了 `safeNew` 的签名，对应的变异都从此没再跑过。

所以变异只写 `swap()` / `cut()`（见 `mutation_helpers.py`），它们自带
「模式恰好匹配 N 处」的断言。裸写 `s.replace()` 有两种烂法：模式不再匹配就
静默空转，模式匹配到多处就一次改坏两个地方——后者测试照样会红，
但红的已经不是你要验的那条承诺了。

**变异要打在调用点上，不只是被调用的函数里。** 「指标的 method 标签收敛」
原先只有一个直接调 `normalizeMethod` 的单元测试：函数本身是对的，
但没人验证中间件真的在用它。把调用点绕开（`normalizeMethod(m)` → `m`）
测试照过，而那正是这个 bug 的形状。

## 三条设计原则

1. **`init()` 只登记，不初始化**——真正的初始化由框架按 Stage 档位执行，
   Go 的 init 顺序完全不影响结果。
2. **每个集成是独立的 Go module**——Go 的 MVS 会把整个模块图的版本要求
   强加给使用者，哪怕他一个包都没 import。零依赖的集成才留在核心里。
3. **每个集成必须导出纯构造器 `New`**——不碰全局、不读文件、不依赖框架。
   会阻塞的（建连、探测）多收一个 `ctx`，不会阻塞的不收：签名如实说明
   这个构造器会不会把你卡住。

## 退出信号

注册信号处理会**取消系统默认的「收到就死」**，所以：信号在读配置之前就接管；
`Component.Init` 收 `ctx` 且框架在组件之间复查；第一个信号之后把默认处置
还回去，好让第二个信号能终止卡住的进程。三件事缺一不可，细节见 README。

## 配置

默认值预填在结构体里，未知字段是错误，`${VAR}` 未设置是错误。
新增或改动 Config 字段必须同步 `docs/config.md`（`check.sh` 会检查）。

## 常用命令

```bash
./test.sh          # 全量测试（跨 module，带 -race）
./check.sh         # 架构约束 + 依赖边界 + gofmt/vet
./mutate.sh        # 变异测试：哪些承诺没有测试盯着（要干净工作区，几分钟）
./release.sh vX.Y.Z --apply
```
