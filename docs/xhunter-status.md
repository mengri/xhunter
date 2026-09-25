# Xhunter 状态（实现现状与排期）

## 0. 文档定位与维护规则

- 本文是 Xhunter **实现现状与排期**的唯一权威（**状态 SSOT**）。它只回答「**当前代码做了什么、没做什么、接下来按什么顺序补**」。
- **本文以「实现现状与排期」为定位，不含治理规则正文**——规则以《产品设计》《架构设计》为准；里程碑与分期的「范围」仅为规划摘要，其约束以对应的 FR / IA 编号为准。与设计文档冲突时，规则以治理文档为准，实现现状以本文为准。
- **每条状态都挂既有编号**（`FR-*` / `AC-*` / `IA-*` / `L-x` / `H-x`），**不引入新编号体系**。
- **核验口径**：下文所有「已落地」均以 **as-of `2026-09-23`** 的代码为准。核验命令（在 `linux/amd64` 上执行）：

  ```sh
  export PATH=/home/kingsword/.gvm/gos/go1.26.5/bin:$PATH
  export GOCACHE=/tmp/xhunter-gocache
  cd /work/agents/xhunter && go build ./... && go vet ./... && go test ./... -count=1
  ```

- **维护规则**：
  1. **里程碑落地只改本文档**——更新 §2 索引对应行（状态 / 证据用例名 / 缺口说明）、§3 派生视图、§4 里程碑状态、§5 待排期小项。
  2. **设计文档仅在「规则」变化时才动**——规则的增删改仍须回写三份治理文档；「是否已实现」不再写进设计文档。
  3. 设计文档中被抽走事实的地方保留指针，格式统一为：`（实现状态见 xhunter-status.md 状态索引 · <编号>）`，深链为 `xhunter-status.md#<锚>`（锚 = 编号小写、点换连字符）。

---

## 1. 现状基线

### 1.1 已落地（有实现、有用例）

| 能力 | 落点 | 证据 |
|---|---|---|
| 中立模型契约 | `llm/`（Message/ToolDecl/Event/Fault/Provider/Session/Caps） | `llm/schema_test.go` |
| 模型调用循环 | `harness/`（双文件：`engine.go` + `types.go`，三组 handler） | `harness/engine_test.go`（IA-1.1~1.12） |
| 工作区读写抽象与实现 | `workspace/` + `internal/workspace/osfs`（路径边界、软链逃逸、模式语义） | `osfs_test.go` |
| git 四动作 | `internal/git/cli`：`PrepareBaseline`（按 SHA 浅取→完整 fetch→建分支→推送）、`Commit`（ff 推送、无改动不空提交）、`Diff`、`Patch`、`Clean` | `git_test.go`（IA-11.1~11.7） |
| 基础原语 5/5 | `hunt/basic`：`read`（行范围＋截断续读提示）、`write`（仅新建）、`edit`（内容寻址唯一性校验）、`find`（内容检索）、`glob` | `read_test.go` / `write_test.go` / `edit_test.go` / `find_test.go` / `glob_test.go` |
| 执行流水线 | `hunt/session.go`：Bind → 查表 → `Policy.Decide` → `Execute` → `Committer` 落盘 → 结果回灌＋事件 | `hunt/session_test.go` |
| 写盘唯一入口与读后校验 | `hunt/commit.go`（指纹校验、只改目标区间、批量先全验证后落盘） | `hunt/commit_test.go`（IA-3.3/3.6） |
| 策略引擎 | `internal/policy`：默认拒绝、路径边界（**只对写操作**；`.xhunter/**` 禁写、`skills.draft/**` 放行）、三重预算（token/轮数/墙钟）；**是否写盘由原语自述**（`Writes()`），策略不维护原语分类表；**两段式止损**（连续同类失败：达 2 次换策略、超上限 3 次终止；连续拒绝：达 3 次终止），阈值由策略自述进 `config_snapshot` | `policy_test.go`（IA-4.5~4.9） |
| 提示词组装 | `prompt/agentsmd`（根级注入）· `prompt/skills`（清单注入）· `prompt/task`；内核条款＋环境事实由内核拼在固定位置 | `hunt/session_test.go`、`cmd/xhunter/prompt_test.go`（IA-2.1/2.10/2.11/2.13） |
| Provider 三协议 | `provider/openaichat` · `openairesponses` · `anthropicmessages` + `provider/adapter` + `providerconfig`（环境变量契约） | 各包 `*_test.go`（IA-8.x/9.x） |
| CLI 与组装层 | `cmd/xhunter`：`--bounty/--log-file/--result/--patch`、`version`；环境变量投递（仓库事实 ＋ 模型接入）、退出码 0/1/2/3、信号接线 | `bounty_test.go`、`channel_test.go`、`provider_test.go`、`signal_test.go`、`e2e_test.go` |
| 端到端夹具 | 真 git（本地裸仓库）＋ 真 SSE 假上游，不联网 | `TestEndToEnd_LocalRunProducesDeliveryCommit` 等 5 条 |
| 事件出口 | 信封四字段统一盖章；已发 `hunt_start` · `config_snapshot` · `tool_call` · `tool_result` · `assistant_text` · `usage` · `heartbeat` · `error` · `policy_denied` · `degraded` · `deliverable` · `hunt_end` | `hunt/session.go`（工具调用/结果/策略拒绝）、`hunt/hooks.go`（起飞/正文/用量/降级/终态） |
| 结果文件 | `--result` 无论成败都写；含终态/退出码/仓库事实/提交/改动清单/用量/error | `cmd/xhunter/result.go`（IA-12.9） |
| 模型声明终态（`needs` / `assumptions`） | `hunt/declare.go`（解析固定小节）＋ `Session.OnTurn`（轮边界登记）＋ `Session.Finalize`（收尾收敛为 `blocked`）；事件 `needs_input` / `assumption`；结果文件 `needs` / `assumptions` | `hunt/declare_test.go`（IA-1.13）、`cmd/xhunter/result_test.go` |
| 会话恢复（resume） | `hunt/hooks.go`（`Prepare` 纯读回灌 ＋ 新条件追加）＋ `cmd/xhunter/material.go`（`Load`）＋ `hunt/session.go`（`SessionDelta`） | `TestEndToEnd_ResumeContinuesFromLastCheckpoint`、`TestLoad_ReadsTurnsOpsAndUsageInOrder`、`TestPrepare_ResumeSeedsContextWithoutExecutingTools` |
| 上下文压缩 | 压缩落在 `cmd/xhunter/context.go` 的 `Assemble` 内（三档水位 ＋ 冷却、L0→L3 按序下压、当前轮永不压）；水位由装配层用 `providerconfig.Resolved.Watermarks` 从可用输入预算算出；结论经 `hunt.Compaction` ＋ 可选上报面 `CompactionReporter` 搬到事件出口 | `TestCompaction_*`（`cmd/xhunter` 15 条）、`TestCompaction_*`／`TestDeliverables_DoNotDependOnContext`（`hunt` 4 条）、`TestEndToEnd_OverWindowCompactsAndStillConverges` |
| 符号后端（内置 ／ 外挂） | 同一份 `ext.ExtHost` 的两种实现：内置语法级 `ext/syntax`（进程内，不需要额外安装任何东西）＋ 外挂 `ext/mcp`（MCP over stdio，懒启动／崩溃与超时隔离／随 Hunt 回收／不继承环境）；由部署事实 `XHUNTER_EXT_COMMAND` 选，宿主与能力指纹**同源** | `TestHost_*`（`ext/mcp` 11 条）、`TestChooseExt_*`、`TestParseExtConfig_*`、`TestAssemblyFacts_ReportsTheAssembledBackend`、`TestFinalize_ClosesTheExtHost`、`TestEndToEnd_ExternalBackendUnavailableStillConverges` |

### 1.2 声明齐备、调用返回 `not_implemented`

| 原语 | 落点 | 行为 |
|---|---|---|
| `symbol_read` / `symbol_edit` / `symbol_rename` | `hunt/symbolic/symbolic.go` | **已随 MS-8 注册**：能力不可用 / 语言未注册时返回结构化错误（`ext_unavailable` / `language_unregistered`），工具名不撤回、不降级为文本替换 |
| `check` | `hunt/gate/gate.go` | **已随 MS-5 实现**：跑具名门禁并按清单判据判定；执行器缺失是**可重试**的装配缺件（`gate_unavailable`） |

> 工具面已定格为 **9 + 1**（`cmd/xhunter/primitives.go` 九个原语 ＋ `Session.decls()` 殿后追加 `checkpoint`），与 FR-4.11 一致——**环境能力不决定注册、同一构建内不增减；随能力接入而扩展**：三个符号原语随 MS-8 接回清单（内置语法级后端已接入），`check` 随 MS-5 接上执行器。这条纪律由 `TestDefaultTools_FaceIsFixed`、`TestDefaultTools_FaceIsIdenticalWhetherTheBackendIsAvailable`、`TestDefaultTools_SymbolPrimitivesAreRegistered` 守住。
>
> 注：`find` 原列于本表，**已于 2026-09-23 落地**（见 §2.3），现为「已落地」。
>
> 本表已无「声明齐备、调用返回 `not_implemented`」的原语（符号三原语随 MS-8、`check` 随 MS-5 落地）；表头与位置保留以兼容既有引用。

### 1.3 规格在、代码缺（缺口清单）

| 缺口 | 现状证据 |
|---|---|
| 恢复的轮数 / 预算口径 | `Load` 读回的轮数按**记录条数**算、不取 `turn.no` 最大值（恢复后本趟轮号从 1 **重新起计**，材料里会出现两段都从 1 开始的 `turn` 记录）；**轮数预算不跨恢复续算**（本次轮号重新起计，无累计口径）；token 预算已续算（`Prepare` 补喂 `Restored.Usage` 的输入/输出）。见 §4.3 MS-7 |
| 上下文压缩 | 已落地（MS-11）：三档水位 ＋ 冷却、分层下压 L0→L3、`context_compacted` 事件、投影式工作日志（零模型调用）。**余**：L4 模型摘要（FR-14.5，默认不启用；且「生成一次即落盘、恢复时读回」要动材料与恢复两条链路） |
| 门禁全链 | 已落地：`Prepare` 做来源裁决（下发 > 基线 `gates.yml` > 无），`check` 跑具名条目、按清单判据判定；收尾补跑 + 终态裁决 + 检查点抑制均已接线（`hunt/hooks.go`、`hunt/gates.go`） |
| 符号能力与扩展宿主 | 同一份 `ext.ExtHost` 的两种实现：`ext/syntax`（内置语法级，进程内，不需要额外安装任何东西）与 `ext/mcp`（外挂，MCP over stdio 子进程）；`hunt/symbolic` 三原语已注册。由部署事实 `XHUNTER_EXT_COMMAND` 选（MS-8） |
| 结构检查（自动检查点的判据） | 已落地：`Session.judgeStructural` 接真判据（① `Parse` 语法完整 ＋ ② `Enclose` 区间封闭，三态收敛）；判不了 → 不提交 ＋ 如实上报（`degraded`，`scope: checkpoint`，每次运行最多一条） |
| **用量口径（输出明细与缓存写）** | 输入侧已按统一口径落地（见 §2.3）：全部输入含缓存读/写、缓存读为子集。仍未取的：OpenAI 的输出明细（`completion_tokens_details` / `output_tokens_details`：reasoning / audio / accepted·rejected prediction），以及把「缓存写」单列出来（Anthropic 的写溢价 1.25×/2× 目前按普通输入价计） |
| 差异排除材料目录 | `internal/git/cli/git.go:173` — `Diff`/`Patch` 无 `.xhunter/<session_id>/**` 排除 |

### 1.4 口径不一致

| 不一致 | 事实 | 影响 |
|---|---|---|
| `edit.literal` 的必填性 | 实现里必填（缺它返回 `bad_selector`），schema 的 `required` 只有 `path`/`content`（`hunt/basic/edit.go:31`） | 模型照 schema 省略 → 本可避免的错误（**已于 2026-09-23 修正**，见 §2.3） |
| ~~文档列了 6 类事件，部分仍未发出~~ | **已全部发出**（2026-09-23，含 `heartbeat`）：心跳按间隔输出、`hunt_end` 为最后一条 | 已消除 |
| `check.py --wsl` 在映射盘工作区不可用 | 脚本由 `__file__` 推导工作目录，映射盘（UNC）路径在 `--wsl` 分支里被原样拼进 bash 命令，`cd` 失败（实测 `exit 1: cd: \localhostworkagentsxhunter`） | 规范验证入口曾在这台机器上跑不起来，基准只能**直接调用 WSL 内 go** 得到（**已于 2026-09-23 修正**，见 §2.3） |
| **IA 编号重复** | `IA-2.11` 出现两次（内核条款 / skill 清单）；`IA-8.15` / `IA-8.16` / `IA-8.17` 各出现两次（协议包通用项 / 单协议形状项） | 引用会指错；**已用限定词消歧、不重编号**（重编号要动架构 §12 与本文档几十处引用，收益只是好看）：架构 §12 与本文档的编号现**逐字一致**，写作 `IA-2.11(a) 内核条款与环境事实` / `IA-2.11(b) skill 发现清单` / `IA-8.15(a) 协议版本基线` / `IA-8.15(b) Messages 协议形状` 等 |

---

## 2. 状态索引（按既有编号；唯一事实清单）

> 本节是「还差什么、凭什么说已落地」的**唯一事实清单**。设计文档与本文档之间、以及原架构 §14 与 §12 状态列之间，都以本节的**编号为键**统一；不再维护第二张并行缺口表。

### 2.1 需求与验收（FR-* / AC-*）

> 状态取值：**已落地**（有实现且有用例）/ **待接入**（实现缺失）/ **待补**（实现或契约在、用例或接线缺）。未列出的 FR/AC 视为已落地（证据见 §1.1）。

| 编号 | 状态 | 对应 MS-n | 说明 |
|---|---|---|---|
| <a id="fr-1-3b"></a>FR-1.3b（提交连败上限） | 已落地 | MS-4 | 检查点**连续**提交失败达 3 次（包内常量，不留可配字段）→ 本轮结束即收敛 `checkpoint_failed_streak`（退出 1）；成功即归零。用例 `TestCheckpoint_StreakLimitConvergesAsEnvError`、`TestCheckpoint_SuccessResetsStreak` |
| <a id="fr-1-3d"></a>FR-1.3d（结构检查） | 已落地 | MS-4 / MS-9 | 判据已接真：① **此刻**目标文件语法完整（`ext.ParseVerdict`）；② 改动区间封闭在某个符号内（`ext.Enclose`，在**写盘前**就地判、与 `s.ops` 平行记下）。三态收敛：通过 → 提交；判过未通过 → 不提交（说「未落在结构完整点」）；判不了 → 不提交 ＋ 一条 `degraded`。取样范围是**尚未提交的改动**，整文件写入不适用判据②。用例 `TestStructural_ParseOKFlipCommits`、`TestStructural_IncompleteSyntaxSuppresses`、`TestStructural_UndecidableDoesNotCommit`、`TestStructural_EditOutsideAnySymbolSuppresses`、`TestStructural_CommittedOpsLeaveTheJudgementWindow`、`TestEndToEnd_CheckpointLandsOnCompleteSyntax`、`TestEndToEnd_IncompleteSyntaxSuppressesCheckpoint` |
| <a id="fr-1-10"></a>FR-1.10（Bounty 生成器 / 本地驱动） | 已落地 | MS-12 | `xhunter run --repo <path> --task <text> [--out <path>]`：**只读**探测本地仓库事实（远端 / 基线 HEAD / 任务分支 / 门禁候选）→ 生成 Bounty（`--out` 可选出清单）→ 用**同一份装配**执行一次 Hunt；工作区由 Xhunter clone 到临时目录，用户当前仓库只被只读探测、不受影响。用例 `TestRunCmd_ClonesIntoIsolatedWorkspace`、`TestRunCmd_ProbeIsReadOnly`、`TestRunCmd_MissingRepoFlag`、`TestRunCmd_RepoPathMissing`、`TestRunCmd_NotAGitRepo`、`TestRunCmd_NoRemote`、`TestRunCmd_EmptyRepo`、`TestEndToEnd_LocalRunDrivesBountyFromRepo` |
| <a id="fr-1-11"></a>FR-1.11①/②（流看门狗 / 提交连败提前收敛） | 已落地 | MS-12（①）/ MS-4（②） | ①接收段不活动超时：`harness.Config.StreamIdleTimeout`（**0 = 取默认 120s；正数 = 不活动上界；负数 = 关闭看门狗**——关闭只在程序内可达、不暴露给部署侧）；相邻两事件之间（含首事件之前）静默超上界即**先落终态、再 `Cancel()`** → 环境错误（退出 1），不进入轮边界。用例 `TestWatchdog_IdleStreamCancelsAsEnvError`、`TestWatchdog_TimeoutCannotBeNoToolCall`、`TestWatchdog_ActivityResetsTimer`、`TestWatchdog_WaitsForFirstEvent`、`TestWatchdog_DisabledWhenNegative`、`TestWatchdog_CancelOutranksTimeout`、`TestWatchdog_TimeoutStopsBeforeOnTurn`、`TestEndToEnd_IdleStreamConvergesToEnvError`、`TestConfigSnapshot_StreamIdleTimeoutFollowsEnv`（部署事实 → 装配 → `config_snapshot` 真值链）；②见 FR-1.3b（已落地） |
| <a id="fr-2-2"></a>FR-2.2（内容检索 `find`） | 已落地 | MS-1 | 2026-09-23 落地（见 §2.3） |
| <a id="fr-4-1"></a>FR-4.1/4.2（符号枚举读取 / 整体替换） | 已落地 | MS-8 | `symbol_read` / `symbol_edit`：定位到字节区间后复用基本读写，**精度与「未穷尽」如实上报**（FR-4.13）。用例 `TestSymbolRead_LocatesAndReadsRange`、`TestSymbolRead_NotFoundIsAVerdictNotAnEnvFault`、`TestSymbolEdit_ReplacesOnlyTargetRange`、`TestSymbolic_ReportsSyntacticPrecision`、`TestEndToEnd_SymbolEditLandsInTheDelivery`、`TestEndToEnd_SymbolReadReturnsJustTheDefinition` |
| <a id="fr-4-3"></a>FR-4.3（符号内插入 / 跨文件重命名） | 已落地 | MS-10 | `symbol_rename`：一次改完**声明 + 全部出现点**（出现点来自同一次定位，与规模上报同源）；后端列不出出现点时返回 `cannot_resolve` **结构化错误**，绝不降级为文本替换。用例 `TestSymbolRename_UpdatesDeclarationAndAllReferences`、`TestSymbolRename_CanResolveFalseIsStructuredErrorNotTextReplace`、`TestSymbolRename_ReportsFootprint`、`TestSymbolRename_UnknownFootprintIsReportedNotGuessed` |
| <a id="fr-5-2b"></a>FR-5.2b~5.2i（门禁全链） | 已落地 | MS-5 | 清单来源裁决（`hunt.GateSource`，下发 > 基线 `gates.yml` > 无，`working_tree` 豁免档受三条护栏约束）；`check` 数组直启（`argv[0]` 为 shell 拒）+ 超时杀进程组 + 输出限长 + 环境白名单；判据 `{kind,pattern,max,stream}` 三态判定；结果按改动指纹缓存；收尾补跑未跑过的 `required` 门禁；`check_result` / `gate_config_changed` 事件；结果文件 `gates` 列出未运行项（`passed: null`）；门禁名注入 user 段。用例 `TestCheck_ArgvIsDirectAndRejectsShell`、`TestCheck_TimeoutIsEnvError`、`TestCheck_NonZeroExitIsQualityFailure`、`TestCheck_UnstartableCommandIsEnvError`、`TestCheck_CacheHitByFingerprint`、`TestCheck_EnvIsMinimalAndCredentialFree`、`TestCheck_ExpectVariantsDecideTheVerdict`、`TestCheck_PassedGateRequestsCheckpoint`、`TestCheck_FailedGateDoesNotRequestCheckpoint`、`TestResolveGates_WorkingTreeExemptionRequiresMetaGate`、`TestResolveGates_WorkingTreeCannotLowerStrength`、`TestResolveGates_WorkingTreeEmitsGateConfigChanged`、`TestEndToEnd_RequiredGateNeverRunFailsWithDelivery`、`TestEndToEnd_GateFailureFailsButStillDelivers`、`TestEndToEnd_RequiredGatePassedSucceeds` |
| <a id="fr-6-3"></a>FR-6.3/6.4（`needs` / `assumptions` / `unverified`） | 已落地 | MS-2 | 三类自陈均由正文固定小节采集（`## 需要补全` / `## 假设` / `## 未验证`）：轮边界登记（`AppendDeclared`）、收尾可读；`needs` 非空收敛 `blocked`（退出 0），`unverified` 只陈述、不改终态。结果文件 `needs` / `assumptions` / `unverified` 三态。用例 `TestParseDeclared_ThreeSectionsAreSeparated`、`TestParseDeclared_UnverifiedAloneIsNotNeeds`、`TestParseDeclared_UnverifiedThreeState`、`TestFinalize_UnverifiedDoesNotBlock`、`TestResultFile_UnverifiedIsThreeState` |
| <a id="fr-9-4"></a>FR-9.4（两段式止损） | 已落地 | MS-3 | 连续同类失败达 2 次换策略（回灌「换一种做法」提示）、超上限 3 次终止（`stop_loss_same_kind`）；连续拒绝达 3 次终止（`stop_loss_denied`）；阈值由策略自述（`policy.Facts()`）进 `config_snapshot`，机制硬顶由装配层注入。用例 `TestObserveFailure_SwitchThenTerminate`、`TestSameKindLimitsAreTwoAndThree`、`TestDeniedStreakLimitIsThree`、`TestOnTurn_SameKindStopLossOutranksBudget`、`TestOnTurn_CommitStreakOutranksStopLoss`、`TestEndToEnd_RepeatedFailureSwitchesBeforeFailing`、`TestEndToEnd_DeniedStreakFailsTheRun` |
| <a id="fr-9-7"></a>FR-9.7（用量不可得时不得估算） | 已落地 | MS-2 | 上游未回报用量时不报数字：`usage` 事件三字段全零则不发、结果文件 `usage.reported: false` ＋ 一条 `degraded`（`scope: usage`）；预算仍按上报值计（不拿 0 触发/不触发）。用例 `TestOnTurn_NoUsageEventWhenUpstreamSilent`、`TestFinalize_DegradedOnceWhenUsageUnavailable`、`TestResultFile_UsageReportedFalseWhenUpstreamSilent` |
| <a id="fr-10-1"></a>FR-10.1~10.3（心跳 / 终态事件） | 已落地 | MS-2 | 任务级心跳（间隔可配、随取消即停、带 `phase`/ `elapsed_ms`）与全部轮级/终态事件已发，`hunt_end` 为最后一条。用例 `TestHeartbeat_*`、`TestSession_PhaseTracksStages`、`TestFinalize_StopsHeartbeatBeforeHuntEnd`、`TestParseHeartbeatInterval_DefaultOverrideAndRejects`、`TestEndToEnd_HeartbeatEmittedAndHuntEndLast` |
| <a id="fr-10-4"></a>FR-10.4（事件/心跳写失败即终止） | 已落地 | MS-2 | 出口自述健康状态（`EventSink.Failed()`，覆盖事件与心跳两条写路径）；`OnTurn` 轮前/轮末复查 → `event_channel_failed`（退出 1），取消优先（不被通道问题改写）；进程末尾 `sink.Failed()` 收口保留为兜底。用例 `TestOnTurn_StopsBeforeWorkWhenChannelAlreadyFailed`、`TestOnTurn_StopsAfterTurnWhenChannelFailsDuringTurn`、`TestOnTurn_ChannelFailureDoesNotOverrideCancellation`、`TestEventSink_FailedCoversHeartbeatWrites`、`TestEndToEnd_BrokenEventChannelIsEnvError` |
| <a id="fr-11-1"></a>FR-11.1（每次模型/工具调用产出结构化事件，含耗时与用量） | 已落地 | MS-2 / MS-3 / MS-5 / MS-11 | 模型侧 `assistant_text` / `usage`（每轮增量）；工具侧 `tool_call`（`args`）/ `tool_result`（含 `duration_ms`）；终态 `hunt_end` 带累计用量；`check_result` / `gate_config_changed`（MS-5）、`config_snapshot`（MS-3）、`context_compacted`（MS-11）均已接线。**契约里的 16 种事件全部有发出点**——这条不靠记忆：`docs-refcheck.py` 每次体检都比对「SSOT 定义」与「代码发出」两侧，任一侧多出或少了一个都会被报出来 |
| <a id="fr-11-2"></a>FR-11.2（错误结构化） | 已落地 | MS-2 | 终态 `failed` 发一条 `error`（`kind`＝原因首段 / `retryable`＝与退出码同源 / `context`＝阶段与轮次）；取消与 `blocked` 不发。`kind` / `retryable` 的口径上收为 `hunt.ErrorKind` / `hunt.RetryableForExitCode`，事件与结果文件同源。用例 `TestFinalize_EmitsErrorOnFailureMatchingExitCode`、`TestFinalize_NoErrorEventOnBlockedOrCancelled` |
| <a id="fr-11-6"></a>FR-11.6（生效配置快照） | 已落地 | MS-2 | 装配完成后冻结一次，进 `hunt_start` 与结果文件 `effective_config`；门禁清单已随 MS-5 接入（清单来源档位进每条门禁的 `source`）。用例 `TestEffectiveConfig_ListsPrimitivesInToolFaceOrder`、`TestEffectiveConfig_CarriesPluginAndFilterNames`、`TestEffectiveConfig_CarriesAssemblyFacts`、`TestPrepare_EmitsHuntStart`、`TestResultFile_EffectiveConfigIsWritten` |
| <a id="fr-12-1"></a>FR-12.1（会话恢复 resume） | 已落地 | MS-7 | `Prepare` 读回材料（**纯读、先于 `Open`**）→ 回灌上下文 → 从下一轮继续；全程**零工具执行、零模型调用、不重放写操作**（工作区已由分支 tip 给出）。用例 `TestEndToEnd_ResumeContinuesFromLastCheckpoint`、`TestPrepare_ResumeSeedsContextWithoutExecutingTools`、`TestPrepare_ResumeAppendsNewConditionsAfterRestoredHistory`、`TestPrepare_FreshSessionWithoutMaterialIsNotResumed` |
| <a id="fr-12-2"></a>FR-12.2/12.2b/12.2c/12.3（会话材料） | 已落地 | MS-6 / MS-7 | 材料已落盘（`.xhunter/<session_id>/session.jsonl`，meta 带 `schema_version`、按任务隔离、追加写）；**读回**随 MS-7 落地（`Load` 纯读、先于 `Open`；turn/op/usage 三类按行序解析、usage 按记录累加）。用例 `TestLoad_ReadsTurnsOpsAndUsageInOrder`、`TestLoad_MissingMaterialIsZeroValueNotAnError`、`TestLoad_UnknownRecordTypeIsAnError`、`TestSave_MaterialLandsUnderSessionDirWithSchemaVersion` |
| <a id="fr-13-1"></a>FR-13.1~13.10（扩展接入） | 已落地 | MS-8 | 同一份 `ext.ExtHost` 契约的**两种实现**：① 内置语法级后端（`ext/syntax`，进程内，不需要额外安装任何东西、首发 Go）；② **外挂通道**（`ext/mcp`，MCP over stdio 子进程）——传输与握手是 MCP 标准、符号能力方法走自有命名空间 `xhunter/*`，**懒启动**（首次调用才拉起）、**崩溃与超时即隔离**（杀进程组、原因钉成终态，绝不让主循环挂住）、**随 Hunt 回收**（`Finalize` 末尾 `Close`）、**不继承环境**（只拿 `XHUNTER_EXT_ENV` 点名授予的那几项）。选哪个后端是部署事实（`XHUNTER_EXT_COMMAND` 非空即用外挂，否则内置，FR-13.9），写错即启动期退出 1；宿主与能力指纹**同源**（同一份选择给出两端），换后端一定体现在 `effective_config.ext`。用例 `TestHost_*`（`ext/mcp` 11 条）、`TestChooseExt_*`、`TestParseExtConfig_*`、`TestAssemblyFacts_ReportsTheAssembledBackend`、`TestFinalize_ClosesTheExtHost`、`TestFinalize_WithoutExtHostDoesNotPanic`、`TestEndToEnd_ExternalBackendUnavailableStillConverges`。FR-13.7 的另一半（能力指纹进会话材料 `meta.ext`）**已随 `IA-6.5` 落地**：与 `effective_config.ext` 同源，续跑时指纹变了**只记录、不阻断** |
| <a id="fr-14-1"></a>FR-14.1~14.8（上下文压缩） | 部分已落地 | MS-11 | 14.1/14.2/14.3/14.4/14.6/14.7/14.8 已落地：三档水位（预警 70% / 目标 50% / 硬上限 90%，基数＝**可用输入预算**，由 `providerconfig.Resolved.Watermarks` 算出）＋ 冷却 3 轮；`Assemble` 内按 **L0→L3** 分层下压、够用即停；**当前轮永不压**；永不丢清单（Bounty 正文／当前轮／写操作记录）；工作日志由会话记录**投影**（零模型调用）；`context_compacted` 事件带 `level`/`released_tokens`/`watermark`；交付物不依赖上下文；恢复回灌走同一管线。**压完仍不低于硬上限才终止**——按预算耗尽处理（`budget_exhausted:context`，退出 2、不重派，usage§7「上下文达硬上限」）：撞硬上限本身不判错，照样一路压到目标、只把 `watermark` 记 `hard`。用例 `TestWatermark_TriggersAtWarnAndCoolsDown`、`TestCompaction_LayersDownToFit`、`TestCompaction_PressesThroughAllLayersWhenTargetIsUnreachable`、`TestCompaction_ReleasedTokensIsAMeasuredDelta`、`TestCompaction_ProjectionIsReproducible`、`TestCompaction_NeverDropsBountyOrWriteOps`、`TestCompaction_HardWatermarkIsReported`、`TestCompaction_WarnWatermarkBelowHard`、`TestCompaction_L0KeepsBothEndsAndMarksTheDrop`、`TestCompaction_L2KeepsAddressability`、`TestCompaction_FoldPreservesDeclaredSections`、`TestCompaction_NoWatermarksMeansNoCompaction`、`TestCompaction_CurrentTurnIsNeverCompacted`、`TestCompaction_NothingToCompactDoesNotArmCooldown`、`TestCompaction_HardLimitIsJudgedAfterPressing`、`TestCompaction_ResumeUsesTheSamePipeline`、`TestEndToEnd_OverWindowCompactsAndStillConverges`、`TestEndToEnd_HardWatermarkStopsTheRun`（`cmd/xhunter`）、`TestCompaction_EventCarriesLevelAndReleasedTokens`、`TestCompaction_NoReporterMeansNoEvent`、`TestCompaction_EmittedOnTurnBoundary`、`TestCompaction_OverHardLimitStopsTheRun`、`TestDeliverables_DoNotDependOnContext`（`hunt`）。**FR-14.5（L4 模型摘要）待补**：它默认不启用（FR-14.4 确定性优先），且「生成一次即落盘、恢复时读回」要动材料与恢复两条链路，缺了它就不许声称做了 L4 |
| <a id="fr-15-2"></a>FR-15.2/15.5/15.6（skill 清单注入与解析容错） | 已落地 | — | `prompt/skills`；正文按需读取（FR-15.3） |
| <a id="ac-6"></a>AC-6（SIGTERM 取消） | 已落地 | — | 进程级断言 `TestEndToEnd_SigtermConvergesToCancelled`；其余信号形态待补（见 §5） |
| <a id="ac-7"></a>AC-7（崩溃重派 resume） | 已落地 | MS-7 | 投递同一 `XHUNTER_SESSION_ID` → checkout 分支 tip（最后检查点）＋ 读回材料 ＋ 从下一轮继续（不重做已完成轮次）；恢复失败（版本不兼容 / 坏材料）退出 1。用例 `TestEndToEnd_ResumeContinuesFromLastCheckpoint`、`TestEndToEnd_ResumeCorruptedMaterialIsEnvError`、`TestEndToEnd_ResumeAfterClarificationAppliesNewConditions`、`TestEndToEnd_FreshSessionWithoutMaterialIsNotAnError` |
| <a id="ac-8"></a>AC-8（凭据不落事件/日志/patch/材料） | 待补 | §5 | 需 CI 级静态扫描，非单测能覆盖 |
| <a id="ac-9"></a>AC-9（stdout 全为合法事件行） | 已落地 | MS-2 | 逐行合法 JSON ＋ 信封四字段（`ts` 为 RFC3339），无杂质。用例 `TestEventSink_StdoutIsPureNDJSON`、`TestHuntCmd_EventsGoToStdoutAndLogsGoToStderr` |
| <a id="ac-17"></a>AC-17（压缩保真） | 已落地 | MS-11 | 永不丢（Bounty 正文与验收标准、当前轮 messages、写操作记录）由 `TestCompaction_NeverDropsBountyOrWriteOps` 钉住；结果文件不因压缩缺失任何已发生的改动由 `TestDeliverables_DoNotDependOnContext` 钉住（FR-14.6） |
| <a id="ac-18"></a>AC-18（压缩可观测可复现） | 已落地 | MS-11 | `context_compacted` 事件带 `level`/`released_tokens`/`watermark`（`TestCompaction_EventCarriesLevelAndReleasedTokens`、`TestCompaction_HardWatermarkIsReported`、`TestCompaction_WarnWatermarkBelowHard`）；同一份记录两次投影逐字一致（`TestCompaction_ProjectionIsReproducible`） |
| <a id="ac-20"></a>AC-20（交付形态 / fast-forward） | 已落地 | — | `TestEndToEnd_LocalRunProducesDeliveryCommit`；`find`/edit 相关注脚见 §2.3 |
| <a id="ac-22"></a>AC-22（检查点自愈与止损） | 已落地 | MS-4 | 自愈（单次失败不中止、下一轮累积重提）＋ 连败上限（连续 3 次 → 退出 1）。用例 `TestCheckpoint_SingleFailureSelfHeals`、`TestCheckpoint_StreakLimitConvergesAsEnvError` |
| <a id="ac-26"></a>AC-26（工具面恒定） | 已落地 | — | 规则已落地并有用例（`TestDefaultTools_FaceIsFixed`、`TestDefaultTools_FaceIsIdenticalWhetherTheBackendIsAvailable` 等）；**环境能力不决定注册、同一构建内工具名与 schema 不增减**；符号三原语**已随 MS-8 / MS-10 注册**，`check` **已随 MS-5 实现**（不再返回 `not_implemented`） |
| <a id="product-6"></a>产品§6（工具集规格的实现状态） | 9 个原语全部已实现并注册 | MS-1 / MS-5 / MS-8 / MS-10 | 工具面 = **9 + 1**：基础 5（`read`/`write`/`edit`/`find`/`glob`，MS-1）＋ 符号 3（`symbol_read`/`symbol_edit`/`symbol_rename`，MS-8 / MS-10）＋ `check`（MS-5），加执行体殿后追加的 `checkpoint`。**已无「声明齐备、调用返回 `not_implemented`」的原语**（见 §1.2） |

### 2.2 接口验收（IA-* / L-x / H-x）

> 列：**编号 | 状态 | 证据用例名 | 对应 MS-n | 缺口说明**。状态取值同 §2.1。本表**以 IA 编号为键**，已合并原架构 §12 的「待补/待接入」标记与 §14 的缺口行（**不保留两张并行缺口表**）。

| 编号 | 状态 | 证据用例名 | 对应 MS-n | 缺口说明 |
|---|---|---|---|---|
| <a id="ia-1-1"></a>IA-1.1 | 已落地 | `TestEngine_PrepareFailureStopsBeforeInference` | — | — |
| <a id="ia-1-2"></a>IA-1.2 | 已落地 | `TestEngine_HandlerTerminalWinsOverStop` | — | — |
| <a id="ia-1-3"></a>IA-1.3 | 已落地 | `TestEngine_CancelledBeforeFirstTurn`、`TestEngine_CancelDuringInferIsCancelled` | — | — |
| <a id="ia-1-4"></a>IA-1.4 | 已落地 | `TestEngine_TurnLimitIsMechanicalCap` | — | — |
| <a id="ia-1-5"></a>IA-1.5 | 已落地 | `TestEngine_FailStreakStopsLoss` | — | — |
| <a id="ia-1-6"></a>IA-1.6 | 已落地 | `TestEngine_NoToolCallSucceedsAndCollectsTurn` | — | — |
| <a id="ia-1-7"></a>IA-1.7 | 已落地 | `TestEngine_PanicBecomesEnvFailureAndFinalStillRuns` | — | — |
| <a id="ia-1-8"></a>IA-1.8 | 已落地 | 代码检查（`harness` 无执行/写盘调用） | — | — |
| <a id="ia-1-9"></a>IA-1.9 | 已落地 | `TestEngine_PanicBecomesEnvFailureAndFinalStillRuns`、`TestEngine_PrepareFailureStopsBeforeInference` | — | — |
| <a id="ia-1-10"></a>IA-1.10 | 已落地 | `TestNew_RequiresProvider` | — | — |
| <a id="ia-1-11"></a>IA-1.11 | 已落地 | `TestEngine_InferFailureIsEnvError`、`TestEngine_StreamErrorIsEnvError` | — | — |
| <a id="ia-1-12"></a>IA-1.12 | 已落地 | `TestEngine_OnTurnErrorIsFailed` | — | — |
| <a id="ia-1-13"></a>IA-1.13 | 已落地 | `TestFinalize_NeedsInputConvergesToBlocked`、`TestFinalize_BlockedDoesNotOverrideMechanicalTerminal`、`TestFinalize_NoDeclarationStaysSucceeded`、`TestOnTurn_DeclaredIsRecordedAtTheTurnItWasSaid`、`TestParseDeclared_SectionsAndEntries` | — | 自陈只在引擎给出 `succeeded` 时改写终态；机制性终止不被改写；未声明保持 `succeeded` |
| <a id="ia-2-1"></a>IA-2.1 | 已落地 | `TestFirstPrompt_KeepsOrderAndAppendsKernelBlocks`、`TestDefaultPromptPlugins_OrderIsThePromptOrder`、`TestPrepare_FirstPromptCarriesTaskAndConventions` | — | — |
| <a id="ia-2-11a"></a>IA-2.11(a) 内核条款与环境事实 | 已落地 | `TestFirstPrompt_KeepsOrderAndAppendsKernelBlocks`、`TestPrepare_FirstPromptCarriesTaskAndConventions` | — | 编号重复，限定词 = 内核条款与环境事实 |
| <a id="ia-2-2"></a>IA-2.2 | 已落地 | `TestContextBuilder_AssemblesPromptThenHistory` | — | — |
| <a id="ia-2-3"></a>IA-2.3 | 已落地 | `TestOnTurn_FiltersRunBeforeRecording` | — | — |
| <a id="ia-2-4"></a>IA-2.4 | 已落地 | 同 `TestOnTurn_FiltersRunBeforeRecording`（值拷贝断言之一） | — | — |
| <a id="ia-2-5"></a>IA-2.5 | 已落地 | `TestCompaction_NeverDropsBountyOrWriteOps`、`TestCompaction_CurrentTurnIsNeverCompacted` | MS-11 | 当前轮与 Bounty 正文永不裁剪（首轮提示词不在下压范围内） |
| <a id="ia-2-6"></a>IA-2.6 | 已落地 | `TestWatermark_TriggersAtWarnAndCoolsDown`、`TestCompaction_NoWatermarksMeansNoCompaction`、`TestCompaction_NothingToCompactDoesNotArmCooldown` | MS-11 | 低于预警不压、达预警才压、冷却期内不重复压；**一层都没压动时不上冷却**（只有当前轮时无从压，冷却若在那时上膛，冷却若在「压不动」时就上膛，下一轮第一个可压的老轮会被自己的冷却挡住（该压的时候压不动）；硬上限在**压完之后**才问——压得下来就不终止，哪怕压之前确实站在它之上 |
| <a id="ia-2-7"></a>IA-2.7 | 部分已落地 | `TestCompaction_LayersDownToFit`、`TestCompaction_PressesThroughAllLayersWhenTargetIsUnreachable`、`TestCompaction_L0KeepsBothEndsAndMarksTheDrop`、`TestCompaction_L2KeepsAddressability`、`TestCompaction_FoldPreservesDeclaredSections` | MS-11 | L0→L3 按序下压、够用即停、零模型调用；**L4（模型摘要）不实现**——它默认不启用（FR-14.4），且「生成一次即落盘」要动材料与恢复链路，不做半个版本 |
| <a id="ia-2-8"></a>IA-2.8 | 已落地 | `TestCompaction_ProjectionIsReproducible`、`TestCompaction_ResumeUsesTheSamePipeline` | MS-11 | 同一份记录两次投影逐字一致；恢复回灌与正常运行时投影一致（FR-14.8） |
| <a id="ia-2-9"></a>IA-2.9 | 已落地 | `TestInputBudget`、`TestWatermarks` | — | — |
| <a id="ia-2-10"></a>IA-2.10 | 已落地 | `TestBuild_InjectsRootConventions`、`TestBuild_CaseVariantIsNotRecognized`、`TestBuild_BlankContentIsTreatedAsAbsent`、`TestBuild_OversizeTruncatesWithNotice`、`TestBuild_WithoutBaseCommitFallsBackToWorktree` | — | 嵌套约定附注待排期（见 §5） |
| <a id="ia-2-11b"></a>IA-2.11(b) skill 发现清单 | 已落地 | `TestBuild_ListsNameDescriptionAndPath`、`TestBuild_InvalidEntryIsSkippedWithNotice`、`TestBuild_NameMustMatchDirectory`、`TestBuild_IgnoresSkillFilesOutsideDirectory`、`TestBuild_TruncatesBeyondLimit`、`TestBuild_NoSkillsYieldsEmptyPart`、`TestParseFrontmatter`、`TestParseFrontmatter_LengthLimitsComeFromSpec` | — | 编号重复，限定词 = skill 发现清单 |
| <a id="ia-2-12"></a>IA-2.12 | 已落地 | `TestDecide_WriteToControlDirDenied`、`TestDecide_WriteToSkillsDraftAllowed`、`TestDiff_ExcludesMaterialDirButKeepsSkillsDraft`、`TestDiff_NoMaterialDirKeepsEverything` | MS-6 | 策略侧与 git 侧都已落地：交付 diff/patch 只排除**本次会话**的材料目录，`.xhunter/` 下其它路径（如 `skills.draft/**`）是交付内容、照进 diff |
| <a id="ia-2-13"></a>IA-2.13 | 已落地 | `TestDefaultPromptPlugins_OrderIsThePromptOrder`、`TestDefaultPromptPlugins_NoDuplicate`、`TestFirstPrompt_KeepsOrderAndAppendsKernelBlocks`、`TestPrepare_PluginFailureConvergesAsEnvError` | MS-2 | 插件失败按环境错误收敛（`prepare_failed` / 退出 1，循环不开始） |
| <a id="ia-3-1"></a>IA-3.1 | 已落地 | `TestDefaultTools_FaceIsFixed` | — | — |
| <a id="ia-3-2"></a>IA-3.2 | 已落地 | `TestDefaultTools_ShapeIsDeclared`、各原语 `*_DeclShape`、`TestObjectSchema_CompactsSyntaxButKeepsStringContent`、`TestObjectSchema_InvalidFragmentPanics` | — | — |
| <a id="ia-3-3"></a>IA-3.3 | 已落地 | `TestCommitter_RejectsWriteWithoutPriorRead`、`TestCommitter_RejectsStaleRead` | — | — |
| <a id="ia-3-4"></a>IA-3.4 | 已落地 | `TestEdit_NoMatchReportsNotFound`、`TestEdit_AmbiguousReportsCount` | — | — |
| <a id="ia-3-5"></a>IA-3.5 | 已落地 | `TestSymbolEdit_ReplacesOnlyTargetRange`、`TestEndToEnd_SymbolEditLandsInTheDelivery` | MS-8 | 符号原语落盘仍走 `Committer`：原语只产出 `FileEdit` |
| <a id="ia-3-6"></a>IA-3.6 | 已落地 | `TestCommitter_OnlyReplacesTargetRange`、`TestCommitter_RejectsOutOfRange`、`TestCommitter_BatchIsAllOrNothingOnValidation`、`TestCommitter_NewFileRules`、`TestCommitter_MarksNewFingerprintAfterWrite` | — | — |
| <a id="ia-3-7"></a>IA-3.7 | 已落地 | `TestResolve_RejectsPathsOutsideWorkspace`、`TestResolve_RejectsSymlinkEscape`、`TestResolve_RejectsSymlinkEscapeForNewFile`、`TestResolve_AllowsSymlinkInsideWorkspace` | — | — |
| <a id="ia-3-8"></a>IA-3.8 | 已落地 | `TestList_SkipsNoiseButKeepsControlDir`、`TestList_OrderIsStable`、`TestList_PatternSemantics`、`TestGlob_DirectoryAndDeepPatterns` | — | — |
| <a id="ia-3-9"></a>IA-3.9 | 已落地 | `TestBindToolCall_RejectsShapesThatWouldExecuteTheWrongThing`、`TestBindToolCall_MapsSlots`、`TestBindToolCall_DoesNotKnowToolNames`、`TestUnbindToolCall_RoundTripsNonEmptyFields` | — | — |
| <a id="ia-3-10"></a>IA-3.10 | 已落地 | 代码检查（`produced()` 携带 `Results[].CallID`） | — | — |
| <a id="ia-3-11"></a>IA-3.11 | 已落地 | `TestOnTurn_AnsweredCallIsNotReExecuted` | — | — |
| <a id="ia-3-12"></a>IA-3.12 | 已落地 | `TestRead_OversizeTruncatesWithContinuationHint`、`TestRead_LineRangeIsHonoured`、`TestRead_ReportsFirstLineOfRange` | — | — |
| <a id="ia-3-13"></a>IA-3.13 | 已落地 | `TestWrite_InvalidPathPropagates`、`TestGlob_InvalidPatternPropagates`、`TestRead_MissingFileIsError` | — | — |
| <a id="ia-3-14"></a>IA-3.14 | 已落地 | `TestWrite_ExistingFileIsRejectedWithGuidance` | — | — |
| <a id="ia-3-15"></a>IA-3.15 | 已落地 | `TestSymbolics_UnavailableBackendIsStructuredError`、`TestCheck_UnavailableRunnerIsRetryableEnvFault` | MS-5 / MS-8 | **已无未实现原语**：`check` 随 MS-5 实现（执行器缺失是**可重试的装配缺件** `gate_unavailable`），符号三原语随 MS-8 / MS-10 注册。能力不可用走的是**另一条**——结构化错误（`ext_unavailable` / `language_unregistered`），与 `not_implemented` 不是一回事 |
| <a id="ia-3-16"></a>IA-3.16 | 已落地 | `TestCheck_DeclSurfaceIsExactlyTheGateName` | — | — |
| <a id="ia-3-17"></a>IA-3.17 | 已落地 | `TestSymbolics_SurfaceHasNoContentAddressingSlot`、`TestSymbolics_DeclShapes` | — | — |
| <a id="ia-3-18"></a>IA-3.18 | 已落地 | `TestPrepare_IncompleteAssemblyFailsLoudly`、`TestExecuteCall_MissingPolicyFailsClosed` | — | — |
| <a id="ia-3-19"></a>IA-3.19 | 已落地 | `TestFind_MatchesWithLineNumbers`、`TestFind_NoMatchIsSuccessWithExplicitText`、`TestFind_ScopeLimitsSearch`、`TestFind_PathLimitsToSingleFile`、`TestFind_TruncatesHitsButReportsTotal`、`TestFind_RequiresLiteral` | — | — |
| <a id="ia-3-20"></a>IA-3.20 | 已落地 | `TestBuildTools_RejectsDuplicateName`、`TestBuildTools_RejectsEmptyName`、`TestBuildTools_RejectsCheckpointFromFactory`、`TestBuildTools_KeepsOrderAndAcceptsAFullFace`、`TestBuildTools_NoFactoryIsNotAnError`、`TestPrepare_FailsOnBrokenToolFace` | — | 框架侧校验工具面自洽（架构 §7.3）；此前只有装配层自查，重复 / 匿名声明会一路发到供应商 |
| <a id="ia-4-1"></a>IA-4.1 | 已落地 | 代码检查（无改动规模维度） | — | 裁决输入 = 「目标路径 ＋ 是否写盘」 |
| <a id="ia-4-2"></a>IA-4.2 | 已落地 | `TestExecuteCall_PolicyDenialIsReported` | — | — |
| <a id="ia-4-3"></a>IA-4.3 | 已落地 | `TestExecuteCall_PolicyDenialIsReported`、`TestExecuteCall_MissingPolicyFailsClosed` | — | — |
| <a id="ia-4-4"></a>IA-4.4 | 已落地 | 代码检查（`llm.Session` 只有 `Events` / `Cancel`） | — | — |
| <a id="ia-4-5"></a>IA-4.5 | 已落地 | `TestDecide_ReadAndGateAllowed`、`TestDecide_WriteAllowed`、`TestDecide_NonWritingCallSkipsPathRules`、`TestDefaultTools_WritesIsDeclared` | — | 写盘性质由原语自述（`Writes()`）；「未知原语默认拒绝」换到执行体查表处（`unknown_tool`） |
| <a id="ia-4-6"></a>IA-4.6 | 已落地 | `TestDecide_PathEscapeDenied`、`TestDecide_WriteToControlDirDenied`、`TestDecide_WriteToSkillsDraftAllowed` | — | — |
| <a id="ia-4-7"></a>IA-4.7 | 已落地 | `TestChargeAndExhausted_Tokens`、`TestExhausted_Turns`、`TestExhausted_WallClock`、`TestExhausted_ZeroMeansUnlimited` | — | — |
| <a id="ia-4-7b"></a>IA-4.7b | 已落地 | `TestBountyFromEnv_DeliversBudget`、`TestEndToEnd_BudgetExhaustionStopsTheRun` | — | — |
| <a id="ia-4-8"></a>IA-4.8 | 已落地 | `TestDeniedStreakLimitIsThree`、`TestDeniedCount_TerminatesAfterThreshold`、`TestDecide_DeniedStreakResetsOnAllow`（policy）、`TestOnTurn_DeniedStreakOutranksBudget`（hunt）、`TestEndToEnd_DeniedStreakFailsTheRun`（cmd） | MS-3 | `DeniedCount`：连续拒绝达 3 次终止；出现一次 Allow 归零 |
| <a id="ia-4-9"></a>IA-4.9 | 已落地 | `TestObserveFailure_SwitchThenTerminate`、`TestSameKindLimitsAreTwoAndThree`、`TestObserveFailure_ResetsOnSuccessAndKind`（policy）、`TestExecuteCall_ObservesOutcomePerCall`、`TestOnTurn_SwitchHintAppendedToNextTurnMessages`（hunt） | MS-3 | 止损三态 continue/switch/terminate，阈值 2／上限 3 分离（FR-9.4） |
| <a id="ia-4-10"></a>IA-4.10 | 已落地 | `TestPolicy_ChargeReceivesIncrements` | MS-2 | `Session.charge` 把**增量**交策略：按用量水位算、非增长轮不上报 |
| <a id="h4-裁决点"></a>H4-裁决点 | 已落地 | 见 IA-4.5~4.9 | — | 路径边界 / 破坏性 / 预算 / 止损**全部已落地**（MS-3 收口：两段式止损两轴）；影响面**不做**（设计）；权限询问**不适用**（设计，路径不存在） |
| <a id="ia-5-1"></a>IA-5.1 | 已落地 | `TestEventSink_EmitsFlatJSONLine` | — | — |
| <a id="ia-5-2"></a>IA-5.2 | 已落地 | `TestEventSink_StdoutIsPureNDJSON`、`TestHuntCmd_EventsGoToStdoutAndLogsGoToStderr` | MS-2 | stdout 逐行合法事件行（含信封四字段、`ts` RFC3339），无杂质 |
| <a id="ia-5-3"></a>IA-5.3 | 已落地 | `TestEventSink_RemembersFirstWriteFailure`、`TestEventSink_FailedCoversHeartbeatWrites`、`TestEndToEnd_BrokenEventChannelIsEnvError` | — | — |
| <a id="ia-5-4"></a>IA-5.4 | 已落地 | `TestHeartbeat_EmitsAtInterval`、`TestHeartbeat_StopsOnContextCancel`、`TestHeartbeat_StopIsIdempotent`、`TestSession_PhaseTracksStages` | MS-2 | 心跳按任务输出（非 runtime），带 `phase` 与 `elapsed_ms`；间隔缺省 30s、`XHUNTER_HEARTBEAT_INTERVAL` 可覆盖；随 ctx 取消即停（INV-8），停止点在 `Finalize` 的终态块之前 |
| <a id="ia-5-5"></a>IA-5.5 | 已落地 | `TestExecuteCall_EveryOutcomeEmitsOneToolResult`、`TestExecuteCall_SuccessRecordsOpsAndSummary` | — | — |
| <a id="ia-5-6"></a>IA-5.6 | 已落地 | 代码检查（FR-11.5） | — | — |
| <a id="ia-5-7"></a>IA-5.7 | 已落地 | `TestEventSink_StampsEnvelopeOnEveryEvent`、`TestHuntCmd_EventsGoToStdoutAndLogsGoToStderr`、`TestBountyFromEnv_TraceID` | — | — |
| <a id="ia-5-8"></a>IA-5.8 | 已落地 | `TestExecuteCall_PolicyDenialIsReported` | — | — |
| <a id="ia-6-1"></a>IA-6.1 | 已落地 | `TestSave_MaterialLandsUnderSessionDirWithSchemaVersion`、`TestRecorder_RecordsOpsAndUsageFromTheSession`、`TestMaterial_LoadRejectsUnknownSchemaVersion`、`TestEndToEnd_MaterialIsSelfSufficientForResume`、`TestRecorder_SnapshotAfterCharge` | MS-6 | 材料自含续跑信息（对话历史 ＋ 写操作序列 ＋ **每轮**用量 ＋ 能力指纹位），meta 带 `schema_version`；读取端版本不认识即报错 |
| <a id="ia-6-1b"></a>IA-6.1b | 已落地 | `TestSave_MaterialLandsUnderSessionDirWithSchemaVersion` | MS-6 | 按任务隔离 `.xhunter/<session_id>/session.jsonl`；不同 session 互不覆盖 |
| <a id="ia-6-1c"></a>IA-6.1c | 已落地 | `TestCommit_ForceAddsMaterialEvenWhenGitignored`、`TestCommit_MissingMaterialDirDoesNotFail`、`TestCommit_WithoutMaterialDirIsUnchanged` | MS-6 | `Commit` 在 `add -A` 后对本次会话材料目录再 `add -f`（仓库忽略 `.xhunter/` 时材料仍随提交）；目录尚未落盘则跳过、不判死；`MaterialDir` 为空时不加 |
| <a id="ia-6-2"></a>IA-6.2 | 已落地 | `TestPrepare_ResumeSeedsContextWithoutExecutingTools` | MS-7 | 恢复段不执行任何原语、不提交；写操作序列**不重放**（工作区已由分支 tip 给出） |
| <a id="ia-6-3"></a>IA-6.3 | 已落地 | `TestEndToEnd_ResumeContinuesFromLastCheckpoint`、`TestPrepare_ResumeAppendsNewConditionsAfterRestoredHistory` | MS-7 | 回灌读回的轮次（按原有顺序）后从下一轮继续；本次补充条件作为新 user 消息追加在历史之后 |
| <a id="ia-6-4"></a>IA-6.4 | 已落地 | `TestEndToEnd_ResumeCorruptedMaterialIsEnvError`、`TestMaterial_LoadRejectsUnknownSchemaVersion` | MS-7 | `schema_version` 不兼容 → 退出 1（`prepare_failed`、可重试），不自动迁移；材料**不存在**是零值、不是错误 |
| <a id="ia-6-5"></a>IA-6.5 | 已落地 | `TestMaterial_MetaCarriesTheExtFingerprint`、`TestPrepare_ExtFingerprintChangeIsRecordedNotFatal`、`TestEndToEnd_MaterialIsSelfSufficientForResume` | MS-8 | 能力指纹同时进**生效配置快照**（`effective_config.ext`、`hunt_start`）与**会话材料**（`meta.ext`），两处与宿主**同源**（都来自 `chooseExt` 选出的那个后端）。没有符号能力时材料里是空数组（已知事实，不是 null）。续跑时指纹不一致**只记录、不阻断**（诊断，不是恢复闸门） |
| <a id="ia-6-5b"></a>IA-6.5b | 已落地 | `TestDecide_WriteToControlDirDenied` | — | 策略侧已落地；git 侧靠工具面不含 git 原语 |
| <a id="ia-6-6"></a>IA-6.6 | 已落地 | `TestSnapshot_FailureDoesNotBlockTheRun`、`TestRecorder_OpenFailureDegradesWithoutFailingPrepare` | — | 材料绑定/落盘失败只降级（warn），不阻断任务 |
| <a id="ia-6-7"></a>IA-6.7 | 已落地 | 代码检查；`TestFinalize_TextOnlyDeliverySucceeds` | — | — |
| <a id="ia-6-8"></a>IA-6.8 | 待补 | — | §5 | 凭据不落材料的断言扫描（FR-8.4、AC-8） |
| <a id="ia-7-1"></a>IA-7.1 | 已落地 | `TestDefaultTools_FaceIsIdenticalWhetherTheBackendIsAvailable` | MS-8 | 「工具面恒定」对照：同一份装配在「符号后端可用 / 不可用」两情形下**已接入工具**的名字集合、schema 与说明完全相同；并反证两种装配确实一有一无 |
| <a id="ia-7-2"></a>IA-7.2 | 已落地 | `TestHost_UnconfiguredCommandIsUnavailable`、`TestHost_UnstartableCommandIsUnavailableNotFatal`、`TestChooseExt_ToolFaceIsIdenticalAcrossBackends`、`TestEndToEnd_ExternalBackendUnavailableStillConverges` | MS-8 | 扩展不可用 → 符号原语给结构化错误（模型退回文本寻址），**任务不失败**、工具名不撤回。换**后端种类**（内置 ↔ 外挂）同样不改工具面 |
| <a id="ia-7-3"></a>IA-7.3 | 已落地 | `TestHost_CapabilitiesAreSyntacticAndCannotResolve`（内置）、`TestHost_HandshakeCarriesTheServerSelfDescription`（外挂：能力来自扩展自述，不是核心猜的） | MS-8 | 能力描述符上报，两种后端形状一致、消费方分不出区别 |
| <a id="ia-7-4"></a>IA-7.4 | 已落地 | `TestSymbolic_ReportsSyntacticPrecision`、`TestHost_StartsLazilyAndLocates`、`TestHost_ParseAndEncloseAreMappedFromTheWire` | MS-8 | 精度如实上报（`Prepared.Precision`）：内置恒 `syntactic`，外挂按线路自述（如 `semantic`） |
| <a id="ia-7-5"></a>IA-7.5 | 已落地 | `TestSymbolRename_CanResolveFalseIsStructuredErrorNotTextReplace`、`TestHost_CapabilitiesAreSyntacticAndCannotResolve` | MS-8 | `CanResolve == false` 时仍注册、不降级（结构化错误，绝不静默退回文本替换） |
| <a id="ia-7-6"></a>IA-7.6 | 已落地 | `TestHost_CrashDoesNotHangTheLoop`、`TestHost_TimeoutIsolatesTheExtension`、`TestHost_CloseReclaimsProcess`、`TestFinalize_ClosesTheExtHost` | MS-8 | 崩溃隔离（真子进程夹具：四种失败形态——进程退了 / 握手不上 / 超时 / 输出不是 JSON，各有各的钉法）；回收由 `Finalize` 调 `Close` |
| <a id="ia-7-7"></a>IA-7.7 | 已落地 | `TestSymbolics_UnavailableBackendIsStructuredError` | MS-8 | 后端不可用不使任务失败：符号原语返回结构化错误（改用文本寻址），任务照常收敛。外挂通道已补：`TestEndToEnd_ExternalBackendUnavailableStillConverges`（起不来的外挂后端 → 任务照常收敛，且生效快照如实报出"这次用的是外挂后端"） |
| <a id="ia-8-1"></a>IA-8.1 | 已落地 | `TestFromEnv_ReportsAllIssuesAtOnce`、`TestProviderFor_PropagatesMissingContextWindow`、`TestNew_FailsAtStartupWhenFactsAreMissing` | — | — |
| <a id="ia-8-2"></a>IA-8.2 | 已落地 | 代码检查（`llm.Session` 方法集） | — | — |
| <a id="ia-8-3"></a>IA-8.3 | 已落地 | `TestProvider_MethodSetIsInferAndCapabilities`、`TestSession_HasNoPermissionChannel`、`TestCaps_DeclaresBehaviourFields` | MS-12 | 中立契约的方法集/字段集写死为期望：`Provider` 只有 `Infer`/`Capabilities`（工具执行不委托 Provider）、`Session` 只有 `Events`/`Cancel`（无权限应答通道）、`Caps` 只有 `MaxContextTokens` |
| <a id="ia-8-4"></a>IA-8.4 | 待补 | — | §5 | 源码静态扫描（重构后需重建） |
| <a id="ia-8-5"></a>IA-8.5 | 已落地 | `TestNew_BuildsEndpointAndDeclaresWindow`、`TestNew_BuildsEndpointAndPassesHeadersThrough`、`TestNew_AppliesProtocolHeadersAndLetsConfigOverride` | — | — |
| <a id="ia-8-6"></a>IA-8.6 | 已落地 | 代码检查（上游线格式类型不导出） | — | — |
| <a id="ia-8-7"></a>IA-8.7 | 已落地 | `TestInfer_TruncatedStreamIsExplicit`、`TestInfer_DoesNotInterpretArguments`、`TestBindToolCall_RejectsShapesThatWouldExecuteTheWrongThing` | — | — |
| <a id="ia-8-8"></a>IA-8.8 | 已落地 | `TestInfer_AssemblesRawToolCallsAcrossChunks`、`TestInfer_AssemblesRawFunctionCallFromDeltas`、`TestInfer_AssemblesRawToolUseFromJSONDeltas` | — | — |
| <a id="ia-8-9"></a>IA-8.9 | 已落地 | `TestInfer_SendsProtocolRequest`、`TestToWireTools_PassesSchemaThrough` | — | — |
| <a id="ia-8-10"></a>IA-8.10 | 已落地 | 代码检查（协议包不 import `providerconfig`） | — | — |
| <a id="ia-8-11"></a>IA-8.11 | 已落地 | `TestProviderFor_UnknownProtocolTellsWhereToAddOne`、`TestProviderFor_BuildsEveryKnownProtocol` | — | — |
| <a id="ia-8-12"></a>IA-8.12 | 已落地 | `TestProviderFor_RequiresEndpoint`、`TestFromEnv_HeadersExpandEnvRefs`、`TestProviderFor_APIKeyBecomesBearerHeader`、`TestProviderFor_NoCredentialMeansNoAuthHeader`、`TestProviderFor_NewProtocolIsJustAnotherEntry` | — | — |
| <a id="ia-8-13"></a>IA-8.13 | 已落地 | 结构自证（互相引用会构成导入循环） | — | — |
| <a id="ia-8-14"></a>IA-8.14 | 已落地 | 代码检查（`protocolFactories()` 返回新表） | — | — |
| <a id="ia-8-15a"></a>IA-8.15(a) 协议版本基线 | 已落地 | 代码检查（三个协议包的包注释） | — | 编号重复，限定词 = 协议版本基线 |
| <a id="ia-8-16a"></a>IA-8.16(a) 自持传输层 | 已落地 | 代码检查（`go.mod` 无 `require`） | — | 编号重复，限定词 = 自持传输层 |
| <a id="ia-8-17a"></a>IA-8.17(a) 未知角色 | 已落地 | `TestInfer_RejectsUnknownRole`、`TestToWireMessages_UnknownRoleIsRejected` | — | 编号重复，限定词 = 未知角色 |
| <a id="ia-8-15b"></a>IA-8.15(b) Messages 协议形状 | 已落地 | `TestInfer_SendsProtocolRequest`、`TestInfer_UsageIsNotDoubleCounted`、`TestProviderFor_BuildsEveryKnownProtocol` | — | 编号重复，限定词 = Messages 协议形状 |
| <a id="ia-8-16b"></a>IA-8.16(b) Responses 协议形状 | 已落地 | `TestInfer_SendsProtocolRequest`、`TestInfer_AssemblesRawFunctionCallFromDeltas`、`TestInfer_FallsBackToItemArguments` | — | 编号重复，限定词 = Responses 协议形状 |
| <a id="ia-8-17b"></a>IA-8.17(b) 收尾语义 | 已落地 | `TestInfer_TruncatedWithoutMessageStopIsExplicit`、`TestInfer_TruncatedWithoutCompletedIsExplicit`、`TestInfer_IncompleteIsNormalEnd` | — | 编号重复，限定词 = 收尾语义 |
| <a id="ia-8-18"></a>IA-8.18 | 已落地 | `TestInfer_ErrorEventIsClassified`（两协议各一） | — | — |
| <a id="ia-8-19"></a>IA-8.19 | 已落地 | `TestInfer_ReportsCachedInputTokens`、`TestInfer_MissingCacheDetailsReportsZero`、`TestInfer_InputIncludesCacheReadAndCreation`、`TestInfer_NoCacheReportsNativeInput`、`TestEndToEnd_LocalRunProducesDeliveryCommit` | — | — |
| <a id="ia-9-1"></a>IA-9.1 | 已落地 | `TestFromEnv_ResolvesFullFacts`、`TestFromEnv_ProtocolDefaults`、`TestFromEnv_APIKeyIsOptional` | — | — |
| <a id="ia-9-2"></a>IA-9.2 | 已落地 | `TestFromEnv_ReportsAllIssuesAtOnce` | — | — |
| <a id="ia-9-3"></a>IA-9.3 | 已落地 | `TestFromEnv_RejectsBadLimitValues` | — | — |
| <a id="ia-9-4"></a>IA-9.4 | 已落地 | `TestFromEnv_UnknownProtocolIsExplicit` | — | — |
| <a id="ia-9-5"></a>IA-9.5 | 已落地 | `TestFromEnv_HeadersExpandEnvRefs`、`TestFromEnv_ReportsBadHeaders`、`TestExpandEnvRef` | — | — |
| <a id="ia-9-6"></a>IA-9.6 | 已落地 | `TestFromEnv_NoHeadersIsNil` | — | — |
| <a id="ia-9-7"></a>IA-9.7 | 已落地 | `TestFromEnv_UnknownProtocolIsExplicit` | — | — |
| <a id="ia-9-8"></a>IA-9.8 | 已落地 | `TestInputBudget` | — | — |
| <a id="ia-9-9"></a>IA-9.9 | 已落地 | `TestWatermarks` | — | — |
| <a id="ia-9-10"></a>IA-9.10 | 已落地 | `TestEnvRefName`、`TestExpandEnvRef` | — | — |
| <a id="ia-11-1"></a>IA-11.1 | 已落地 | `TestPrepareBaseline_FreshTaskCreatesAndPushesBranch`、`TestEndToEnd_LocalRunProducesDeliveryCommit` | — | — |
| <a id="ia-11-2"></a>IA-11.2 | 已落地 | `TestPrepareBaseline_ResumeChecksOutBranchTip`、`TestPrepareBaseline_DivergedBranchIsEnvError` | — | — |
| <a id="ia-11-3"></a>IA-11.3 | 已落地 | `TestPrepareBaseline_FailsAtStartupOnUnusableFacts` | — | — |
| <a id="ia-11-4"></a>IA-11.4 | 已落地 | `TestCommit_PushesFastForwardAndSkipsEmptyCommit`、`TestCheckpoint_LogsNoOpWhenNothingWasCommitted`、`TestEndToEnd_LocalRunProducesDeliveryCommit` | — | — |
| <a id="ia-11-5"></a>IA-11.5 | 已落地 | `TestCommit_RejectsNonFastForward` | — | — |
| <a id="ia-11-6"></a>IA-11.6 | 已落地 | `TestDiff_ListsFilesChangedSinceBaseline`、`TestPatch_AppliesCleanlyToBaseline`、`TestDiff_ExcludesMaterialDirButKeepsSkillsDraft`、`TestDiff_NoMaterialDirKeepsEverything`、`TestEndToEnd_LocalRunProducesDeliveryCommit` | MS-6 | `Diff`/`Patch` 按 `RepoRef.MaterialDir` 排除本次会话材料目录，**同目录下其他路径（如 `skills.draft/**`）保留**；`MaterialDir` 为空时不加排除 |
| <a id="ia-11-7"></a>IA-11.7 | 已落地 | `TestClean_RemovesWorktreeAndIsIdempotent` | — | — |
| <a id="ia-11-8"></a>IA-11.8 | 已落地 | `TestWiring_PrepareBaselineRunsBeforeAnyTool`、`TestCheckpoint_CommitOnlyAtTurnBoundaryAndFinalize`、`TestDefaultTools_HasNoGitPrimitive` | MS-4 | `PrepareBaseline` 是第一个动作（先于 Open / 构造原语 / 构造正文）；Commit 只在轮边界与收尾；工具面不含 git 原语（类型级证据：工厂入参只有 `workspace.Workspace` 与 `ext.ExtHost`、`Primitive` 方法集不涉 `git.GitWorktree`） |
| <a id="ia-11-9"></a>IA-11.9 | 已落地 | `TestCheckpoint_SingleFailureSelfHeals`、`TestCheckpoint_StreakLimitConvergesAsEnvError`、`TestCheckpoint_SuccessResetsStreak` | MS-4 | 自愈（单次失败只记 warn、下一轮重提）＋ 连败上限（连续 3 次 → `checkpoint_failed_streak`，退出 1） |
| <a id="ia-11-10"></a>IA-11.10 | 已落地 | `TestCheckpoint_CommitsOnlyThisTurnsChanges` | MS-4 | 第 N 轮的检查点提交发生在第 N 轮工具之后、第 N+1 轮工具之前；末轮未落检查点的改动由收尾交付提交带上（交付口径） |
| <a id="ia-11-11"></a>IA-11.11 | 已落地 | `TestCheckpoint_NoAutoCheckpointOffStructuralPoint`、`TestCheckpoint_StructuralFailDoesNotCommit`、`TestCheckpoint_StructuralPassCommits`、`TestCheckpoint_UndecidableReportsDegradedOnce`、`TestCheckpoint_LogsNoOpWhenNothingWasCommitted`、`TestStructural_ParseOKFlipCommits`、`TestStructural_IncompleteSyntaxSuppresses`、`TestStructural_UndecidableDoesNotCommit` | MS-4 / MS-9 | 三态（pass/fail/undecidable）与不可判定如实上报已落地；真实判据（MS-9）已接 `ext.ExtHost` 的 `Parse` / `Enclose` |
| <a id="ia-11-12"></a>IA-11.12 | 已落地 | — | MS-5 | 门禁驱动检查点：通过 → 本轮必提交；未通过 → 抑制后续自动检查点（优先于结构判据）。用例 `TestCheckpoint_GateFailureSuppressesAutoCheckpoint`、`TestCheckpoint_WithoutGateFailureAutoCheckpointStillCommits`、`TestCheck_PassedGateRequestsCheckpoint`、`TestCheck_FailedGateDoesNotRequestCheckpoint` |
| <a id="ia-11-13"></a>IA-11.13 | 已落地 | `TestCheckpoint_ModelRequestIsConsumedOnceAndSkipsEmptyCommit`、`TestCheckpointMessage_Composition`、`TestSanitizeIntent` | MS-4 | 一次请求只兑现一次（重复请求不覆盖首次理由、消费后意图清空）；无改动不产生空提交但意图照样消费；提交信息由执行体合成（前缀/分隔符固定，模型改不了） |
| <a id="ia-12-1"></a>IA-12.1 | 已落地 | `TestWiring_SatisfiesContracts` | — | — |
| <a id="ia-12-2"></a>IA-12.2 | 已落地 | `TestDefaultTools_FaceIsFixed`、`TestDefaultTools_ShapeIsDeclared` | — | — |
| <a id="ia-12-3"></a>IA-12.3 | 已落地 | `TestDefaultPromptPlugins_OrderIsThePromptOrder`、`TestDefaultPromptPlugins_NoDuplicate` | — | — |
| <a id="ia-12-4"></a>IA-12.4 | 已落地 | `TestBackends_SatisfyContracts`、`TestBackends_RejectBadRoot` | — | — |
| <a id="ia-12-5"></a>IA-12.5 | 已落地 | `TestBountyFromEnv_RequiresRepoFacts`、`TestFromEnv_ReportsAllIssuesAtOnce`、`TestProviderFor_UnknownProtocolTellsWhereToAddOne` | — | — |
| <a id="ia-12-6"></a>IA-12.6 | 已落地 | `TestEndToEnd_LocalRunProducesDeliveryCommit`、`TestEndToEnd_EventSequenceIsComplete`、`TestWiring_SingleAssemblyPath`（源码扫描：装配入口字面调用恰好一次且在 `executeHunt` 内——FR-1.9「同一份装配」） | MS-2 | 事件序列加强断言（`hunt_start` 首、`hunt_end` 尾、`tool_call`↔`tool_result` 配对、`deliverable` 在 `hunt_end` 前）见后者 |
| <a id="ia-12-7"></a>IA-12.7 | 已落地 | 代码检查（`harness` 只有 `New(provider, ...)`） | — | — |
| <a id="ia-12-8"></a>IA-12.8 | 部分待补 | `TestSignalContext_CancelsOnSignal`、`TestSignalContext_StopCancelsContext` | — | 进程级 AC-6 断言待补 |
| <a id="ia-12-9"></a>IA-12.9 | 已落地 | `TestEndToEnd_LocalRunProducesDeliveryCommit`、`TestEndToEnd_ResultFileWrittenOnFailure`、`TestResultFile_DeclarationsAreThreeState`、`TestResultFile_EffectiveConfigIsWritten`、`TestFinalize_RequiredGateNeverRunFailsTheDelivery`、`TestFinalize_BackfillRunsOnlyRequiredGatesThatNeverRan`、`TestEndToEnd_RequiredGateNeverRunFailsWithDelivery` | — | `needs` / `assumptions` 三态（未提供 → `null`）；`effective_config`（MS-2）、`gates`（MS-5，以清单为准列出未运行项）**均已接线**，契约里的结果文件字段已无未接线项 |

**运行段 L1~L7 的逐步骤状态**（对应架构 §6.3；未列出的步骤为已落地）：

| 编号 | 状态 | 对应 MS-n | 缺口说明 |
|---|---|---|---|
| <a id="l1-4"></a>L1-4 门禁清单来源裁决 | 已落地 | MS-5 | `Prepare` 裁决「Bounty 下发 > 基线 commit 的 `gates.yml` > 无」；`working_tree` 豁免档只能由 Bounty 授予，护栏三条（元门禁 / 强度不得降低 / `gate_config_changed`）。用例 `TestResolveGates_BountyOverridesRepoDeclaration`、`TestResolveGates_RepoDeclarationIsUsedWhenNothingIsHandedDown`、`TestResolveGates_NoSourceAtAllMeansNone`、`TestResolveGates_ExemptionRequiresABountyGrant`、`TestResolveGates_WorkingTreeReadsTheWorkspaceFile`、`TestResolveGates_ExemptionWithoutGateSourceFails` |
| ~~契约：会话恢复接入位 ＋ 结果文件 `session_delta` 字段位~~（MS-7） | **已落地**（2026-09-24）：`hunt.Restored`（轮次 / 写操作序列 / 用量）＋ `SessionRecorder.Load(root string)`（**纯读、先于 `Open`**；与 `Ops()` 分工：本次运行 vs 读回上次）；`Prepare` 在 `Bounty.Session != nil` 时读回并回灌上下文；结果文件 `session_delta`（形状上收为 `hunt.SessionDelta`，`{turns_from, turns_to, ops_count}`，`omitempty`——**仅恢复时出现**）。**归属修正**：`session_delta` 原挂 MS-6，改挂 **MS-7**。用例 `TestEndToEnd_ResumeContinuesFromLastCheckpoint`、`TestPrepare_ResumeSeedsPolicyBudgetWithRestoredUsage` |
| ~~契约：流看门狗、压缩位与 config_snapshot 形状~~（MS-11 / MS-12） | **契约已定义**（2026-09-23，未冻结期）：`harness.Config.StreamIdleTimeout`（接收段不活动超时；0=不限、缺省 120s；接入点在接收循环、注释标明计时器留 MS-12）；`hunt.CompactionConfig`（三档水位 ＋ 冷却，水位来自 `providerconfig.Resolved.Watermarks`）＋ `context_compacted` 载荷（`level`/`released_tokens`/`watermark`）＋ `config_snapshot` 载荷（三个阈值及其来源）。**发出点均不接**（`config_snapshot` 的 `max_denied_streak` 来源未定死 / 压缩未实现）。**更正（MS-12，2026-09-24）**：`StreamIdleTimeout` 的「0=不限」口径**当时就不成立**（`withDefaults` 一直把 0 变成 120s），现更正为 **0 = 取默认 120s / 正数 = 不活动上界 / 负数 = 关闭看门狗**，且计时器已随 MS-12 落地（见 §2.3）。**实现已全部落地**：计时器随 MS-12、压缩随 MS-11、`config_snapshot` 随 MS-3（分别见本表各条） |
| <a id="l1-5"></a>L1-5 扩展能力描述符 | 已落地 | MS-8 | 装配层注入 `ext.ExtHost`（默认 `ext/syntax` 内置语法级后端）；`Capabilities` / `Locate` / `Fingerprint` / `Close` 全部实现，`ext.Unimplemented{}` 的 4 处 panic 哨兵**已回收**为如实降级。**外挂通道**（`ext/mcp`）按部署事实 `XHUNTER_EXT_COMMAND` 选中：宿主工厂与能力指纹**同源**（`cmd/xhunter` 的 `chooseExt` 一次给出两端），`Close` 由 `Finalize` 末尾调用（收尾没走到造宿主那一步时不调——零值不是可关的东西） |
| <a id="l1-8"></a>L1-8 会话恢复（条件） | 已落地 | MS-7 | `XHUNTER_SESSION_ID` 非空时：`Prepare` 在 `openRecorder` 之前**纯读**回材料（`Load(root)`）→ 回灌上下文 → 从下一轮继续；材料不存在 → 零值（不算恢复），存在但读不出来 → 环境错误（退出 1）。用例 `TestPrepare_ResumeSeedsContextWithoutExecutingTools`、`TestEndToEnd_ResumeCorruptedMaterialIsEnvError` |
| <a id="l1-9"></a>L1-9 生效配置快照 | 已落地 | MS-2 | 装配完成后冻结一次，进 `hunt_start` 与结果文件 `effective_config`（含原语顺序与殿后 `checkpoint`）；门禁清单随 MS-5。证据 `TestEffectiveConfig_ListsPrimitivesInToolFaceOrder`、`TestPrepare_EmitsHuntStart` |
| <a id="l2-事件通道"></a>L2-事件通道健康 | 已落地 | MS-2 | `OnTurn` 入口复查 `Sink.Failed()`：通道已断则本轮零工具执行，收敛 `event_channel_failed`（退出 1）。用例 `TestOnTurn_StopsBeforeWorkWhenChannelAlreadyFailed` |
| <a id="l2-压缩"></a>L2-压缩 | 部分已落地 | MS-11 | 压缩已落在 `ContextBuilder`（`cmd/xhunter/context.go`）内：水位 ＋ 分层下压 ＋ `context_compacted`；**余 L4 模型摘要**（FR-14.5） |
| <a id="l4-流看门狗"></a>L4-流看门狗 | 已落地 | MS-12 | 接收段不活动超时随 MS-12 接入：相邻两事件之间（含首事件之前）静默超上界 → **先落终态、再 `Cancel()`**，收敛为 `stream_idle_timeout`（环境错误，退出 1）；心跳不算活动、取消优先于超时、超时结构上不落 `no_tool_call`。取值口径：`StreamIdleTimeout` **0 = 取默认 120s / 正数 = 不活动上界 / 负数 = 关闭看门狗**（关闭不暴露给部署侧）。用例 `TestWatchdog_IdleStreamCancelsAsEnvError`、`TestWatchdog_TimeoutCannotBeNoToolCall`、`TestWatchdog_CancelOutranksTimeout`、`TestEndToEnd_IdleStreamConvergesToEnvError` |
| <a id="l6-检查点"></a>L6-检查点决策 | 已落地 | MS-4 / MS-5 / MS-9 | 模型显式请求 ＋ 门禁驱动（MS-5）＋ 结构判据（MS-9）＋ 收尾交付提交，四条触发链已补全 |
| <a id="l6-结构检查"></a>L6-结构检查 | 已落地 | MS-9 | 判据已接入（`judgeStructural`）：① 此刻语法完整 ② 改动封闭在符号内；三态收敛，判不了即不提交并如实上报。用例 `TestStructural_*`（8 条）、`TestHost_Parse*` / `TestHost_Enclose*`（`ext/syntax`）、`TestEndToEnd_CheckpointLandsOnCompleteSyntax` |
| <a id="l6-事件通道复查"></a>L6-事件通道复查 | 已落地 | MS-2 | 轮末守卫复查（取消之后、预算之前）：本轮发出时断则不再进入下一轮，收敛 `event_channel_failed`（退出 1）。用例 `TestOnTurn_StopsAfterTurnWhenChannelFailsDuringTurn`、`TestOnTurn_ChannelFailureDoesNotOverrideCancellation` |
| <a id="l6-提交连败"></a>L6-提交连败复查 | 已落地 | MS-4 | 连续 3 次提交失败 → 本轮结束即收敛为环境错误（退出 1），不跑完剩余轮次。用例 `TestCheckpoint_StreakLimitConvergesAsEnvError` |
| <a id="l7-gates"></a>L7-gates 补跑 | 已落地 | MS-5 | 收尾补跑**未跑过**的 `required` 门禁（预算已耗尽则不补跑、按未运行判失败）；执行失败记 degraded(`scope: gate`) 并按未运行计入终态。用例 `TestFinalize_BackfillRunsOnlyRequiredGatesThatNeverRan`、`TestFinalize_RequiredGateNeverRunFailsTheDelivery`、`TestFinalize_RequiredGateFailureFailsTheDelivery`、`TestFinalize_OptionalGateDoesNotRequireEvidence` |

**组件级状态（H1~H7）**：

| 编号 | 状态 | 说明 |
|---|---|---|
| <a id="h1-loop"></a>H1 Loop | 已落地 | 全部 IA-1.x |
| <a id="h2-context"></a>H2 Context | 已落地（组装 ＋ 压缩） | 见 IA-2.x；压缩随 MS-11 落地（L4 除外） |
| <a id="h3-tools"></a>H3 Tools | 已落地 | 见 IA-3.x；基础＋流水线（MS-1）、符号（MS-8 / MS-10）、门禁（MS-5）均已落地 |
| <a id="h4-policy"></a>H4 Policy | 见 H4-裁决点 | 见上 |
| <a id="h5-stream"></a>H5 Stream | 已落地 | 出口/信封 ＋ 心跳（MS-2）＋ 会话材料落盘（MS-6）＋ 恢复读回（MS-7） |
| <a id="h6-session"></a>H6 Session | 已落地 | 材料落盘（MS-6）与恢复（MS-7）均已落地：`Load(root)` 纯读回灌、`SessionDelta` 随交付事实定型 |
| <a id="h7-ext"></a>H7 Ext | 已落地 | 两种后端同一份 `ext.ExtHost`：内置语法级 `ext/syntax` ＋ 外挂 `ext/mcp`（MCP over stdio，懒启动／隔离／回收／不继承环境），由部署事实选（MS-8）；能力指纹进生效快照与会话材料 |

**使用手册（usage）外部契约的状态键**：

| 编号 | 状态 | 对应 MS-n | 缺口说明 |
|---|---|---|---|
| <a id="usage-1-2-bounty"></a>usage§1.2·Bounty生成器 | 已落地 | MS-12 | `xhunter run --repo <path> --task <text> [--out <path>]`：探测 → 生成 Bounty（`--out` 可选）→ 用同一份装配执行一次 Hunt。当前 CLI 子命令只有 `version` 与 `run`，以及缺省执行形态 `--bounty <path>`（另有 `--log-file`/`--result`/`--patch`）；不存在 `models` 子命令 |
| <a id="usage-5-events"></a>usage§5·已发出事件 | 已全部发出 | MS-2 / MS-3 / MS-5 / MS-11 | 契约里的 **16 种事件全部有发出点**：`hunt_start`、`hunt_end`、`tool_call`、`tool_result`、`assistant_text`、`usage`、`heartbeat`、`error`、`policy_denied`、`deliverable`、`degraded`、`needs_input`、`assumption`、`check_result`、`gate_config_changed`（MS-5）、`config_snapshot`（MS-3）、`context_compacted`（MS-11）。两侧一致性由 `docs-refcheck.py` 的「事件契约 vs 实现」体检守住（多一个、少一个都会被报出） |
| <a id="usage-6-fields"></a>usage§6·结果文件字段 | 已接线 | MS-2 / MS-5 / MS-6 / MS-7 | 已接线：`needs` / `assumptions`（2026-09-23）、`effective_config`（2026-09-23）、`summary`（2026-09-23）、`unverified`（2026-09-23）、`session_delta`（2026-09-24，MS-7——**仅恢复时出现**，随交付事实定型、`omitempty`）。`gates`（2026-09-24，MS-5——以**清单**为准列出未运行项，`passed` 三态）。**已无未接线字段** |

### 2.3 已结清（本周期）

> 保留删除线，显示「缺口 → 已结清」的轨迹。

| 已结清项 | 结清内容 |
|---|---|
| ~~事件信封四字段（IA-5.7）~~ | **已落地**：出口统一盖章（`type` / `bounty_id` / `trace_id` / `ts`） |
| ~~写盘与执行器级用例（IA-3.3、IA-3.6）~~ | **已落地**：`hunt/commit_test.go`（未读即写、读后过期、只改目标区间、越界、新建形态、台账更新、批量先校验后写） |
| ~~绑定层用例（IA-3.9）~~ | **已落地**：`hunt/bind_test.go`（槽位映射、空参数、五类形状不成立、不判断名字、往返、可重试性） |
| ~~事件写入失败（IA-5.3 / AC-19）~~ | **已落地**：断管（读端已关的管道）→ 退出 1 并记原因（`TestEndToEnd_BrokenEventChannelIsEnvError`） |
| ~~装配层端到端（IA-12.6）~~ | **已落地**：`TestEndToEnd_LocalRunProducesDeliveryCommit`（真 git 夹具 + 本地假上游，不联网） |
| ~~缓存用量与输入口径统一~~ | **已落地**：`InputTokens` = 全部输入（含缓存读/写）、`CachedInputTokens` = 其中从缓存读取的部分（子集）、`OutputTokens` = 全部生成（输出侧无缓存）；`llm.Usage` 加字段、`harness` 累加、三协议各自翻译（Messages 三项相加）、结果文件追加 `cached_input_tokens`。用例：`TestInfer_ReportsCachedInputTokens`（两协议）、`TestInfer_InputIncludesCacheReadAndCreation`（Messages）、`TestEndToEnd_LocalRunProducesDeliveryCommit`。剩：`CacheWriteInputTokens` 预留位、`usage` 事件载荷（属 MS-2） |
| ~~`find` 落地~~（MS-1） | **已落地**（2026-09-23）：`hunt/basic/find.go`，复用 `List` ＋ `Read`，命中带「文件 ∶ 行号 ∶ 该行」；枚举面与 `glob` 完全一致；「未找到」是结论而非错误；超 100 处只显示前 100 处但总量照报；单行限宽 160；跳过的大文件（>2MB）与读取失败一律如实附注。用例 6 条（`TestFind_*`） |
| ~~`edit.literal` 进 schema `required`~~（MS-1） | **已落地**（2026-09-23）：`TestEdit_DeclShape` 逐项断言 `path` / `literal` / `content`；消除 §1.4 的「已知不一致」 |
| ~~产品 §6「已知不一致」条目~~（MS-1） | **已随修正删除**；产品 §6 一期状态改为「基础 5 个已实现」，并说明 `find` 与 `glob` 共用同一条枚举面 |
| ~~工具面自洽校验（架构 §7.3）~~ | **已落地**（2026-09-23）：`Session.buildTools` 校验名字非空 / 不重复 / 实现非 nil / 工厂不得自带 `checkpoint`，失败即装配期缺件（退出码 1，与缺 git / 缺策略同一出口）；用例 6 条（`TestBuildTools_*`、`TestPrepare_FailsOnBrokenToolFace`）。同时收敛架构 §7.3 的承诺措辞——删去**不可校验**的「声明名一致」（`Primitive` 没有自述名，名字的唯一来源就是 `Decl().Name`）；并修正 `hunt/hooks.go` 里「装配缺件退出码 2」的陈旧注释（事实为 1） |
| ~~`check.py --wsl` 在映射盘工作区不可用~~（MS-1） | **已落地**（2026-09-23）：`scripts/check.py` 内建 WSL 路径映射（映射盘 → `/work`）、`shquote` 写保护、cwd 移出映射盘、过滤 PATH 中不可翻译条目；实测 `python scripts/check.py --wsl --race` → `exit 0`。消除 §1.4 的「已知不一致」 |
| ~~模型声明终态（澄清回路采集）~~ | **已落地**（2026-09-23）：`hunt/declare.go` 解析正文固定小节（`## 需要补全` / `## 假设`），`OnTurn` 逐轮登记、`Finalize` 只在引擎给出 `succeeded` 时收敛为 `blocked`（退出码 0；机制性终止不被改写）；事件 `needs_input` / `assumption` 逐条发；结果文件新增 `needs` / `assumptions`（三态，未提供为 `null`）。消除 §5 的「假设外化机制尚未定」与 §2.1 FR-6.3 的待接入 |
| ~~写盘性质跨包镜像表~~ | **已落地**（2026-09-23）：`Primitive` 加 `Writes() bool`、`Call` 加由执行体回填的 `Writes`，删掉 `internal/policy` 的 `passPrimitives` / `writePrimitives` 两张表与 `default: deny`。消掉三件事：① 与「原语自己决定寻址、降级，不看性质表」的原则自相矛盾；② 新增写原语要改两个包，漏改会被误报成「未识别的原语」；③ `internal/policy` 对 `hunt/basic` / `hunt/gate` / `hunt/symbolic` 的**分层倒挂**（三个 import 已删）。**「未知原语默认拒绝」这条防线换了位置**（执行体查表处的 `unknown_tool`，本就在 `Decide` 之前），架构 §7.4 / §12.4 口径同步 |
| ~~生效配置快照~~（MS-2） | **已落地**（2026-09-23）：`Prepare` 装配完成后把「本次实际生效的规则」冻结成快照，进 `hunt_start` 事件与结果文件 `effective_config`——原语清单与顺序（含殿后 `checkpoint`）、两段插件名与顺序、结果过滤器链名、策略口径、三重预算、检查点行为、扩展能力指纹、目标平台。快照是**只读事实、模型不可影响**，同时服务远程诊断与 MR 评审；`Prepare` 失败不发起飞事件、结果文件省略该字段（不摆空壳）。配套：`PromptPlugin` 加自述名 `Name()`、结果过滤器具名化为 `NamedFilter`。用例：`TestEffectiveConfig_ListsPrimitivesInToolFaceOrder`、`TestEffectiveConfig_CarriesPluginAndFilterNames`、`TestEffectiveConfig_CarriesAssemblyFacts`、`TestPrepare_EmitsHuntStart`（`hunt`）、`TestResultFile_EffectiveConfigIsWritten`（`cmd/xhunter`） |
| ~~轮级事件补齐（`tool_call` / `usage` 增量 / `assistant_text` / 结果文件 `summary`）~~（MS-2） | **已落地**（2026-09-23）：`tool_call` 在**执行前**发（`call_id` / `tool`（模型原始名）/ `args`；参数非法时退化成字符串，绝不因它让通道报错）；`usage` 每轮末发**增量**（增量在用量水位那一处算出、**上报与计费同源**；三字段全零不发——上游沉默不得报 0 装作有数）；`assistant_text` 在收流合并后发，与进历史、自陈解析**同一份**正文；结果文件加 `summary`（模型最后一轮答复，无答复则省略该键）。用例：`TestOnTurn_EmitsToolCallBeforeResult`、`TestOnTurn_ToolCallArgsSurviveMalformedJSON`、`TestOnTurn_EmitsUsageDeltaPerTurn`、`TestOnTurn_NoUsageEventWhenUpstreamSilent`、`TestOnTurn_EmitsAssistantTextMatchingHistory`（`hunt`）、`TestResultFile_SummaryIsWritten`（`cmd/xhunter`），并扩 `TestEndToEnd_LocalRunProducesDeliveryCommit` |
| ~~终态可观测（`hunt_end` 累计用量 ＋ `usage.reported` ＋ `error` 事件）~~（MS-2） | **已落地**（2026-09-23）：用量对外口径上收为 `hunt.UsageReport`（含 `reported`），`hunt_end` 带累计用量、与结果文件 `usage` **同一份**；`Reported` 的判据只在 `Session.charge`（本轮增量有任一非零）；不可得时发一条 `degraded`（`scope: usage`）且结果文件 `usage.reported: false`；终态 `failed` 发结构化 `error`（`kind` / `retryable` / `context`），`kind` 与 `retryable` 口径上收为 `hunt.ErrorKind` / `hunt.RetryableForExitCode`（事件与结果文件同源），取消与 `blocked` 不发。用例：`TestHuntEnd_CarriesCumulativeUsage`、`TestHuntEnd_IsTheLastEvent`、`TestFinalize_EmitsErrorOnFailureMatchingExitCode`、`TestFinalize_NoErrorEventOnBlockedOrCancelled`、`TestFinalize_DegradedOnceWhenUsageUnavailable`（`hunt`）、`TestResultFile_UsageReportedFalseWhenUpstreamSilent`（`cmd/xhunter`），并扩 `TestEndToEnd_ResultFileWrittenOnFailure` / `TestEndToEnd_BudgetExhaustionStopsTheRun` |
| ~~心跳接入~~（MS-2） | **已落地**（2026-09-23）：任务级心跳由 `Session` 掌管——`Prepare` 末尾启动、`Finalize` 的终态事件块之前停止（`hunt/heartbeat.go` 的 `startHeartbeat`），`stop` 幂等且阻塞到心跳 goroutine 退出，因此 `hunt_end` 仍是最后一条事件（时钟不会滴答到收尾之后）。间隔缺省 30s、`XHUNTER_HEARTBEAT_INTERVAL` 可覆盖（非法即退出 1）、随 ctx 取消即停（INV-8）；阶段由并发安全的 `Session.Phase()` 提供（bootstrap/assemble/infer/tools/finalize）。用例 `TestHeartbeat_EmitsAtInterval`、`TestHeartbeat_StopsOnContextCancel`、`TestHeartbeat_StopIsIdempotent`、`TestSession_PhaseTracksStages`、`TestFinalize_StopsHeartbeatBeforeHuntEnd`（`hunt`）、`TestParseHeartbeatInterval_DefaultOverrideAndRejects`、`TestEndToEnd_HeartbeatEmittedAndHuntEndLast`（`cmd/xhunter`） |
| ~~事件通道健康复查~~（MS-2） | **已落地**（2026-09-23）：`EventSink` 加自述健康状态 `Failed()`（覆盖事件与心跳两条写路径），`OnTurn` 轮前（L2）与轮末（L6）复查出口断线即收敛 `event_channel_failed`（退出 1）——早停，不跑完剩余轮次；取消优先；进程末尾 `sink.Failed()` 收口保留为兜底。用例 `TestOnTurn_StopsBeforeWorkWhenChannelAlreadyFailed`、`TestOnTurn_StopsAfterTurnWhenChannelFailsDuringTurn`、`TestOnTurn_ChannelFailureDoesNotOverrideCancellation`（`hunt`）、`TestEventSink_FailedCoversHeartbeatWrites`（`cmd/xhunter`），并扩 `TestEndToEnd_BrokenEventChannelIsEnvError` |
| ~~`unverified` 采集（FR-6.4 第三类）~~（MS-2） | **已落地**（2026-09-23）：新增固定小节 `## 未验证`（小节名的唯一来源是常量，解析与内核条款同改）；`Declared` 加 `Unverified`、`sectionTarget` 加第三分支、`AppendDeclared` 一并登记；结果文件加 `unverified`（三态，未提供 → `null`）。**只陈述、不判定**——它不改变终态（只有 `needs` 非空才收敛 `blocked`）。用例 `TestParseDeclared_ThreeSectionsAreSeparated`、`TestParseDeclared_UnverifiedAloneIsNotNeeds`、`TestParseDeclared_UnverifiedThreeState`、`TestFinalize_UnverifiedDoesNotBlock`（`hunt`）、`TestResultFile_UnverifiedIsThreeState`（`cmd/xhunter`） |
| ~~MS-2 收口验收（纯 NDJSON ＋ 事件序列 ＋ 增量语义 ＋ 插件失败）~~ | **已落地**（2026-09-23）：`TestEventSink_StdoutIsPureNDJSON`（stdout 逐行合法 JSON ＋ 信封四字段 ＋ `ts` RFC3339）、`TestEndToEnd_EventSequenceIsComplete`（首尾、`tool_call`↔`tool_result` 配对、`deliverable` 在 `hunt_end` 前）、`TestPolicy_ChargeReceivesIncrements`（`Policy.Charge` 收增量、非增长轮不上报）、`TestPrepare_PluginFailureConvergesAsEnvError`（插件失败 → `prepare_failed`/退出 1）。MS-2 由此收口 |
| ~~结构判据三态与"不可判定"如实上报~~（MS-4） | **已落地**（2026-09-23）：`structuralPoint() bool`（恒 false）换成三态 `structuralVerdict`（pass/fail/undecidable）；默认实现是 **undecidable**（符号扩展未接入——我们没**判过**，不是"没通过"），日志如实说「结构判据不可判定」、并发一条 `degraded`（`scope: checkpoint`，每次运行最多一条）；pass 照常提交、fail 说「未落在结构完整点」。判据当时留了**包内可替换位置**（`Session.structuralJudge`，未加公开配置字段）——**已于 2026-09-25 随 MS-9 撤掉**，换成真判据 `Session.judgeStructural`（见 §2.3）。用例 `TestCheckpoint_NoAutoCheckpointOffStructuralPoint`、`TestCheckpoint_StructuralFailDoesNotCommit`、`TestCheckpoint_StructuralPassCommits`、`TestCheckpoint_UndecidableReportsDegradedOnce` |
| ~~检查点连败上限~~（MS-4 / FR-1.3b / IA-11.9 / AC-22） | **已落地**（2026-09-23）：`Session` 记**连续**提交失败数（成功即归零，含 `Created=false` 的空操作；只统计阶段性检查点提交，交付提交不计入）；达包内常量 `checkpointFailStreakLimit=3` 时由 `OnTurn` 守卫统一收敛为 `checkpoint_failed_streak`（退出 1），本轮结束即收敛、不跑完剩余轮次。守卫次序固定为「取消 → 通道 → 提交连败 → 预算」。用例 `TestCheckpoint_StreakLimitConvergesAsEnvError`、`TestCheckpoint_SingleFailureSelfHeals`、`TestCheckpoint_SuccessResetsStreak` |
| ~~检查点时序、不可见性与意图兑现断言~~（MS-4 / IA-11.8 / IA-11.10 / IA-11.13） | **已落地**（2026-09-23）：IA-11.8（`TestWiring_PrepareBaselineRunsBeforeAnyTool`、`TestCheckpoint_CommitOnlyAtTurnBoundaryAndFinalize`、`TestDefaultTools_HasNoGitPrimitive`——git 不可见性用类型级证据）、IA-11.10（`TestCheckpoint_CommitsOnlyThisTurnsChanges`）、IA-11.13（`TestCheckpoint_ModelRequestIsConsumedOnceAndSkipsEmptyCommit`、`TestCheckpointMessage_Composition`）。MS-4 由此收口 |
| ~~会话材料落盘 ＋ Recorder 接口一次加齐~~（MS-6） | **已落地**（2026-09-23 落盘，2026-09-24 随 MS-7 补齐恢复）：`hunt.SessionRecorder` 方法集**一次加齐**（`Open` / `RecordTurn` / `RecordOp` / `RecordUsage` / `Ops` / `Snapshot`）；`cmd/xhunter` 把材料落成 JSONL（`.xhunter/<session_id>/session.jsonl`，首行 meta 带 `schema_version`，追加写、周期 flush），中立的 `llm.Message`/`ToolCall`/`ToolResult` 与 `harness.Turn`/`hunt.WriteOp` 直接复用为记录形状（不另写平行 DTO）。用例 `TestSave_MaterialLandsUnderSessionDirWithSchemaVersion`、`TestMaterial_LoadRejectsUnknownSchemaVersion`（`cmd`）、`TestRecorder_RecordsOpsAndUsageFromTheSession`、`TestRecorder_OpenFailureDegradesWithoutFailingPrepare`、`TestSnapshot_FailureDoesNotBlockTheRun`（`hunt`）。**余的"恢复"已随 MS-7 落地** |
| ~~会话材料随提交强制加入~~（IA-6.1c） | **已落地**（2026-09-23）：`internal/git/cli` 的 `Commit` 在 `add -A` 之后对本次会话材料目录再 `add -f -- <MaterialDir>`——仓库忽略 `.xhunter/`（常见做法）时 `add -A` 不会加入它，材料会写在工作区却不进任何提交、"唯一状态源"悄悄失效且不报错；材料目录尚未落盘则**跳过**（不因它把整个提交判死）；`MaterialDir` 为空时行为完全不变（"无改动不产生空提交"照旧）。用例 `TestCommit_ForceAddsMaterialEvenWhenGitignored`、`TestCommit_WithoutMaterialDirIsUnchanged`、`TestCommit_MissingMaterialDirDoesNotFail`；`cmd` e2e 补断言"分支 tip 含材料文件、交付清单不含材料"。MS-6 由此收口 |
| ~~修复：会话材料缺末轮用量~~（快照与记账次序） | **已修复**（2026-09-23）：`OnTurn` 里 `snapshot()` 原排在 `charge()` **之前** → 本轮用量要等下一次快照才落盘；`Finalize` 的最终 `snapshot()` 原排在**交付提交之后** → 那份写在工作树里的记录进不了提交、还被 `Clean` 删掉。两处次序对调后，交付分支上的材料含**每轮各一条** usage。收紧 `TestEndToEnd_MaterialIsSelfSufficientForResume`（改断言"每轮各一条"，修复前实测**红**）、新增 `TestRecorder_SnapshotAfterCharge`（钉调用次序） |
| ~~契约：扩展接入位与 panic 哨兵~~（H7 / MS-8） | **契约已定义**（2026-09-23，未冻结期：先定契约后填实现）：`ext.ExtHost` 补 `Fingerprint() []string`（服务会话材料 `meta.ext` 与生效快照的能力指纹，IA-6.5）；新增 `ext.Unimplemented{}` **panic 哨兵**；`cmd/xhunter` 把它**显式装上**（不再传 `nil`——A 类入口，符号原语声明不实现，走不到）。**实现已随 MS-8 落地**（2026-09-25：`ext/syntax` 内置语法级 ＋ `ext/mcp` 外挂通道；`ext.Unimplemented{}` 的 panic 哨兵回收为如实降级）。**哨兵自此清零** |
| ~~契约：门禁清单来源 ＋ 结果文件 `gates` 形状~~（MS-5 / H4） | **已落地**（2026-09-24）：`hunt.GateSource`（`Load` ＋ `Parse`）＋ 档位常量 `bounty` / `repo` / `none` / `working_tree`；`hunt.Config.Gates` 与 `GateRunner` 两个装配槽；结果文件 `gateFile`（`passed` 三态 `*bool`）已接写入端（以**清单**为准列出未运行项）。护栏（元门禁 / 强度不得降低）落在 `hunt` 包，工作区豁免档与基线档共用同一份 |
| ~~两段式止损（MS-3）~~ | **已落地**（2026-09-24，MS-3）：`Policy.ObserveFailure` / `DeniedCount` 从 panic 哨兵回收为运行期如实判定——同类 = 同 `Fault.Kind`（空串 = 一次成功，归零；`policy_denied` 忽略），连续同类失败达 2 次换策略（回灌「换一种做法」提示、只影响下一轮）、超上限 3 次终止（`stop_loss_same_kind`）；连续拒绝在 `Decide` 计数、达 3 次终止（`stop_loss_denied`）。守卫次序「取消 → 通道 → 提交连败 → 止损 → 预算」由 `TestOnTurn_CommitStreakOutranksStopLoss` / `TestOnTurn_SameKindStopLossOutranksBudget` / `TestOnTurn_DeniedStreakOutranksBudget` 咬住。`config_snapshot` 与 `hunt_start` 同时点发出：两个止损阈值来自 `policy.Facts()`、两个机制硬顶由装配层从 `harness.DefaultConfig()` 注入。e2e：`TestEndToEnd_RepeatedFailureSwitchesBeforeFailing`（第 3 轮请求体含「换一种做法」）、`TestEndToEnd_DeniedStreakFailsTheRun`、`TestPrepare_EmitsConfigSnapshot` |
| ~~交付 diff/patch 排除会话材料目录~~（IA-11.6） | **已落地**（2026-09-23）：`git.RepoRef` 加 `MaterialDir`（本次运行的仓库事实）；`GitWorktree.Diff`/`Patch` 改收 `RepoRef`，材料目录非空时加 `:(exclude)` pathspec——**只排本次会话的材料目录**，`.xhunter/` 下其它路径（如 `skills.draft/**`）是交付内容、照进 diff。材料目录路径唯一来源 `materialDirFor`（与落盘同源）；e2e 的临时过滤 `nonMaterial` 撤掉、改成直接断言交付清单不含材料。用例 `TestDiff_ExcludesMaterialDirButKeepsSkillsDraft`、`TestDiff_NoMaterialDirKeepsEverything`、`TestEndToEnd_LocalRunProducesDeliveryCommit` |
| ~~一期移除符号操作（工具面口径变更）~~ | **已随 MS-8 反转**（2026-09-24）：装配层把 `symbol_read` / `symbol_edit` / `symbol_rename` 接回工具面（符号后端已接入）；**反转前**的工具面是 **6 + 1**（`read`/`write`/`edit`/`find`/`glob`/`check` ＋ 殿后 `checkpoint`；`check` 当时注册但返回 `not_implemented`，是刻意的不对称），**反转后为 9 + 1**。**「工具面恒定」由双轴改单轴**——环境能力不决定注册、同一构建内不增减，**随能力接入而扩展**；代价如实记：跨构建版本工具名集合会变，「前缀与 schema 稳定（缓存友好）」只在同一构建内成立。用例 `TestDefaultTools_FaceIsFixed`、`TestDefaultTools_FaceIsIdenticalWhetherTheBackendIsAvailable` |
| ~~流看门狗（FR-1.11① / L4-流看门狗）~~（MS-12） | **已落地**（2026-09-24）：`harness` 接收段用 `select` + `time.Timer` 落看门狗——相邻两事件之间（含首事件之前）静默超 `StreamIdleTimeout` 即收敛。默认值唯一来源 `defaultStreamIdleTimeout`（120s）；**0 = 取默认、负数 = 关闭看门狗、正数 = 上界**。**超时先落终态、再 `sess.Cancel()`**：返回时 `run.Terminal != nil`，结构上不可能落进 `no_tool_call`；取消优先于超时（两路同时就绪时超时分支再复查 ctx）。部署事实 `XHUNTER_STREAM_IDLE_TIMEOUT`（Go duration；未设取默认，非法即启动期退出 1；"关闭"只程序内可达）。生效值经装配层注入 `AssemblyFacts.StreamIdleTimeoutMS` 进 `config_snapshot.stream_idle_timeout_ms`（0 = 不限/关闭）。用例 `TestWatchdog_*`（7 条）、`TestDefaultConfig_StreamIdleTimeoutDefaultsTo120s`、`TestConfig_ZeroIdleTimeoutTakesDefaultNegativeStays`、`TestParseStreamIdleTimeout_DefaultOverrideAndRejects`、`TestConfigSnapshot_CarriesStreamIdleTimeout`、`TestEndToEnd_IdleStreamConvergesToEnvError` |
| ~~结构检查判据接入（FR-1.3d / IA-11.11 / L6-结构检查）~~（MS-9） | **已落地**（2026-09-25）：`Session.structuralJudge`（包内可替换位置）撤掉，换成真判据 `Session.judgeStructural`——判据①「此刻语法完整」在**判定时**取（`ext.ExtHost.Parse`），判据②「改动封闭在某个符号内」在**写盘前**就地取、记在与 `s.ops` 平行的 `opEnclose`（字节区间只有在它产生的那一瞬才与文件内容对齐，事后重问会被同文件后续编辑平移）。取样范围是**尚未提交的改动**（`pendingFrom` 起点，一次成功提交即推进——否则一次早年的越界编辑会永久关掉自动检查点）；整文件写入（含新建）不适用判据②，由判据①单独覆盖。宿主由装配层经 `hunt.ExtHostFactory` 注入、与符号原语**共用同一份实例**（`hunt.ToolFactory` 随之加 `ext.ExtHost` 入参）。`ext` 契约加 `Parse` / `Enclose` 与三态 `ParseVerdict`；`ext/syntax` 用 `go/parser` 实现（未注册语言一律"判不了"，不谎报"不封闭"）。触发链补全：门禁通过 > 模型显式 > 结构检查通过 > 不提交。用例 `TestStructural_ParseOKFlipCommits`、`TestStructural_IncompleteSyntaxSuppresses`、`TestStructural_UndecidableDoesNotCommit`、`TestStructural_EditOutsideAnySymbolSuppresses`、`TestStructural_WholeFileWriteIsNotJudgedAsUnenclosed`、`TestStructural_CommittedOpsLeaveTheJudgementWindow`、`TestStructural_ParseIsAskedOncePerFile`、`TestStructural_UndecidableDoesNotBlockWriting`（`hunt`）、`TestHost_Parse*` / `TestHost_Enclose*`（`ext/syntax`，10 条）、`TestEndToEnd_CheckpointLandsOnCompleteSyntax`、`TestEndToEnd_IncompleteSyntaxSuppressesCheckpoint`（`cmd/xhunter`）。MS-9 由此收口 |
| ~~上下文压缩（FR-14 / AC-17 / AC-18 / IA-2.5~2.8 / L2-压缩）~~（MS-11） | **已落地**（2026-09-25）：压缩落在 `cmd/xhunter/context.go` 的 `Assemble` 内——三档水位（预警 70% / 目标 50% / 硬上限 90%，基数＝**可用输入预算**，由 `providerconfig.Resolved.Watermarks` 算出）＋ 冷却 3 轮；命中即按 **L0→L3** 按序下压、够用即停，且**当前轮永不压**（它正被模型用来接着说，压它等于抽掉脚下的地板）。L0 工具输出两头各留 512 字符并标注丢弃量、L1 丢推理过程、L2 换成**可寻址摘要**（丢内容不丢地址）、L3 折叠为**结构化工作日志**（由会话记录**投影**：已完成改动／失败尝试与原因／未决问题与假设，零模型调用、可复现）。释放量是**同一把尺子的前后差**（不另估一次；可以为负——折叠日志本身占一点，如实报、不夹到 0，否则事件与前后差就对不上）。**一层都没压动时不上冷却**：只有当前轮时无从压，冷却若在那时上膛，下一轮第一个可压的老轮会被自己的冷却挡住（该压的时候压不动）。结论经 `hunt.Compaction` ＋ **可选**上报面 `CompactionReporter.TakeCompactions()` 搬到 Session 的事件出口（取走即清空，一次下压一条），发 `context_compacted`（`level`/`released_tokens`/`watermark`）。**压完仍不低于硬上限则终止**：按预算耗尽处理（`budget_exhausted:context`，退出 2、不重派，usage§7「上下文达硬上限」）——撞硬上限本身不判错（照样一路压到目标），判据问的是**压完之后**的水位，压得下来的超窗不该被误杀。**L4 模型摘要不实现**：它默认就不启用（FR-14.4 确定性优先），而「生成一次即落盘、恢复时读回」要动材料与恢复两条链路——宁可留缺口，也不做一个会在恢复路径上重新生成的版本（留 §5）。用例 `TestCompaction_*`（`cmd/xhunter` 15 条）、`TestCompaction_*`/`TestDeliverables_DoNotDependOnContext`（`hunt` 4 条）、`TestEndToEnd_OverWindowCompactsAndStillConverges`（窗口调小到 6000、两次读 20k 字符文件触发压缩，压完照常收敛且交付提交与改动清单不缺）。MS-11 由此收口 |
| ~~外挂符号后端通道（MCP over stdio）~~（MS-8 / FR-13.1/13.5/13.6 / IA-7.2~7.6 / H7） | **已落地**（2026-09-25）：新增 `ext/mcp`——传输与握手是 **MCP 标准**（stdio 行分隔 JSON-RPC 2.0，`initialize` ＋ `notifications/initialized`），符号能力方法走**自有命名空间** `xhunter/{capabilities,locate,parse,enclose}`（MCP 的标准面回答"有哪些工具"，而我们要的是"文件 ＋ 字节区间"；能力方法不进 `tools/*`，因此与 FR-13.2「扩展不新增工具名」不冲突）。四种失败各有钉法：进程退了／握手不上／超时／输出不是 JSON——一律**钉成终态**（只记第一次原因）并杀掉进程组，绝不让主循环挂住。装配层 `chooseExt` 按部署事实选后端（`XHUNTER_EXT_COMMAND` 非空即用外挂，否则内置语法级），**宿主工厂与能力指纹同源**（换后端一定体现在 `effective_config.ext`），且**选的过程不启动进程**（懒启动）。回收接在 `Finalize` 末尾、排在 `hunt_end` **之后**（回收可能超时，不该把终态一起吞掉）。用例 `TestHost_*`（`ext/mcp` 11 条）、`TestChooseExt_*`/`TestParseExtConfig_*`/`TestAssemblyFacts_ReportsTheAssembledBackend`/`TestEndToEnd_ExternalBackendUnavailableStillConverges`（`cmd/xhunter`）、`TestFinalize_ClosesTheExtHost`/`TestFinalize_WithoutExtHostDoesNotPanic`（`hunt`）。能力指纹由此进会话材料 `meta.ext`（与 `effective_config.ext` 同源；没有符号能力时是空数组），续跑时不一致只记一条日志、不阻断（`TestPrepare_ExtFingerprintChangeIsRecordedNotFatal`）。MS-8 由此收口 |
 **已落地**（2026-09-24）：新增 `xhunter run --repo <path> --task <text> [--out <path>]`——只读探测（`internal/git/cli.ProbeLocalRepo`，仅 `rev-parse`/`remote`/`cat-file -e`，绝不 checkout/add/commit/fetch/push；门禁候选从基线 commit 读 `HEAD:gates.yml`）→ 组装 Bounty（分支/材料目录/预算复用既有唯一来源 `branchFor`/`materialDirFor`/`parseBudget`）→ **经同一条 `executeHunt`** 执行一次 Hunt。从 `huntCmd` 抽出 `executeHunt` 后两条驱动路径共用一份装配（零行为变化，既有 e2e 全绿）。用例 `TestRunCmd_ClonesIntoIsolatedWorkspace`、`TestRunCmd_ProbeIsReadOnly`、四类启动期失败、`TestEndToEnd_LocalRunDrivesBountyFromRepo` |

---

## 3. 尚未覆盖的验收（待补）—— 由 §2 索引派生

> 本节保留原架构 §14 的标题以**兼容既有引用**；**内容由 §2 索引派生**，不再维护第二张并行缺口表——唯一事实清单是 §2。下面的「未覆盖项」= §2 中状态为「待接入 / 待补 / 部分待接入 / 部分待补」的行。

### 3.1 实现未落地（对应原 §14.1）

| 主题 | §2 索引键 |
|---|---|
| 压缩（仅余 L4 模型摘要） | `IA-2.7`、`FR-14.1` |

### 3.2 判定方式缺用例（对应原 §14.2）

| 主题 | §2 索引键 |
|---|---|
| 进程级信号（其余信号形态） | `IA-12.8` |
| 凭据静态扫描 | `IA-6.8`、`IA-8.4`、`AC-8` |
| 嵌套约定附注 | `IA-2.10` |
| 「唯一来源」的重复实现（如另写一份 `branchFor`） | 静态扫描/人工审查——**无法用行为用例检出**（重复代码行为等价，同 AC-8 / IA-8.4 / IA-6.8 的静态扫描类） |
| 看门狗「负数关闭」的行为层区分力 | `TestWatchdog_DisabledWhenNegative` 在行为层**分不清"关闭"与"启用且超时极大"**（150ms 内结束的流下两者都通过）；真正的区分点在配置层，已由 `TestConfig_ZeroIdleTimeoutTakesDefaultNegativeStays` 咬住 |
| 看门狗用例对"计时器挪到首事件之后"的健壮性 | 该假设性破坏下 `TestWatchdog_IdleStreamCancelsAsEnvError` 会**挂住**（Go 测试超时兜底）而非快速失败——属**测试健壮性**、非产品缺陷；canary `TestWatchdog_WaitsForFirstEvent` 会快速失败。**不在断言里做取舍来规避它**（那会改掉被测语义） |

---

## 4. 分期与里程碑（排期）

### 4.1 产品分期（M1/M1.5/M2/M3）范围与现状

> 范围来自产品设计 §8（**范围即规则，保留在设计文档**；此处只记录**各分期的现状**）。

| 阶段 | 范围（设计文档 §8） | 现状 |
|---|---|---|
| **M1 最小可用** | 进程契约、git 基线获取与**任务分支 + 阶段性检查点 + 交付推送**、**基础原语 5 个**（`read`/`write`/`edit`/`find`/`glob`）、**工具面定格为 6 + 1**（三个符号原语本期**不注册**、随 MS-8 接入；`check` 本期注册、返回 `not_implemented`）、内置提示词、整体 diff 交付、策略与预算、事件流、**会话材料随检查点提交**。目标平台 linux/amd64 | **已落地**（主干已跑通，见 §1.1；**会话材料随检查点提交**已随 MS-6 落地）。**「工具面定格为 6 + 1」是 M1 当时的范围**：符号三原语已随 MS-8 / MS-10 注册，`check` 已随 MS-5 实现，当前工具面是 9 + 1 |
| **M1.5 会话恢复** | resume：checkout 任务分支 tip + 读回会话材料 + 回灌上下文继续（不重放写操作）；恢复失败 fallback 重跑 | **已落地**（MS-6 → MS-7） |
| **M2 读精度与符号替换** | MCP 扩展接入框架 + 首个符号扩展（`symbol_read`/`symbol_edit`，首发语言 Go） | **已落地**（MS-8 / MS-9：外挂通道 `ext/mcp` ＋ 内置语法级后端 `ext/syntax`，符号读写与结构检查落地） |
| **M3 手术能力与校验** | `symbol_rename`（含引用解析）、改动规模上报、扩充扩展语言覆盖 | **已落地**（MS-10：语法级一次改完声明与全部出现点，规模与编辑计划同源、未穷尽如实标注）。**扩充扩展语言覆盖**是持续项（新增语言只动后端，不改上层契约） |

### 4.2 里程碑总览与依赖

> 编号是**执行顺序**，与产品分期的 `M1 / M1.5 / M2 / M3` 不是同一套编号；下表给出映射。

| # | 名称 | 对应分期 | 依赖 | 体量 | 主要 FR / IA | 状态 |
|---|---|---|---|---|---|---|
| **MS-1** | 验收入口与契约对齐 | 一期收口 | — | 小 | FR-2.2、IA-3.16 | **已完成**（2026-09-23；三项全部落地，见 §2.3） |
| **MS-2** | 运行可观测补齐（事件流 ＋ 生效配置快照） | 一期收口 | MS-1 | 中 | FR-10、FR-11.1/11.6、FR-6.3/6.4、IA-5.2/5.4 | **已完成**（2026-09-23；事件流、生效配置快照、澄清回路三类自陈与收口验收（纯 NDJSON／事件序列／增量语义／插件失败）全部落地，见 §2.3） |
| **MS-3** | 止损完备（两段式止损） | 一期收口 | MS-2 | 中 | FR-9.1/9.4、IA-4.8/4.9 | **已完成**（2026-09-24；两段式止损两轴（同类失败 2/3、连续拒绝 3）、守卫次序、`config_snapshot` 落地，见 §4.3 与 §2.3） |
| **MS-4** | 检查点分档与提交健壮性 | 一期收口 | MS-1 | 中 | FR-1.3b/1.3c/1.11②、IA-11.8/11.10/11.11/11.13 | **已完成**（2026-09-23；结构判据三态、连败上限、时序/不可见性/意图兑现断言全部落地，见 §2.3） |
| **MS-5** | 门禁落地（`check` 实现 ＋ 全链护栏） | 一期收口 | MS-4 | 大 | FR-5.2b~5.2i、IA-11.12 | **已完成**（2026-09-24；来源裁决、`check` 执行器与判据、收尾补跑、终态与检查点联动、事件与结果文件全部落地，见 §4.3 与 §2.3） |
| **MS-6** | 会话材料落盘 | M1.5 前置 | MS-4 | 中 | FR-12.2/12.2b/12.3、IA-6.1/6.1b/6.1c | **已完成**（2026-09-23；材料落盘、Recorder 接口一次加齐、交付 diff/patch 排除材料目录、随检查点 `add -f` 全部落地，见 §2.3） |
| **MS-7** | 会话恢复（resume） | M1.5 | MS-6 | 大 | FR-12.1/12.6、AC-7、IA-6.2/6.3/6.4 | **已完成**（2026-09-24；`Load` 真读回、`Prepare` 纯读先于 `Open`、回灌上下文＋新条件追加、`session_delta` 接线、token 预算续算，见 §4.3） |
| **MS-8** | 扩展接入与符号读写 | M2 | MS-2 | 大 | FR-13、FR-4.1/4.2/4.4~4.8/4.13、IA-7.1~7.6 | **已完成**（2026-09-25：外挂通道 `ext/mcp` 落地并接入装配层，见 §2.3 与 §4.3；含能力指纹进会话材料 `meta.ext`，`IA-6.5`） |
| **MS-9** | 结构检查与 `on_structure` 默认档 | M2 收尾 | MS-5、MS-8 | 中 | FR-1.3d、IA-11.12 | **已完成**（2026-09-25；真判据接 `ext.ExtHost` 的 `Parse` / `Enclose`、取样时点与窗口定死、触发链补全，见 §2.3 与 §4.3） |
| **MS-10** | `symbol_rename`（跨文件重命名）与规模上报 | M3 | MS-8 | 中 | FR-4.3、FR-6.2 | **已完成**（2026-09-24；语法级一次改完声明与全部出现点，规模与编辑计划同源，未穷尽如实标注，见 §4.3 与 §2.3） |
| **MS-11** | 上下文压缩 | 二期 | MS-6、MS-7 | 大 | FR-14 全部、AC-17/AC-18 | **已完成**（2026-09-25；水位＋冷却、分层下压 L0→L3、`context_compacted`、投影式工作日志、保丢优先级与交付物不依赖上下文全部落地；**仅余 L4 模型摘要**，见 §2.3 与 §4.3） |
| **MS-12** | 运行段看护与本地驱动 | 二期 | MS-3、MS-2 | 中 | FR-1.11①、FR-1.10 | **已完成**（2026-09-24；流看门狗与本地驱动落地，见 §4.3 与 §2.3） |

**依赖与车道**：

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

### 4.3 逐个里程碑 MS-1 ~ MS-12

#### MS-1 验收入口与契约对齐

**目标**：让「规范验证入口」在这台机器上能用，并让代码与已声明的契约一致（三处小账一次结清）。

**状态**：**三项全部完成，本里程碑可收口**（2026-09-23，见 §2.3）——`find` 落地、`edit.literal` 进 `required`、`check.py` 的 `--wsl` 修法。

**范围**
- 修 `scripts/check.py` 的 `--wsl` 分支：工作目录按 WSL 视角给出（映射盘/UNC 下不能把 Windows 路径原样拼进 bash），使 `check.py --wsl --race` 一条命令可用（**已完成**，见 §2.3）。
- `find` 落地（**已完成**，见 §2.3）。
- `edit` 的 `literal` 进 schema `required`（**已完成**，见 §2.3）。
- 文档口径同步（**已完成**）。

**独立验收的证据**
- `python scripts/check.py --wsl --race` → `exit 0`（本里程碑第一验收项，也是后续所有里程碑的 DoD 前置）。
- `hunt/basic/find_test.go`（**已完成**）：`TestFind_MatchesWithLineNumbers`、`TestFind_NoMatchIsSuccessWithExplicitText`、`TestFind_ScopeLimitsSearch`、`TestFind_PathLimitsToSingleFile`、`TestFind_TruncatesHitsButReportsTotal`、`TestFind_RequiresLiteral`、`TestFind_DeclShape`。
- `TestEdit_DeclShape` 断言 `literal` ∈ schema `required`。

**依赖**：无。**不做**：不动 `symbolic`/`gate` 的 `not_implemented` 行为。

**风险**：`check.py` 的修法要避免把「本机路径映射」写成产品文档口径——映射知识只留在脚本里。

---

#### MS-2 运行可观测补齐（事件流 ＋ 生效配置快照）

**目标**：把使用手册 §5 里「契约目标」的事件补齐，让平台侧能看见「开始了／还活着／花了多少／怎么死的」，并把本次运行的装配快照落进 `hunt_start` 与结果文件。

**范围**
- `hunt_start`：`Prepare` 成功后立即发（含任务与仓库事实），并按 FR-11.6 带上**生效配置快照**：原语清单与顺序、两段插件名与顺序、结果过滤器链、策略配置、三重预算上限、检查点策略、扩展能力指纹（当前为空）、目标平台。
- `heartbeat`：接上已有实现（`cmd/xhunter/sink.go:80`），运行期按固定间隔发，携带 `phase` 与 `elapsed_ms`；定时器随 `ctx` 取消立即停（INV-8）。
- `usage`：**每轮末发增量**，**`hunt_end` 带累计**。
- `assistant_text`：收流合并后**必发**，与 `ContextBuilder` 历史一致、不得改写；同一份正文进结果文件 `summary`。
- `tool_call`：执行前发（`call_id` / `tool` / `args`）。
- `error`：各阶段失败的结构化事件，带 `retryable`。
- 结果文件补 `effective_config`。
- 澄清回路的三件产物（FR-6.3）：终态新增 `StatusBlocked`；事件 `needs_input`；结果文件 `needs` 与 `assumptions` / `unverified`。解析对象是正文里的固定小节（`## 需要补全` / `## 假设` / `## 未验证`），只做切行去前缀；**在轮边界登记一次**（`AppendDeclared`，收尾只读累积结果——压缩接入后下压掉的轮次里就捞不回来，所以要在说出的当轮登记）；三态：未提供 → `null`（不写 `[]`）。另加 `usage.reported` 与 `degraded`（`scope: usage`）。

**独立验收的证据**
- 事件序列：`TestEndToEnd_EventSequenceIsComplete`（`hunt_start` 首、`hunt_end` 尾、`tool_call`↔`tool_result` 配对、`deliverable` 在 `hunt_end` 前）。
- stdout 纯 NDJSON：`TestEventSink_StdoutIsPureNDJSON`、`TestHuntCmd_EventsGoToStdoutAndLogsGoToStderr`。
- 心跳：`TestHeartbeat_EmitsAtInterval`、`TestHeartbeat_StopsOnContextCancel`、`TestHeartbeat_StopIsIdempotent`、`TestSession_PhaseTracksStages`、`TestFinalize_StopsHeartbeatBeforeHuntEnd`、`TestEndToEnd_HeartbeatEmittedAndHuntEndLast`。
- 结果文件断言：`TestResultFile_EffectiveConfigIsWritten`（`effective_config` 含原语清单顺序）、`TestResultFile_SummaryIsWritten`、`TestResultFile_UsageReportedFalseWhenUpstreamSilent`。
- 澄清回路：`TestParseDeclared_SectionsAndEntries`、`TestParseDeclared_AbsentSectionIsNilNotEmpty`、`TestParseDeclared_ThreeSectionsAreSeparated`、`TestParseDeclared_UnverifiedAloneIsNotNeeds`、`TestParseDeclared_UnverifiedThreeState`、`TestFinalize_NeedsInputConvergesToBlocked`、`TestFinalize_UnverifiedDoesNotBlock`、`TestResultFile_DeclarationsAreThreeState`、`TestResultFile_UnverifiedIsThreeState`。
- 增量语义：`TestPolicy_ChargeReceivesIncrements`（`Policy.Charge` 收增量、非增长轮不上报）。
- 插件失败收敛：`TestPrepare_PluginFailureConvergesAsEnvError`（`prepare_failed` / 退出 1）。

**依赖**：MS-1。**不做**：压缩事件（MS-11）、门禁事件（MS-5）。

**风险**：`heartbeat` 与「stdout 写失败 = 环境错误」的关系要一次说清。

---

#### MS-3 止损完备（两段式）

**状态**：**已完成**——两段式止损两轴与守卫次序落地，`config_snapshot` 与起飞同时点发出。

**目标**：把 FR-9.4 的两段式止损补上——达到阈值先换策略，超过上限才失败；并把「连续策略拒绝」纳入终止条件。

**范围**
- 业务止损（`hunt.Policy` 扩展）：`ObserveFailure`（连续同类失败达 2 次 → 回灌「换策略」提示，不终止）、超上限 3 次 → 失败；`DeniedCount`（连续策略拒绝达 3 次 → 失败）。
- 与 harness 的分工写清：机制硬顶留在 `harness.Config`，业务止损在 `Policy`。
- 新终止条件进使用手册 §7 的映射表。

**独立验收的证据**
- 单测（`internal/policy`）：`TestObserveFailure_SwitchThenTerminate`、`TestObserveFailure_ResetsOnSuccessAndKind`、`TestObserveFailure_KindScopedAndDenialIgnored`、`TestDeniedCount_TerminatesAfterThreshold`、`TestDeniedStreakLimitIsThree`、`TestSameKindLimitsAreTwoAndThree`、`TestDecide_DeniedStreakResetsOnAllow`、`TestFacts_CarriesStopLossLimits`。
- 接线（`hunt`）：`TestExecuteCall_ObservesOutcomePerCall`、`TestOnTurn_SameKindStopLossOutranksBudget`、`TestOnTurn_DeniedStreakOutranksBudget`、`TestOnTurn_CommitStreakOutranksStopLoss`、`TestOnTurn_SwitchHintAppendedToNextTurnMessages`。
- e2e（`cmd/xhunter`）：`TestEndToEnd_RepeatedFailureSwitchesBeforeFailing`、`TestEndToEnd_DeniedStreakFailsTheRun`、`TestPrepare_EmitsConfigSnapshot`。
- IA-4.8 / IA-4.9 的「待接入」改成用例名。

**依赖**：MS-2。**不做**：影响面阈值（已决：不做，见 §7）。

**风险**：`hunt.Policy` 是公开契约，加方法属接口扩展；「同类失败」的判据要先用测试定死。

---

#### MS-4 检查点语义与提交健壮性

**目标**：把检查点收敛成「只在结构完整点上自动产生」（判据不可判定 → 不提交这一半已落地），并补齐连败收敛与提交语义的断言。

**范围**
- **判据接入位**：`Session.structuralJudge`（三态 `structuralVerdict`）默认返回 **undecidable**；保证**不可判定时确实不提交**、且跳过原因如实进日志与事件（`degraded`，`scope: checkpoint`，每次运行最多一条）。符号扩展接入时替换它。
- **连败上限**：检查点连续提交失败达 3 次 → 本轮结束即收敛（`checkpoint_failed_streak`，退出 1），不跑完剩余轮次；守卫次序「取消 → 通道 → 提交连败 → 预算」。（**已落地**）
- **时间语义与不可见性**：断言 `Commit` 只在轮边界与收尾被调用、`PrepareBaseline` 是 `Prepare` 第一步、工具面不含任何 git 原语（IA-11.8 / IA-11.10）。
- **意图兑现**：一次请求只兑现一次；无改动不产生空提交但意图照样消费；提交信息由执行体合成。

**独立验收的证据**
- 结构判据三态：`TestCheckpoint_NoAutoCheckpointOffStructuralPoint`（不可判定）、`TestCheckpoint_StructuralFailDoesNotCommit`（未通过）、`TestCheckpoint_StructuralPassCommits`（通过）、`TestCheckpoint_UndecidableReportsDegradedOnce`（降级每次运行最多一条）。
- `TestCheckpoint_LogsNoOpWhenNothingWasCommitted`（模型请求路径，已有）。
- 连败上限：`TestCheckpoint_StreakLimitConvergesAsEnvError`（真实引擎：连败 3 次即收敛、infer 次数==3）、`TestCheckpoint_SingleFailureSelfHeals`、`TestCheckpoint_SuccessResetsStreak`。
- 意图兑现与提交信息合成（IA-11.13）：`TestCheckpoint_ModelRequestIsConsumedOnceAndSkipsEmptyCommit`、`TestCheckpointMessage_Composition`、`TestSanitizeIntent`（净化，已有）。
- 时序与不可见性（IA-11.8 / IA-11.10）：`TestWiring_PrepareBaselineRunsBeforeAnyTool`、`TestCheckpoint_CommitOnlyAtTurnBoundaryAndFinalize`、`TestDefaultTools_HasNoGitPrimitive`、`TestCheckpoint_CommitsOnlyThisTurnsChanges`。

**依赖**：MS-1（真实判据随 MS-9）。**不做**：门禁驱动检查点（MS-5）。

**风险**：未注册语言的仓库只在收尾提交一次——刻意取舍；必须在生效快照与事件里如实标注「结构判据不可判定」。

---

#### MS-5 门禁落地（`check` 实现 ＋ 全链护栏）

**状态**：**已完成**（2026-09-24）。

**目标**：把 §7.8 从规格变成产品能力。这是本排期里最需要先做设计确认的两块之一（另一块是 MS-8）。

**范围**
- **清单来源裁决**（FR-5.2b）：Bounty 下发 > 基线 commit 的仓库根 `gates.yml` > 无。不限制模型修改该文件。（**契约已定义**（D2）：`hunt.GateSource` ＋ 档位常量 ＋ `Config.Gates` 装配槽。）
- **执行语义**：`argv` 数组直启、`argv[0]` 为 shell 一律拒绝、`timeout`（`context` ＋ 杀进程组）、`dir`、输出限长保留头尾、非交互。
- **环境隔离**（FR-5.2i）：最小集 ＋ 清单显式 `env`；Xhunter 自身的 git 凭据绝不下传。
- **判据 `expect`**：对象 `{kind*, pattern, max, stream}`；判据在全量输出上算。
- **两类失败分开**（FR-5.2d）：执行失败 = 环境错误（2）；判定不通过 = 质量结论；`required` 未通过或从未运行 → 终态失败（1），但改动照常交付（FR-6.5）。
- **结果缓存**（FR-5.2e）：按待提交改动的内容指纹缓存。
- **检查点联动**（FR-5.2c）：门禁通过 → 本轮必提交；未通过 → 抑制后续自动检查点。
- **收尾补跑**（FR-5.2f）。
- **上报**：`check_result` 事件；结果文件 `gates` 数组含未运行项（`passed: null`）；门禁名注入 user 段。
- **豁免护栏**（FR-5.2h）：元门禁 ＋ 强度不得降低 ＋ `gate_config_changed` 事件。
- 清空 `Prepare` 里的 `s.gates = nil`（来源裁决接上 `Config.Gates`）。

**独立验收的证据**
- 单测（`hunt/gate`）：`TestCheck_ArgvIsDirectAndRejectsShell`、`TestCheck_ExpectVariantsDecideTheVerdict`、`TestCheck_TimeoutIsEnvError`、`TestCheck_NonZeroExitIsQualityFailure`、`TestCheck_UnstartableCommandIsEnvError`、`TestCheck_CacheHitByFingerprint`、`TestCheck_EnvIsMinimalAndCredentialFree`、`TestCheck_PassedGateRequestsCheckpoint`、`TestCheck_FailedGateDoesNotRequestCheckpoint`、`TestCheck_UnknownNameIsAVerdictNotAnEnvFault`、`TestCheck_UnavailableRunnerIsRetryableEnvFault`。
- git 侧：`TestReadFileAtCommit_ReadsTheBaselineNotTheWorktree`、`TestReadFileAtCommit_MissingFileIsNotAnError`。
- 清单与护栏（`hunt`）：`TestResolveGates_BountyOverridesRepoDeclaration`、`TestResolveGates_RepoDeclarationIsUsedWhenNothingIsHandedDown`、`TestResolveGates_NoSourceAtAllMeansNone`、`TestResolveGates_ExemptionRequiresABountyGrant`、`TestResolveGates_WorkingTreeReadsTheWorkspaceFile`、`TestResolveGates_WorkingTreeExemptionRequiresMetaGate`、`TestResolveGates_WorkingTreeCannotLowerStrength`、`TestResolveGates_WorkingTreeEmitsGateConfigChanged`、`TestResolveGates_WorkingTreeMissingManifestCannotDropRequiredGates`、`TestResolveGates_ExemptionWithoutGateSourceFails`、`TestMetaGate_RejectsUndecidableExpect`、`TestMetaGate_RequiresTheCommandToBeStartable`、`TestMetaGate_ReportsEveryProblemAtOnce`、`TestCheckStrength_RejectsWeakening`。
- 解析与加载（`internal/gates`）：`TestParse_ExpectComesFromTheConfig`、`TestParse_RejectsUnknownFields`、`TestSource_ParseIsTheSameRuleTheLoaderUses`、`TestSource_MissingDeclarationIsNone`、`TestSource_LoadsAndValidatesTheDeclaredList`。
- 接线（`hunt`）：`TestGateResult_EmitsCheckResultEvent`、`TestGateFacts_NamesReachTheUserTurn`、`TestFinalize_RequiredGateFailureFailsTheDelivery`、`TestFinalize_RequiredGateNeverRunFailsTheDelivery`、`TestFinalize_RequiredGatePassedSucceeds`、`TestFinalize_OptionalGateDoesNotRequireEvidence`、`TestFinalize_BackfillRunsOnlyRequiredGatesThatNeverRan`、`TestCheckpoint_GateFailureSuppressesAutoCheckpoint`、`TestCheckpoint_WithoutGateFailureAutoCheckpointStillCommits`。
- e2e（`cmd/xhunter`）：`TestEndToEnd_RequiredGateNeverRunFailsWithDelivery`、`TestEndToEnd_GateFailureFailsButStillDelivers`、`TestEndToEnd_RequiredGatePassedSucceeds`。

**依赖**：MS-4。**不做**：结构检查（MS-9）、每轮自动跑门禁（架构 §7.8 明确不做）。

**风险**：`expect` 语义已定死（见 §7）；`working_tree` 豁免是模型能触发的路径，护栏必须先于清单生效；跨平台 `/bin/sh` 差异。

---

#### MS-6 会话材料落盘

**目标**：把会话材料从「内存里的记录」变成「随检查点进分支的文件」，为恢复准备唯一状态源。

**范围**
- 材料路径 `.xhunter/<session_id>/session.jsonl`（按任务隔离），带 `schema_version`，内容含对话历史、写操作序列、用量、扩展能力指纹字段（MS-8 填充）。**（已落地）**
- **随检查点提交**：`add -f`（仓库忽略 `.xhunter/` 时材料仍随提交）；周期性落盘（FR-12.3b）。**（已落地）**
- **排除交付 diff**：`Diff`/`Patch` 排除 `.xhunter/<session_id>/**`（`RepoRef.MaterialDir`），不排除该目录之外的路径（如 `skills.draft/**`）。**（已落地）**
- 材料不可篡改：策略已禁写 `.xhunter/**`（`TestDecide_WriteToControlDirDenied`），git 侧模型无能力（`TestDefaultTools_HasNoGitPrimitive`）——**既有断言已覆盖**。
- **接口一次加齐**（**已落地**）：`hunt.SessionRecorder` 方法集一次补齐 `Open(root)` / `RecordTurn` / `RecordOp(WriteOp)` / `RecordUsage(llm.Usage)` / `Ops() []WriteOp` / `Snapshot()`——写操作序列是恢复的唯一刚需，此前 `WriteOp` 只被 `Session.ops` 攥着、没有通道，与落盘实现一次改完、不分两次扩公开接口。`RecordUsage` 是文档之外多出的一项：材料要含用量而 `Snapshot()` 不接受参数，且它让"每轮用量增量"与计费、`usage` 事件同源（只有一处计算）。**刻意不加** `Delta` / `Fingerprint`：没有消费方（同 `llm.Caps` 原则）。

**独立验收的证据**
- 材料落盘：`TestSave_MaterialLandsUnderSessionDirWithSchemaVersion`、`TestMaterial_LoadRejectsUnknownSchemaVersion`（**已有**）。
- 接口接上（`hunt`）：`TestRecorder_RecordsOpsAndUsageFromTheSession`、`TestRecorder_OpenFailureDegradesWithoutFailingPrepare`、`TestSnapshot_FailureDoesNotBlockTheRun`（IA-6.6）（**已有**）。
- 随检查点 `add -f`：`TestCommit_ForceAddsMaterialEvenWhenGitignored`、`TestCommit_MissingMaterialDirDoesNotFail`、`TestCommit_WithoutMaterialDirIsUnchanged`（**已有**）。
- diff 排除材料目录：`TestDiff_ExcludesMaterialDirButKeepsSkillsDraft`、`TestDiff_NoMaterialDirKeepsEverything`（**已有**）。

**依赖**：MS-4。**不做**：恢复流程（MS-7）。

**风险**：材料体积随轮次增长并进每次提交——需给出截断/上限口径（超限须在事件里可见）。

---

#### MS-7 会话恢复（resume）

**状态**：**已完成**（2026-09-24）。

**目标**：跨机器 failover 成立：崩溃后向新机器投递同一会话，接着最后一个检查点继续，**不重放写操作**。

**范围**
- 投递给出 `XHUNTER_SESSION_ID` → `Prepare` 里：checkout 任务分支 tip → 读回材料（`SessionRecorder.Load(root)`，**纯读、先于 `openRecorder`**）→ 回灌上下文（把读回的轮次按原有顺序 `ContextBuilder.Append`）→ 把本次补充条件作为新 user 消息追加在历史之后 → 从下一轮继续。（**已落地**。）
- **零工具执行、零模型调用**：恢复段不调用任何原语、不提交；`Restored.Ops` 不重放、不并入本次 `s.ops`。用例 `TestPrepare_ResumeSeedsContextWithoutExecutingTools`、`TestEndToEnd_ResumeContinuesFromLastCheckpoint`。
- **恢复失败** → 退出码 1；材料**不存在**是正常情况，与「损坏」分开。用例 `TestEndToEnd_ResumeCorruptedMaterialIsEnvError`、`TestEndToEnd_FreshSessionWithoutMaterialIsNotAnError`。
- 分支语义沿用已有实现（`TestPrepareBaseline_ResumeChecksOutBranchTip`）。
- **token 预算续算**：`Prepare` 把 `Restored.Usage` 的输入/输出补喂一次策略，使同一任务多次重派不重置 token 上限（**不动**本次运行的水位）。用例 `TestPrepare_ResumeSeedsPolicyBudgetWithRestoredUsage`。
- **澄清后续跑**：新 Bounty 的补充条件作为新的 user 消息追加在回灌历史之后（定位语 `本次投递的补充条件：`）；断言对象是假上游收到的请求内容。用例 `TestEndToEnd_ResumeAfterClarificationAppliesNewConditions`、`TestPrepare_ResumeAppendsNewConditionsAfterRestoredHistory`。

**独立验收的证据**
- e2e：`TestEndToEnd_ResumeContinuesFromLastCheckpoint`——断言：第二趟首轮请求体含第一趟历史、第二趟零写操作（`session_delta.ops_count == 0`）、分支 tip 未推进、`session_delta == {turns_from:2, turns_to:2, ops_count:0}`（字面量）。
- `TestEndToEnd_ResumeCorruptedMaterialIsEnvError`（退出 1、`prepare_failed`、可重试）。
- `TestEndToEnd_FreshSessionWithoutMaterialIsNotAnError`（结果文件**不含** `session_delta` 键）。
- `TestEndToEnd_ResumeAfterClarificationAppliesNewConditions`（新条件下标 > 回灌历史下标）。
- 读回单测：`TestLoad_MissingMaterialIsZeroValueNotAnError`、`TestLoad_ReadsTurnsOpsAndUsageInOrder`、`TestLoad_UnknownRecordTypeIsAnError`。
- AC-7 的完整断言。

**依赖**：MS-6。**不做**：压缩回灌（MS-11 接上同一管线）。

**口径（明确接受）**
- **交付 diff 一律相对原始基线 commit**（`files_changed` 与 `patch`，**恢复趟也是**）：交付物是**分支对基线**的整体差异——MR 评审要看的正是"这条交付分支改了什么"。若改成"相对恢复 tip"，平台在续跑趟会看到**空清单**，比"多报一条上一趟的改动"更坏。续跑趟的**增量**由 `session_delta`（`turns_from` / `turns_to` / `ops_count`）单独回答，两者不重复表达同一件事。
- **轮数预算不跨恢复续算**（本次轮号从 1 重新起计，无累计口径）；token 预算已续算（`Prepare` 补喂 `Restored.Usage` 输入/输出）。轮数一律按**记录条数**算，不取 `turn.no` 最大值——恢复后材料里会出现两段都从 1 开始的 `turn` 记录。用例 `TestPrepare_ResumeCountsTurnsByRecordsNotByMaxTurnNo`。
- **"存在即已恢复"**：判据只有一处——`Load` 返回的 `SchemaVersion != 0`。材料存在（含"只有 meta、零记录"的退化情形）即算已恢复；该退化情形在正常路径上不可达（材料要进分支必先有轮次与写操作），此处口径只为"存在即恢复"这一条判据自洽。用例 `TestLoad_MetaOnlyMaterialIsNotAnError`、`TestPrepare_MetaOnlyMaterialIsTreatedAsResumed`。

**风险**：假上游夹具要能「制造崩溃点」，并让第二次运行的请求序列可断言——最容易被低估。

---

#### MS-8 扩展接入与符号读写

**状态**：**已完成**（2026-09-25）——同一份 `ext.ExtHost` 契约的两种后端都已可用：内置语法级后端（进程内，不需要额外安装任何东西）＋ **外挂通道**（`ext/mcp`，MCP over stdio 子进程：懒启动／崩溃与超时隔离／随 Hunt 回收／不继承环境）。三个符号原语已在工具面上，`symbol_read` / `symbol_edit` / `symbol_rename` 均真实现。能力指纹同时进生效配置快照与会话材料 `meta.ext`（`IA-6.5` / FR-13.7），两处同源。

**目标**：符号能力以本地进程外挂接入，`symbol_read` / `symbol_edit` 落地（首发语言 Go），基础原语不受影响。

**范围**
- `ext.ExtHost` 实现：本地 stdio 子进程 ＋ 懒启动 ＋ 随 Hunt 回收 ＋ 崩溃隔离 ＋ 超时（FR-13.1/13.5/13.6）。**已落地**（`ext/mcp`）：传输与握手是 MCP 标准（stdio 行分隔 JSON-RPC 2.0；`initialize` → `notifications/initialized`），符号能力方法走自有命名空间 `xhunter/{capabilities,locate,parse,enclose}`；线格式**手写**（未用 MCP SDK——这是实现选择，**不是** NFR-1 的要求：NFR-1 管的是「目标环境要不要额外装东西」，手写与否不影响它）。
- 能力描述符 `ExtCaps` → 决定符号路径可用性与结果标注；核心不感知后端种类。
- `symbol_read` / `symbol_edit`：先定位到字节区间，再复用 `hunt/basic` 的读写；写盘仍走 `Committer`。
- **恢复注册**：符号三原语（`symbol_read` / `symbol_edit` / `symbol_rename`）在一期已从装配层移除（不注册）；MS-8 把它们的注册恢复回装配层（构造器与实现一直保留在 `hunt/symbolic`）。
- **降级**：扩展缺失/启动失败/超时/语言未注册 → 结构化错误 ＋ 显式提示；工具名**接入后**不撤回（FR-13.4、AC-11/AC-12/AC-26）；非法源码 → 降级文本并提示（FR-4.5）。
- 寻址可观测：上报 `precision` 与降级原因（FR-4.13）。
- 能力指纹写入会话材料（与 MS-6 字段对接；不一致只记录不阻断）。

**独立验收的证据**
- 符号原语（`hunt/symbolic`）：`TestSymbolRead_LocatesAndReadsRange`、`TestSymbolRead_NotFoundIsAVerdictNotAnEnvFault`、`TestSymbolEdit_ReplacesOnlyTargetRange`、`TestSymbolic_ReportsStructuredErrorWhenLanguageUnregistered`、`TestSymbolic_ReportsSyntacticPrecision`、`TestSymbolics_UnavailableBackendIsStructuredError`、`TestSymbolics_WritesIsDeclaredByThePrimitive`。
- 语法级后端（`ext/syntax`）：`TestHost_CapabilitiesAreSyntacticAndCannotResolve`、`TestHost_FingerprintIsAvailableWithoutAWorkspace`、`TestHost_LocateFindsTheDeclarationRange`、`TestHost_QualifierNarrowsTheSameNamedSymbol`、`TestHost_UnparsableFileIsSkippedNotAnError`、`TestHost_MissingSymbolIsAnError`、`TestHost_AllListsDeclarationAndReferences`、`TestHost_SitesSkipFieldNamesAndSelectors`、`TestHost_CloseIsIdempotent`。
- **工具面恒定对照**（IA-7.1）：`TestDefaultTools_FaceIsIdenticalWhetherTheBackendIsAvailable`、`TestDefaultTools_FaceIsFixed`、`TestDefaultTools_SymbolPrimitivesAreRegistered`。
- e2e：`TestEndToEnd_SymbolEditLandsInTheDelivery`、`TestEndToEnd_SymbolReadReturnsJustTheDefinition`、`TestEndToEnd_SymbolEditOnUnregisteredLanguageIsStructuredError`。
- **外挂通道**（`ext/mcp`，11 条）：`TestHost_StartsLazilyAndLocates`（懒启动）、`TestHost_HandshakeCarriesTheServerSelfDescription`、`TestHost_UnconfiguredCommandIsUnavailable`、`TestHost_UnstartableCommandIsUnavailableNotFatal`（不可用是**事实**、不是装配缺陷）、`TestHost_CrashDoesNotHangTheLoop`、`TestHost_TimeoutIsolatesTheExtension`、`TestHost_CloseReclaimsProcess`、`TestHost_FingerprintDoesNotStartTheProcess`（指纹在装配冻结时就读，不为一个诊断字段开进程）、`TestHost_UntrustedBoundaryDoesNotInheritEnv`、`TestHost_UnknownVerdictIsNotReadAsBroken`、`TestHost_ParseAndEncloseAreMappedFromTheWire`。
- **装配层**（`cmd/xhunter`）：`TestChooseExt_UnconfiguredIsTheBuiltInBackend`、`TestChooseExt_ConfiguredIsTheExternalChannelAndDoesNotStartIt`、`TestChooseExt_ToolFaceIsIdenticalAcrossBackends`、`TestChooseExt_RejectsMalformedDeploymentFacts`、`TestParseExtConfig_ReadsDeploymentFacts`、`TestParseExtConfig_RejectsMalformedFacts`、`TestAssemblyFacts_ReportsTheAssembledBackend`、`TestEndToEnd_ExternalBackendUnavailableStillConverges`。
- **回收**（`hunt`）：`TestFinalize_ClosesTheExtHost`、`TestFinalize_WithoutExtHostDoesNotPanic`。

**依赖**：MS-2。**前置决策已定**（2026-09-25，见 §7.1 决策 21 / 26）：协议形态＝MCP 标准传输 ＋ 自有能力命名空间；首发后端＝内置语法级，外挂是**可选**后端。

**风险**：区间替换必须复用 `Committer` 校验；扩展不可信边界（不继承凭据）；外挂通道不得引入运行期安装要求（NFR-1：装好二进制就能跑；线格式手写，未引 SDK）。三条均已由上列用例钉住。

---

#### MS-9 结构检查（自动检查点的判据）—— **已完成**（2026-09-25）

**落地形态**
- 判据接在 `ext.ExtHost` 上：① `Parse`（**此刻**目标文件语法完整）＋ ② `Enclose`（改动区间封闭在某个符号内）。`Session.structuralJudge` 这个"包内可替换位置"已撤掉，换成真判据 `Session.judgeStructural`（三态 `structuralVerdict` 保留）。
- **取样时点两条不同**：① 在判定时取（检查点钉的是此刻内容）；② 在**写盘前**就地取、与 `s.ops` 平行记进 `opEnclose`——字节区间只有在它产生的那一瞬才与文件内容对齐。
- **取样范围是尚未提交的改动**（`pendingFrom`）：一次成功的提交把起点推到末尾，判据因此问的是"这批改动"，不是"这一路走来的每一笔"。整文件写入（含新建）不适用判据②，由判据①单独覆盖。
- 宿主由装配层经 `hunt.ExtHostFactory` 注入、与符号原语**共用同一份实例**（`hunt.ToolFactory` 随之加 `ext.ExtHost` 入参）；未装配即"没有符号能力"，判据落成不可判定。
- 触发链补全：门禁通过 > 模型显式 > 结构检查通过 > 都不满足则不提交。

**独立验收的证据**
- `TestStructural_ParseOKFlipCommits`（残缺 → 补齐后提交）、`TestStructural_IncompleteSyntaxSuppresses`、`TestStructural_UndecidableDoesNotCommit`、`TestStructural_EditOutsideAnySymbolSuppresses`、`TestStructural_WholeFileWriteIsNotJudgedAsUnenclosed`、`TestStructural_CommittedOpsLeaveTheJudgementWindow`、`TestStructural_ParseIsAskedOncePerFile`、`TestStructural_UndecidableDoesNotBlockWriting`。
- `ext/syntax`：`TestHost_ParseOKOnCompleteSyntax`、`TestHost_ParseBrokenOnIncompleteSyntax`、`TestHost_ParseUnknownOnUnregisteredLanguage`、`TestHost_EncloseFindsTheSmallestEnclosingDeclaration`、`TestHost_EncloseReportsNotEnclosedOutsideDeclarations`、`TestHost_EncloseOnUnregisteredLanguageIsNotAVerdict` 等 10 条。
- e2e（真 git ＋ 真语法后端）：`TestEndToEnd_CheckpointLandsOnCompleteSyntax`、`TestEndToEnd_IncompleteSyntaxSuppressesCheckpoint`。

**依赖**：MS-5、MS-8。

**风险**：`ParseOK` 依赖符号能力——判据不可用时绝不能退化成「按轮提交」，也不能静默不报（判不了即不提交 ＋ 一条 `degraded`）。

---

#### MS-10 `symbol_rename`（跨文件重命名）与规模上报

**状态**：**已完成**（2026-09-24）。

**裁决（2026-09-24）**：首发只做语法级后端时，`symbol_rename` **照常实现**——出现点由语法树扫描给出（结构化定位，不是文本替换），并**如实标注未穷尽**（`Impact.Unknown`）；后端列不出出现点时返回 `cannot_resolve` 结构化错误。理由：不做就得让工具名长期返回 `not_implemented`，而「半套重命名」（改了声明没改引用）才是真正要避免的事——它由「出现点为空即拒绝」这一条结构性地挡住。

**目标**：从「能改」到「改得对」：跨文件重命名一次完成全部改动，并把改动规模作为证据上报。

**范围**
- `symbol_rename`：改名 ＋ 更新全部引用点；不静默降级为文本替换。
- **规模上报**（`ext.Impact`：文件数 / 处数 / `Unknown`）→ 进结果摘要、事件流与变更说明（FR-6.2）；不做阈值、不做拦截。
- `Unknown`（语法级后端无法穷尽引用）如实标注。
- **同源要求**：上报的规模与落盘的编辑计划必须来自同一次定位结果。`ext.Prepared` 需扩展为「一次定位给出完整编辑计划」的形状。

**独立验收的证据**
- `TestSymbolRename_UpdatesDeclarationAndAllReferences`（声明 + 全部引用点一次改完）。
- `TestSymbolRename_CanResolveFalseIsStructuredErrorNotTextReplace`（列不出出现点 → 结构化错误，零编辑）。
- `TestSymbolRename_ReportsFootprint`（上报处数 == 落盘编辑数：同源）＋ `TestSymbolRename_UnknownFootprintIsReportedNotGuessed`。
- 后端侧：`TestHost_AllListsDeclarationAndReferences`、`TestHost_SitesSkipFieldNamesAndSelectors`。
- 策略侧不再有影响面用例：影响面阈值未采纳（「改得多就拒绝」是偏好不是判据），故无对应用例。

**依赖**：MS-8。**不做**：影响面阈值（见 §7）。

---

#### MS-11 上下文压缩

**目标**：长任务不因窗口耗尽而硬失败，且压缩可复现、可观测、不丢交付物。

**范围**
- 水位触发（默认 70% / 50% / 90%）＋ 冷却期；不允许「超限才压」。
- 分层下压 L0→L4；默认零模型调用；L4 摘要生成一次即落盘、恢复时读回。
- **保丢优先级**：永不丢（Bounty 正文与验收标准、工作区约定、当前轮 messages、写操作记录）。
- 结构化工作日志由会话材料投影。
- `context_compacted` 事件。
- 交付物不依赖上下文。

**状态**：**已完成**（2026-09-25，见 §2.3）——水位与冷却、分层下压 L0→L3、`context_compacted` 事件、投影式工作日志、保丢优先级与「交付物不依赖上下文」全部落地；**不做** L4 模型摘要（FR-14.5，见 §5）。

**独立验收的证据**
- 水位与分层：`TestWatermark_TriggersAtWarnAndCoolsDown`、`TestCompaction_LayersDownToFit`、`TestCompaction_PressesThroughAllLayersWhenTargetIsUnreachable`、`TestCompaction_NothingToCompactDoesNotArmCooldown`、`TestCompaction_NoWatermarksMeansNoCompaction`、`TestCompaction_HardWatermarkIsReported`、`TestCompaction_WarnWatermarkBelowHard`。
- 各层形状：`TestCompaction_L0KeepsBothEndsAndMarksTheDrop`、`TestCompaction_L2KeepsAddressability`、`TestCompaction_FoldPreservesDeclaredSections`、`TestCompaction_CurrentTurnIsNeverCompacted`、`TestCompaction_ReleasedTokensIsAMeasuredDelta`。
- 可复现与恢复：`TestCompaction_ProjectionIsReproducible`（AC-18）、`TestCompaction_ResumeUsesTheSamePipeline`（FR-14.8）。
- 保真：`TestCompaction_NeverDropsBountyOrWriteOps`（AC-17）、`TestDeliverables_DoNotDependOnContext`（FR-14.6）。
- 事件：`TestCompaction_EventCarriesLevelAndReleasedTokens`、`TestCompaction_NoReporterMeansNoEvent`、`TestCompaction_EmittedOnTurnBoundary`。
- 硬上限终止：`TestCompaction_OverHardLimitStopsTheRun`、`TestCompaction_HardLimitIsJudgedAfterPressing`、`TestEndToEnd_HardWatermarkStopsTheRun`。
- e2e：`TestEndToEnd_OverWindowCompactsAndStillConverges`（窗口调小到 6000、两次读 20k 字符文件）→ 有 `context_compacted` 事件，且任务仍收敛、交付提交与改动清单不缺。

**依赖**：MS-6、MS-7。**风险**：压缩改的是已缓存前缀——低频一次压到位。

---

#### MS-12 运行段看护与本地驱动

**状态**：**已完成**（2026-09-24；流看门狗与本地驱动落地，见 §2.3）。

**目标**：把「挂住」这条无头场景的硬要求接上，并补上 FR-1.10 的本地驱动便利工具。

**范围**
- **流看门狗**（FR-1.11①）：接收段不活动超时（`harness.Config.StreamIdleTimeout`，默认 120s；**0 = 取默认 / 正数 = 不活动上界 / 负数 = 关闭看门狗**）→ **先落终态、再 `Cancel()`** → 环境错误（**1**）。心跳不算活动、取消优先于超时；超时结构上不可能落进 `no_tool_call`。
- **本地驱动**（FR-1.10）：`xhunter run --repo <path> --task <text> [--out <path>]`：只读探测本地仓库事实 → 生成 Bounty（`--out` 可选）→ 用**同一份装配**执行一次 Hunt；必须 clone 到临时工作区（用户仓库只被只读探测）；不放松任何不变量。它同时是澄清回路的入口。

**独立验收的证据**
- 流看门狗：`TestWatchdog_IdleStreamCancelsAsEnvError`、`TestWatchdog_TimeoutCannotBeNoToolCall`、`TestWatchdog_ActivityResetsTimer`、`TestWatchdog_WaitsForFirstEvent`、`TestWatchdog_DisabledWhenNegative`、`TestWatchdog_CancelOutranksTimeout`、`TestWatchdog_TimeoutStopsBeforeOnTurn`、`TestDefaultConfig_StreamIdleTimeoutDefaultsTo120s`、`TestConfig_ZeroIdleTimeoutTakesDefaultNegativeStays`、`TestEndToEnd_IdleStreamConvergesToEnvError`。
- 部署事实与机制硬顶上报：`TestParseStreamIdleTimeout_DefaultOverrideAndRejects`、`TestConfigSnapshot_CarriesStreamIdleTimeout`、`TestPrepare_EmitsConfigSnapshot`（增 `stream_idle_timeout_ms: 120000`）。
- 本地驱动：`TestRunCmd_ClonesIntoIsolatedWorkspace`、`TestRunCmd_ProbeIsReadOnly`、`TestEndToEnd_LocalRunDrivesBountyFromRepo`，以及启动期失败 `TestRunCmd_MissingRepoFlag` / `TestRunCmd_RepoPathMissing` / `TestRunCmd_NotAGitRepo` / `TestRunCmd_NoRemote` / `TestRunCmd_EmptyRepo`。
- 中立契约方法集：`TestSession_HasNoPermissionChannel`、`TestProvider_MethodSetIsInferAndCapabilities`、`TestCaps_DeclaresBehaviourFields`。

**依赖**：MS-2。**不做**：权限询问（已决：路径不存在）、单步驱动（FR-1.9 形态 C）、探测期远端可推送性预检。

---

### 4.4 拆解原则与 DoD

**拆解原则：什么算「可独自验收」**——一个里程碑只有同时满足下面 6 条，才算独立可验收：

1. **验收对象是对外可见的**——一条 FR/AC/IA，或使用手册里的外部契约；不是「重构了某层」。
2. **自带证据**——有用例（`go test`）或端到端夹具（真 git ＋ 假上游）；用例不依赖模型与网络（NFR-8）。
3. **无前向依赖**——不需要后续里程碑的实现来证明自己成立；允许依赖之前的里程碑。
4. **结束时是完整的下一步**——不留在「半截状态」（若必须切开，切在「能独立证明」的边界上）。
5. **不放松任何不变量**——INV-1~INV-11 一条不破；新增能力只经既有落点。
6. **文档同步**——该里程碑涉及的 FR/IA 状态句、缺口行、外部契约在同一次改动里更新（**改本文档**，不改设计文档的规则）。

**每个里程碑共用的完成定义（DoD）**：

1. `python scripts/check.py --wsl --race` **exit 0**（build ＋ vet ＋ test ＋ race），目标平台 linux/amd64。
2. 新增用例**不依赖模型与网络**（NFR-8）；端到端用例走「真 git 夹具 ＋ 本地假上游」。
3. 外部契约变更**只追加**（INV-5）：事件类型与字段、结果文件字段、退出码映射——同批更新 `xhunter-usage.md` §5/§6/§7。
4. 文档同步：**更新本文档 §2 索引对应行与 §3**；设计文档仅在「规则」变化时才动。
5. **不放松任何不变量**（INV-1~INV-11）：新增能力只经既有落点——策略裁决、`Committer` 写盘、事件出口；新增代码不引入平台相关假设。
6. 引入第三方库**不违反** NFR-1——判据是「目标环境不需要额外安装」（静态链入即可）；**不变的是**：模型接入不得用厂商 SDK（协议线格式手写）。准入三条（NFR-9）：纯 Go 无 cgo、不需运行期数据文件、模型接入不用厂商 SDK。

---

## 5. 待排期小项

| 项目 | 说明 |
|---|---|
| **写权限由任务内容决定**（取向，待排期） | 2026-09-23 用户明确：**以后再通过任务内容来决定所有的写权限**——写权限从「目录黑名单（`.xhunter/**` 禁写）」演进为「**任务声明的可写面**」（最小授权）。三个要点：① 权限面来自 Bounty（任务内容），而不是引擎硬编码的目录清单；② 引擎自有材料（会话材料 `.xhunter/<session_id>/**`）仍需强制禁写——那是**恢复正确性**，不是权限偏好；③ 落地时要解决：未声明可写面时「默认拒绝」的语义（**写原语名单的归属已结清**——2026-09-23 把「是否写盘」回收为原语自述，见 §2.3） |
| **嵌套 AGENTS.md 附注** | FR-2.6 的 monorepo 部分（AC-23）。补法：需要给 system 段的结果带上一份「约定清单（路径 ＋ 正文）」，而当前插件契约只交正文（`PromptPart.Body`）。等真有消费方时再加 |
| **静态扫描类断言** | 凭据不落事件/日志/patch/材料（AC-8、IA-6.8）、核心不出现供应商名（INV-1、IA-8.4）——属 CI 级扫描，非单测能覆盖 |
| **信号组合** | SIGTERM 的进程级断言已落地（`TestEndToEnd_SigtermConvergesToCancelled`，AC-6）；SIGKILL / 中断时机的更多组合仍可补 |
| **单步驱动（FR-1.9 形态 C）** | 明确不进一期 |
| **多 agent 路线**（备忘录，未采纳） | V 层已定判据路线，多 agent 不作为补法；若将来要做，落点是**操作原语**（`harness` 不动），缺口清单（用量回流 / 事件嵌套标识 / 递归上限 / 工具面冲突）见 `docs/xhunter-memo-multi-agent-route.md`。其中**最硬的是第 7 条**：「子 agent 裁剪工具面」与已承诺的「**同一构建版本内**工具面恒定 ＋ **环境能力不决定注册** ＋ **随能力接入而扩展**」（AC-26）直接冲突——**同一构建、同一能力集之下所有 agent 的工具面必须一致**（不再是"双轴都不变"）；要按角色给不同工具面，必须先改这条口径或解决该冲突再编码 |
| **L4 模型摘要（FR-14.5）** | 压缩的最深一层**不实现**：它默认就不启用（FR-14.4 确定性优先），而「生成一次即落盘、恢复时读回」要动会话材料与恢复两条链路——缺了它就不许声称做了 L4（宁可留缺口，也不做一个会在恢复路径上重新生成的版本）。将来要补时的落点：材料加一个 `compaction` 记录类型，`Load` 读回后直接注入 `contextBuilder`，**不在恢复路径上重新调用模型**（对齐 FR-12.1） |
| ~~冻结前回收：panic 哨兵~~（**已清零**，2026-09-25） | 未冻结期曾以 `panic` 哨兵装配尚未实现的装配扩展点（见 §7.1 决策 18、架构 §8 例外）。**四处已全部回收**并各有用例：`Policy.ObserveFailure` / `DeniedCount`（MS-3，回收为运行期如实判定）、`cmd/xhunter/material.go` 的 `Load`（MS-7，真实现：纯读回灌）、`ext/ext.go` 四处（MS-8，`Unimplemented` 改为如实降级）。**"走到即炸"已不存在于代码里**；「未冻结期口径」（决策 18、架构 §8 例外）随之**作废**，原文保留只为沿革 |

---

## 6. 与设计文档的同步点

> 本文档是状态 SSOT；三份治理文档只负责规则。任一里程碑落地时，默认**只回写本文档**（§2 索引 / §3 / §4），设计文档仅在「规则」变化时才动。规则变化时的对应关系如下：

| 规则落地点（设计文档） | 何时需要回写 |
|---|---|
| `xhunter-product-design.md` §6 工具集规格 | 仅当工具面规则本身变化（新增/删除工具名或改注册规则） |
| `xhunter-product-design.md` §8 分期计划（命名 stub） | 仅当**分期命名**变化；分期范围与排期以本文档 §4.1 为准 |
| `xhunter-usage.md` §5/§6/§7 | 外部契约**只追加**（事件类型/字段、结果文件字段、退出码映射）时同步 |
| `xhunter-architecture.md` §12 | 仅当 IA 的**验收项**（规则）变化；状态改本文档 |
| ~~`xhunter-architecture.md` §14~~ | **已并入本文档 §3**（原 §14 的缺口行以此为准，§14 节已从架构文档移除） |

**状态落地点（本文档）**：里程碑落地改 §2 索引对应行（状态 / 证据用例名 / 缺口说明）、§3 派生视图、§4 里程碑状态、§5 待排期小项。

---

## 7. 待澄清决策

### 7.1 已决（只记「已采纳进设计文档」＋ 指针，不复制决策正文）

> 下列决策已落在治理文档里；此处只留指针，避免与设计文档重复而漂移。

| # | 议题 | 落点 |
|---|---|---|
| 1 | 门禁清单位置（`.xhunter/gates.yml` → 仓库根 `gates.yml`，不限制修改） | 已采纳进设计文档：FR-5.2g / FR-5.2b、架构 §6.3 L1-4 / §7.8、使用手册 §1.1/§1.2 |
| 2 | 检查点档位（只要 `on_structure`，取消档位配置面） | 已采纳进设计文档：AC-20 / AC-22 / IA-11.11 / 架构 §6.3 L5⑥、使用手册 §3 |
| 3 | 默认档在结构检查落地前（判据不可判定 → 不提交） | 已采纳进设计文档：FR-1.3c / FR-1.3d；代码侧见 §1.3（`Session.structuralJudge` 默认返回不可判定） |
| 4 | 假设外化升级为「澄清回路」 | 已采纳进设计文档：FR-6.3、§3「澄清回路」、FR-12.1 第二场景、FR-1.10、架构 §6.5 / §7.2、使用手册 §5/§6/§7 |
| 5 | 权限询问（不做，路径不存在） | 已采纳进设计文档：架构 §10.3、§7.4、§8、INV-4、IA-4.4 / IA-8.2、§5；产品 FR-7.2；使用手册 §5 |
| 6 | 影响面阈值（不做） | 已采纳进设计文档：产品设计 §1.2「判据优先于偏好」、FR-8.7、§6 硬约束 #4、AC-16、§8 M3、FR-6.2、FR-2.8；架构 §7.4 / §10.1 / §10.5 / IA-4.1；MS-10 改为「规模上报」 |
| 7 | 预算是否计入缓存命中（计入） | 已采纳进设计文档：使用手册 §5「用量口径」、架构 §8「用量口径」 |
| 8 | 变量命名（方案 B：彻底分组前缀） | 已采纳进设计文档：使用手册 §3「前缀即分组」；代码常量同步 |
| 9 | MS-1 的两个小账（`find`、`edit.literal`） | 已落地（2026-09-23），见 §2.3；代码侧 `hunt/basic/find.go`、`TestEdit_DeclShape` |
| 10 | `usage` 事件粒度（每轮末增量 ＋ `hunt_end` 累计） | 已采纳进设计文档：使用手册 §5、架构 §9.2 |
| 11 | `assistant_text` 发不发（发） | 已采纳进设计文档：使用手册 §5/§6（`summary`）、架构 §9.2 |
| 12 | 心跳间隔（默认 30s ＋ `XHUNTER_HEARTBEAT_INTERVAL` 可配） | 已采纳进设计文档：FR-10.1、使用手册 §3、架构 §7.5 |
| 13 | `llm.Caps` 补齐还是改文档（改文档 ＋ 删死字段） | 已采纳进设计文档：架构 §8「只声明用到的能力」、FR-9.7（`degraded`/`scope: usage`） |
| 14 | 门禁 `expect` 的细节（对象 `{kind*, pattern, max, stream}`，RE2，全量输出判定） | 已采纳进设计文档：FR-5.2b / FR-5.2h、架构 §7.8、使用手册 §1.1 |
| 15 | 压缩的三个数（L4 默认不启用；冷却 3 轮；材料上限 2 MB） | 已采纳进设计文档：FR-14.4、FR-14.1、FR-12.2c |
| 16 | 两个载荷形状（`effective_config` / `session_delta`） | 已采纳进设计文档：FR-11.6、FR-1.5、使用手册 §6 |
| 17 | **V 层的路线选择：判据路线**（具名门禁 ＋ 结构检查）；多 agent 不作 V 层补法，但其调度形态定为**操作原语**（不动 harness） | 已采纳进设计文档：产品设计 §1.2、架构 §3 / §7.8；备查与缺口清单见 `docs/xhunter-memo-multi-agent-route.md` |
| 19 | **NFR-1 的口径是「运行时不需要安装第三方依赖库」，不是「代码不使用第三方库」**（2026-09-24 提出基础设施开放，**2026-09-25 用户更正规则本义**）：源码**可以**引用第三方库，只要它编译进二进制、不产生"目标环境必须先装点什么"的要求。首个实例：门禁清单 YAML 解析用 `gopkg.in/yaml.v3`（`go.mod` 已有 `require`）。**唯一保留的边界**：模型接入不得使用**厂商 SDK**（协议线格式一律手写）——理由是**维护面**（协议实现只应随协议变），不是依赖 |
| 21 | **首发符号后端形态**（2026-09-24）：内置**语法级**后端（`ext/syntax`，标准库语法树、进程内、首发 Go），它是首发也是兜底；第三方后端走**外挂通道**（MCP over stdio），**已随 MS-8 落地**（2026-09-25：`ext/mcp`；协议形态见决策 26）。核心只认 `ext.ExtHost`，换后端不改业务代码 |
| 22 | **语法级也做 `symbol_rename`**（2026-09-24）：出现点来自语法树扫描（结构化，非文本替换）＋ 如实标注未穷尽；列不出出现点即结构化错误。**不做**的只有"半套重命名"，不是重命名本身 |
| 23 | **结构判据的取样时点与窗口**（2026-09-25）：①「语法完整」在**判定时**取（问的是此刻内容）；②「区间封闭」在**写盘前**就地取并存下来（字节区间只对它产生的那一瞬有效）；窗口是**尚未提交的改动**，一次成功提交即推进——否则一次早年的越界编辑会永久关掉自动检查点。整文件写入不适用判据②（文件本身就是单位），由判据①单独覆盖 |
| 24 | **压缩的冷却只在实际压动后才上膛**（2026-09-25）：「当前轮永不压」意味着第一轮之后历史里还没有可压的老轮；冷却若在「压不动」时就上膛，下一轮第一个可压的老轮会被自己的冷却挡住（该压的时候压不动），与冷却「别反复改写已缓存前缀」的初衷正好相反。落点：`cmd/xhunter/context.go` 的 `compact`（`applied == 0` 直接返回、不上冷却）＋ 用例 `TestCompaction_NothingToCompactDoesNotArmCooldown` |
| 25 | **硬上限：撞上不判错，压不下来才终止**（2026-09-25）：撞硬上限照样一路压到目标水位，只是事件里 `watermark` 记 `hard`（它是「差点撞墙」的诊断标记，不是终止理由）；**压完仍不低于硬上限**才按预算耗尽处理（`budget_exhausted:context`，退出 2、不重派，usage§7「上下文达硬上限」）。判据是「压完之后」而不是「压之前」——否则一次本可以压下来的超窗会被误杀。落点：架构 §6.3 L2 表「压缩」行、用例 `TestCompaction_HardWatermarkIsReported`、`TestCompaction_HardLimitIsJudgedAfterPressing`、`TestCompaction_OverHardLimitStopsTheRun`、`TestEndToEnd_HardWatermarkStopsTheRun` |
| 26 | **外挂通道的协议形态**（2026-09-25）：传输与握手走 **MCP 标准**（stdio 行分隔 JSON-RPC 2.0；`initialize` → `notifications/initialized`），符号能力方法进**自有命名空间** `xhunter/{capabilities,locate,parse,enclose}`——MCP 的标准面回答的是「有哪些工具」，而我们要的是「文件 ＋ 字节区间」，借 `tools/call` 去表达只会把能力描述符塞进字符串里。**能力方法不进 `tools/*`**，因此与 FR-13.2「扩展不新增模型可见工具名」不冲突；**更不碰 `sampling`**（它会把模型调用倒灌回核心，与「工具执行不经供应商侧」直接冲突）。另：外挂后端起不来时**不静默退回内置**——静默退回会让精度档位（syntactic ↔ semantic）悄悄变化而不上报，那正是 FR-13.7 要诊断的东西 |

| 20 | **门禁终态口径**（2026-09-24）：执行失败（起不来 / 超时）**不是质量结论**——按"未运行"计入终态并发 `degraded(scope: gate)`；`required` 门禁未通过或从未运行 → `status=failed` 且**退出码 0**，改动照常交付。终态只改写引擎给出的 `succeeded`，机制性终止不被抹掉 |

| 18 | **未冻结期：装配扩展点先定义、未实现以 `panic` 哨兵装配；不写降级防护**（**对决策 13 的限定**：13 管"能力声明"，本条管"装配扩展点"，不是推翻） | 已采纳进设计文档：架构 §8「例外：装配扩展点可以先定义后实现（未冻结期口径）」；冻结前回收项见本文档 §5。**该口径已随哨兵清零而作废**（2026-09-25）：代码里已无 `panic` 哨兵，装配扩展点要么已实现、要么显式失败／如实降级。§8 例外保留为沿革、**不再适用** |

### 7.2 仍待澄清

| # | 项 | 说明 |
|---|---|---|
| 17 | `docs/` 放第四份文档 | **已由本次重构落地**：新增 `xhunter-status.md`（状态 SSOT），原 `xhunter-milestones.md` 并入本文档后删除；`docs/` 现为四份（产品设计 / 使用手册 / 架构设计 / 状态）。2026-09-23 补：新增 `xhunter-memo-multi-agent-route.md`（**备忘录，非规范**，只备查未采纳的路线与缺口清单）；治理仍以四份规范文档为准 |
| 18 | 里程碑编号与产品分期是否统一 | 本文档用 `MS-n`，产品设计用 `M1 / M1.5 / M2 / M3`；两套编号并存容易指错，待定是否统一 |
| 19 | `check.py --wsl` 的修法 | **已按「脚本内建路径映射」落地**（2026-09-23，见 §2.3），实测 `--wsl --race` → `exit 0`；MS-1 不再被它阻塞。属本机约定，不进产品文档 |
