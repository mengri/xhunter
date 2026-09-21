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
| **Bounty 三要素** | `repo.remote`（可推送的远端）+ `repo.base_commit` + `repo.branch`（**分支名由驱动者指定，Xhunter 自己创建并推送**） |
| **凭据写权限** | 任务分支由 Xhunter 创建并推送，**只读凭据跑不通** |
| **门禁清单** | 来自 Bounty，或仓库 `.xhunter/gates.yml`（**从基线 commit 读**，FR-5.2g） |
| **事件消费** | 读 stdout NDJSON |
| **工作区隔离** | **必须由 Xhunter 自行 clone 到临时工作区**，不得把用户当前的仓库目录当工作区——直接改用户工作区会污染其未提交改动 |

**驱动者无关性不放松任何不变量**：编排校验仍共用同一份 `Validate()`、门禁清单来源不变、模型仍无 git 能力、权限仍经策略层。**换驱动者只换"谁投递、谁消费"，不换"什么被允许"。**

为便于本地驱动，Xhunter 提供 **Bounty 生成能力**（`xhunter run`）：给定本地仓库路径 + 任务描述，探测 `remote` / 基线 commit（取 HEAD）/ 任务分支名 / 门禁候选（`.xhunter/gates.yml` 存在性）/ 预算与策略默认值，生成 Bounty 文件（FR-1.10）。

---

## 2. 命令行入参

```
xhunter --bounty <path>           # Bounty 文件：任务描述、git 仓库 + 基线 commit、预算、策略、session 信息
        [--log-file <path>]       # 人类可读日志（默认 stderr）

xhunter run --repo <path> --task <text>   # 本地驱动：探测仓库并生成 Bounty
xhunter models update [--source <url>] [--dir <path>] [--output-reserve <n>]
xhunter models status [--dir <path>]
xhunter version
```

`models` 子命令是**环境侧动作**：网络或磁盘问题一律以退出码 2 退出（可重试），与任务失败（1）区分。

凭据通过环境变量注入（前缀 `XHUNTER_*`），不通过参数传递。

---

## 3. Bounty 结构

```json
{
  "bounty_id": "...",
  "task": "...",
  "repo": {"remote": "...", "branch": "task/<bounty-id>", "base_commit": "..."},
  "budget": {...},
  "policy": {...},
  "checkpoint": {"mode": "on_structure", "interval": null},
  "pipeline": {"pre": ["..."], "turn": {"guards": ["..."], "steps": ["..."]}, "post": ["..."]},
  "session": null
}
```

字段约定：

- `repo.branch` **必填**：本任务要使用的分支名（如 `xhunter/<bounty-id>`）。**该分支由 Xhunter 创建并推送**（FR-1.3）：不存在则从 `base_commit` 创建后推送；已存在且 tip 等于 `base_commit` 则为幂等成功；tip 不同即环境错误（退出码 2）。
- `repo.base_commit` **必填**：完整哈希，作为 patch 与提交的父提交基准。
- `checkpoint`（可选）：检查点策略，缺省即 `on_structure`（FR-1.3c）。**由平台按任务类型给**——小任务可 `final_only` 省推送，跨多文件的长任务宜 `every_turn` 或 `every_write`。
- `pipeline`（可选）：**流程编排**（FR-1.8）。缺省即默认编排，与不配置时行为一致；给出即**整体替换**默认编排（不做字段级合并），且必须通过启动期校验，否则以退出码 2 拒绝。字段形如 `{"pre": [...], "turn": {"guards": [...], "steps": [...]}, "post": [...]}`；环节名、默认编排与校验规则见 `xhunter-architecture.md` §4 与 §6。
- `session` 为 `null` 表示新任务；非空（或字段存在）时包含上次执行的写操作序列与 turn 序号，触发 FR-12.1 续跑流程。**存储位置在平台侧**。

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
{"type":"tool_call","tool":"edit","args":{...}}
{"type":"tool_result","tool":"edit","ok":true,"resolved_mode":"symbol","precision":"syntactic","summary":"...","duration_ms":12}
{"type":"check_result","cmd":"...","exit_code":0,"summary":"..."}
{"type":"policy_denied","action":"...","reason":"..."}
{"type":"usage","input_tokens":0,"output_tokens":0,"cost":0}
{"type":"heartbeat","phase":"...","elapsed_ms":0}
{"type":"context_compacted","level":"L2","released_tokens":0,"watermark":"warn"}
{"type":"error","kind":"...","retryable":false,"context":"..."}
{"type":"hunt_end","status":"succeeded|failed|cancelled","reason":"..."}
```

> 第二条起为载荷示例，**省略公共字段**（信封四字段每行都有）。

**事件顺序**：`tool_result` / `policy_denied` / `check_result` 三类事件**在模型流结束后（运行段 L5 响应处理）发出**，不随流内 `tool_use` 即时产生——接收段零副作用（流内挂起无半写状态）。事件**类型与字段不变**，仅时序后移。`assistant_text` / `usage` / `permission_request` 应答仍为流内实时。

**约束**：stdout 只允许出现事件行；日志、进度、调试信息一律走 stderr。字段只能追加，不可修改或删除（INV-5）。

---

## 6. 结果文件

```json
{
  "bounty_id": "...",
  "status": "succeeded",
  "base_commit": "...",
  "branch": "xhunter/<bounty_id>",
  "commit_sha": "...",
  "patch_path": "...",
  "files_changed": ["..."],
  "assumptions": ["..."],
  "usage": {"input_tokens": 0, "output_tokens": 0, "cost": 0, "turns": 0},
  "error": {"kind": "...", "message": "...", "retryable": false},
  "unverified": ["..."],
  "gates": [{"name": "test", "required": true, "passed": true, "cached": false, "source": "repo"}],
  "effective_config": {"gates_source": "base_commit", "checkpoint": {"mode": "on_structure"}, "budget": {"...": 0}, "ext_fingerprint": {"...": "..."}, "target_platform": "linux/amd64"},
  "session_delta": {"ops": 0, "turns": 0, "path": "..."}
}
```

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
| 校验未通过（check 失败） | 1 | patch 照常交付、状态为失败（FR-6.5） |
| SIGTERM / SIGINT | 3 | 平台主动取消 |

判据一句话：**"换个环境或修好配置就能成功"的算 2，其余失败算 1。**

---

## 8. 信号

| 信号 | 行为 |
|---|---|
| SIGTERM | 停止新调用，落盘状态与已有变更，以取消状态退出 |
| SIGINT | 同 SIGTERM |
| SIGKILL | 不可捕获，依赖定期落盘 + 平台侧超时兜底 |
