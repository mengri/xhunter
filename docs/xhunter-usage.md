# Xhunter 使用手册

## 0. 文档定位

- 本文是 Xhunter 的**外部契约与使用方式**：如何启动、投递什么、会收到什么、如何判断结果。
- 读者：平台侧集成方，以及任何驱动者（调度 daemon、本地脚本、UI、测试工具）。
- 产品边界与能力见 `xhunter-product-design.md`；内部分层、组件与控制流时序见 `xhunter-architecture.md`。
- **本文 §5 是外部事件契约的唯一权威（SSOT）**，其他文档不得另行定义事件字段。

---

## 1. 基本使用模型

| 事实 | 含义 |
|---|---|
| **一个进程 = 一个 Bounty = 一次 Hunt** | 进程启动即开始，进程退出即终结；不提供进程内复用与跨任务会话 |
| **投递而非拉取（Xhunter 侧）** | Xhunter **不主动联系平台**。Bounty 由驱动者投递（命令行 + 文件）；**pull 留在平台侧**——调度器拉取任务后再投递给具体机器。任务活跃度按 Bounty **输出**，与 runtime 心跳解耦 |
| **stdout 是唯一外部通道** | 事件流独占 stdout，人类可读日志走 stderr；**心跳即进度**，无网络上报、无传输层 |
| **恢复靠检查点而非重放** | 崩溃后重派时 checkout 任务分支 tip（最后一个检查点）即为工作区状态，再读回会话材料继续推理 |

### 1.1 三条使用约束（不满足则语义不成立）

| 约束 | 说明 |
|---|---|
| **stdout 是唯一外部通道** | 事件流独占 stdout，日志走 stderr；**写失败即环境错误终止**（退出码 2，FR-10.4） |
| **分支与交付不由模型驱动** | 任务分支的创建/推送只发生在初始化阶段，检查点与交付提交的**执行**只由引擎在轮边界与收尾完成；工具面不含能改变分支、推送目标或已推送历史的操作（INV-11） |
| **门禁清单来自受审配置** | 来自 Bounty 或**基线 commit**（不从工作区读取，防模型中途削弱门禁）；豁免只能由 Bounty 授予（FR-5.2b/g/h） |

### 1.2 驱动者无关性

**任何满足两个条件的实体都是合法驱动者**：① 能准备 Bounty 文件；② 能启动 `xhunter` 进程并消费其 stdout 事件流。平台 daemon 只是其中之一——**本地运行的 UI / 脚本 / 测试工具同样合法，无需核心改动**。

本地驱动必须满足的最小条件（否则跑不起来）：

| 条件 | 说明 |
|---|---|
| **投递三要素** | 环境变量 `XHUNTER_REPO_URL`（可推送的远端）+ `XHUNTER_REPO_BASE_COMMIT` + `XHUNTER_REPO_BRANCH`（**分支名由驱动者指定，Xhunter 自己创建并推送**）；任务正文单独放在 `--bounty` 指向的文件里 |
| **凭据写权限** | 任务分支由 Xhunter 创建并推送，**只读凭据跑不通** |
| **门禁清单** | 来自 Bounty，或仓库 `.xhunter/gates.yml`（**从基线 commit 读**，FR-5.2g） |
| **事件消费** | 读 stdout NDJSON |
| **工作区隔离** | **必须由 Xhunter 自行 clone 到临时工作区**，不得把用户当前的仓库目录当工作区——直接改用户工作区会污染其未提交改动 |

**驱动者无关性不放松任何不变量**：装配仍是**同一份装配代码**（缺件与就绪失败都在启动期以退出码 2 失败）、门禁清单来源不变、模型仍无 git 能力、权限仍经策略层。**换驱动者只换"谁投递、谁消费"，不换"什么被允许"。**

为便于本地驱动，Xhunter 规划了 **Bounty 生成能力**（`xhunter run`，FR-1.10）：给定本地仓库路径 + 任务描述，探测 `remote` / 基线 commit（取 HEAD）/ 任务分支名 / 门禁候选（`.xhunter/gates.yml` 存在性）/ 预算与策略默认值，生成 Bounty 文件。

> **状态：尚未接线。** 当前 CLI 只实现了 `models` / `version` 子命令与 `--bounty` 入口（且后者为占位实现）。本地驱动者在此之前需自行生成 Bounty 文件——生成器只是便利工具，不进入交付路径（FR-1.10 边界③）。

---

## 2. 命令行入参

```
xhunter --bounty <path>           # 任务正文文件（自然语言描述；部署事实走环境变量，见 §3）
        [--log-file <path>]       # 人类可读日志（默认 stderr）
        [--result <path>]         # 结果文件（JSON；无论成败都写，见 §6）
        [--patch <path>]          # 补丁文件（相对基线的统一 diff，git apply 兼容）

xhunter run --repo <path> --task <text>   # 本地驱动：探测仓库并生成 Bounty（尚未接线，见 §1.2）
xhunter models update [--source <url>] [--dir <path>] [--output-reserve <n>]
xhunter models status [--dir <path>]
xhunter version
```

`models` 子命令是**环境侧动作**：网络或磁盘问题一律以退出码 2 退出（可重试），与任务失败（1）区分。

凭据通过环境变量注入（前缀 `XHUNTER_*`），不通过参数传递；**仓库与模型接入等部署事实同样走环境变量**（§3）。

---

## 3. 投递形态：任务正文 + 环境变量

**投递 = 一个任务正文文件 + 一组环境变量。** 分工的依据是"谁最了解什么"：任务正文每个任务都不同，放文件里便于阅读、diff 与存档；仓库地址、分支、基线、模型接入这些是"这次跑在哪儿"的部署事实，同一台机器上往往固定，走环境变量就不必每个任务重复写。

**任务正文**（`--bounty <path>`）：纯文本 / Markdown 文件，内容就是任务描述，可含验收标准。**不解析任何结构**——正文是自然语言，解析只会逼着写的人去迁就格式。

**部署事实**（环境变量，前缀 `XHUNTER_`）：

| 变量 | 必填 | 说明 |
|---|---|---|
| `XHUNTER_REPO_URL` | ✅ | 可推送的远端地址 |
| `XHUNTER_REPO_BASE_COMMIT` | ✅ | 完整哈希，作为 patch 与提交的父提交基准 |
| `XHUNTER_REPO_BRANCH` | — | 任务分支名（缺省 `xhunter/<session_id>`）。**该分支由 Xhunter 创建并推送**（FR-1.3）：不存在则从基线创建后推送；**已存在且 tip 为基线或其后代 → 幂等成功**（续跑时 tip 本来就在基线之后，这不是错误）；tip 与基线**分叉**（非后代）即环境错误（退出码 2） |
| `XHUNTER_BOUNTY_ID` | — | **本次投递**的标识（缺省取基线前 12 位）。事件流信封与结果文件按它记账 |
| `XHUNTER_SESSION_ID` | — | **会话**的标识（可选）。同一个会话下的多次投递**共享记忆与分支**；不传时本次投递自成一次新会话（会话标识即本任务的 id） |
| `XHUNTER_PROVIDER` / `XHUNTER_MODEL` | ✅ | 本次使用哪个供应商与哪个模型 |
| `XHUNTER_PROVIDER_CONFIG` | ✅ | Provider 配置文件路径（形态见 §4） |

**两个标识的分工**：`bounty_id` 回答"这次谁在跑"（每次投递一个），`session_id` 回答"接的是哪份工作"（可跨多次投递）。**会话记忆的目录与分支名都以会话标识为准**——`.xhunter/<session_id>/`、`xhunter/<session_id>`。因此同一会话的第 N 次投递不必各自指定分支：它天然接在同一份工作与历史上（前几次的提交就在那条分支的分支 tip 上，前几次的对话就在那份记忆里）；反过来，不同会话之间不会互相看到对方的记忆。

**会话记忆可以还没有**：会话标识指向一个尚无历史的会话（它的第一次投递）不是错误——此时工作区起点即分支 tip（首次就是基线），上下文没有可回灌的历史。**"不存在"与"损坏"要分开**：材料不存在 = 该会话暂无历史；材料存在但损坏或版本不兼容 = 环境错误（退出码 2）。

**记账口径**：事件流信封与结果文件里的 `bounty_id` 是本次投递的标识，而分支与记忆路径用会话标识——同一会话的多次投递之间，两者不同，这是刻意的。

**尚未定投递方式**：预算上限、检查点策略属运行策略，它们的投递形态（环境变量还是配置文件）随策略层实现一起定；当前实现不读取它们。**流程本身不投递**——可组装的件（原语清单、两段提示词插件、结果过滤器链、三组 handler）在组装层装配，见 `xhunter-architecture.md` §4。

---

## 4. Provider 配置

供应商与模型的连接参数、能力上限**全部来自配置**，不写进代码。

```json
{
  "provider": {
    "<provider-id>": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "显示名",
      "options": {
        "baseURL": "https://api.example.com/v1",
        "apiKey": "{env:MY_PROVIDER_API_KEY}",
        "headers": {"Authorization": "Bearer ..."}
      },
      "models": {
        "<model-id>": {"limit": {"context": 200000, "output": 65536}}
      }
    }
  }
}
```

| 字段 | 含义 | 对应需求 |
|---|---|---|
| `limit.context` | 模型接受的最大输入 token | **FR-9.5 的唯一来源**（不得估算） |
| `limit.output` | 模型可生成的最大 token | FR-9.6 的输出预留项 |
| `options.apiKey` | 凭据**引用**：`{env:VAR}` | FR-1.2、FR-8.4——凭据值不落配置 |
| `options.baseURL` + `npm` | 自建网关 / OpenAI 兼容端点 | 使供应商成为**部署期配置**而非编译期决策 |
| `options.headers` | 自定义请求头 | 企业网关鉴权 |

**协议取值**（`npm` 字段指的是**协议**，不是厂商——同一个协议可以由多家提供）：

| 取值 | 协议 | 协议版本基线 | 端点与协议头 |
|---|---|---|---|
| `@ai-sdk/openai-compatible`（以及 `builtin` 或省略） | OpenAI 兼容对话补全：按角色平铺消息 + 分片流式 | 无版本头；形状基线 2026-09 | **必须**由 `baseURL` 给出；`Content-Type` / `Accept` 由实现补 |
| `@ai-sdk/openai` | OpenAI Responses：类型化条目（消息 / 函数调用 / 结果各占一条）+ 语义事件流 | 无版本头；以 `response.completed` 收尾 | **必须**由 `baseURL` 给出（如 `https://api.openai.com/v1`） |
| `@ai-sdk/anthropic` | Anthropic Messages：顶层系统提示 + 内容块（工具结果挂在用户消息下） | `anthropic-version: 2023-06-01`（默认发送，可用 `headers` 覆盖） | **必须**由 `baseURL` 给出（如 `https://api.anthropic.com/v1`） |

**端点不预设**：三种协议的 `baseURL` 都必须由配置给出——实现里不带任何厂商地址（换一家官方供应商就是改一行配置）。

**鉴权方式可配置**：请求头怎么写由配置决定，实现只负责原样发出去。

```json
{
  "options": {
    "baseURL": "https://api.anthropic.com/v1",
    "headers": {"x-api-key": "{env:ANTHROPIC_API_KEY}"}
  }
}
```

| 写法 | 含义 |
|---|---|
| `headers.<name>` 的值 | 可以是字面量，也可以含 `{env:VAR}` 引用（**可带前缀**，如 `"Bearer {env:KEY}"`）；引用在启动期展开，变量未设置即显式失败 |
| `options.apiKey` | 便捷形式：声明了它、且 `headers` 里没有 `Authorization` 时，补一个标准的 `Authorization: Bearer <值>`；想要别的形状就用 `headers` 显式写 |

因此非标准鉴权（`x-api-key` 裸值、网关签名、租户头）不需要改代码。凭据值始终只在环境变量里，配置中只有引用（FR-1.2、FR-8.4）。

注意：Anthropic Messages 的请求体**要求生成上限**，该值取 `limit.output`（即输出预留）——两者口径一致，都是"留给模型生成的空间"。

**三层解析**（对齐 FR-9.5 的来源优先级）：

| 优先级 | 来源 | 说明 |
|---|---|---|
| ① | **本地目录快照** `~/.xhunter/models.json` | 安装时从 LiteLLM 目录拉取一份并转换落盘；`xhunter models update` 手动刷新 |
| ② | 用户配置文件 | **覆盖片段**语义：可以只给 `options`，模型上限由目录补齐 |
| ③ | 皆无 | **启动期显式失败**（退出码 2）——不采用保守默认值 |

**目录快照的获取与运行期边界**：数据源是 LiteLLM 的 `model_prices_and_context_window.json`（一份文件同时给出上下文上限、输出上限与每 token 单价，正好覆盖 FR-9.5、FR-9.6 与 FR-9.1 的费用维度）。**拉取是安装期与 CLI 期动作，运行期只读本地快照、不联网**——Hunt 期间不得依赖外部目录服务。快照记录来源地址、抓取时间、sha256 与转换统计，便于事后核对。

**事实与策略的区分**：上游有大量条目只给出"总窗口"一个数（三个字段相等），未区分输入与输出。此时**上下文上限仍取上游事实值**，而输出预留取策略默认值（`--output-reserve`，默认 8192）并逐条打标 `output_policy_default`。理由：输出预留是**预算参数**，不是模型能力声明。

**校验分两层**（因覆盖片段允许不完整）：*结构校验*在解析时进行（凭据引用形式、OpenAI 兼容必须有 `baseURL`、已给出的 limit 必须自洽、未知字段拒绝）；*生效校验*在合并之后进行（目标模型的上限必须齐备）。

---

## 5. 事件流契约（stdout NDJSON）

**本节是外部事件契约的唯一权威（SSOT）**。

**事件信封**（所有事件共有，由 FR-11.3 的统一追踪标识推出）：

```json
{"type": "<事件名>", "bounty_id": "...", "trace_id": "...", "ts": "<RFC3339>"}
```

`trace_id` 串联平台侧记录：Xhunter 启动时从 Bounty 继承（缺省则自行生成并回填），全流程不变。

```json
{"type":"hunt_start","bounty_id":"...","trace_id":"...","ts":"..."}
{"type":"assumption","text":"..."}                    // 代替追问的假设外化
{"type":"assistant_text","text":"..."}
{"type":"tool_call","call_id":"...","tool":"edit","args":{...}}
{"type":"tool_result","call_id":"...","tool":"symbol_edit","ok":true,"precision":"syntactic","degrade":null,"summary":"...","duration_ms":12}
{"type":"check_result","gate":"unit-test","passed":true,"cached":false,"exit_code":0,"duration_ms":1234,"source":"repo","summary":"..."}
{"type":"policy_denied","action":"...","reason":"..."}
{"type":"gate_config_changed","source":"working_tree","gates":["..."]}   // 仅当 Bounty 授予 working_tree 时
{"type":"config_snapshot","max_denied_streak":5,"max_fail_streak":3,"max_turns_hard":0}
{"type":"usage","input_tokens":0,"output_tokens":0,"cost":0}
{"type":"heartbeat","phase":"...","elapsed_ms":0}
{"type":"context_compacted","level":"L2","released_tokens":0,"watermark":"warn"}
{"type":"deliverable","files":["..."]}
{"type":"degraded","scope":"prompt.skills","subject":"...","reason":"..."}   // 非致命降级：跳过/截断了什么
{"type":"error","kind":"...","retryable":false,"context":"..."}
{"type":"hunt_end","status":"succeeded|failed|cancelled","reason":"..."}
```

> 第二条起为载荷示例，**省略公共字段**（信封四字段每行都有）。
>
> `call_id` 是调用与结果配对的唯一标识：一轮内可能出现同一原语的多次调用，外部消费者据此配对（IA-1.5）；`policy_denied` 与 `check_result` 同属 L5 后的时点，故这两类事件也应携带相应 `call_id` 以便回溯到具体调用。
>
> **当前实现已发出的事件**（其余为规格先行，见 `xhunter-architecture.md` §14）：`hunt_end`、`tool_result`、`policy_denied`、`deliverable`、`degraded`、`heartbeat`（调用点待接入）。本节其余事件类型与字段是**契约目标**，实现状态以架构文档 §14 为准；`effective_config` 的完整语义以结果文件（§6）与 FR-11.6 为准。

**事件顺序**：`tool_result` / `policy_denied` / `check_result` 三类事件**在模型流结束后（运行段 L5 响应处理）发出**，不随流内 `tool_use` 即时产生——接收段零副作用（流内挂起无半写状态）。事件**类型与字段不变**，仅时序后移。`assistant_text` / `usage` / `permission_request` 应答仍为流内实时。

**降级与错误的区别**：`degraded` 表示"照常跑下去了，但少了一部分"——例如某份技能说明格式非法被跳过、约定文件超限被截断。它是**非致命**的，任务继续；`error` 才表示当前动作失败。降级必须可见，因为"少了一部分"若不暴露，产出偏差要到评审时才看得出来。

**约束**：stdout 只允许出现事件行；日志、进度、调试信息一律走 stderr。字段只能追加，不可修改或删除（INV-5）。

---

## 6. 结果文件

由 `--result <path>` 指定；**无论成败都写**（FR-1.5）——平台靠它记账、决定是否重派。
写不出来属环境问题（退出码 2），不会静默继续。

**当前形状**（字段只含实现真能给出的事实）：

```json
{
  "bounty_id": "...",
  "session_id": "...",
  "status": "succeeded|failed|cancelled",
  "reason": "no_tool_call",
  "exit_code": 0,
  "base_commit": "...",
  "branch": "xhunter/<session_id>",
  "commit_sha": "...",
  "patch_path": "...",
  "files_changed": ["..."],
  "usage": {"input_tokens": 0, "output_tokens": 0, "turns": 0, "elapsed_ms": 0},
  "error": {"kind": "prepare_failed", "message": "...", "retryable": true}
}
```

> `error` 仅失败时出现；`retryable` 与退出码同源（环境问题才为 `true`）。
> `patch_path` 仅在给了 `--patch` 且补丁产出成功时出现；补丁**排除会话材料目录**的
> 语义尚未接入（会话材料尚未落盘）。

**尚未落地、因而不写空壳的字段**（空数组会被读成"没有门禁、没有假设"，那是另一句话）：

| 字段 | 状态 |
|---|---|
| `assumptions` / `unverified` | **待接入**（§FR-6.3/6.4 的假设外化） |
| `gates`（含未运行门禁的 `passed: null`） | **待接入**（门禁清单与 `check` 未实现） |
| `effective_config`（FR-11.6 的只读快照） | **待接入**（装配快照未落盘） |
| `session_delta` | **待接入**（会话材料未落盘） |
| `cost` | **待接入**（费用维度未计量；目录里已有单价） |

> `gates` 必须**列出未运行的门禁**（`passed: null`），`effective_config` 是**只读快照**（FR-11.6）——两者都是 MR 评审的直接证据：前者回答"验收跑没跑、过没过"，后者回答"用的是哪套规则"。

---

## 7. 退出码

| 退出码 | 含义 | 平台侧建议动作 |
|---|---|---|
| 0 | 成功 | 取 patch，进入 review |
| 1 | 任务失败（不可重试） | 标记失败，不重派 |
| 2 | 环境或资源问题（可重试） | 允许重派 |
| 3 | 被取消 | 按取消流程处理 |

**终止条件 → 退出码映射**（消除"哪个失败算 1、哪个算 2"的歧义）：

| 终止条件 | 退出码 | 说明 |
|---|---|---|
| 本轮无 tool_call 且有写操作 | 0 | 唯一成功路径 |
| 本轮无 tool_call 但无任何写操作 | 1 | `no_output`：空手而归不算成功 |
| 轮数 / token / 费用 / 墙钟耗尽 | 1 | 任务本身的预算问题，重派同样会耗尽 |
| 止损超上限、策略连续拒绝累积 | 1 | 模型无法完成任务 |
| 上下文达硬上限 | 1 | 按预算耗尽处理 |
| **基线不可获取 / 工作区脏** | **2** | 环境问题：平台修好即可重派 |
| **恢复失败（材料损坏/版本不兼容、任务分支不可达、checkout 失败）** | **2** | resume 是优化，重跑是兜底 |
| **扩展能力上限未配置（FR-9.5）** | **2** | 配置问题，修配置后重派 |
| 校验未通过（check 判定不通过） | 1 | patch 照常交付、状态为失败（FR-6.5） |
| **门禁执行失败**（命令不存在 / 无法创建进程 / 超时） | **2** | 不是质量结论，是环境问题（FR-5.2d）——与上一行的"判定不通过"必须分开 |
| **检查点连续提交失败达上限（默认 3 次）** | **2** | 远端不可用，本轮结束即收敛，不跑完剩余轮次（FR-1.3b、FR-1.11②） |
| **stdout 写失败（通道断裂）** | **2** | 消费者已不在通道上，写丢弃比继续跑更危险（FR-10.4、AC-19） |
| **结果文件 / 补丁写失败** | **2** | 交不出交付记录与补丁（路径不可写、磁盘满）——环境问题，修好可重派 |
| SIGTERM / SIGINT | 3 | 平台主动取消 |

判据一句话：**"换个环境或修好配置就能成功"的算 2，其余失败算 1。**

---

## 8. 信号

| 信号 | 行为 |
|---|---|
| SIGTERM | 停止新调用，落盘状态与已有变更，以取消状态退出 |
| SIGINT | 同 SIGTERM |
| SIGKILL | 不可捕获，依赖定期落盘 + 平台侧超时兜底 |
