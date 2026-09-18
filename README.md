# xtow

Go 三方库集成脚手架：**统一管理配置、屏蔽初始化过程、业务拿到的是原生 client**。

```go
package main

import (
	"github.com/xiaoshicae/xtow"

	_ "github.com/xiaoshicae/xtow/xgorm"  // 想要哪个组件就 import 哪个
	_ "github.com/xiaoshicae/xtow/xredis"
)

func main() {
	xtow.MustRun(&server{})
}
```

```go
// 业务代码里 —— xgorm.C() 返回的就是 *gorm.DB，没有任何包装
err := xgorm.C().WithContext(ctx).First(&u, id).Error
```

```yaml
# conf/application.yml
XGorm:
  DSN: "${DB_DSN}"              # 必填，没设置就启动失败
  MaxOpenConns: 50
XRedis:
  Addr: "${REDIS_ADDR:127.0.0.1:6379}"
```

没有 `Manage`、没有 `Serve`、没有适配器类型、没有初始化样板。

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
github.com/xiaoshicae/xtow          核心，依赖只有 yaml
github.com/xiaoshicae/xtow/xgorm    独立 module，依赖 gorm
github.com/xiaoshicae/xtow/xredis   独立 module，依赖 go-redis
...
```

**你不用的集成，它的依赖不会进你的模块图。** 每个集成也能独立升大版本，
不会因为某个集成要改 API 就逼着整个框架升级。

### 三、每个集成必须导出纯构造器

```go
func New(cfg Config) (*gorm.DB, error)   // 不碰全局、不读文件、不依赖框架
func C(name ...string) *gorm.DB          // 全局便利层，是 New 的薄封装
```

`C()` 是默认路径，99% 的场景只用它。`New()` 保证**零装配永远只是默认路径，不是唯一路径**：
测试直接调它拿干净实例（不需要 mock），需要两套配置时也有出路。

这一条写进了 CI 检查。

---

## 配置

一个文件，框架统一读，按顶层 key 分发给各组件。

| 行为 | 说明 |
|---|---|
| 默认值 | 预填在结构体里，文件没写的字段保持不变。**不用 `*bool` 指针** |
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
└── check.sh             把设计约束编译成检查
```

`check.sh` 在 CI 里跑，内容包括：核心模块图不超过 3 个模块、registry 与基础包零依赖、
核心无 `init()` 副作用、根包公开 API 不超过 15 个、每个集成子模块必须导出 `New`
且不许 import 根包。

---

## 状态

第 0 波（地基）已完成。集成模块正在逐波添加。
