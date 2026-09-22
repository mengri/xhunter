# Xhunter

自主编码任务的核心执行引擎（harness）。以 **Go 库 + 单进程 CLI** 两种形态使用：库供其他项目嵌入自己的 agent 平台，CLI 供平台以"投递 Bounty → 消费事件流"的方式驱动。

**一期目标平台**：`linux/amd64`。**零第三方运行时依赖**（标准库；Git 适配器将引入 go-git v5 稳定线，纯 Go，保自包含）。

## 目录结构

```
llm/                          公开：中立契约——对话形状、流式事件、Provider/Session/Caps
harness/                      公开：核心库——模型调用循环（Engine 驱动 Prepare → infer/receive → OnTurn → Finalize）；导出值类型 + 三组 handler 契约 + 一个构造方法 New
prompt/<kind>/                公开：默认提示词插件（agentsmd 项目约定 / skills 技能清单），构造时显式接收工作区
provider/adapter/             公开：协议实现的共用件（SSE 分帧、调用拼装、错误形状、上限契约）
provider/openaichat/          公开：协议——OpenAI 兼容对话补全（自持 wire，包注释含协议版本基线）
provider/openairesponses/     公开：协议——OpenAI Responses
provider/anthropicmessages/   公开：协议——Anthropic Messages
providerconfig/               公开：Provider 配置模型与解析（组装层消费的连接事实）
cmd/xhunter/                  CLI 入口 + 组装层（SDK 值 → 针对性工厂；工具集 / 插件 / 后端 / 策略的注入点）
internal/                     CLI 侧实现（不对外）：policy 策略引擎、workspace/osfs、git/cli、modelcatalog
docs/                         治理文档（产品设计 / 使用手册 / 架构设计）
scripts/check.py              一键 build + vet + test + 竞态检测（目标平台 linux/amd64）
scripts/build.sh              单二进制构建，版本号经 ldflags 注入 → bin/xhunter
Makefile                      构建与验证编排（build / cross / test / vet / check / clean）
```

**公开层与组装层严格分离**：`llm/`、`harness/`、`provider/...`、`providerconfig/` 都不依赖 `cmd/` 与 `internal/`，因此可被其他项目单独引用；装配（业务装配、哪个配置值对应哪种协议、针对厂商怎么连线）只发生在 CLI 侧，`internal/modelcatalog` 则是安装期与 CLI 期的动作，不属于运行期契约。

## 作为库使用

```go
import (
    "context"

    "xhunter/harness"
    "xhunter/hunt"
)

// 循环只认识「模型 + 三组 handler」：换一套 handler 就是换一套业务。
// 业务的一切（任务、工具面、上下文、门禁、检查点、交付）都在 hunt.Session 里，
// 它的构造参数就是装配点——原语清单、策略、工作区、git、协作者都由调用方注入。
session := hunt.NewSession(hunt.Config{
    Bounty:  bounty,              // 任务分派事实（正文、仓库、基线、预算）
    Tools:   tools,               // 工具集工厂：func(Workspace) []Primitive（工作区是运行期产物）
    Policy:  policy,              // 策略裁决、预算与止损
    Opener:  opener,              // 工作区打开点
    Git:     gitwt,               // 基线获取 / 任务分支 / 检查点 / 交付
    Context: &contextBuilder{},   // 上下文组装（提示词 + 历史），hunt.ContextBuilder
    Session: &sessionRecorder{},  // 会话材料与检查点，hunt.SessionRecorder
    Sink:    sink,                // 外部事件流（stdout），hunt.EventSink
    Filters: filters,             // 结果加工链：执行之后、落历史之前
    SystemPlugins: systemPlugins, // 提示词插件工厂：func(Workspace) []PromptPlugin
    UserPlugins:   userPlugins,   // 同上（user 段）
})

engine, err := harness.New(provider,
    []harness.PrepareHandler{session.Prepare},   // 循环前一次：准备基线、打开工作区、定格工具面与首轮消息
    []harness.OnTurnHandler{session.OnTurn},     // 每个轮边界：执行工具、加工结果、记录、组装下一轮、守卫
    []harness.FinalHandler{session.Finalize},    // 循环后一次：交付提交、差异、清理、终态上报
)
if err != nil {
    return err                // 缺模型即装配错误，不会跑到一半才炸
}

out, err := engine.Run(ctx, harness.Input{Meta: map[string]any{"bounty_id": bounty.ID}})
```

`harness` 只导出**值类型（`Run`/`Turn`/`Terminal`/`Outcome`）+ 三组 handler 契约 + 一个构造方法 `New`**——其余构造都是包内私有，循环自身不持有任何协作者。因此可以在**无模型、无网络、无仓库**的条件下测试控制流（`go test ./harness/`）。

**扩展面在业务侧，不在框架里**：想改「模型看到的文本」就在 `Config.Filters` 里加一个 `hunt.ResultFilter`（它跑在工具执行之后、落历史之前，所以加工结果一定会进下一轮上下文）；想改「某一次调用的语义」就包一层 `hunt.Primitive`（拦截、改参数后转交、美化结果——但写盘仍只走 `Committer`）；想改「模型打算怎么做」就在 `Prepare` 里调整 `run.Tools` 的声明。三条都不需要动 `harness`。

### 接入自己的模型协议

职责分三层，因此"接一家厂商"的改动面很小：

| 层 | 知道什么 | 在哪 |
|---|---|---|
| **契约** | 对话的形状、流式事件的形状、Provider/Session/Caps | `llm/`，公开包；`harness` 依赖它 |
| **协议实现** | 只有协议：请求形状、流式分帧、增量拼装、错误分类（**自持 wire，不引厂商 SDK**） | `provider/<protocol>`，公开包；包注释里写着**协议版本基线** |
| **共用件** | 与具体协议无关的部分：SSE 分帧、调用按位置拼装、错误形状、上限契约 | `provider/adapter`，公开包 |
| **绑定** | 模型给的"名字 + 参数 JSON"落到哪个原语、定位参数是什么 | `hunt/bind.go`——**使用方**的词汇，协议层不认识 |
| **组装** | 哪个配置值对应哪种协议；**端点与鉴权形状都来自配置** | CLI 侧的组装层 |

目前内置三个协议：

| SDK 取值 | 协议 | 协议版本基线 | 端点 | 鉴权 |
|---|---|---|---|---|
| `@ai-sdk/openai-compatible`（缺省） | OpenAI 兼容对话补全 | 无版本头，记形状 | 由 `baseURL` 给出 | 由配置的请求头决定 |
| `@ai-sdk/openai` | OpenAI Responses | 无版本头，`response.completed` 收尾 | 由 `baseURL` 给出 | 同上 |
| `@ai-sdk/anthropic` | Anthropic Messages | `anthropic-version: 2023-06-01` | 由 `baseURL` 给出 | 同上（惯用 `x-api-key`） |

协议实现是普通构造函数，参数是已经解析好的连接事实——**端点必填、鉴权以请求头形式传入**，因此它不依赖配置模型、不做注册、也不提供工厂，可以脱离本项目的配置体系被独立复用：

```go
c, err := openaichat.New(openaichat.Config{
    BaseURL:          "https://gateway.example/v1",       // 不预设端点：永远来自调用方
    Model:            "my-model",
    Headers:          map[string]string{"x-api-key": key}, // 鉴权形状由调用方决定
    MaxContextTokens: 128000,                             // 必填：上限是唯一不允许估算的能力
})
```

接入一家新厂商 = 在组装层的厂商表里加一条**针对性工厂**：

```go
// cmd/xhunter/providers.go
func vendorFactories() map[string]providerFactory {
    return map[string]providerFactory{
        "": openAICompatible,                    // 内置形态：OpenAI 兼容
        "@vendor/private-gateway": func(r providerconfig.Resolved) (harness.Provider, error) {
            // 该厂商特有的连线方式全在这里：端点约定、鉴权头怎么拼、要不要补租户头
            return mygateway.New(mygateway.Config{...})
        },
    }
}
```

| 约定 | 说明 |
|---|---|
| **协议实现不含装配知识** | 它不知道自己被哪个配置值选中；新增厂商只改组装层，协议实现与 Harness 都不动 |
| **一个协议一个包** | `provider/<protocol>` 之间互不引用；共享件下沉到 `provider/adapter`（互相引用即导入循环，编译期失败） |
| **组装期显式失败** | 缺上限、缺端点、凭据引用解析为空、厂商表里没有对应工厂 → 启动期退出码 2，并指出答案在组装层 |
| **事件序列即协议状态** | 流的结束由通道关闭表达，正常结束与中断的区别由最后一条事件表达；形状不成立的调用带结构化错误上报，不得静默丢弃 |

### 三条使用约束（不满足则语义不成立）

| 约束 | 说明 |
|---|---|
| **stdout 是唯一外部通道** | 事件流独占 stdout，日志走 stderr；写失败即环境错误终止（`docs/xhunter-product-design.md` FR-10.4） |
| **分支与交付不由模型驱动** | 任务分支的创建/推送只发生在初始化，检查点与交付提交的**执行**只由引擎在轮边界与收尾完成；工具面不含能改变分支、推送目标或已推送历史的操作（`docs/xhunter-architecture.md` INV-11） |
| **门禁清单来自受审配置** | 来自 Bounty 或**基线 commit**（不从工作区读取，防模型中途削弱门禁）；豁免只能由 Bounty 授予（FR-5.2b/g/h） |

## 构建与验证

```bash
make build                 # 本机构建 → bin/xhunter
make cross                 # 交叉编译一期目标平台 linux/amd64 → bin/xhunter
make check                 # 完整验证（build + vet + test + 交叉编译）
```

版本号经 `-ldflags` 注入 `cmd/xhunter` 的 `version` 变量——它既是 `xhunter version` 的输出，也是发给上游的 User-Agent 标识 `xhunter/<version>` 里的版本段。优先级：`make build VERSION=v0.1.0` > `$VERSION` 环境变量 > `git describe --tags --always --dirty` > `devel`。

等价的脚本直调：

```bash
scripts/build.sh v0.1.0                        # 指定版本构建
GOOS=linux GOARCH=amd64 scripts/build.sh       # 交叉编译目标平台
python scripts/check.py --race                 # 完整验证：build + vet + test + 竞态检测
python scripts/check.py                        # 本机：逻辑验证 + 交叉编译 linux/amd64
python scripts/check.py --probe                # 仅探测工具链与平台
```

## 文档入口

| 文档 | 内容 |
|---|---|
| `docs/xhunter-product-design.md` | **产品设计**：目标与边界、功能需求（FR）、非功能需求（NFR）、工具集规格、验收标准（AC）、分期计划 |
| `docs/xhunter-usage.md` | **使用手册**：基本使用模型、CLI 入参、Bounty 结构、Provider 配置、事件流契约（SSOT）、结果文件、退出码、信号 |
| `docs/xhunter-architecture.md` | **架构设计**：分层与职责边界、流程组装、任务生命周期（G/R/D）、组件规格（H1~H7）、Provider 抽象、数据流、不变量（INV）、接口验收标准（IA） |
