# Xhunter 里程碑拆解（以 2026-09-23 代码状态为基准）

> 本文是**执行排期**，不属于三份治理文档（产品设计 / 使用手册 / 架构设计）。它只回答一件事：**从今天的代码状态出发，接下来按什么顺序补齐、每一段凭什么能独立验收。**
> 若希望 `docs/` 保持只有三份，可整体移到仓库根 `MILESTONES.md` 或 `.workbuddy/` 下。

---

## 0. 结论先行

- **现状**：一期主干已经跑通——基线获取与任务分支推送、基础原语、执行流水线、策略与三重预算、三协议 Provider、事件出口与结果文件、信号取消，全部有实现且有用例；`linux/amd64` 上 `build`/`vet`/`test` 全绿（本次核对）。
- **缺口有两类**：① **能力缺口**（会话材料与恢复、门禁、符号原语、压缩、结构检查）；② **契约缺口**（事件流少发 `hunt_start`/`heartbeat`/`usage`/`error` 等，结果文件缺 `effective_config`/`gates`）。**已结清**：`find` 已落地、`edit` 的 `literal` 已进 `required`（2026-09-23）。
- **结论**：把剩余工作拆成 **12 个里程碑（MS-1 ~ MS-12）**，分五条车道推进，每条车道的里程碑都自带可跑的证据，不依赖后续里程碑才能证明自己成立。
- **建议起点**：`MS-1`（小、立刻可交付）与 `MS-4`（解锁产品分期 M1.5 的前置）可并行。体量最大、最需要先做一次设计确认的是 `MS-5`（门禁）与 `MS-8`（扩展接入 + 符号读写）。
- **关键路径**：`MS-1 → MS-2 → MS-4 → MS-6 → MS-7 → MS-11`（交付形态线）与 `MS-8 → MS-9 / MS-10`（符号线）。

---

## 1. 基准：当前代码状态（逐项核对过）

### 1.1 已落地：有实现、有用例

| 能力 | 落点 | 证据 |
|---|---|---|
| 中立模型契约 | `llm/`（Message/ToolDecl/Event/Fault/Provider/Session/Caps） | `llm/schema_test.go` |
| 模型调用循环 | `harness/`（双文件：`engine.go` + `types.go`，三组 handler） | `harness/engine_test.go`（IA-1.1~1.12） |
| 工作区读写抽象与实现 | `workspace/` + `internal/workspace/osfs`（路径边界、软链逃逸、模式语义） | `osfs_test.go` |
| git 四动作 | `internal/git/cli`：`PrepareBaseline`（按 SHA 浅取→完整 fetch→建分支→推送）、`Commit`（ff 推送、无改动不空提交）、`Diff`、`Patch`、`Clean` | `git_test.go`（IA-11.1~11.7） |
| 基础原语 4/5 | `hunt/basic`：`read`（行范围＋截断续读提示）、`write`（仅新建）、`edit`（内容寻址唯一性校验）、`glob` | `read_test.go` / `write_test.go` / `edit_test.go` / `glob_test.go` |
| 执行流水线 | `hunt/session.go`：Bind → 查表 → `Policy.Decide` → `Execute` → `Committer` 落盘 → 结果回灌＋事件 | `hunt/session_test.go` |
| 写盘唯一入口与读后校验 | `hunt/commit.go`（指纹校验、只改目标区间、批量先全验证后落盘） | `hunt/commit_test.go`（IA-3.3/3.6） |
| 策略引擎 | `internal/policy`：默认拒绝、路径边界（`.xhunter/**` 禁写、`skills.draft/**` 放行）、三重预算（token/轮数/墙钟） | `policy_test.go`（IA-4.5~4.7） |
| 提示词组装 | `prompt/agentsmd`（根级注入）· `prompt/skills`（清单注入）· `prompt/task`；内核条款＋环境事实由内核拼在固定位置 | `hunt/session_test.go`、`cmd/xhunter/prompt_test.go`（IA-2.1/2.10/2.11/2.13） |
| Provider 三协议 | `provider/openaichat` · `openairesponses` · `anthropicmessages` + `provider/adapter` + `providerconfig`（环境变量契约） | 各包 `*_test.go`（IA-8.x/9.x） |
| CLI 与组装层 | `cmd/xhunter`：`--bounty/--log-file/--result/--patch`、`version`；环境变量投递（仓库事实 ＋ 模型接入）、退出码 0/1/2/3、信号接线 | `bounty_test.go`、`channel_test.go`、`provider_test.go`、`signal_test.go`、`e2e_test.go` |
| 端到端夹具 | 真 git（本地裸仓库）＋ 真 SSE 假上游，不联网 | `TestEndToEnd_LocalRunProducesDeliveryCommit` 等 5 条 |
| 事件出口 | 信封四字段统一盖章；已发 `tool_result` · `policy_denied` · `degraded` · `deliverable` · `hunt_end` | `hunt/session.go:286,303`、`hunt/hooks.go:136,307,326` |
| 结果文件 | `--result` 无论成败都写；含终态/退出码/仓库事实/提交/改动清单/用量/error | `cmd/xhunter/result.go`（IA-12.9） |

### 1.2 声明齐备、但调用返回 `not_implemented`

| 原语 | 落点 | 行为 |
|---|---|---|
| `find` | `hunt/basic/find.go:33` | 结构化错误（不可重试） |
| `symbol_read` / `symbol_edit` / `symbol_rename` | `hunt/symbolic/symbolic.go:73` | 同上 |
| `check` | `hunt/gate/gate.go:42` | 同上 |

> 工具面本身已定格为 **9 + 1**（`cmd/xhunter/primitives.go` 九个原语 ＋ `Session.decls()` 殿后追加 `checkpoint`），与 FR-4.11 一致——**未实现只改调用结果，不改工具面**，这条纪律已被测试守住。

### 1.3 规格在、代码缺（缺口清单）

| 缺口 | 现状证据 |
|---|---|
| 会话材料落盘与恢复 | `cmd/xhunter/context.go:55` — `sessionRecorder` 只记内存，`Snapshot()` 直接返回 `nil` |
| 上下文压缩 | `contextBuilder` 只有组装（`Assemble`），无水位、无下压 |
| 门禁全链 | `hunt/hooks.go:59` — `s.gates = nil`，一期暂空 |
| 符号能力与扩展宿主 | `ext/ext.go` 只有接口，无实现；`hunt/symbolic` 声明不实现 |
| 结构检查（自动检查点的判据） | `hunt/hooks.go` 的 `structuralPoint()` 恒为 false——判据随符号扩展接入；**接入前不产生自动检查点**（判据不可判定 → 不提交），跳过原因如实进日志 |
| 结构检查 | 无 |
| 提交连败上限 | `checkpoint` 失败只记 warn，无连败计数 |
| 流看门狗 | 无（`StreamIdleTimeout` 未接入） |
| 止损两段式 | `hunt/policy.go` 只有 `Decide`/`Charge`/`Exhausted`，无 `ObserveFailure`/`DeniedCount` |
| 生效配置快照 | 无 `effective_config`，`hunt_start` 也不存在 |
| **用量口径（输出明细与缓存写）** | 输入侧已按统一口径落地（见 §7）：全部输入含缓存读/写、缓存读为子集。仍未取的：OpenAI 的输出明细（`completion_tokens_details` / `output_tokens_details`：reasoning / audio / accepted·rejected prediction），以及把「缓存写」单列出来（Anthropic 的写溢价 1.25×/2× 目前按普通输入价计） |
| 差异排除材料目录 | `internal/git/cli/git.go:173` — `Diff`/`Patch` 无 `.xhunter/<session_id>/**` 排除 |
| 本地驱动 | `xhunter run`（Bounty 生成器）尚未接线（使用手册 §1.2 已标注） |

### 1.4 本次核对发现的**口径不一致**（不影响编译，但影响"文档说的 = 代码做的"）

| 不一致 | 事实 | 影响 |
|---|---|---|
| `edit.literal` 的必填性 | 实现里必填（缺它返回 `bad_selector`），schema 的 `required` 只有 `path`/`content`（`hunt/basic/edit.go:31`） | 模型照 schema 省略 → 本可避免的错误（产品设计 §6 已自标注"待修"） |
| 文档列了 6 个 `hunt_start` 载荷事件，实际未发出 | `hunt_start` / `heartbeat`（无调用点）/ `usage` / `assistant_text` / `tool_call` / `error` 均未发 | 平台侧看不到"开始了""还活着""花了多少" |
| `check.py --wsl` 在映射盘工作区不可用 | 脚本由 `__file__` 推导工作目录，映射盘（UNC）路径在 `--wsl` 分支里被原样拼进 bash 命令，`cd` 失败（本次实测 `exit 1: cd: \localhostworkagentsxhunter`） | 规范验证入口在这台机器上跑不起来；本次基准是**直接调用 WSL 内 go** 得到的（`wsl.exe -e bash -c 'export PATH=/home/kingsword/.gvm/gos/go1.26.5/bin:$PATH; cd /work/agents/xhunter && go build ./... && go vet ./... && go test ./... -count=1'` → 全绿） |

---

## 2. 拆解原则：什么算"可独自验收"

一个里程碑只有同时满足下面 6 条，才算独立可验收：

1. **验收对象是对外可见的**——一条 FR/AC/IA，或使用手册里的外部契约（CLI、事件流、结果文件、退出码）；不是"重构了某层"。
2. **自带证据**——有用例（`go test`）或端到端夹具（真 git ＋ 假上游）；**用例不依赖模型与网络**（NFR-8）。
3. **无前向依赖**——不需要后续里程碑的实现来证明自己成立；允许依赖之前的里程碑。
4. **结束时是完整的下一步**——不留在"半截状态"（若必须切开，切在"能独立证明"的边界上，例如材料落盘 vs 恢复）。
5. **不放松任何不变量**——INV-1~INV-11 一条不破；新增能力只经既有落点（策略裁决、`Committer` 写盘、事件出口）。
6. **文档同步**——该里程碑涉及的 `FR/IA` 状态句、§14 缺口行、使用手册外部契约在同一次改动里更新。

---

## 3. 里程碑总览

> 编号是**执行顺序**，与产品设计的 `M1 / M1.5 / M2 / M3` 分期不是同一套编号；下表给出映射。

| # | 名称 | 对应分期 | 依赖 | 体量 | 主要 FR / IA |
|---|---|---|---|---|---|
| **MS-1** | 验收入口与契约对齐 | 一期收口 | — | 小 | FR-2.2、IA-3.16 |
| **MS-2** | 运行可观测补齐（事件流 ＋ 生效配置快照） | 一期收口 | MS-1 | 中 | FR-10、FR-11.1/11.6、IA-5.2/5.4 |
| **MS-3** | 止损完备（两段式止损） | 一期收口 | MS-2 | 中 | FR-9.1/9.4、IA-4.8/4.9 |
| **MS-4** | 检查点分档与提交健壮性 | 一期收口 | MS-1 | 中 | FR-1.3b/1.3c/1.11②、IA-11.8/11.10/11.11/11.13 |
| **MS-5** | 门禁落地（`check` 实现 ＋ 全链护栏） | 一期收口 | MS-4 | 大 | FR-5.2b~5.2i、IA-11.12 |
| **MS-6** | 会话材料落盘 | M1.5 前置 | MS-4 | 中 | FR-12.2/12.2b/12.3、IA-6.1/6.1b/6.1c |
| **MS-7** | 会话恢复（resume） | M1.5 | MS-6 | 大 | FR-12.1/12.6、AC-7、IA-6.2/6.3/6.4 |
| **MS-8** | 扩展接入与符号读写 | M2 | MS-2 | 大 | FR-13、FR-4.1/4.2/4.4~4.8/4.13、IA-7.1~7.6 |
| **MS-9** | 结构检查与 `on_structure` 默认档 | M2 收尾 | MS-5、MS-8 | 中 | FR-1.3d、IA-11.12 |
| **MS-10** | `symbol_rename`（跨文件重命名）与规模上报 | M3 | MS-8 | 中 | FR-4.3、FR-6.2 |
| **MS-11** | 上下文压缩 | 二期 | MS-6、MS-7 | 大 | FR-14 全部、AC-17/AC-18 |
| **MS-12** | 运行段看护与本地驱动 | 二期 | MS-3、MS-2 | 中 | FR-1.11①、INV-4、FR-1.10 |

---

## 4. 依赖与车道

```
车道 A（契约与观测）  MS-1 ──► MS-2 ──► MS-3 ──────────────┐
车道 B（交付形态）    MS-4 ──► MS-6 ──► MS-7 ──► MS-11      │（MS-12 需要 MS-3）
车道 C（门禁）        MS-5（依赖 MS-4）                      │
车道 D（符号）        MS-8 ──► MS-9（另需 MS-5）             │
                            └─► MS-10                       │
车道 E（看护与驱动）                            MS-12 ◄────┘

关键路径：MS-1 → MS-2 → MS-4 → MS-6 → MS-7 → MS-11
可并行：MS-3 / MS-4 / MS-5 三者互不依赖；MS-8 可与车道 B 并行
```

---

## 5. 逐个里程碑

### MS-1 验收入口与契约对齐

**目标**：让"规范验证入口"在这台机器上能用，并让代码与已声明的契约一致（三处小账一次结清）。

**状态**：`find` 落地与 `edit.literal` 已进 `required` **均已完成**（2026-09-23，见 §9 第 9 条）；**只剩 `check.py` 的 `--wsl` 修法**。

**范围**
- 修 `scripts/check.py` 的 `--wsl` 分支：工作目录按 WSL 视角给出（映射盘/UNC 下不能把 Windows 路径原样拼进 bash），使 `check.py --wsl --race` 一条命令可用。
- `find` 落地（**已完成**）：内容检索（`literal` ＋ 可选 `path`/`scope`），复用 `glob` 枚举 ＋ `read` 取内容，命中带 **文件 ∶ 行号 ∶ 该行**；`scope` 收窄到目录、`path` 收窄到单文件；超上限只显示前 100 处但**总量照报**、单行限宽 160；跳过的大文件（>2MB）与读取失败如实附注；**未找到是结论而非错误**。
- `edit` 的 `literal` 进 schema `required`（消除产品设计 §6 自标注的"已知不一致"）。
- 文档口径同步（**已完成**）：产品设计 §6 的一期实现状态与「已知不一致」、架构 §12.3 的 IA-3.15（去掉 find）＋ 新增 IA-3.19。

**独立验收的证据**
- `python scripts/check.py --wsl --race` → `exit 0`（这是本里程碑的第一验收项，也是后续所有里程碑的 DoD 前置）。
- `hunt/basic/find_test.go`（**已完成**）：`TestFind_MatchesWithLineNumbers`、`TestFind_NoMatchIsSuccessWithExplicitText`、`TestFind_ScopeLimitsSearch`、`TestFind_PathLimitsToSingleFile`、`TestFind_TruncatesHitsButReportsTotal`（只显示前 N 处但总量照报）、`TestFind_RequiresLiteral`、`TestFind_DeclShape`。
- `TestEdit_DeclShape`（或 `primitives` 侧的形状用例）断言 `literal` ∈ schema `required`，且与绑定层"拒绝未知字段"的口径一致（产品设计 §6 的两条纪律）。

**依赖**：无。**不做**：不动 `symbolic`/`gate` 的 `not_implemented` 行为（它们正确地体现"分期不裁剪工具面"）。

**风险**：`check.py` 的修法要避免把"本机路径映射"写成产品文档口径——映射知识只留在脚本里，不进 `AGENTS.md`/`README.md`。

---

### MS-2 运行可观测补齐（事件流 ＋ 生效配置快照）

**目标**：把使用手册 §5 里"契约目标"的事件补齐，让平台侧能看见"开始了／还活着／花了多少／怎么死的"，并把本次运行的装配快照落进 `hunt_start` 与结果文件。

**范围**
- `hunt_start`：`Prepare` 成功后立即发（含任务与仓库事实），并按 FR-11.6 带上**生效配置快照**：原语清单与顺序、两段插件名与顺序、结果过滤器链、策略配置、三重预算上限、检查点策略、扩展能力指纹（当前为空）、目标平台。
- `heartbeat`：接上已有实现（`cmd/xhunter/sink.go:80`），运行期按固定间隔发，携带 `phase` 与 `elapsed_ms`；定时器随 `ctx` 取消立即停（INV-8）。
- `usage`：**每轮末发增量**（这一轮的），**`hunt_end` 带累计**（整次 Hunt 的）——平台读终态即可记账。
- `assistant_text`：收流合并后**必发**（最终答复是交付物的一部分——任务可能只要那份小结），且与 `ContextBuilder` 的历史保持一致、不得改写；同一份正文进结果文件 `summary`。
- `tool_call`：执行前发（`call_id` / `tool` / `args`）。
- `error`：各阶段失败的结构化事件（`prepare_failed` / `infer_failed` / 流错误 / 交付写失败），带 `retryable`。
- 结果文件补 `effective_config`（只读快照，模型改不了）。
- **澄清回路的三件产物**（FR-6.3）：终态新增 `harness.StatusBlocked`（`status=blocked` / `reason=needs_input` / **退出码 0**；分类靠 `status`）。**终态采纳**：`status` 由模型声明（引擎不推断）；**已取消 `no_output` 闸门**（零写也可能是合法交付——任务本身可能就是产出一份小结），但 `required` 门禁未过仍按**证据**覆盖为 `failed`。另两件产物：事件 `needs_input`（逐条）、结果文件 `needs` 与 `assumptions` / `unverified`。解析对象是**正文里的固定小节**（`## 需要补全` / `## 假设`），解析只做**切行去前缀**、不做语义理解，且**轮边界与收尾都解析**（读到 `## 需要补全` 即收敛，不等收尾）；**三态**：未提供 → `null` ＋ 原因（**不写 `[]`**）。**另加**：`usage.reported`（上游未回报用量时标 false）与该情形的 `degraded`（`scope: usage`）——见 §9 第 13 条。

**独立验收的证据**
- `cmd/xhunter/e2e_test.go` 扩一条事件序列断言：一次成功运行的事件流**包含** `hunt_start` / `tool_call` / `tool_result` / `usage` / `deliverable` / `hunt_end`，且 `hunt_end` 是最后一条（IA-12.6 的加强版）。
- 新用例 `TestEventSink_StdoutIsPureNDJSON`（IA-5.2 待补项）：stdout 每行合法 JSON、`type` 在顶层、无误入内容；日志仍只在 stderr。
- `TestEventSink_HeartbeatStopsOnCancel`：取消后不再有心跳（定时器不泄漏）。
- 结果文件断言：`effective_config` 存在且含原语清单顺序（与 `TestDefaultTools_FaceIsFixed` 的期望同源）。
- 澄清回路：`TestFinalize_NeedsInputConvergesToBlocked`、`TestFinalize_SectionsParsedFromLastAssistantText`、`TestFinalize_UnprovidedSectionIsNullNotEmpty`（未提供 ≠ 空 ≠ N 条）。

**依赖**：MS-1。**不做**：压缩事件（MS-11）、门禁事件（MS-5）、`assumption`（见 §7）。

**风险**：`heartbeat` 与"stdout 写失败 = 环境错误"的关系要一次说清——心跳也是事件，写失败同样要能被装配层取到（`sink.Failed()` 已具备）。

---

### MS-3 止损完备（两段式）

**目标**：把 FR-9.4 的两段式止损补上——达到阈值先换策略，超过上限才失败；并把"连续策略拒绝"纳入终止条件。

**范围**
- 业务止损（`hunt.Policy` 扩展）：`ObserveFailure`（连续同类失败达阈值 → 回灌"换策略"提示，不终止）、超上限 → 失败；`DeniedCount`（连续策略拒绝累积 → 失败）。
- 与 harness 的分工写清：机制硬顶（轮数、连续失败轮数）留在 `harness.Config`，业务止损在 `Policy`——前者不认识原因，后者认得。
- 新终止条件进使用手册 §7 的映射表；原因进事件流与结果文件。

**独立验收的证据**
- 单测：`TestObserveFailure_SwitchThenTerminate`、`TestDeniedCount_TerminatesAfterThreshold`。
- e2e：`TestEndToEnd_DeniedStreakFailsTheRun`（策略连续拒绝 → 退出 2：被引擎中止）；`TestEndToEnd_RepeatedFailureSwitchesBeforeFailing`（同类失败达阈值时先出现"换策略"提示，超上限才失败）。
- IA-4.8 / IA-4.9 的"待接入"改成用例名。

**依赖**：MS-2（失败与拒绝要能在事件流里如实上报）。**不做**：影响面阈值（已决：不做，见 §9 第 6 条）。

**风险**：`hunt.Policy` 是公开契约，加方法属接口扩展——需同批更新架构 §7.4/§12.4 与使用手册 §7。"同类失败"的判据要先用测试定死（按错误 kind 还是按原语），否则"换策略"会退化成"永不触发"或"过早触发"。

### MS-4 检查点语义与提交健壮性

**目标**：把检查点收敛成「**只在结构完整点上自动产生**」（判据不可判定 → 不提交这一半已落地），并补齐连败收敛与提交语义的断言。

**范围**
- **判据接入位**：`hunt/hooks.go` 的 `structuralPoint()` 目前恒 false；本里程碑保证**判据不可判定时确实不提交**、且跳过原因如实进日志与事件。
- **连败上限**：检查点连续提交失败达 3 次 → **本轮结束即收敛**为环境错误（**退出 1**），不跑完剩余轮次。
- **时间语义与不可见性**：断言 `Commit` 只在轮边界与收尾被调用、`PrepareBaseline` 是 `Prepare` 第一步、工具面不含任何 git 原语（IA-11.8 / IA-11.10）。
- **意图兑现**：一次请求只兑现一次；无改动不产生空提交但意图照样消费；提交信息由执行体合成（前缀固定 ＋ 净化后的理由）。

**独立验收的证据**
- `TestCheckpoint_NoAutoCheckpointOffStructuralPoint`、`TestCheckpoint_LogsNoOpWhenNothingWasCommitted`（均已有）。
- `TestCheckpoint_StreakLimitConvergesAsEnvError`（假 git 实现连续失败 → **退出 1**，且不再进入下一轮）。
- `TestCheckpoint_ModelRequestIsConsumedOnceAndSkipsEmptyCommit`（IA-11.13 补全）＋ 提交信息合成断言（`TestSanitizeIntent` 已有）。
- 顺序与不可见性：`TestWiring_PrepareBaselineRunsBeforeAnyTool`、`TestDefaultTools_HasNoGitPrimitive`（IA-11.8）。

**依赖**：MS-1（真实判据随 MS-9）。**不做**：门禁驱动检查点（MS-5，那条会插进触发链最前）。

**风险**：判据接入后，「未注册语言的仓库」只在收尾提交一次——这是刻意的取舍（残次品不是可用的检查点）。必须在生效快照与事件里如实标注「结构判据不可判定」，否则平台会误以为有检查点。

### MS-5 门禁落地（`check` 实现 ＋ 全链护栏）

**目标**：把 §7.8 从规格变成产品能力：门禁是**具名条目**，不是任意命令执行。这是本排期里最需要先做设计确认的两块之一（另一块是 MS-8）。

**范围**
- **清单来源裁决**（FR-5.2b）：Bounty 下发 > **基线 commit** 的仓库根 `gates.yml`（`git show <base_commit>:gates.yml`，不从工作区读）> 无。**不限制模型修改该文件**——写它照样进交付 diff，只是本次不生效。
- **执行语义**：`argv` 数组直启（不经 shell）、`argv[0]` 为 `sh`/`bash`/`zsh`/`dash` 一律拒绝、`timeout`（`context` ＋ 杀进程组）、`dir`（monorepo）、输出限长保留头尾、非交互（`CI=1`/`NO_COLOR=1`/`GIT_TERMINAL_PROMPT=0`）。
- **环境隔离**（FR-5.2i）：最小集 ＋ 清单显式 `env`；**Xhunter 自身的 git 凭据绝不下传**。
- **判据 `expect`**：对象 `{kind*, pattern, max, stream}`——`exit_zero` / `empty_output` / `regex` / `max_count`；`gofmt -l` 这类必须能表达"输出为空"；**判据在全量输出上算**（截断不影响判定）。
- **两类失败分开**（FR-5.2d）：执行失败（命令不存在/起不来/超时）= 环境错误（2）；判定不通过 = 质量结论。`required` 未通过**或从未运行** → 终态失败（1），**但改动照常交付**（FR-6.5）。
- **结果缓存**（FR-5.2e）：按待提交改动的内容指纹缓存，命中标 `cached`。
- **检查点联动**（FR-5.2c）：门禁通过 → 本轮必提交（插到 MS-4 的优先级链最前）；未通过 → 抑制后续自动检查点直到下次通过或收尾。
- **收尾补跑**（FR-5.2f）：交付提交前对未通过的 `required` 门禁各跑一次；墙钟预算已耗尽则不补跑，按"未运行"判失败。
- **上报**：`check_result` 事件（`gate`/`passed`/`cached`/`exit_code`/`duration_ms`/`source`）；结果文件 `gates` 数组**含未运行项**（`passed: null`）；门禁名注入 user 段供模型调用。
- **豁免护栏**（FR-5.2h）：Bounty 授予 `gates_source: working_tree` 时**元门禁**（schema 合法 ＋ 每个 `argv` 可启动）＋ **强度不得降低**（同名 `required` 门禁的 `argv`/`expect` 不可改，只允许新增）＋ `gate_config_changed` 事件与清单来源标注；模型拿不到豁免。**清单文件不限制模型改，因此这三条护栏在豁免生效时是唯一防线**，必须一起落地。
- 清空 `hunt/hooks.go:59` 的 `s.gates = nil`。

**独立验收的证据**
- 单测（`hunt/gate`）：`TestCheck_ArgvIsDirectAndRejectsShell`、`TestCheck_ExpectVariants`（四档各一）、`TestCheck_TimeoutIsEnvError`、`TestCheck_NonZeroExitIsQualityFailure`、`TestCheck_CacheHitByFingerprint`、`TestCheck_EnvIsMinimalAndCredentialFree`（断言子进程环境里没有 git 凭据变量）。
- git 侧：`TestGates_LoadedFromBaseCommitNotWorktree`（工作区篡改清单不影响本次运行）。
- e2e：`TestEndToEnd_RequiredGateNeverRunFailsWithDelivery`（`status=failed` ＋ 退出码 0、提交仍在远端、`gates` 含 `passed: null`）；`TestEndToEnd_GateFailureSuppressesCheckpoint`（破损状态不被钉住）。
- `TestGate_SourcePrecedence`（Bounty 覆盖仓库声明）＋ `TestGate_WorkingTreeExemptionRequiresMetaGate`。

**依赖**：MS-4（检查点优先级链与连败语义先到位）。**不做**：结构检查（MS-9）、影响的"每轮自动跑门禁"（明确不做，见架构 §7.8）。

**风险**：① `expect` 的 `regex` / `max_count` 语义已在 §9 第 14 条定死（否则门禁会"永远通过"）；② `working_tree` 豁免是模型能触发的路径，元门禁与强度校验必须先于清单生效；③ 跨平台 `/bin/sh` 差异——确认只依赖 `argv` 直启，不依赖 shell 解释器存在。

---

### MS-6 会话材料落盘

**目标**：把会话材料从"内存里的记录"变成"随检查点进分支的文件"，为恢复准备唯一状态源。

**范围**
- 材料路径 `.xhunter/<session_id>/session.jsonl`（**按任务隔离**，FR-12.2b），带 `schema_version`，内容含对话历史、写操作序列（工具名 ＋ 完整参数 ＋ 结果摘要 ＋ turn 序号）、用量、扩展能力指纹字段（MS-8 填充）。
- **随检查点提交**：`add -f`（仓库 `.gitignore` 恰好忽略 `.xhunter/` 时不得静默跳过）；周期性落盘（FR-12.3b），与关键状态同源。
- **排除交付 diff**：`Diff`/`Patch` 排除 `.xhunter/<session_id>/**`，**不排除**该目录之外的 `.xhunter/` 路径（如 `skills.draft/**`，AC-25）。
- 材料不可篡改：策略已禁写 `.xhunter/**`（已有），git 侧模型无能力（已有）——本里程碑只补断言。

**独立验收的证据**
- `TestSave_MaterialLandsUnderSessionDirWithSchemaVersion`（形状与版本字段）。
- `TestDiff_ExcludesMaterialDirButKeepsSkillsDraft`（两条排除语义分开断言）。
- `TestCommit_ForceAddsMaterialEvenWhenGitignored`（夹具仓库带 `.gitignore` 忽略 `.xhunter/`）。
- `TestSnapshot_FailureDoesNotBlockTheRun`（IA-6.6，材料写失败只降级）。
- `TestMaterial_LoadRejectsUnknownSchemaVersion`（为 MS-7 的准备：不兼容 → 明确错误类型，不自动迁移）。

**依赖**：MS-4（提交时机稳定后再谈"材料随哪个提交走"）。**不做**：恢复流程（MS-7）。

**风险**：材料体积会随轮次增长并进每次提交——需在实现里给出截断/上限口径（超限如何处理要在事件里可见，不得静默截断）。

---

### MS-7 会话恢复（resume）

**目标**：跨机器 failover 成立：崩溃后向新机器投递同一会话，接着最后一个检查点继续，**不重放写操作**。

**范围**
- 投递给出 `XHUNTER_SESSION_ID` → `Prepare` 里：checkout **任务分支 tip（最后检查点）** → 读回材料 → 回灌上下文（过 `ContextBuilder`，压缩未接入时直通，FR-14.8）→ 从**下一轮**继续。
- **零工具执行、零模型调用**：恢复段不调用任何原语。
- **恢复失败**（材料损坏/版本不兼容/分支不可达/checkout 失败）→ **退出码 1**；材料**不存在**是正常情况（该会话暂无历史），与"损坏"分开。
- 分支语义沿用已有实现：`PrepareBaseline` 已能判定"已存在且 tip 为基线或其后代 = 续跑"（`TestPrepareBaseline_ResumeChecksOutBranchTip`）。
- **澄清后续跑**（FR-12.1 的第二场景，见 §9 第 4 条）：新 Bounty 的补充条件作为**新的 user 消息**追加在回灌历史之后——模型因此看得到「上次列了什么问题、这次给了什么答案」；断言对象是**假上游收到的请求内容**，而不是只看终态。

**独立验收的证据**
- e2e：`TestEndToEnd_ResumeContinuesFromLastCheckpoint`——第一次投递跑到中途取消/失败 → 第二次投递同一 `session_id` → 断言：工作区 == 最后检查点、**未重做已完成轮次**（假上游记录的第二轮请求数可判定）、恢复段零写操作。
- `TestEndToEnd_ResumeCorruptedMaterialIsEnvError`（**退出 1**）。
- `TestEndToEnd_FreshSessionWithoutMaterialIsNotAnError`（"不存在"与"损坏"分开）。
- `TestEndToEnd_ResumeAfterClarificationAppliesNewConditions`（第一次投递收敛为 `blocked` ＋ `needs`；补条件后同 session 重投 → 不重做已完成轮次，且新条件出现在请求里）。
- AC-7 的完整断言（架构 §14.2 已把这个缺口写清）。

**依赖**：MS-6。**不做**：压缩回灌（MS-11 接上同一管线）。

**风险**：假上游夹具要能"制造崩溃点"（例如在某一轮返回流错误），并能让第二次运行的请求序列可断言——夹具能力需先确认，这是本里程碑最容易被低估的部分。

---

### MS-8 扩展接入与符号读写

**目标**：符号能力以本地进程外挂接入，`symbol_read` / `symbol_edit` 落地（首发语言 Go），基础原语不受影响。

**范围**
- `ext.ExtHost` 实现：本地 **stdio** 子进程 ＋ 懒启动 ＋ 随 Hunt 回收 ＋ 崩溃隔离 ＋ 超时（FR-13.1/13.5/13.6）。
- 能力描述符 `ExtCaps`（能力集 ＋ 精度等级）→ 决定符号路径可用性与结果标注；**核心不感知后端种类**。
- `symbol_read` / `symbol_edit`：先定位到**字节区间**，再复用 `hunt/basic` 的读写；写盘仍走 `Committer`（区间替换，不重新打印语法树）。
- **降级**：扩展缺失/启动失败/超时/语言未注册 → 结构化错误 ＋ 显式提示"改用 `edit`"，**工具名不撤回**（FR-13.4、AC-11/AC-12/AC-26）；非法/不可解析源码 → 降级到文本并显式提示（FR-4.5）。
- 寻址可观测：每次调用上报 `precision`（syntactic / semantic）与降级原因（FR-4.13）。
- 能力指纹写入会话材料（与 MS-6 的字段对接；不一致**只记录不阻断**）。

**独立验收的证据**
- 假扩展夹具（真子进程）四态：可用 / 不可用 / 崩溃 / 超时——`TestExt_UnavailableDegradesWithoutFailingTheTask`、`TestExt_CrashDoesNotHangTheLoop`、`TestExt_CloseReclaimsProcess`。
- 符号原语：`TestSymbolRead_LocatesAndReadsRange`、`TestSymbolEdit_ReplacesOnlyTargetRange`（复用 `Committer` 的区间断言）、`TestSymbolic_ReportsStructuredErrorWhenLanguageUnregistered`。
- **工具面恒定对照**（IA-7.1 待补）：同一份装配在"扩展可用 / 不可用"两种情形下，工具名集合与参数 schema **完全相同**。
- 精度标注：`TestSymbolic_ReportsSyntacticPrecision`。

**依赖**：MS-2（事件里要能看到降级与精度）。**前置决策（必须先定）**：扩展进程的协议形态（MCP stdio 还是自有最小协议）与首发后端形态（自包含语法解析后端的载体：独立二进制？语言清单？）。

**风险**：① 这是唯一"轻则误改用户代码"的组件，区间替换必须复用已有 `Committer` 校验（越界/指纹）；② 扩展不可信边界：不继承凭据环境（FR-8.4/13.6）；③ NFR-1 要求"零第三方运行时依赖"——后端不能拖进一个语言运行时。

---

### MS-9 结构检查（自动检查点的判据）

**目标**：让 `structuralPoint()` 有真实判据，自动检查点从此落在结构完整点上。

**范围**
- 判据：① 目标文件**语法完整**（`ParseOK`）；② 单次编辑的字节区间**封闭在某个符号范围内**（依赖 MS-8 的符号范围）。
- **三态**：通过 → 提交；可用但未通过（语法残缺）→ 不提交，等下一个结构完整点；**不可判定**（语言未注册 / 扩展不可用）→ **不提交**（已落地），并如实上报「结构判据不可判定」。
- 触发链补全：门禁通过 > 模型显式 > 结构检查通过 > **都不满足则不提交**。

**独立验收的证据**
- `TestStructural_ParseOKFlipCommits`、`TestStructural_IncompleteSyntaxSuppresses`、`TestStructural_UndecidableDoesNotCommit`。
- 未注册语言仓库的 e2e：**只在收尾提交一次**，且事件与生效快照标注「结构判据不可判定」（这是预期行为，不是缺陷）。

**依赖**：MS-5（触发链的门禁一段）、MS-8（符号范围）。

**风险**：`ParseOK` 依赖扩展——判据不可用时**绝不能退化成「按轮提交」**（那就是残次品），也不能静默不报：两种选择必须显式且如实上报。

### MS-10 `symbol_rename`（跨文件重命名）与规模上报

**目标**：从"能改"到"改得对"：跨文件重命名一次完成全部改动，并把**改动规模**作为证据上报。

**范围**
- `symbol_rename`：改名 ＋ 更新全部引用点，一次调用完成；**不静默降级为文本替换**——半完成的重命名比失败更坏。
- **规模上报**（`ext.Impact`：文件数 / 处数 / `Unknown`）→ 进结果摘要、事件流与变更说明（FR-6.2），作为无人 review 时下游判断"能不能继续往下自动化"的依据；**不做阈值、不做拦截**——"改得多就保守拒绝"是偏好，不是判据。
- `Unknown`（语法级后端无法穷尽引用）**如实标注**，不得当作"没有引用"——"不可知"与"零"是两件事。
- **同源要求**：上报的规模与落盘的编辑计划必须来自**同一次定位结果**（`Impact` 由扩展自报，分两次取就能被低报）。`ext.Prepared` 目前只能表达单个文件 ＋ 单个区间，rename 需要"一次定位给出完整编辑计划"的形状——这是本里程碑要一并立起来的。

**独立验收的证据**
- `TestSymbolRename_UpdatesDeclarationAndAllReferences`（夹具仓库多文件引用）。
- `TestSymbolRename_CanResolveFalseIsStructuredErrorNotTextReplace`。
- `TestSymbolRename_ReportsFootprint`（摘要与事件含文件数/处数）＋ `TestSymbolRename_UnknownFootprintIsReportedNotGuessed`。
- 策略侧**不再有**影响面用例——原 `TestDecide_ImpactAboveThresholdDenied` 随阈值一并删除。

**依赖**：MS-8。**不做**：影响面阈值（已决，见 §9 第 6 条）。

### MS-11 上下文压缩

**目标**：长任务不因窗口耗尽而硬失败，且压缩**可复现、可观测、不丢交付物**。

**范围**
- 水位触发（默认 70% 预警 / 50% 目标 / 90% 硬上限，基数 = `limit.context − 固定开销 − limit.output`）＋ 冷却期；**不允许"超限才压"**。
- 分层下压 L0→L4，够用即停；**默认零模型调用**；L4 摘要**生成一次即落盘、恢复时读回**（不在恢复路径上重新生成）。
- **保丢优先级**：永不丢（Bounty 正文与验收标准、工作区约定、当前轮 messages、**写操作记录**）。
- 结构化工作日志由会话材料**投影**（内容与顺序可复现，可脱离模型单测）。
- `context_compacted` 事件（层级 ＋ 释放 token 量）。
- 交付物不依赖上下文：patch / 改动清单 / 假设清单一律由执行体从材料生成（补断言）。

**独立验收的证据**
- `TestWatermark_TriggersAtWarnAndCoolsDown`、`TestCompaction_LayersDownToFit`、`TestCompaction_ProjectionIsReproducible`（同一份材料两次投影一致，AC-18）。
- 保真：`TestCompaction_NeverDropsBountyOrWriteOps`（AC-17）、`TestDeliverables_DoNotDependOnContext`（FR-14.6）。
- e2e：构造超窗场景（假上游返回大体积文本）→ 有 `context_compacted` 事件且任务仍收敛。

**依赖**：MS-6（材料是投影来源）、MS-7（恢复回灌同管线）。**风险**：压缩改的是"已缓存前缀"——低频一次压到位，避免持续微调让前缀缓存全失效。

---

### MS-12 运行段看护与本地驱动

**目标**：把"挂住"这条无头场景的硬要求接上（流内挂起必须有上界），并补上 FR-1.10 的本地驱动便利工具。

**范围**
- **流看门狗**（FR-1.11①）：接收段不活动超时（默认 120s）→ `Cancel()` → 环境错误（2）；流内挂起时长有上界。
- **本地驱动**（FR-1.10）：`xhunter run --repo <path> --task <text>` 生成 Bounty 文件（探测 remote / HEAD / 分支名 / 门禁候选 / 预算与策略默认值）；**必须 clone 到临时工作区**（不得把用户当前仓库当工作区）；不放松任何不变量（门禁来源、凭据写权限、模型无 git 能力）。**它同时是澄清回路的入口**：人照着结果文件里的 `needs` 补条件、生成新 Bounty、带**同一 session** 重投。

**独立验收的证据**
- `TestWatchdog_IdleStreamCancelsAsEnvError`（假上游 accept 后不写任何数据）。
- 权限裁决的接口面：`TestSession_HasNoPermissionChannel`（断言 `llm.Session` 的方法集只有 `Events`/`Cancel`，`EventKind` 无授权请求形态）。
- 生成器：`TestRunCmd_GeneratesBountyFromLocalRepo`（生成文件可被 `--bounty` 直接投递）、`TestRunCmd_ClonesIntoIsolatedWorkspace`（断言未创建修改用户仓库）。
- `TestCaps_DeclaresBehaviourFields`（MS-1 基线里 `Caps` 只有两个字段的补齐）。

**依赖**：MS-2（事件）。**不做**：权限询问（已决：这条路径不存在，见 §9 第 5 条）、单步驱动（FR-1.9 形态 C）。

---

## 6. 每个里程碑共用的完成定义（DoD）

一个里程碑只有下列全部成立才算完成：

1. `python scripts/check.py --wsl --race` **exit 0**（build ＋ vet ＋ test ＋ race），目标平台 `linux/amd64`。
2. 新增用例**不依赖模型与网络**（NFR-8）；端到端用例走"真 git 夹具 ＋ 本地假上游"。
3. 外部契约变更**只追加**（INV-5）：事件类型与字段、结果文件字段、退出码映射——同批更新 `xhunter-usage.md` §5/§6/§7。
4. 文档同步：`xhunter-product-design.md`§6 的一期状态句、`xhunter-architecture.md`§12 对应 IA 的"待接入/待补"改成用例名、§14 该行移除。
5. **不放松任何不变量**（INV-1~INV-11）：新增能力只经既有落点——策略裁决、`Committer` 写盘、事件出口；新增代码不引入平台相关假设。
6. `go.mod` 仍无第三方 `require`（NFR-1）。

---

## 7. 待排期小项（不单列里程碑）

| 项目 | 说明 |
|---|---|
| ~~缓存用量与输入口径统一~~ **已落地** | 口径：`InputTokens` = 全部输入（含缓存读与缓存写）、`CachedInputTokens` = 其中从缓存读取的部分（子集）、`OutputTokens` = 全部生成（输出侧无缓存）；`llm.Usage` 加字段、`harness` 累加、三协议各自翻译（Messages 三项相加）、结果文件追加 `cached_input_tokens`。用例：`TestInfer_ReportsCachedInputTokens`（两协议）、`TestInfer_InputIncludesCacheReadAndCreation`（Messages）、`TestEndToEnd_LocalRunProducesDeliveryCommit`（全链）。剩：`CacheWriteInputTokens` 预留位、`usage` 事件载荷（属 MS-2） |
| **写权限由任务内容决定**（取向，待排期） | 2026-09-23 用户明确：**以后再通过任务内容来决定所有的写权限**——写权限从「目录黑名单（`.xhunter/**` 禁写）」演进为「**任务声明的可写面**」（最小授权）。三个要点：① 权限面来自 Bounty（任务内容），而不是引擎硬编码的目录清单；② 引擎自有材料（会话材料 `.xhunter/<session_id>/**`）仍需强制禁写——那是**恢复正确性**，不是权限偏好；③ 落地时要解决两件事：未声明可写面时「默认拒绝」的语义，以及写原语名单（`passPrimitives` / `writePrimitives`）的归属 |
| **假设外化（`assumption` / `unverified`）** | FR-6.3/6.4 要求"信息不足时采取的最保守默认逐条列出"。**机制尚未定**：是要求模型以结构化形式输出并由执行体采集，还是由执行体从最终文本提取？两种做法的可靠性不同，建议先定设计再排期（当前内核条款已要求模型"最后如实说明假设"，但没有采集路径）。 |
| **嵌套 AGENTS.md 附注** | FR-2.6 的 monorepo 部分（AC-23）。架构 §14.2 已给出补法：需要给 system 段的结果带上一份"约定清单（路径 ＋ 正文）"，而当前插件契约只交正文。等真有消费方时再加，眼下不必摆无人读的契约。 |
| **静态扫描类断言** | 凭据不落事件/日志/patch/材料（AC-8、IA-6.8）、核心不出现供应商名（INV-1、IA-8.4）——属 CI 级扫描，非单测能覆盖。 |
| **信号组合** | SIGTERM 的进程级断言已落地；SIGKILL 与"中断时机"的更多组合可补。 |
| **单步驱动（FR-1.9 形态 C）** | 明确不进一期。 |

---

## 8. 与现有文档的同步点

本文不替代任何治理文档，只提供排期视角。任一里程碑落地时，按下表回写：

| 落地点 | 回写内容 |
|---|---|
| `xhunter-product-design.md` §6 工具集规格 | 一期实现状态句（`find` 与三符号原语、`check` 的实际状态） |
| `xhunter-product-design.md` §8 分期计划 | 若分期口径与本文顺序不一致，以分期表为准调整本文 |
| `xhunter-usage.md` §5/§6/§7 | 事件清单的"当前实现已发出"标注、结果文件字段状态表、退出码映射新增行 |
| `xhunter-architecture.md` §12 | 各 IA 的"待接入/待补"→ 用例名 |
| `xhunter-architecture.md` §14 | 缺口行逐条移除（这是"还差什么"的唯一权威） |


---

## 9. 待澄清的决策（按阻塞程度排序）

**A. 必须先定**——不定就会写出自相矛盾的东西，或实现直接卡住。

| # | 问题 | 现状（代码 / 文档事实） | 选项 | 阻塞 |
|---|---|---|---|---|
| 1 | ~~`.xhunter/gates.yml` 能不能被模型改~~ **已决** | **2026-09-23 用户明确**：门禁清单**移出 `.xhunter/`、落到仓库根 `gates.yml`**（它是仓库级配置、别的工具也会读，不是引擎私有文件），并且**不限制修改**。已改：FR-5.2g 重写（位置 ＋ 理由改为「判据必须在运行前定死」）、FR-5.2b / FR-1.10 / 架构 §6.3 L1-4 / §7.8 / 使用手册 §1.1 与 §1.2 / README 的路径与措辞、`internal/policy` 的控制目录注释、policy/osfs 用例里把 gates.yml 换成材料路径；FR-5.2h 补「三条护栏是唯一防线」。**代码无需改动**——移出控制目录之后它本来就是可写的（策略只按目录判） | MS-5 |
| 2 | ~~检查点档位~~ **已决：只要 `on_structure`** | **2026-09-23 用户明确**：自动检查点只要 `on_structure`；**残次品对任务没有太大好处**。落地：删掉 `CheckpointMode` / `CheckpointPolicy` / `Bounty.Checkpoint`（**档位配置面整个取消**——只有一个取值就没有可配的东西，不留死字段）；`every_turn` / `interval:N` / `final_only` / `every_write` 四档全部取消；AC-20 / AC-22 / IA-11.11 / §6.3 L5⑥ 同步改写；使用手册 §3 写明「检查点是固定行为、不提供档位」 | MS-4 |
| 3 | ~~默认档在结构检查落地前用什么~~ **已决：判据不可判定 → 不提交** | **2026-09-23 用户明确**：残次品对任务没有太大好处 ⇒ 判据缺失时**不提交**（不引入第二个默认值、也不要「轮兜底」）。落地：`hunt/hooks.go` 新增 `structuralPoint()`（现恒 false＝不可判定）＋「未请求且不在结构点 → 不提交」的分支；FR-1.3c / FR-1.3d 改写（第三态的动作从「轮兜底提交」改成「不提交」）；新增用例 `TestCheckpoint_NoAutoCheckpointOffStructuralPoint`；e2e 的 tip 断言改为收尾交付提交 | MS-4 |
| 4 | ~~假设外化~~ **已决：做，并升级为「澄清回路」** | **2026-09-23 用户明确**：判据来自**任务描述与系统提示词**；缺内容时**输出需要补全的清单**，然后**用新 bounty 补充条件 ＋ 旧 session 重新执行**——**无人值守 ≠ 无人干预**（干预发生在投递层，不在运行期）。已改：FR-6.3 拆成 `needs` / `assumptions` 两类、§3 新增「澄清回路」、FR-12.1 补 resume 的第二场景、FR-1.10 标注生成器 = 回路入口、内核条款改为两个固定小节、架构 §6.5 加恢复矩阵一行、§7.2 修正「假设从记录生成」的矛盾、使用手册 §5/§6/§7 加 `needs_input` / `blocked`。**两处形状是我先定的，待你确认**：终态 `StatusBlocked` ＋ **退出码 0**（它不是失败：进程正常结束、清单已产出、改动已交付；**分类完全由 `status` 承载**——是否合入由平台与人决定，不在 Xhunter 契约内）、小节名固定 `## 需要补全` / `## 假设`。**「继续做完」还是「立即停下」不由引擎规定——由模型依任务描述与项目约定判断**（不同用户、不同任务的严谨度要求不同），引擎只负责执行：**读到小节即收敛**（写小节 = 停止信号） | MS-2（机制）＋ MS-7（续跑） |
| 5 | ~~权限询问~~ **已决：不做** | **2026-09-23 用户明确**：无人值守下不应该有权限询问——这不是「给它配默认值」，而是这条路径不存在。已改：架构 §10.3 整节改写（改为「权限裁决只在调用点」）、§7.4 裁决点表标「不适用」、§8 接口去掉 `Decide` 与 `Caps.PermissionCallback`、INV-4 / IA-4.4 / IA-8.2 重述、§5 单步驱动删掉「补权限询问事件」、§14 缺口行去掉该词；产品 FR-7.2 补「也不得请求权限」；使用手册 §5 事件顺序句去掉 `permission_request` |
| 6 | ~~影响面阈值~~ **已决：不做** | **2026-09-23 用户明确**：终极目标是无人开发、最终无人 review，**不应该有影响面限制**，也不把「任务白跑」当判据。已改：产品设计新增设计原则「判据优先于偏好」、FR-8.7 去掉阈值、§6 硬约束 #4 改为「规模只上报不裁决」、AC-16 重写、§8 M3 改为「改动规模上报」、FR-6.2 补改动规模、FR-2.8 标注「能力不是闸门」、FR-5.2f/FR-1.3b 与架构 §7.8 的「白跑」理由改为「判据是证据」；架构 §7.4 裁决点表标「不做」、§10.1 / §10.5 / IA-4.1 重述；`ext.Impact` 注释改为「事实，不是闸门」；`internal/policy` 里「影响面过大另行排期」的注释删除；MS-10 改为「规模上报」 |

**B. 已替你默认，请确认**——现在是我拍的，改起来便宜。**阻塞关系**：第 9 条 → MS-1；第 10 / 12 / 16 条 → MS-2；第 14 条 → MS-5；第 15 条 → MS-11；第 7 / 8 / 13 条不阻塞任何里程碑，但**第 8 条改得越晚越贵**（它是外部契约，平台侧脚本与文档都要跟）。

| # | 项 | 我现在的口径（备选） |
|---|---|---|
| 7 | ~~预算是否把缓存命中计入~~ **已决：计入** | **2026-09-23 按建议定**：`InputTokens` = 全部输入（含缓存读/写）——配了预算就是「用量」口径而非「计费」口径；缓存命中同样占窗口、同样计入速率额度，保守是对的。前提是预算是**可选、不配即不限**（FR-9.1），所以这一条只在配了 token 预算时才有意义 | — |
| 8 | ~~变量命名~~ **已决：方案 B（彻底分组）** | **2026-09-23 用户选 B**：按「事实 vs 策略」分组前缀——`XHUNTER_PROVIDER` → **`XHUNTER_PROTOCOL`**；`XHUNTER_MAX_CONTEXT_TOKENS` → **`XHUNTER_MODEL_CONTEXT_TOKENS`**；`XHUNTER_MAX_OUTPUT_TOKENS` → **`XHUNTER_MODEL_OUTPUT_TOKENS`**；预算三项 → **`XHUNTER_BUDGET_TURNS` / `XHUNTER_BUDGET_TOKENS` / `XHUNTER_BUDGET_WALL_CLOCK`**。代码常量同步（`EnvProtocol` / `EnvModelContext` / `EnvModelOutput` / `envBudget*`）；使用手册 §3 加了「前缀即分组」说明；**不做兼容别名**（别名会引入两种写法）。全仓约 40 处替换，`build`/`vet`/`test` 全绿 | — |
| 9 | ~~MS-1 的两个小账~~ **已决：两项都落地（2026-09-23）** | ① **`find` 已实现**（`hunt/basic/find.go`）：复用 `List` ＋ `Read`，命中带 **文件 ∶ 行号 ∶ 该行**；`path` 限定单文件、`scope` 限定目录；**枚举面与 glob 完全一致**（同一条面两种用法）；**「未找到」是结论而非错误**（成功结果 ＋ 说明范围与扫描量）；命中超 100 处只显示前 100 处但**总量照报**；单行限宽 160 字符；**跳过的大文件（>2MB）与读取失败一律如实附注**（不静默——否则「没找到」会被读成「不存在」）。用例 6 条（`TestFind_*`）。② **`edit` 的 `literal` 已进 `required`**，`TestEdit_DeclShape` 改成逐个断言 `path` / `literal` / `content`。文档同步：产品 §6 的一期状态改为「基础 5 个已实现」、删掉「已知不一致」那条（改为说明 find 与 glob 同一条枚举面）；架构新增 **IA-3.19**、IA-3.15 去掉 find。**MS-1 只剩 `check.py --wsl` 的修法** | MS-1 |
| 10 | ~~`usage` 事件粒度~~ **已决：增量 ＋ 终态累计** | **2026-09-23 按建议定**：`usage` 事件**每轮末发增量**（配对这一轮），**`hunt_end` 带累计**——平台读到终态即可记账，不必再去读结果文件。已写进使用手册 §5 与架构 §9.2 | MS-2 |
| 11 | ~~`assistant_text` 发不发~~ **已决：发** | **2026-09-23 用户明确**：任务内容可能就是要那份小结，因此最终答复是**交付物**，必须发（事件流 + 结果文件 `summary`）；体积与噪音的代价由平台自己控（可按需截断）。 |
| 12 | ~~心跳间隔~~ **已决：默认 30s ＋ 可配** | **2026-09-23 按建议定**：默认 30s，`XHUNTER_HEARTBEAT_INTERVAL`（Go duration）可覆盖——任务时长差很多，平台判断「卡死」的灵敏度得能调，把间隔写死等于替平台定了灵敏度。已写进 FR-10.1、使用手册 §3、架构 §7.5 | MS-2 |
| 13 | ~~`llm.Caps` 补齐还是改文档~~ **已决：改文档 ＋ 删死字段（按建议）** | **2026-09-23**：查清实质差别不是字段数，而是「按能力降级」的机制只落了一半，且 `Caps` 在运行期**没有消费方**。落地：① **删 `ParallelToolCalls`**（三个协议实现硬编码 `true`、无人读 → 死字段）、`llm.Caps` 只留 `MaxContextTokens`，注释写明「只声明用到的能力，行为类等有消费方再加」；② 架构 §8 的能力表改为「只声明用到的能力，不预埋降级逻辑」；③ **`UsageReporting` 不实现估算**，改为**如实上报**：新增 FR-9.7，上游不回用量时发 `degraded`（`scope: usage`）＋ 结果文件 `usage.reported: false`；④ 架构 §8 补「两个用量别混」（上报口径不估算 / 上下文占用必须估） | — |
| 14 | ~~门禁 `expect` 的细节~~ **已决（按建议）** | **2026-09-23**：`expect` 定为对象 `{kind*, pattern, max, stream}`——`kind` **必填**（`exit_zero` / `empty_output` / `regex` / `max_count`）；`pattern` 用 **RE2**（零依赖；不支持环视与反向引用，写清单的人要知道）；`regex` = **找到至少一处即通过**；`max_count` = 匹配次数 ≤ `max`；`stream` 缺省 `stdout`。**判据一律在全量输出上算**（截断不影响判定）。元门禁补第四条：**`expect` 必须可判定**（kind 已知／字段齐备／pattern 能编译），否则**启动期拒掉清单**。已写进 FR-5.2b / FR-5.2h / 架构 §7.8，使用手册 §1.1 加了指针 | MS-5 |
| 15 | ~~压缩的三个数~~ **已决：按建议的三个默认值** | **2026-09-23 按建议定**：① **L4（模型摘要）默认不启用**（确定性优先，FR-14.4）；② **冷却期默认 3 轮**（防震荡）；③ **材料体积上限默认 2 MB**，超限时**丢最早轮次的原文**，但**写操作记录与最终答复永不丢**，丢弃**如实上报 `degraded`**（FR-12.2c 新增） | MS-11 |
| 16 | ~~两个载荷形状~~ **已决** | **2026-09-23 按建议定**：`effective_config` = 装配清单（原语与顺序、两段插件与顺序、过滤器链）＋ 门禁清单（含来源与 `required`）＋ 策略/预算/检查点事实 ＋ 扩展能力指纹 ＋ 目标平台（FR-11.6）；`session_delta` = **最小事实集** `{turns_from, turns_to, ops_count}`（FR-1.5、使用手册 §6）——只要回答「接上了哪几轮、写了多少次」 | MS-2 / MS-6 |

**C. 工程流程类**——不影响产品，但影响文档与日常。

| # | 项 | 说明 |
|---|---|---|
| 17 | `docs/` 是否放第四份 | 本文是执行排期、非治理文档；要保持"只有三份"就把它移到仓库根或 `.workbuddy/` |
| 18 | 里程碑编号与产品分期是否统一 | 本文用 MS-n，产品设计用 M1 / M1.5 / M2 / M3；两套编号并存容易指错 |
| 19 | `check.py --wsl` 的修法 | 映射盘工作区下跑不通（见 §1.4）：修法有二——脚本内建路径映射，或让 WSL 侧自己算路径。属本机约定，不进产品文档 |
