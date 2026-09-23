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
| **stdout 是唯一外部通道** | 事件流独占 stdout，日志走 stderr；**写失败即环境错误终止**（退出码 1，FR-10.4） |
| **分支与交付不由模型驱动** | 任务分支的创建/推送只发生在初始化阶段，检查点与交付提交的**执行**只由引擎在轮边界与收尾完成；工具面不含能改变分支、推送目标或已推送历史的操作（INV-11） |
| **门禁清单来自受审配置** | 来自 Bounty 或**基线 commit**（仓库根 `gates.yml`；不从工作区读取——判据在运行前定死）；豁免只能由 Bounty 授予（FR-5.2b/g/h）。条目的字段与 `expect` 判据形状见 FR-5.2b 与 `xhunter-architecture.md` §7.8 |

### 1.2 驱动者无关性

**任何满足两个条件的实体都是合法驱动者**：① 能准备 Bounty 文件；② 能启动 `xhunter` 进程并消费其 stdout 事件流。平台 daemon 只是其中之一——**本地运行的 UI / 脚本 / 测试工具同样合法，无需核心改动**。

本地驱动必须满足的最小条件（否则跑不起来）：

| 条件 | 说明 |
|---|---|
| **投递三要素** | 环境变量 `XHUNTER_REPO_URL`（可推送的远端）+ `XHUNTER_REPO_BASE_COMMIT` + `XHUNTER_REPO_BRANCH`（**分支名由驱动者指定，Xhunter 自己创建并推送**）；任务正文单独放在 `--bounty` 指向的文件里 |
| **凭据写权限** | 任务分支由 Xhunter 创建并推送，**只读凭据跑不通** |
| **门禁清单** | 来自 Bounty，或仓库根的 `gates.yml`（**从基线 commit 读**——判据在运行前定死，本次不受工作区改动影响；FR-5.2g） |
| **事件消费** | 读 stdout NDJSON |
| **工作区隔离** | **必须由 Xhunter 自行 clone 到临时工作区**，不得把用户当前的仓库目录当工作区——直接改用户工作区会污染其未提交改动 |

**驱动者无关性不放松任何不变量**：装配仍是**同一份装配代码**（缺件与就绪失败都在启动期以退出码 1 失败）、门禁清单来源不变、模型仍无 git 能力、权限仍经策略层。**换驱动者只换"谁投递、谁消费"，不换"什么被允许"。**

为便于本地驱动，Xhunter 规划了 **Bounty 生成能力**（`xhunter run`，FR-1.10）：给定本地仓库路径 + 任务描述，探测 `remote` / 基线 commit（取 HEAD）/ 任务分支名 / 门禁候选（仓库根 `gates.yml` 存在性）/ 预算与策略默认值，生成 Bounty 文件。

> 实现状态见 xhunter-status.md 状态索引 · usage§1.2·Bounty生成器。生成器只是便利工具，**不进入交付路径**（FR-1.10 边界③）。

---

## 2. 命令行入参

```
xhunter --bounty <path>           # 任务正文文件（自然语言描述；部署事实走环境变量，见 §3）
        [--log-file <path>]       # 人类可读日志（默认 stderr）
        [--result <path>]         # 结果文件（JSON；无论成败都写，见 §6）
        [--patch <path>]          # 补丁文件（相对基线的统一 diff，git apply 兼容）

xhunter run --repo <path> --task <text>   # 本地驱动：探测仓库并生成 Bounty（见 §1.2）
xhunter version
```

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
| `XHUNTER_REPO_BRANCH` | — | 任务分支名（缺省 `xhunter/<session_id>`）。**该分支由 Xhunter 创建并推送**（FR-1.3）：不存在则从基线创建后推送；**已存在且 tip 为基线或其后代 → 幂等成功**（续跑时 tip 本来就在基线之后，这不是错误）；tip 与基线**分叉**（非后代）即环境错误（退出码 1） |
| `XHUNTER_BOUNTY_ID` | — | **本次投递**的标识（缺省取基线前 12 位）。事件流信封与结果文件按它记账 |
| `XHUNTER_TRACE_ID` | — | 贯穿平台侧记录的**追踪标识**（FR-11.3），进每个事件的信封；缺省回填为 `bounty_id` |
| `XHUNTER_SESSION_ID` | — | **会话**的标识（可选）。同一个会话下的多次投递**共享记忆与分支**；不传时本次投递自成一次新会话（会话标识即本任务的 id） |
| `XHUNTER_MODEL` | ✅ | 模型标识（原样进请求体） |
| `XHUNTER_BASE_URL` | ✅ | 模型端点（协议不预设任何主机） |
| `XHUNTER_MODEL_CONTEXT_TOKENS` | ✅ | 模型接受的输入上限（FR-9.5，**不得估算**） |
| `XHUNTER_MODEL_OUTPUT_TOKENS` | ✅ | 留给模型生成的空间，用作输出预留（FR-9.6） |
| `XHUNTER_PROTOCOL` | — | **协议**取值：`openaichat`（缺省）｜`openairesponses`｜`anthropicmessages`。它指协议不指厂商——同一个协议可以由多家提供 |
| `XHUNTER_API_KEY` | — | 凭据值：补一个标准的 `Authorization: Bearer`，除非 `XHUNTER_HEADERS` 里显式写了鉴权头 |
| `XHUNTER_HEADERS` | — | 自定义请求头（JSON 对象，如 `{"x-api-key":"{env:MY_KEY}"}`）；值可写 `{env:VAR}` 引用，启动期展开 |
| `XHUNTER_BUDGET_TURNS` | — | 轮数上限（正整数；未设置 = 不限） |
| `XHUNTER_BUDGET_TOKENS` | — | 累计 token 上限（输入+输出，正整数；未设置 = 不限） |
| `XHUNTER_BUDGET_WALL_CLOCK` | — | 墙钟上限（Go duration，如 `90m`、`2h`；未设置 = 不限） |
| `XHUNTER_HEARTBEAT_INTERVAL` | — | 心跳间隔（Go duration，如 `45s`、`2m`；缺省 30s）——平台判断"卡死"的灵敏度由它定；**取值非法（含 `0`、负数、非 duration）即启动期退出 1** |

**前缀即分组**（命名是刻意的，防错靠名字而不是靠文档提醒）：`XHUNTER_MODEL_*` 是**模型接入事实**（模型是什么——上限两项必填、其余可选）；`XHUNTER_BUDGET_*` 是**任务预算**（三项都可选，不配即不限）；`XHUNTER_REPO_*` 是仓库事实。三组的**必填性相反**，因此让名字完全不重叠。

**两个标识的分工**：`bounty_id` 回答"这次谁在跑"（每次投递一个），`session_id` 回答"接的是哪份工作"（可跨多次投递）。**会话记忆的目录与分支名都以会话标识为准**——`.xhunter/<session_id>/`、`xhunter/<session_id>`。因此同一会话的第 N 次投递不必各自指定分支：它天然接在同一份工作与历史上（前几次的提交就在那条分支的分支 tip 上，前几次的对话就在那份记忆里）；反过来，不同会话之间不会互相看到对方的记忆。

**会话记忆可以还没有**：会话标识指向一个尚无历史的会话（它的第一次投递）不是错误——此时工作区起点即分支 tip（首次就是基线），上下文没有可回灌的历史。**"不存在"与"损坏"要分开**：材料不存在 = 该会话暂无历史；材料存在但损坏或版本不兼容 = 环境错误（退出码 1）。

**记账口径**：事件流信封与结果文件里的 `bounty_id` 是本次投递的标识，而分支与记忆路径用会话标识——同一会话的多次投递之间，两者不同，这是刻意的。

**投递形态已定**：三重预算上限走环境变量（上表），**三项都可选——不配就是不限制**（三个变量一个都不设 = 全程没有预算止损，只受机制硬顶约束）。**「不配」与「配错」必须分开**：不配 = 该维度不限；显式写 `0`、负数、`abc`、`半小时` 一律在启动期以退出码 1 失败并指名是哪个变量——「以为设了限制、其实没有」是最危险的静默失效。任一维度耗尽的终态是 `budget_exhausted:<维度>`（**退出 2**，见 §7）。**检查点是固定行为、不提供档位**：自动检查点只落在结构完整点上（FR-1.3c/1.3d），另有收尾的交付提交；判据不可判定时不提交。**流程本身不投递**——可组装的件（原语清单、两段提示词插件、结果过滤器链、三组 handler）在组装层装配，见 `xhunter-architecture.md` §4。

---

## 4. 模型接入

接入事实**只有一处来源：环境变量**——没有配置文件、没有内置模型目录、不做合并与回退。
依据是"谁最了解什么"：同一台机器上的部署事实往往固定，而任务正文每个任务都不同；
分开之后部署事实只写一次，也就不存在"配置里写的是这个、运行时用的是那个"的漂移。

**变量清单在 §3**（与仓库事实、预算上限并列）。本节说取值与语义。

**协议取值**（`XHUNTER_PROTOCOL`，指的是**协议**，不是厂商——同一个协议可以由多家提供）：

| 取值 | 协议 | 协议版本基线 | 端点与协议头 |
|---|---|---|---|
| `openaichat`（缺省） | OpenAI 兼容对话补全：按角色平铺消息 + 分片流式 | 无版本头；形状基线 2026-09 | **必须**由 `XHUNTER_BASE_URL` 给出；`Content-Type` / `Accept` 由实现补 |
| `openairesponses` | OpenAI Responses：类型化条目（消息 / 函数调用 / 结果各占一条）+ 语义事件流 | 无版本头；以 `response.completed` 收尾 | **必须**由 `XHUNTER_BASE_URL` 给出（如 `https://api.openai.com/v1`） |
| `anthropicmessages` | Anthropic Messages：顶层系统提示 + 内容块（工具结果挂在用户消息下） | `anthropic-version: 2023-06-01`（默认发送，可用 `XHUNTER_HEADERS` 覆盖） | **必须**由 `XHUNTER_BASE_URL` 给出（如 `https://api.anthropic.com/v1`） |

**端点不预设**：三种协议的端点都必须由 `XHUNTER_BASE_URL` 给出——实现里不带任何厂商地址，换一家官方供应商就是改一个环境变量。

**鉴权形状也由环境变量决定**：

| 写法 | 含义 |
|---|---|
| `XHUNTER_HEADERS` | 任意头名与值（JSON 对象）。值可以是字面量，也可以含 `{env:VAR}` 引用（**可带前缀**，如 `"Bearer {env:KEY}"`）；引用在**启动期**展开，变量未设置即显式失败 |
| `XHUNTER_API_KEY` | 便捷形式：声明了它、且 `XHUNTER_HEADERS` 里没有 `Authorization` 时，补一个标准的 `Authorization: Bearer <值>`；想要别的形状就用 `XHUNTER_HEADERS` 显式写 |

因此非标准鉴权（`x-api-key` 裸值、网关签名、租户头）不需要改代码。凭据值始终只在环境变量里，不落任何文件（FR-1.2、FR-8.4）。

注意：Anthropic Messages 的请求体**要求生成上限**，该值取 `XHUNTER_MODEL_OUTPUT_TOKENS`——两者口径一致，都是"留给模型生成的空间"。

**上限来自投递，不来自探测**（FR-9.5/9.6）：`XHUNTER_MODEL_CONTEXT_TOKENS` 与
`XHUNTER_MODEL_OUTPUT_TOKENS` 都必填，且必须是正整数；输出预留不得大于等于上下文上限。
可用输入预算 = `窗口上限 − 固定开销（系统段 + 工具 schema） − 输出预留`，三档水位以它
为基数（FR-9.6、FR-14.1）。

**校验在启动期一次报出全部问题**：缺项、取值非法、`XHUNTER_HEADERS` 不是 JSON 对象、
引用指向未设置的变量——一律退出码 1，并把**全部**问题一次列出。理由是无人值守场景下
"修一次再重派"远便宜于逐个发现：报一项、改一项、再派一次，每一轮都要重新烧一遍预算。

## 5. 事件流契约（stdout NDJSON）

**本节是外部事件契约的唯一权威（SSOT）**。

**事件信封**（所有事件共有，由 FR-11.3 的统一追踪标识推出）：

```json
{"type": "<事件名>", "bounty_id": "...", "trace_id": "...", "ts": "<RFC3339>"}
```

`trace_id` 串联平台侧记录：由 `XHUNTER_TRACE_ID` 给出（FR-11.3），缺省回填为 `bounty_id`，全流程不变——**每个事件都带这四字段**，由事件出口统一盖章，发出点只需交业务载荷。

```json
{"type":"hunt_start","session_id":"...","base_commit":"...","branch":"xhunter/<session_id>","task":"...","effective_config":{...}}
{"type":"assumption","text":"..."}                    // 代替追问的假设外化（模型采取了哪些默认）
{"type":"needs_input","text":"..."}                   // 需要补全的条件（逐条；模型写完即停止，终态为 blocked）
{"type":"assistant_text","text":"..."}                    // 模型的答复正文；**最终答复是交付物的一部分**（可能整份交付物就是它）
{"type":"tool_call","call_id":"...","tool":"edit","args":{...}}
{"type":"tool_result","call_id":"...","tool":"symbol_edit","ok":true,"precision":"syntactic","degrade":null,"summary":"...","duration_ms":12}
{"type":"check_result","gate":"unit-test","passed":true,"cached":false,"exit_code":0,"duration_ms":1234,"source":"repo","summary":"..."}
{"type":"policy_denied","action":"...","reason":"..."}
{"type":"gate_config_changed","source":"working_tree","gates":["..."]}   // 仅当 Bounty 授予 working_tree 时
{"type":"config_snapshot","max_denied_streak":5,"max_fail_streak":3,"max_turns_hard":0}
{"type":"usage","input_tokens":0,"output_tokens":0,"cached_input_tokens":0}   // 每轮末发**增量**（配对的是这一轮）
{"type":"heartbeat","phase":"...","elapsed_ms":0}
{"type":"context_compacted","level":"L2","released_tokens":0,"watermark":"warn"}
{"type":"deliverable","files":["..."]}
{"type":"degraded","scope":"prompt.skills","subject":"...","reason":"..."}   // 非致命降级：跳过/截断了什么
                                              // scope: "usage" 表示上游未回报用量（此时结果文件 usage.reported=false）
{"type":"error","kind":"...","retryable":false,"context":"..."}
{"type":"hunt_end","status":"succeeded|blocked|failed|cancelled","reason":"..."}
```

> 第二条起为载荷示例，**省略公共字段**（信封四字段每行都有）。
>
> `call_id` 是调用与结果配对的唯一标识：一轮内可能出现同一原语的多次调用，外部消费者据此配对（IA-1.5）；`policy_denied` 与 `check_result` 同属 L5 后的时点，故这两类事件也应携带相应 `call_id` 以便回溯到具体调用。
>
> **实现状态**见 xhunter-status.md 状态索引 · usage§5·已发出事件；本节事件类型与字段的**契约**以本手册为准，哪些已发出以状态文档为准；`effective_config` 的完整语义以结果文件（§6）与 FR-11.6 为准。
>
> **`hunt_end` 带累计用量**：终态事件里附上整次 Hunt 的累计 `usage`（形状同结果文件的 `usage`，含 `reported`）——平台读到终态即可记账，不必再去读结果文件。每轮的 `usage` 事件仍是**增量**。
>
> **终态由模型声明**：`hunt_end.status` 的取值来自模型最后答复里的声明（取值域由 Xhunter 定，**何时给哪个由平台的系统提示词决定**）；引擎只如实上报，不替模型下结论——机制性终止（取消、预算耗尽、推理失败等）除外，那些由引擎强制给出。
>
> **用量口径**（`usage` 事件与结果文件的 `usage` 同一口径）：`input_tokens` 是**全部输入** token——含从缓存读取的，也含写入缓存的；`cached_input_tokens` 是其中**从缓存读取**的部分（`input_tokens` 的子集，恒有 `cached ≤ input`）；`output_tokens` 是全部生成（含思考 token，上游也按输出计价）。**输出侧没有缓存**——被缓存的是请求前缀，命中永远记在**下一次请求的输入**上，本次输出全额计价。写入缓存的 token（cache write）当前并入 `input_tokens`、不单列。

**事件顺序**：`tool_call` / `tool_result` / `policy_denied` / `check_result` / `assistant_text` / `usage` 都在**轮边界**（模型流结束后立即）发出，不在接收段随流内事件即时产生——接收段零副作用（流内挂起无半写状态）；正文与用量在轮边界才被业务看到（收流在引擎里，而引擎没有事件出口）。同一轮内 `tool_call` 排在对应 `tool_result` 之前，按同一个 `call_id` 配对。事件**类型与字段不变**，仅时序从流内后移到轮边界。

**降级与错误的区别**：`degraded` 表示"照常跑下去了，但少了一部分"——例如某份技能说明格式非法被跳过、约定文件超限被截断。它是**非致命**的，任务继续；`error` 才表示当前动作失败。降级必须可见，因为"少了一部分"若不暴露，产出偏差要到评审时才看得出来。

**约束**：stdout 只允许出现事件行；日志、进度、调试信息一律走 stderr。字段只能追加，不可修改或删除（INV-5）。**通道写失败（含心跳）在轮边界复查即收敛为环境错误（退出 1）**——早停，不跑完剩余轮次。

---

## 6. 结果文件

由 `--result <path>` 指定；**无论成败都写**（FR-1.5）——平台靠它记账、决定是否重派。
写不出来属环境问题（退出码 1），不会静默继续。

**字段清单**（契约；哪些字段当前会写出见 xhunter-status.md 状态索引 · usage§6·待接入字段）：

```json
{
  "bounty_id": "...",
  "session_id": "...",
  "status": "succeeded|blocked|failed|cancelled",
  "reason": "no_tool_call",
  "exit_code": 0,
  "base_commit": "...",
  "branch": "xhunter/<session_id>",
  "commit_sha": "...",
  "patch_path": "...",
  "files_changed": ["..."],
  "effective_config": {
    "primitives": ["read","write","edit","find","glob","symbol_read","symbol_edit","symbol_rename","check","checkpoint"],
    "system_plugins": ["agentsmd","skills"],
    "user_plugins": ["task"],
    "filters": [],
    "policy": {"default":"deny","write_protected":".xhunter","write_exception":".xhunter/skills.draft"},
    "budget": {"max_turns":0,"max_tokens":0,"max_wall_clock_ms":0},
    "checkpoint": "on_structure",
    "ext": [],
    "platform": "linux/amd64"
  },
  "needs": ["..."],
  "assumptions": ["..."],
  "summary": "...",
  "usage": {"reported": true, "input_tokens": 0, "output_tokens": 0, "cached_input_tokens": 0, "turns": 0, "elapsed_ms": 0},
  "error": {"kind": "prepare_failed", "message": "...", "retryable": true}
}
```

> `error` 仅失败时出现；`retryable` 与退出码同源（环境问题才为 `true`）。
> `needs` / `assumptions` 是模型在正文固定小节里的自陈（FR-6.3）：`needs` 非空即表示模型选择停下，`status` 为 `blocked`（退出码 0，改动照常交付）。**未提供写成 `null`，不是 `[]`**——空数组会被读成"没有需要补全的条件"，那是另一句话。
> `usage.reported: false` 表示**上游未回报用量**——各项为 0 **不代表真的没用**，事件流里有对应的 `degraded` 记录（FR-9.7）。
> `patch_path` 仅在给了 `--patch` 且补丁产出成功时出现；补丁**排除会话材料目录**
> （`.xhunter/<session_id>/**`，FR-6.1）——实现状态见 xhunter-status.md 状态索引 · IA-11.6。

**尚未接线的字段**见 xhunter-status.md 状态索引 · usage§6·待接入字段。（不写空壳——空数组会被读成"没有门禁、没有假设"，那是另一句话；各字段的形状见 FR-6.3/6.4、FR-11.6、FR-1.5。）

> `gates` 必须**列出未运行的门禁**（`passed: null`），`effective_config` 是**只读快照**（FR-11.6）——两者都是 MR 评审的直接证据：前者回答"验收跑没跑、过没过"，后者回答"用的是哪套规则"。

---

## 7. 退出码

| 退出码 | 含义 | 平台侧建议动作 |
|---|---|---|
| 0 | **模型正常完成对话**——无论 `status` 是 `succeeded`、`blocked`，还是被**证据**判定的 `failed` | 退出码只说"对话走完了"；**任务处于什么状态看 `status`**。是否合入由平台与人决定（不在 Xhunter 契约内） |
| 1 | **这一趟没走成（环境/上游问题）**：未进入对话，或对话中被上游与环境打断 | **修好环境后可重跑**（允许重派） |
| 2 | **被引擎中止**：预算耗尽、连续失败止损、轮数硬顶、引擎侧错误 | **不重派**——重跑会停在同一个地方 |
| 3 | 被取消（SIGTERM / SIGINT） | 按取消流程处理 |

**终止条件 → 退出码映射**（消除"哪个算 1、哪个算 2"的歧义）：

| 终止条件 | 退出码 | 说明 |
|---|---|---|
| 本轮无 tool_call（含只有小结、零写操作） | 0 | **模型正常完成对话**；`status` 由模型声明（`succeeded` / `blocked`） |
| `required` 门禁未通过，或从未运行 | 0 | 对话正常走完，失败由**证据**给出：`status=failed`（FR-5.2d、FR-6.5），改动照常交付 |
| 缺内容 → `status=blocked` / `reason=needs_input` | 0 | 需要补全的清单已产出、改动已交付；补齐条件后带同一 session 重投（见 §3 澄清回路） |
| 轮数 / token / 墙钟耗尽 | 2 | 任务自身的预算问题，重跑同样会耗尽 |
| 连续失败止损、策略连续拒绝累积 | 2 | 模型在撞不该撞的墙 |
| 上下文达硬上限 | 2 | 按预算耗尽处理 |
| 轮数达机制硬顶 | 2 | 兜住编排缺陷导致的死循环 |
| `OnTurn` 报错（引擎侧错误，如过滤器挂掉） | 2 | 重跑是同一结果 |
| **未进入对话**：基线不可获取 / 工作区脏 / 部署事实缺失 / 模型接入缺失 / 装配缺件 | **1** | 环境问题：修好即可重跑 |
| **对话中被上游打断**：推理失败 / 流中断且不可重试 | **1** | 上游或环境问题，修好后重跑 |
| 恢复失败（材料损坏/版本不兼容、任务分支不可达、checkout 失败） | **1** | resume 是优化，重跑是兜底 |
| 门禁执行失败（命令不存在 / 无法创建进程 / 超时） | **1** | **不是质量结论**，是环境问题（FR-5.2d）——与"判定不通过"必须分开 |
| 检查点连续提交失败达上限（默认 3 次） | **1** | 远端不可用，本轮结束即收敛，不跑完剩余轮次 |
| stdout 写失败（通道断裂） | **1** | 消费者已不在通道上，写丢弃比继续跑更危险（FR-10.4、AC-19）；**轮边界复查**即收敛，不跑完剩余轮次 |
| 结果文件 / 补丁写失败 | **1** | 交不出交付记录与补丁（路径不可写、磁盘满） |
| SIGTERM / SIGINT | 3 | 平台主动取消 |

判据一句话：**"再来一次会不会是同样的结果"**——同样的算 **2**（被引擎中止）；换个环境、修好上游就会不同的算 **1**；对话正常走完的算 **0**（成败另看 `status`）；被外部打断的算 **3**。

---

## 8. 信号

| 信号 | 行为 |
|---|---|
| SIGTERM | 停止新调用，落盘状态与已有变更，以取消状态退出 |
| SIGINT | 同 SIGTERM |
| SIGKILL | 不可捕获，依赖定期落盘 + 平台侧超时兜底 |
