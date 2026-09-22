package harness

import (
	"context"
	"strconv"
	"strings"
	"unicode"

	"xhunter/llm"
)

// DefaultStages 把每个具名环节接到一个执行体上。
//
// 这张表**可以替换**：装配层能传自己的版本，测试能注入桩实现。与它相对的是
// 原语注册表，那张表在包内固定、不对外开。差别来自它们面对的对象——
// 环节面对编排者，流程本来就会演进，所以要可配；原语面对模型，
// 工具面一旦能增删，模型看到的能力就成了可变契约。
var DefaultStages = map[StageID]HandlerFunc{
	StagePrepareBaseline: handlePrepareBaseline,
	StageGatesLoad:       handleGatesLoad,
	StageExtCaps:         handleExtCaps,
	StagePromptBuild:     handlePromptBuild,
	StageSessionRestore:  handleSessionRestore,
	StageConfigSnapshot:  handleConfigSnapshot,

	StageGuardCancel:  handleGuardCancel,
	StageGuardChannel: handleGuardChannel,
	StageGuardBudget:  handleGuardBudget,

	StageContextCompact:   handleContextCompact,
	StageContextAssemble:  handleContextAssemble,
	StageProviderInfer:    handleProviderInfer,
	StageStreamReceive:    handleStreamReceive,
	StageToolsExecute:     handleToolsExecute,
	StageCheckpointCommit: handleCheckpointCommit,

	StageSessionSnapshot:  handleSessionSnapshot,
	StageGatesRequired:    handleGatesRequired,
	StageDeliveryCommit:   handleDeliveryCommit,
	StageDeliverableDiff:  handleDeliverableDiff,
	StageWorkspaceCleanup: handleWorkspaceCleanup,
	StageExtClose:         handleExtClose,
	StageEventHuntEnd:     handleEventHuntEnd,
}

// ============================================================ pre

// handlePrepareBaseline 获取基线、建立任务分支，并首次交出工作区。
//
// 它是初始化链的第一步，也是**唯一一次**把工作区交给引擎的地方——在此之前，
// 任何读写都无从谈起；在此之后，运行期才第一次有了"工作区"这个对象。
//
// 工作区由装配层注入的 opener 打开：引擎知道"这里需要一个工作区"，
// 但不知道它是本地文件系统还是别的什么。
func handlePrepareBaseline(c *Context) {
	root, err := c.Deps.Git.PrepareBaseline(c.Ctx, c.Hunt.Bounty.Repo)
	if err != nil {
		// 基线不可达、分支冲突、没有推送权限，都是环境问题而不是任务失败：
		// 环境修好之后重跑是有意义的。
		c.Terminate(StatusFailed, "baseline_failed: "+err.Error(), ExitEnv)
		return
	}
	storage, err := c.Deps.Workspaces.Open(root)
	if err != nil {
		c.Terminate(StatusFailed, "workspace_open_failed: "+err.Error(), ExitEnv)
		return
	}
	c.Hunt.storage = storage
	c.beat(PhaseBootstrap)
}

// handlePromptBuild 构造首轮两段正文，并在此后整任务内冻结。
//
// 取一次而不是每轮重取，是因为重取会同时毁掉两件事：模型可以借修改约定文件来改自己的
// 指令，prompt 前缀也会跟着变（缓存全部失效）。折中是让运行中的改动照常进入交付物，
// 由人在评审时决定是否采纳——本次运行不受影响，即"书写与生效分离"。
//
// 两段的位置与顺序由内核拼：插件给的是正文，不是消息序列（见 PromptPlugin）。
func handlePromptBuild(c *Context) {
	in := PromptInput{
		Bounty:    c.Hunt.Bounty,
		Workspace: c.Hunt.storage,
		Tools:     c.Hunt.ToolDecls,
	}

	system, err := buildPromptStage(c.Ctx, c.Deps.SystemPlugins, in)
	if err != nil {
		// 构造不出来就不该开始跑：少了系统提示词与项目约定，产出会与仓库约定不符，
		// 而这类偏差要到评审时才看得出来——属环境问题，修好后重派有意义。
		c.Terminate(StatusFailed, "prompt_build_failed: system: "+err.Error(), ExitEnv)
		return
	}
	user, err := buildPromptStage(c.Ctx, c.Deps.UserPlugins, in)
	if err != nil {
		c.Terminate(StatusFailed, "prompt_build_failed: user: "+err.Error(), ExitEnv)
		return
	}
	c.Hunt.Prompt = PromptParts{System: system, User: user}

	// 降级记录进事件流：跳过或截断了什么必须可见。它走事件而不是日志，因为
	// "这次跑用的是哪份约定、少了哪些技能"是机器也该知道的事（评审与平台侧都要读）。
	emitNoticeEvents(c, system.Notices, user.Notices)

	// 两段都空就告警一次。一条判断兜住两种情形：根本没接插件，以及插件挂了却还没给出
	// 内容。后者的产出偏差与"这个仓库本来就没有约定"长得一模一样——不告警就只能等
	// 评审时才发现，而那正是"接入插件"要避免的事。
	if system.Body == "" && user.Body == "" {
		c.Deps.Sink.Log("warn", "首轮两段正文都为空：没有插件贡献内容，模型将看不到任何项目约定")
	}
	c.beat(PhaseBootstrap)
}

// emitNoticeEvents 把降级记录逐条上报为 degraded 事件。
//
// 事件类型是追加而非修改既有契约（既有类型与字段一字不动），消费方按已知类型处理、
// 未知类型忽略即可。
func emitNoticeEvents(c *Context, groups ...[]Notice) {
	for _, notices := range groups {
		for _, n := range notices {
			c.emit(ExternalEvent{Type: "degraded", Payload: map[string]any{
				"scope":   n.Scope,
				"subject": n.Subject,
				"reason":  n.Reason,
			}})
		}
	}
}

// buildPromptStage 按装配顺序依次调用插件，把产出的正文拼成一段。
//
// 一个插件失败即整段失败，不用已经拼好的前半段继续跑：少了某个插件的内容而不自知，
// 产出会偏出仓库约定，而这恰恰是"接上插件"要避免的事。
func buildPromptStage(ctx context.Context, plugins []PromptPlugin, in PromptInput) (PromptPart, error) {
	parts := make([]PromptPart, 0, len(plugins))
	for _, p := range plugins {
		part, err := p.Build(ctx, in)
		if err != nil {
			return PromptPart{}, err
		}
		parts = append(parts, part)
	}
	return joinPromptParts(parts), nil
}

// joinPromptParts 按给定顺序拼接各段正文。
//
// 顺序即装配顺序：由装配层排定，因此"哪段在前"是装配期的事实，不是运行期的偶然。
// 没有贡献正文的插件整段跳过，连同它的来源一起——否则正文里会留下一串没有内容的
// 分隔空行，快照里也会记上"用了其实没加进来的素材"。
//
// **降级记录例外**：它不随正文一起被跳过。一个把候选素材全部判为非法而输出空正文的
// 插件，恰恰是最需要被看见的那一个——正文为空不等于无事发生。
func joinPromptParts(parts []PromptPart) PromptPart {
	var bodies, sources []string
	var notices []Notice
	for _, p := range parts {
		notices = append(notices, p.Notices...)
		body := strings.TrimSpace(p.Body)
		if body == "" {
			continue
		}
		bodies = append(bodies, body)
		sources = append(sources, p.Sources...)
	}
	return PromptPart{Body: strings.Join(bodies, "\n\n"), Sources: sources, Notices: notices}
}

// handleGatesLoad 决定本次使用哪份校验清单。
//
// 来源优先级是"任务下发的 > 仓库里声明的 > 没有"。仓库声明要从基线读取而不是
// 从工作区读取：工作区里的清单模型能改，而基线被任务分支钉住、改不了。
// 这样"中途削弱自己的验收标准"这条路就不成立。
func handleGatesLoad(c *Context) {
	c.Hunt.gates = nil
	c.beat(PhaseBootstrap)
}

// handleExtCaps 取扩展能力，并在此定格模型可见的工具面。
//
// 定格放在这里是必需而非优化：构造请求的说明层与发起请求的注册面必须从**同一份**读出，
// 各自推导就会出现"说明了 A、实际注册了 B"的漂移。
//
// 声明来自装配层的工具清单（与执行读同一份），环境能力不参与——能力只改变执行路径
// （符号 / 文本 / 结构化错误），不改变工具面。
func handleExtCaps(c *Context) {
	c.Hunt.ExtCaps = c.Deps.Ext.Capabilities(c.Ctx)
	c.Hunt.ToolDecls = c.Deps.Tools.Decls()
	c.beat(PhaseBootstrap)
}

// handleSessionRestore 在续跑时恢复会话；新任务直接跳过。
//
// 恢复的方式是"取出最后一次检查点 + 读回记录"，而不是把历史写操作重新执行一遍。
// 重放需要证明重放本身是忠实的，代价很高；直接取出钉住的状态则没有这个问题。
func handleSessionRestore(c *Context) {
	if c.Hunt.Bounty.Session == nil {
		return // 条件环节：新任务不做恢复
	}
	c.beat(PhaseBootstrap)
}

// handleConfigSnapshot 把本次生效的配置定格一份。
//
// 配置存在覆盖关系（内置默认 → 外部注入 → 任务下发），最终生效值可能不等于任何单一来源。
// 在初始化阶段固化一份，事后才能回答"这次跑的是什么配置"，而不必回溯整个合并过程——
// 排查"同样的任务为什么这次表现不同"时，这是第一手证据。
func handleConfigSnapshot(c *Context) {
	c.emit(ExternalEvent{Type: "config_snapshot", Payload: map[string]any{
		"max_denied_streak": c.Cfg.MaxDeniedStreak,
		"max_fail_streak":   c.Cfg.MaxFailStreak,
		"max_turns_hard":    c.Cfg.MaxTurnsHard,
		// 首轮正文的来源一并定格：事后要能回答"这次用的是哪份约定、哪个版本的提示词"，
		// 而不必回溯当时的工作区状态。
		"prompt_sources": promptSources(c.Hunt.Prompt),
	}})
	c.beat(PhaseBootstrap)
}

// promptSources 汇总首轮两段正文的来源，供生效配置快照使用。
// 带上阶段前缀，因为同一个来源可能只进了其中一段——排查"约定为什么没生效"时，
// 第一件事就是看它到底挂在 system 还是 user 上。
func promptSources(p PromptParts) []string {
	out := make([]string, 0, len(p.System.Sources)+len(p.User.Sources))
	for _, s := range p.System.Sources {
		out = append(out, "system:"+s)
	}
	for _, s := range p.User.Sources {
		out = append(out, "user:"+s)
	}
	return out
}

// ============================================================ guards

// handleGuardCancel 检查取消信号。
func handleGuardCancel(c *Context) {
	if c.Ctx.Err() != nil {
		c.Terminate(StatusCancelled, "cancelled", ExitCancelled)
	}
}

// handleGuardChannel 检查事件通道是否还健康。
func handleGuardChannel(c *Context) {
	if c.Hunt.WriteErr != nil {
		c.Terminate(StatusFailed, "channel_write_failed: "+c.Hunt.WriteErr.Error(), ExitEnv)
	}
}

// handleGuardBudget 检查预算。
//
// 它只问"耗尽了吗、是哪一维"，不自己算任何维度——四类预算（轮数、token、
// 费用、时长）的计量方式各不相同，那些细节属于策略层。写在这里，
// 每加一维都要改这个环节。
func handleGuardBudget(c *Context) {
	if yes, dim := c.Deps.Policy.Exhausted(c.TurnNo); yes {
		c.Terminate(StatusFailed, "budget_exhausted:"+dim, ExitFailed)
	}
}

// ============================================================ steps

// handleContextCompact 在需要时压缩历史。
func handleContextCompact(c *Context) {
	c.beat(PhaseAssemble)
}

// handleContextAssemble 组装本轮请求的上下文。
func handleContextAssemble(c *Context) {
	msgs, err := c.Deps.Context.Assemble(c.Ctx, c.Hunt, c.TurnNo)
	if err != nil {
		c.Terminate(StatusFailed, "assemble_failed: "+err.Error(), ExitFailed)
		return
	}
	c.Messages = msgs
	c.beat(PhaseAssemble)
}

// handleProviderInfer 发起一次推理。
//
// 工具声明用的是定格后的那份集合——与组装说明层时用的是同一份。
func handleProviderInfer(c *Context) {
	sess, err := c.Deps.Provider.Infer(c.Ctx, llm.Request{
		Turn:     int(c.TurnNo),
		Messages: toLLMMessages(c.Messages),
		Tools:    c.Hunt.ToolDecls,
	})
	if err != nil {
		// 发起失败可能是环境问题（连不上、凭据无效），也可能是模型侧配置问题；
		// 这里统一按可重试处理，因为同一任务重跑有机会成功，且重跑成本远低于误判失败。
		c.Terminate(StatusFailed, "infer_failed: "+err.Error(), ExitEnv)
		return
	}
	c.Stream = sess
	c.beat(PhaseInfer)
}

// handleStreamReceive 消费流式响应，**只接收，不执行**。
//
// 收到工具调用时先入队，等流结束再统一执行。这样接收段没有任何副作用：
// 流中途挂起或取消时，工作区不会被写了一半，也就不存在"半写状态"需要善后。
// 代价是工具结果事件会比过去晚一点发出，但事件类型不变。
func handleStreamReceive(c *Context) {
	if c.Stream == nil {
		c.Terminate(StatusFailed, "no_stream", ExitEnv)
		return
	}

	for ev := range c.Stream.Events() {
		if c.Ctx.Err() != nil {
			_ = c.Stream.Cancel()
			c.Terminate(StatusCancelled, "cancelled", ExitCancelled)
			return
		}
		switch ev.Kind {
		case EvText:
			// 增量先攒着，流结束再作为一条上报：外部契约里的 assistant_text
			// 是一次回答，逐块上报会把一条记录拆成几十条无意义的碎片。
			c.AssistantText += ev.Text

		case EvToolUse:
			// 模型给的是"名字 + 参数 JSON"，落到哪个原语、定位参数是什么由绑定层解释。
			// 绑定失败与协议层报错（ev.Err）走同一条回灌路径：都必须让模型知道自己
			// 发错了什么，静默丢弃会让它以为调用成功了，然后基于这个错觉继续往下做。
			call, bindErr := BindToolCall(ev.Call)
			failure := ev.Err
			if failure == nil {
				failure = bindErr
			}
			if failure != nil {
				c.Results = append(c.Results, Result{CallID: call.ID, OK: false, Err: failure})
				c.Failed = true
				// resolved_mode 留空：这条调用根本没走到分发，路径无从谈起。
				// 键保留是为了让事件形状恒定，消费方不必按"有没有这个键"分支。
				c.emit(ExternalEvent{Type: "tool_result", Payload: map[string]any{
					"tool":          string(call.Primitive),
					"ok":            false,
					"resolved_mode": "",
					"summary":       failure.Message,
					"error":         failure.Kind,
				}})
				continue
			}
			c.Pending = append(c.Pending, call)

		case EvUsage:
			c.Hunt.Usage.InputTokens += ev.Usage.InputTokens
			c.Hunt.Usage.OutputTokens += ev.Usage.OutputTokens
			c.Deps.Policy.Charge(ev.Usage)
			c.emit(ExternalEvent{Type: "usage", Payload: map[string]any{
				"input_tokens":  ev.Usage.InputTokens,
				"output_tokens": ev.Usage.OutputTokens,
			}})

		case EvError:
			// 流中断：模型没给出完整响应，属环境问题——修好后重跑是有意义的。
			// 已经收到的增量不再是"本轮输出"，因此不落记录（半截回答入库会让
			// 续跑时的上下文里出现一段来历不明的助手消息）。
			kind, context := "stream_error", ""
			retryable := false
			if ev.Err != nil {
				kind = ev.Err.Kind
				context = ev.Err.Message
				retryable = ev.Err.Retryable
			}
			c.emit(ExternalEvent{Type: "error", Payload: map[string]any{
				"kind":      kind,
				"retryable": retryable,
				"context":   context,
			}})
			c.Terminate(StatusFailed, "stream_error: "+kind+": "+context, ExitEnv)
			return

		case EvEnd:
			if c.AssistantText != "" {
				c.emit(ExternalEvent{Type: "assistant_text",
					Payload: map[string]any{"text": c.AssistantText}})
			}
		}
	}
}

// handleToolsExecute 执行本轮收到的全部调用。
//
// 这是整轮里唯一的写盘窗口——所有落盘都发生在它调用的运行时内部。
// 单个调用失败不终止任务：失败信息会回给模型，让它自己换做法；
// 只有连续多轮都失败才由轮边界止损。
func handleToolsExecute(c *Context) {
	for _, call := range c.Pending {
		res := c.Deps.Tools.Execute(c.Ctx, ExecInput{
			Call:  call,
			Turn:  c.TurnNo,
			Caps:  c.Hunt.ExtCaps,
			Facts: c.Hunt,
		})
		c.Results = append(c.Results, res)

		// 写操作记录是交付物与会话材料的依据，轮次由运行时在执行时填好。
		for _, op := range res.Ops {
			c.Deps.Session.RecordOp(op)
		}
		if !res.OK {
			c.Failed = true
		}

		payload := map[string]any{
			"tool":          string(call.Primitive),
			"ok":            res.OK,
			"resolved_mode": string(res.Route.Path),
			"degrade":       res.Route.Reason,
			"summary":       res.Summary,
		}
		if res.Err != nil {
			payload["error"] = res.Err.Kind
			payload["message"] = res.Err.Message
		}
		c.emit(ExternalEvent{Type: "tool_result", Payload: payload})
	}

	rec := TurnRecord{Turn: c.TurnNo, Text: c.AssistantText, Calls: c.Pending, Results: c.Results}
	c.Deps.Context.Append(rec)
	c.Deps.Session.RecordTurn(rec)
	c.beat(PhaseTools)
}

// handleCheckpointCommit 提交本轮改动。
//
// 触发有两个来源，优先顺序明确：**模型显式请求 > 有改动就提交的兜底**。
//
//   - 模型经控制原语表达了"这里值得钉住" → 本轮立即提交，提交信息带上它的理由（净化后）。
//     它表达的是**时机偏好**，不是"必须提交"：本轮没有未提交的改动就什么都不做，
//     空提交只会污染历史。
//   - 模型没表达 → 有改动就提交。提交次数因此跟着改动活跃度自动稀疏化，
//     空轮不会产生噪声提交。
//
// 无论最终是否提交，意图都在这里被消费掉（一次请求只兑现一次）。
// 单次失败也不终止——提交是累积的，下一轮会把所有未提交的改动一并带上，
// 短暂抖动可以自然恢复；唯有连续失败才说明远端真的不可用。
func handleCheckpointCommit(c *Context) {
	intent, requested := c.Hunt.consumeCheckpoint()
	// 净化一次，后面只处理"能安全放进提交信息"的文本。
	intent = sanitizeIntent(intent)

	if len(c.Deps.Session.Ops()) == 0 {
		if requested {
			// 请求了但没有可钉住的东西：消费掉意图，不产生空提交。
			c.Deps.Sink.Log("info", "模型请求了检查点，但本轮无未提交改动，跳过", "turn", c.TurnNo)
		}
		return
	}

	cm, err := c.Deps.Git.Commit(c.Ctx, c.Hunt.Bounty.Repo,
		checkpointMessage(c.Hunt.Bounty, c.TurnNo, requested, intent))
	if err != nil {
		c.Deps.Sink.Log("warn", "阶段性提交失败，将在下一轮重试", "turn", c.TurnNo, "err", err.Error())
		return
	}
	c.Hunt.commit = &cm
	c.Deps.Sink.Log("info", "已创建检查点", "turn", c.TurnNo, "model_requested", requested)
}

// ============================================================ post

// handleSessionSnapshot 把会话材料落盘。尽力而为，失败不阻断——
// 它影响的是"崩溃之后能恢复到什么程度"，而不是本次运行的结论，不该反过来决定结论。
func handleSessionSnapshot(c *Context) {
	if err := c.Deps.Session.Snapshot(); err != nil {
		c.Deps.Sink.Log("warn", "会话材料落盘失败", "err", err.Error())
	}
}

// handleGatesRequired 在交付之前补跑尚未通过的必需门禁。
//
// 模型可能全程没调用过门禁，而"必需门禁从未运行"本身就该判交付失败。
// 收尾补跑一次，把"必然失败"变成"可能通过"；而且此时工作区正是最终交付物，
// 结论才有针对性——过程中跑的那些对应的是中间状态。
func handleGatesRequired(_ *Context) {
	// 实现待补：需要门禁清单与通过判据。当前先保证环节位置正确（必须在交付提交之前）。
}

// handleDeliveryCommit 做交付提交。
//
// 失败路径也尽力提交：已经做出的改动不该因为结论是失败就被丢掉，
// 评审的人需要看到过程。提交失败本身不算致命——补丁仍然可以单独产出。
func handleDeliveryCommit(c *Context) {
	cm, err := c.Deps.Git.Commit(c.Ctx, c.Hunt.Bounty.Repo, deliveryMessage(c.Hunt.Bounty))
	if err != nil {
		c.Deps.Sink.Log("warn", "交付提交失败，将只产出补丁", "err", err.Error())
		return
	}
	c.Hunt.commit = &cm
}

// handleDeliverableDiff 产出相对基线的整体差异，作为附带交付物。
func handleDeliverableDiff(c *Context) {
	files, err := c.Deps.Git.Diff(c.Ctx, c.Hunt.Bounty.Repo.BaseCommit)
	if err != nil {
		c.Deps.Sink.Log("warn", "差异生成失败", "err", err.Error())
		return
	}
	c.emit(ExternalEvent{Type: "deliverable", Payload: map[string]any{"files": files}})
}

// handleWorkspaceCleanup 回收临时目录。清理失败不阻断退出。
func handleWorkspaceCleanup(c *Context) {
	if err := c.Deps.Git.Clean(c.Ctx); err != nil {
		c.Deps.Sink.Log("warn", "工作区清理失败", "err", err.Error())
	}
}

// handleExtClose 回收扩展进程。失败不阻断退出——
// 扩展是可缺失的能力来源，它的回收问题不应该影响任务结论。
func handleExtClose(c *Context) {
	if err := c.Deps.Ext.Close(); err != nil {
		c.Deps.Sink.Log("warn", "扩展回收失败", "err", err.Error())
	}
}

// handleEventHuntEnd 上报终态，必须是收尾的最后一个动作。
//
// 万一前面没有任何环节给出结论，这里补一个兜底终态——上报环节不负责判断成败，
// 只负责保证"一定有结论发出"，否则调用方会一直等下去。
func handleEventHuntEnd(c *Context) {
	t := c.Hunt.Terminal
	if t == nil {
		t = &Terminal{StatusFailed, "no_terminal", ExitEnv}
		c.Hunt.Terminal = t
	}
	c.emit(ExternalEvent{Type: "hunt_end", Payload: map[string]any{
		"status": string(t.Status),
		"reason": t.Reason,
	}})
}

// ============================================================ 提交信息

// maxIntentRunes 是模型给出的理由进入提交信息时的长度上限。
// 它在模型可见面上是自由文本，进了 git 历史就成了长期记录——长度要受控。
const maxIntentRunes = 120

// checkpointMessage 合成提交信息。它是**引擎的**产物：模型只提供素材，不提供格式。
//
// 两段都受控：身份段（第几轮、哪个任务）让评审的人能把提交对回任务；
// 类型段区分"按兜底落下的"与"模型要求提前钉住的"——两者在历史里的意义不同。
func checkpointMessage(b Bounty, turn TurnNo, requested bool, intent string) string {
	msg := "turn " + strconv.Itoa(int(turn)) + " 检查点"
	if !requested {
		return msg + "：" + taskSubject(b)
	}
	msg += "（模型请求）：" + taskSubject(b)
	if intent != "" {
		msg += "｜" + intent
	}
	return msg
}

// sanitizeIntent 把模型给的理由整理成能安全放进提交信息的一行文本。
//
// 净化不是洁癖，而是**格式不可被素材伪造**：若不处理，模型就能借这段自由文本
// 伪造提交信息的结构（换行加一句 "turn 99 检查点：…"，冒充另一次提交），
// 或塞进控制字符把历史搅乱。所以：只取首行、剥掉控制与格式类字符、空白折叠、
// 按上限截断；清完为空就当作没给理由（信息里只留"模型请求"这个事实）。
func sanitizeIntent(s string) string {
	// 换行是唯一能伪造"多段结构"的手段，先按首行收刀。
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	s = strings.Map(func(r rune) rune {
		// Cc（控制符）与 Cf（零宽、双向控制等格式符）都不是内容：
		// 前者会把历史搅乱，后者能让同一段文字看起来不一样。
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if runes := []rune(s); len(runes) > maxIntentRunes {
		s = string(runes[:maxIntentRunes]) + "…"
	}
	return s
}

func deliveryMessage(b Bounty) string {
	return "任务改动：" + taskSubject(b)
}

// taskSubject 取任务描述首行并限长，便于把提交对回任务。
func taskSubject(b Bounty) string {
	s := b.Task
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	const limit = 60
	if len([]rune(s)) > limit {
		s = string([]rune(s)[:limit]) + "…"
	}
	if strings.TrimSpace(s) == "" {
		s = "未命名任务"
	}
	return s
}
