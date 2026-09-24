package hunt

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"xhunter/ext"
	"xhunter/harness"
	"xhunter/llm"
	"xhunter/workspace"
)

// ============================================================ 三组 handler
//
// Session 以三组 handler 的形式接进循环：Prepare（循环前）、OnTurn（每个轮边界）、
// Finalize（循环后）。harness 不认识 Session，只认识这三个函数签名；上下文、会话材料、
// 事件出口、工具执行都由 Session 自己完成。

// Prepare 循环前一次：准备基线、打开工作区、定格工具面、构造首轮提示词、冻结生效配置
// 快照、发出起飞事件并启动心跳。
//
// 工作区在这里就打开并检查——失败即返回错误，循环根本不会开始，因此不存在「跑到一半
// 才发现工作区不可用」的中间态。
//
// 装配缺件也在这里显式失败（FR-1.8：缺件在首轮推理之前失败，退出码 1）。此前
// Git / Opener 缺失是**调用即 panic**（被引擎收敛成一句 "panic: invalid memory
// address"），而 Policy 缺失会被静默跳过——同一份"装配校验"的说法，三种行为。
func (s *Session) Prepare(ctx context.Context, run *harness.Run) error {
	s.setPhase(PhaseBootstrap)

	if s.cfg.Git == nil {
		return errors.New("装配不完整：缺少 git（基线获取、任务分支、检查点与交付提交都靠它）")
	}
	if s.cfg.Opener == nil {
		return errors.New("装配不完整：缺少工作区打开点（Opener）")
	}
	if s.cfg.Policy == nil {
		return errors.New("装配不完整：缺少策略（无人类场景下它是唯一顶替人的位置，不能省）")
	}
	if s.cfg.Context == nil {
		s.logf("warn", "未装配上下文组装器：模型只看得到首轮消息，看不到自己上一轮做过什么")
	}

	root, err := s.cfg.Git.PrepareBaseline(ctx, s.cfg.Bounty.Repo)
	if err != nil {
		return err
	}
	s.root = root
	storage, err := s.cfg.Opener.Open(root)
	if err != nil {
		return err
	}
	s.storage = storage

	// 会话恢复（仅当投递给出会话标识时）：**读材料是纯读，必须排在 `openRecorder`
	// 之前**——`openRecorder` 会为不存在的材料建目录并写 meta，先开记录器就再也分不清"上次留下的
	// 材料"与"刚刚为本趟建的空材料"。checkout 分支 tip 已由 `PrepareBaseline` 覆盖
	// （"分支已存在且 tip 为基线或其后代 → checkout tip"），不重复做。
	//
	// "本次是否恢复"的判据只有一处：`Load` 返回的 `SchemaVersion != 0`（材料存在且 meta 合法）。
	// 材料不存在 → 零值 + nil（新任务，照常跑）；存在但读不出来才是错误，由这里上抛 → 退出码 1
	// （不自动迁移、不猜）。恢复全程**不执行任何工具、不重放写操作**——工作区已由分支 tip 给出，
	// 读回的写操作序列只用于"本次不重做"。
	if s.cfg.Bounty.Session != nil {
		if s.cfg.Session == nil {
			return errors.New("要求恢复会话但未装配会话记录器（SessionRecorder）")
		}
		restored, err := s.cfg.Session.Load(root)
		if err != nil {
			return fmt.Errorf("读回会话材料失败：%w", err)
		}
		if restored.SchemaVersion != 0 {
			s.resumed = &restored
			// 预算续算只做 token：把上次用量**补喂一次**策略，让同一任务多次重派不重置 token
			// 上限。只喂输入／输出（策略管预算、不关心缓存明细）；**不动**本次运行的水位
			// （`s.charged` 记的是"本次运行事件增量"，动了会让首轮增量变负、usage 全线失真）。
			// 轮数预算不续算：本次轮号从 1 重新起计，没有可累加的口径。
			if s.cfg.Policy != nil {
				s.cfg.Policy.Charge(llm.Usage{
					InputTokens:  restored.Usage.InputTokens,
					OutputTokens: restored.Usage.OutputTokens,
				})
			}
		}
	}

	// 材料位置随工作区就绪而定：绑定失败只降级、不阻断（材料丢了最多是崩溃后从头跑）。
	s.openRecorder(root)

	// 符号能力宿主：结构判据与符号原语**共用同一份实例**（见 ExtHostFactory）。未装配即
	// "本次没有符号能力"——那是一条可以带着跑的事实（结构判据落成不可判定、符号原语给
	// 结构化错误），不是装配缺陷。
	s.ext = ext.Unimplemented{}
	if s.cfg.Ext != nil {
		if host := s.cfg.Ext(storage); host != nil {
			s.ext = host
		}
	}

	// 工作区就绪后构造原语并定格工具面：原语读文件需要工作区，声明与执行因此同源。
	// 工具面自身不成立也算装配缺件——错误在首轮推理之前暴露，而不是等供应商拒收请求。
	if err := s.buildTools(storage, s.ext); err != nil {
		return fmt.Errorf("工具面不成立：%w", err)
	}
	run.Tools = s.decls()

	// 门禁清单来源裁决：Bounty 下发 > 仓库声明（基线 commit）> 无。判据必须在运行开始前定死，
	// 所以仓库声明读的是基线那一份；清单不可用即启动期失败——带着一份判不了的清单跑完，
	// 只会得到一个"看起来通过了"的结论。
	gates, source, err := s.resolveGates(ctx)
	if err != nil {
		return err
	}
	s.gates, s.gateSource = gates, source
	if source != GateSourceNone {
		s.logf("info", "本次门禁清单来源", "source", source, "gates", len(gates))
	}

	// 构造首轮两段正文：插件给正文、Session 拼位置与顺序。取一次、整任务内冻结。
	s.setPhase(PhaseAssemble)
	in := PromptInput{Bounty: s.cfg.Bounty, Tools: run.Tools}
	systemPlugins := s.systemPlugins(storage)
	userPlugins := s.userPlugins(storage)
	system, err := buildPromptStage(ctx, systemPlugins, in)
	if err != nil {
		return err
	}
	user, err := buildPromptStage(ctx, userPlugins, in)
	if err != nil {
		return err
	}
	s.emitNotices(system.Notices, user.Notices)

	prompt := firstPrompt(system.Body, user.Body, s.cfg.Bounty, s.gates)
	if system.Body == "" && user.Body == "" {
		s.logf("warn", "没有任何插件贡献正文：模型只会看到内核条款与环境事实，看不到项目约定与任务描述")
	}

	// 生效配置快照冻结一次：起飞事件与结果文件读的是同一份。放在工具面与提示词都定格之后，
	// 快照才如实反映"本次实际生效"的装配；两份各算一遍迟早会漂。
	s.effective = s.freezeEffectiveConfig(systemPlugins, userPlugins)

	// 首轮消息收敛到单一出口：有上下文组装器就交给它持有历史，没有就直接用首轮两段。
	// 提示词交给上下文持有者，历史也归它——harness 只拿组装好的消息。
	//
	// 恢复时把读回的轮次按**原有顺序**逐条回灌（不扩 `ContextBuilder` 接口：它本来就是
	// "提示词 + 历史"的持有者），再把本次投递的补充条件作为**新的 user 消息**追加在回灌历史
	// 之后——模型先看到"上次做到哪"，再看到"这次的新条件"。
	if s.cfg.Context == nil {
		run.Messages = prompt
		if s.resumed != nil {
			run.Messages = append(run.Messages, resumeConditions(s.cfg.Bounty))
		}
	} else {
		s.cfg.Context.SetPrompt(prompt)
		if s.resumed != nil {
			for _, t := range s.resumed.Turns {
				s.cfg.Context.Append(t)
			}
		}
		run.Messages = s.cfg.Context.Assemble()
		if s.resumed != nil {
			run.Messages = append(run.Messages, resumeConditions(s.cfg.Bounty))
		}
	}

	// 起飞事件：Prepare 成功后立即发——没进入对话就没有起飞事件（失败路径在上面直接 return）。
	s.emitHuntStart()

	// 进入默认态（等模型——循环绝大部分时间花在这里）并启动心跳。心跳的生命周期归 Session：
	// 这里起、Finalize 的终态块之前停，因此一条心跳也不会落到 hunt_end 之后（见 startHeartbeat）。
	s.setPhase(PhaseInfer)
	s.stopHeartbeat = startHeartbeat(ctx, s.cfg.Sink, s.cfg.Heartbeat, s.Phase)
	return nil
}

// freezeEffectiveConfig 汇总本次 Hunt 生效的装配清单，冻结成快照。
//
// 原语顺序取自定格后的工具面：Session.order 是工厂给的顺序，checkpoint 由执行体殿后追加
// （与 decls 一致）——顺序即模型看到的顺序，快照必须报同一份。两段插件名与过滤器链名由
// Session 采出（只有它知道工厂返回了什么、按什么顺序调用）；策略口径、检查点行为、扩展
// 指纹、目标平台来自装配层注入的 AssemblyFacts。
func (s *Session) freezeEffectiveConfig(systemPlugins, userPlugins []PromptPlugin) EffectiveConfig {
	primitives := make([]string, 0, len(s.order)+1)
	for _, name := range s.order {
		primitives = append(primitives, string(name))
	}
	primitives = append(primitives, string(PrimCheckpoint))

	filters := make([]string, 0, len(s.cfg.Filters))
	for _, f := range s.cfg.Filters {
		filters = append(filters, f.Name)
	}

	// 扩展指纹在冻结处归一：空数组 = 没有扩展（已知事实），nil 会被序列化成 `null`、被读成
	// "不知道有没有"——第三方装配方漏设 Ext 时也不该写出一句"不知道有没有"。
	ext := s.cfg.Assembly.Ext
	if ext == nil {
		ext = []string{}
	}

	return EffectiveConfig{
		Primitives:    primitives,
		SystemPlugins: pluginNames(systemPlugins),
		UserPlugins:   pluginNames(userPlugins),
		Filters:       filters,
		Policy:        s.cfg.Assembly.Policy,
		Budget:        s.cfg.Bounty.Budget,
		Checkpoint:    s.cfg.Assembly.Checkpoint,
		Ext:           ext,
		Platform:      s.cfg.Assembly.Platform,
	}
}

// emitHuntStart 上报一次起飞事件：任务与仓库事实 ＋ 生效配置快照。
//
// **`config_snapshot` 的发出点也在这里**（与起飞同一时点）：把"这次运行实际生效的四个阈值"
// 一次报给平台——两个止损阈值来自策略自述、两个机制硬顶来自装配层注入的 harness 生效配置。
// 形状见 `configSnapshotPayload`；与 `hunt_start` 前后紧挨发出。
//
// 信封四字段（type / bounty_id / trace_id / ts）由出口统一盖章，这里只交业务载荷。
// 快照取的是冻结的那一份（s.effective），与结果文件同源。缺出口时是空操作——事件是
// 诊断通道，不是装配的必需件。
func (s *Session) emitHuntStart() {
	if s.cfg.Sink == nil {
		return
	}
	_ = s.cfg.Sink.Emit(ExternalEvent{Type: "hunt_start", Payload: map[string]any{
		"session_id":       s.cfg.Bounty.SessionID(),
		"base_commit":      s.cfg.Bounty.Repo.BaseCommit,
		"branch":           s.cfg.Bounty.Repo.Branch,
		"task":             taskSubject(s.cfg.Bounty),
		"effective_config": s.effective,
	}})
	_ = s.cfg.Sink.Emit(ExternalEvent{Type: "config_snapshot", Payload: s.configSnapshot().payload()})
}

// configSnapshot 汇总"这次运行实际生效的阈值"，供 `config_snapshot` 事件。
//
// 两个止损阈值是策略自述的边界常量（从生效配置快照的策略口径里取——策略不另抄一份）；机制侧
// 三条阈值（连续失败、轮数硬顶、接收段不活动超时）由装配层在 AssemblyFacts 里注入（只有装配层
// 知道 harness 实际生效的配置）。取不到的键按 0，如实表达"不知道"，不编一个看着像默认值的数字。
func (s *Session) configSnapshot() configSnapshotPayload {
	return configSnapshotPayload{
		MaxDeniedStreak:     intFact(s.effective.Policy, "max_denied_streak"),
		MaxSameKindStreak:   intFact(s.effective.Policy, "max_same_kind_streak"),
		MaxFailStreak:       s.cfg.Assembly.MaxFailStreak,
		MaxTurnsHard:        s.cfg.Assembly.MaxTurnsHard,
		StreamIdleTimeoutMS: s.cfg.Assembly.StreamIdleTimeoutMS,
	}
}

// intFact 从策略自述的 map 里取一个整数键；缺失或类型不符按 0（"不知道"如实表达为 0）。
func intFact(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	default:
		return 0
	}
}

// firstPrompt 把两段正文摆成首轮消息：system 在前、user 在后。
//
// 内核那两块不由插件贡献，位置也固定（FR-7.8）：system 段 = 插件正文 + **内核条款**
// （末尾追加），user 段 = **环境事实**（最前面）+ **门禁名** + 插件正文。插件只交正文，
// 插不进第三段、也删不掉内核条款与环境事实——它们在这里生成，插件没有表达"删除"的途径。
// 插件正文为空时它不占位置（不留空行），内核那两块照旧。
func firstPrompt(system, user string, b Bounty, gates []Gate) []llm.Message {
	msgs := make([]llm.Message, 0, 2)
	if body := joinBlocks(system, kernelClauses()); body != "" {
		msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: body})
	}
	if body := joinBlocks(environmentFacts(b), gateFacts(gates), user); body != "" {
		msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: body})
	}
	return msgs
}

// resumeConditionsLead 是"本次投递的补充条件"这条新 user 消息的固定定位语。它是请求体里
// **独有**的子串（不撞提示词、内核条款与材料）——e2e 据此断言"新条件排在回灌历史之后"。
const resumeConditionsLead = "本次投递的补充条件："

// resumeConditions 把本次 Bounty 的任务正文摆成一条追加在回灌历史之后的 user 消息。仅恢复时追加。
func resumeConditions(b Bounty) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: resumeConditionsLead + b.Task}
}

// joinBlocks 按顺序拼接非空块（各自 TrimSpace），空块不留下多余空行。
func joinBlocks(blocks ...string) string {
	kept := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if t := strings.TrimSpace(b); t != "" {
			kept = append(kept, t)
		}
	}
	return strings.Join(kept, "\n\n")
}

func (s *Session) systemPlugins(storage workspace.Storage) []PromptPlugin {
	if s.cfg.SystemPlugins == nil {
		return nil
	}
	return s.cfg.SystemPlugins(storage)
}

func (s *Session) userPlugins(storage workspace.Storage) []PromptPlugin {
	if s.cfg.UserPlugins == nil {
		return nil
	}
	return s.cfg.UserPlugins(storage)
}

func (s *Session) emitNotices(groups ...[]Notice) {
	if s.cfg.Sink == nil {
		return
	}
	for _, notices := range groups {
		for _, n := range notices {
			_ = s.cfg.Sink.Emit(ExternalEvent{Type: "degraded", Payload: map[string]any{
				"scope": n.Scope, "subject": n.Subject, "reason": n.Reason,
			}})
		}
	}
}

// emitAssistantText 上报本轮收流合并后的正文。
//
// 它**与进上下文历史的正文是同一份**（见 OnTurn 的调用点：排在过滤器之后、与自陈解析同一
// 文本）：平台据此拿到模型的答复，而"任务就是要产出一份小结"时那份答复就是交付物。缺出口
// 时是空操作。
func (s *Session) emitAssistantText(text string) {
	if s.cfg.Sink == nil {
		return
	}
	_ = s.cfg.Sink.Emit(ExternalEvent{Type: "assistant_text", Payload: map[string]any{"text": text}})
}

// ResultFilter 在「工具已执行、还没落历史」之间加工本轮结果，是轮级的扩展点：
// 拿得到整轮（可跨调用批量处理），改的是模型下一轮将要看到的那份文本。
//
// 位置是刻意的——它排在落历史之前，所以过滤后的文本一定会进下一轮上下文；排在这之后
// 的任何加工都只能改到副本（历史是值拷贝），做完就丢。过滤器因此必须在这一步把活干完。
//
// 顺序即装配顺序：前一个过滤器的输出是后一个的输入。
type ResultFilter func(ctx context.Context, turn *harness.Turn) error

// OnTurn 在每个轮边界调用：执行本轮工具调用、加工结果、记录本轮、提交检查点、组装下一轮
// 输入、做守卫（取消/预算），返回「下一轮是否继续」。
//
// 它是唯一的轮级 handler——Before 与 After 本是同一个边界的两侧，于是合并到这里：
// 「拿本轮结果、处理成下一轮的输入」这一个动作，连同执行、加工、记录、守卫与检查点都在
// 这里完成。
func (s *Session) OnTurn(ctx context.Context, run *harness.Run, turn *harness.Turn) (bool, error) {
	// 本轮期间处于「执行工具」阶段（执行、加工、记录、检查点），返回后回到默认态「等模型」。
	s.setPhase(PhaseTools)
	defer s.setPhase(PhaseInfer)

	// 处置是本轮的：每轮开工先复位止损观测，避免上一轮的失败状态泄漏到本轮、把后面每一轮
	// 都判成终止（观测在 executeCall 里逐次累加，复位必须早于任何 executeCall）。
	s.stopLoss, s.stopMsg = StopContinue, ""

	// 入口守卫：取消优先——已取消就不再开工，也不让通道问题改写取消。
	if ctx.Err() != nil {
		run.SetTerminal(harness.Terminal{Status: harness.StatusCancelled, Reason: "cancelled", Code: harness.ExitCancelled})
		return false, nil
	}
	// 轮前复查（L2）：上一轮之后通道若断了（含 hunt_start 就写失败），本轮一个工具都不该执行——
	// 消费者已不在通道上，继续跑只是在烧预算地自说自话。
	if err := s.channelFailure(); err != nil {
		run.SetTerminal(harness.Terminal{Status: harness.StatusFailed, Reason: eventChannelFailed + ": " + err.Error(), Code: harness.ExitEnv})
		return false, nil
	}

	// 工具调用由业务执行：harness 只从流里收齐「要调什么」，执行、落盘、结果回灌都在这里。
	//
	// 已经有结果的调用不再执行——排在前面 handler 可以直接答复某次调用（缓存命中、一眼
	// 可见的非法规格），把它从执行里摘出去，而不是被再执行一遍。这类调用**不发 tool_call**：
	// 它没有被系统执行，发出去就成了"提了却没结果"的悬空事件。
	answered := make(map[string]bool, len(turn.Results))
	for _, r := range turn.Results {
		answered[r.CallID] = true
	}
	for _, tc := range turn.Calls {
		if answered[tc.ID] {
			continue
		}
		res := s.executeCall(ctx, turn, tc)
		turn.Results = append(turn.Results, res)
		if res.IsError {
			turn.Failed = true
		}
	}

	// 结果加工：按序跑过滤器，改的是「即将发给模型的文本」。失败即上抛——脱敏、截断一类
	// 过滤器挂掉时若静默放行，未处理的内容就会原样进下一轮上下文，这正是它们要防的事。
	for _, f := range s.cfg.Filters {
		if err := f.Run(ctx, turn); err != nil {
			return false, err
		}
	}

	// 收流合并后的正文：上报 assistant_text、记入"最后一轮答复"，**与进历史、自陈解析的是
	// 同一份**。排在过滤器之后，才保证三者同源（过滤器可以改 turn.Text）；正文为空则不占
	// 位置——没有答复就没有可交付的答复，发一条空文本只会给平台添噪音。
	if strings.TrimSpace(turn.Text) != "" {
		s.emitAssistantText(turn.Text)
		s.lastText = turn.Text
	}

	// 登记自陈清单：它记的是「模型说过什么」，不是历史的副本，所以排在记录之前。
	s.AppendDeclared(turn.Text)

	// 记录本轮：一份进上下文历史（下一轮组装的依据），一份进会话材料（崩溃后恢复）。
	if s.cfg.Context != nil {
		s.cfg.Context.Append(*turn)
	}
	if s.cfg.Session != nil {
		s.cfg.Session.RecordTurn(*turn)
	}
	// 记账（含 recordUsage）**先于**快照：否则本轮用量要等下一次快照才落盘，末轮的就永远进不了
	// 交付提交（这正是"写对了但没交上去"的根因）。快照仍排在 checkpoint 之前——检查点的 add -f
	// 才会把本轮材料一起提交进去。
	s.charge(run.Usage)
	s.snapshot()

	s.checkpoint(ctx, turn)

	// 下一轮的输入 = 提示词 + 历史（含刚记下的本轮）。
	if s.cfg.Context != nil {
		run.Messages = s.cfg.Context.Assemble()
	}

	// 守卫：取消优先于通道、通道优先于预算。
	if ctx.Err() != nil {
		run.SetTerminal(harness.Terminal{Status: harness.StatusCancelled, Reason: "cancelled", Code: harness.ExitCancelled})
		return false, nil
	}
	// 轮末复查（L6）：本轮发出时若把通道写断了，本轮工具已执行完（那是已发生的事实），
	// 但不再进入下一轮。
	if err := s.channelFailure(); err != nil {
		run.SetTerminal(harness.Terminal{Status: harness.StatusFailed, Reason: eventChannelFailed + ": " + err.Error(), Code: harness.ExitEnv})
		return false, nil
	}
	// 提交连败（环境问题：远端不可达）——排在预算之前：连败是更根因的诊断，不该被"预算也正好
	// 耗尽"盖掉。达上限即本轮结束收敛，不跑完剩余轮次。
	if s.commitFailStreak >= checkpointFailStreakLimit {
		last := ""
		if s.commitLastErr != nil {
			last = s.commitLastErr.Error()
		}
		run.SetTerminal(harness.Terminal{
			Status: harness.StatusFailed,
			Reason: checkpointFailedStreak + ": 连续 " + strconv.Itoa(s.commitFailStreak) + " 次检查点提交失败：" + last,
			Code:   harness.ExitEnv,
		})
		return false, nil
	}
	// 业务止损：连续同类失败超上限 / 连续拒绝达阈值 → 终止（退出 2）。排在提交连败之后（连败是
	// 环境问题、退出 1、修好可重跑，更根因）、排在预算之前（同为退出 2，但止损说得出撞的是哪堵
	// 墙，比"预算耗尽"更可诊断）。止损内部：同类失败先于连续拒绝——同轮两者皆成立时上报同类失败，
	// 固定次序只为"终态不取决于谁先被读到"。
	if s.cfg.Policy != nil {
		if s.stopLoss == StopTerminate {
			run.SetTerminal(harness.Terminal{Status: harness.StatusFailed,
				Reason: stopLossSameKind + "：" + s.stopMsg, Code: harness.ExitAborted})
			return false, nil
		}
		if n, over := s.cfg.Policy.DeniedCount(); over {
			run.SetTerminal(harness.Terminal{Status: harness.StatusFailed,
				Reason: stopLossDenied + "：连续 " + strconv.Itoa(n) + " 次策略拒绝", Code: harness.ExitAborted})
			return false, nil
		}
	}
	if s.cfg.Policy != nil {
		if yes, dim := s.cfg.Policy.Exhausted(TurnNo(turn.No)); yes {
			run.SetTerminal(harness.Terminal{Status: harness.StatusFailed, Reason: "budget_exhausted:" + dim, Code: harness.ExitAborted})
			return false, nil
		}
	}
	// 换策略提示：达 switch 阈值但本轮不终止时，把提示附在下一轮消息末尾——**只影响下一轮、不进
	// 历史**：这是一次性纠偏，写进历史反而污染上下文；也刻意不扩 ContextBuilder 接口（那会波及
	// 上下文替身与会话材料），只在组装好的消息末尾追加一条 user 消息。
	if s.stopLoss == StopSwitch && s.stopMsg != "" {
		run.Messages = append(run.Messages, llm.Message{Role: llm.RoleUser, Content: s.stopMsg})
		s.logf("info", "连续同类失败达阈值：已注入换策略提示", "turn", turn.No)
	}
	return true, nil
}

// checkpointFailStreakLimit 是"连续提交失败"的上限：达上限即收敛为环境错误。用包内常量而不是
// 可配字段——没有第二个取值就不留旋钮（同 `Session.structuralJudge` 的取舍）。
const checkpointFailStreakLimit = 3

// checkpointFailedStreak 是「提交连败」的固定原因前缀（使用手册 §7 的"检查点连续提交失败达上限"
// 一档）；后接连败次数与最后一次失败原因，便于远程定位。
const checkpointFailedStreak = "checkpoint_failed_streak"

// stopLossSameKind / stopLossDenied 是业务止损两条轴的固定原因前缀（使用手册 §7 的两行）；分别
// 后接"连续 N 次同类失败（kind）"的诊断尾巴与连续拒绝次数，便于远程定位撞的是哪堵墙。
const (
	stopLossSameKind = "stop_loss_same_kind"
	stopLossDenied   = "stop_loss_denied"
)

// checkpoint 在本轮已应用的改动上产生检查点。触发条件只有两条：
//   - **模型显式请求**（`checkpoint` 原语的语义判断——它说「这里自洽」，那是它的判断）；
//   - 落在**结构完整点**上（语法自洽，改动封闭在符号区间内）。
//
// 刻意**没有「按轮提交」这一档**：语法残缺的中间态钉在分支上价值很低——残次品不是可用的
// 检查点，还会污染提交链。判据**不可判定时不提交**（符号能力不可用，或这份文件判不了）：那时我们
// 并没有判过，只是没法判——宁可不留检查点，也不留残次品；收尾的交付提交照常，改动不会因此丢失。
//
// 单次失败不终止：提交是累积的，下一轮会把所有未提交的改动一并带上。
func (s *Session) checkpoint(ctx context.Context, turn *harness.Turn) {
	intent, requested := s.consumeCheckpoint()
	intent = sanitizeIntent(intent)

	if len(s.ops) == 0 {
		if requested {
			s.logf("info", "模型请求了检查点，但本轮无未提交改动，跳过", "turn", turn.No)
		}
		return
	}
	// 模型显式请求不受判据影响；否则先看门禁、再按结构判据三态决定。
	if !requested {
		// 门禁未通过 → 抑制后续自动检查点（FR-5.2c）：不合格的中间态不值得钉在分支上。
		// 直到下一次门禁通过（或收尾的交付提交）才恢复。
		if s.gateFailed {
			s.logf("info", "本轮不产生自动检查点：门禁尚未通过", "turn", turn.No)
			return
		}
		switch s.judgeStructural(ctx) {
		case structuralPass:
			// 落在结构完整点上：照常提交。
		case structuralFail:
			// 判据可用、但本轮改动没通过：不提交。措辞与"不可判定"必须可区分。
			s.logf("info", "本轮不产生自动检查点：未落在结构完整点", "turn", turn.No)
			return
		case structuralUndecidable:
			// **没法判**（符号能力不可用、或这份文件判不了）——不等于"没通过"。不提交，并如实上报。
			s.logf("info", "本轮不产生自动检查点：结构判据不可判定（符号能力不可用或该语言未注册）", "turn", turn.No)
			s.emitStructuralUndecidableOnce()
			return
		}
	}

	cm, err := s.cfg.Git.Commit(ctx, s.cfg.Bounty.Repo, checkpointMessage(s.cfg.Bounty, TurnNo(turn.No), requested, intent))
	if err != nil {
		// 单次失败自愈（提交是累积的，下一轮把未提交改动一并带上），但要记连败：连续失败意味着
		// 远端不可达，那时由 OnTurn 守卫收敛（这里只记事实，不设终态）。
		s.commitFailStreak++
		s.commitLastErr = err
		s.logf("warn", "阶段性提交失败，将在下一轮重试", "turn", turn.No, "err", err.Error(), "streak", s.commitFailStreak)
		return
	}
	// 一次成功的往返即自愈：连败归零（`Created=false` 的空操作也是一次成功往返，同样归零）。
	s.commitFailStreak = 0
	s.commitLastErr = nil
	s.commit = &cm
	// 待提交改动已随这次提交进分支：结构判据下一轮从新的起点算起（判据问的是"这批改动"，
	// 不是"这一路走来的每一笔"）。
	s.pendingFrom = len(s.ops)
	if cm.Created {
		s.logf("info", "已创建检查点", "turn", turn.No, "model_requested", requested)
	} else {
		// 本轮没有新改动：提交是空操作（FR-1.3c）。日志不能报"已创建"。
		s.logf("info", "本轮无新改动，未产生提交", "turn", turn.No)
	}
}

// structuralVerdict 是结构判据（自动检查点的唯一判据）的三态结果。
//
// 三态是刻意的：**"不可判定"不等于"没通过"**。判据由符号能力提供（「语法自洽（ParseOK）＋
// 改动区间封闭在某个符号内」）；能力不可用或语言未注册时我们并不知道改动是否落在结构完整点上，
// 只是**没法判**。
// 把两者混为一谈，接真判据的人会以为这段已工作，运维也无从区分两种沉默。
type structuralVerdict int

const (
	// structuralPass：落在结构完整点上 → 自动检查点照常提交。
	structuralPass structuralVerdict = iota
	// structuralFail：判据可用、但本轮改动未落在结构完整点上 → 不提交。
	structuralFail
	// structuralUndecidable：**没法判**（符号能力不可用、或这份文件判不了）→ 不提交，并如实上报。
	structuralUndecidable
)

// judgeStructural 给出结构判据的三态：自动检查点的**唯一**判据。
//
// 两条判据分工明确，取样时点也不同，这是刻意的：
//
//   - 判据①「语法完整」在**判定时**取：检查点要钉的正是此刻的内容，所以要问"现在这些文件
//     语法完整吗"，而不是"改到一半时完不完整"；
//   - 判据②「改动封闭在某个符号内」在**写盘前**就地判过、记在 opEnclose 里：字节区间只有
//     在它产生的那一瞬才与文件内容对齐，事后再问，同一文件内的后续编辑已经把区间平移了。
//
// 取样范围是**尚未提交的改动**（s.ops[pendingFrom:]）：检查点提交的是这一批，判据也就只
// 问这一批。三态的收敛次序固定——**已判出的否定结论优先于"没判过"**：一处改动确实越界了，
// 那比"另一处没法判"更该决定结论；两条都没有异议才算通过。
func (s *Session) judgeStructural(ctx context.Context) structuralVerdict {
	if s.ext == nil {
		return structuralUndecidable
	}
	pending := s.ops[s.pendingFrom:]
	if len(pending) == 0 {
		// 没有待提交的改动：没有可反对的东西。判据②无从取样，判据①也无处下手——
		// 这趟提交会是一次空操作，用不着拿判据去拦。
		return structuralPass
	}

	verdict := structuralPass

	// 判据②：写盘前就地判下的封闭性结论（与 s.ops 平行，起点同为 pendingFrom）。
	for i := s.pendingFrom; i < len(s.ops) && i < len(s.opEnclose); i++ {
		switch s.opEnclose[i] {
		case structuralFail:
			return structuralFail
		case structuralUndecidable:
			verdict = structuralUndecidable
		}
	}

	// 判据①：此刻这些文件语法完整吗。同一文件只问一次——问的是文件，不是改动笔数。
	seen := map[string]bool{}
	for _, op := range pending {
		if seen[op.File] {
			continue
		}
		seen[op.File] = true
		switch s.ext.Parse(ctx, op.File) {
		case ext.ParseBroken:
			return structuralFail
		case ext.ParseUnknown:
			verdict = structuralUndecidable
		}
	}
	return verdict
}

// judgeEnclosure 在**写盘之前**就地判一次"这次改动是否封闭在某个符号内"（判据②），
// 与编辑计划一一对应。
//
// 判不出来不阻断写盘：判据只决定"值不值得在这里留检查点"，不决定"能不能改"。
func (s *Session) judgeEnclosure(ctx context.Context, edits []workspace.FileEdit) []structuralVerdict {
	out := make([]structuralVerdict, len(edits))
	if s.ext == nil {
		for i := range out {
			out[i] = structuralUndecidable
		}
		return out
	}
	for i, e := range edits {
		if e.ByteRange == (workspace.ByteRange{}) {
			// 整文件写入：**文件本身就是单位**，没有"封闭在哪个符号内"可言——判据②对它不适用，
			// 由判据①（写完之后语法完整）单独覆盖。把新建文件判成"不封闭"会让新建文件
			// 永远留不下检查点。
			out[i] = structuralPass
			continue
		}
		_, found, err := s.ext.Enclose(ctx, ext.EncloseRequest{File: e.File, ByteRange: e.ByteRange})
		switch {
		case err != nil:
			// 后端这次判不了（语言未注册、读不到、语法残缺）→ 没判过。
			out[i] = structuralUndecidable
		case !found:
			// 判过、确实没有声明包含它：改动横跨了符号边界，或落在声明之间。
			out[i] = structuralFail
		default:
			out[i] = structuralPass
		}
	}
	return out
}

// emitStructuralUndecidableOnce 上报一次「结构判据不可判定」——**每次运行最多一条**：这是运行级的
// 常量事实，不是每轮新闻；每轮一条会把事件流刷满（与 usage 降级同一取舍）。只在真的要用判据时
// 调用（`len(s.ops) == 0` 的早退路径不碰它：没有改动可提交时"判据不可用"毫无信息量）。
func (s *Session) emitStructuralUndecidableOnce() {
	if s.structuralDegradedReported {
		return
	}
	s.structuralDegradedReported = true
	if s.cfg.Sink == nil {
		return
	}
	_ = s.cfg.Sink.Emit(ExternalEvent{Type: "degraded", Payload: map[string]any{
		"scope":   "checkpoint",
		"subject": "structural",
		"reason":  "结构判据不可判定（符号能力不可用或该语言未注册），本轮不产生自动检查点",
	}})
}

// snapshot 让会话材料落盘，供崩溃后按「tip + 会话材料」恢复。
//
// 失败只降级、不终止：材料丢了最多是崩溃后从头跑，不该把一个正在收敛的任务判失败。
func (s *Session) snapshot() {
	if s.cfg.Session == nil {
		return
	}
	if err := s.cfg.Session.Snapshot(); err != nil {
		s.logf("warn", "会话材料落盘失败，崩溃后将无法恢复", "err", err.Error())
	}
}

// logf 在装了事件出口时记一条人类可读日志。日志是诊断，不是装配的必需件：
// 没有出口时它是空操作，而不是让每个调用点各写一遍 nil 判断。
func (s *Session) logf(level, msg string, kv ...any) {
	if s.cfg.Sink == nil {
		return
	}
	s.cfg.Sink.Log(level, msg, kv...)
}

// channelFailure 返回事件出口的断线原因（无出口时 nil）。健康状态的唯一判据是出口自己记着的
// 第一次写失败——Session 不另记一份（两份必然漂）。它同时覆盖事件与心跳两条写路径。
func (s *Session) channelFailure() error {
	if s.cfg.Sink == nil {
		return nil
	}
	return s.cfg.Sink.Failed()
}

// eventChannelFailed 是「事件通道断裂」的固定原因前缀（使用手册 §7 的 stdout 写失败一档）；
// 后接出口给出的原因，便于远程定位。
const eventChannelFailed = "event_channel_failed"

// openRecorder 把材料位置交给记录器。失败只降级、不阻断（IA-6.6）：材料丢了最多是崩溃后从头跑，
// 不该把一个正在收敛的任务判失败。
func (s *Session) openRecorder(root string) {
	if s.cfg.Session == nil {
		return
	}
	if err := s.cfg.Session.Open(root); err != nil {
		s.logf("warn", "会话材料存放位置不可用，崩溃后将无法恢复", "err", err.Error())
	}
}

// recordOps 把本轮产出的写操作交给记录器：与 `s.ops` 的累积同源，不另开一份数据。
func (s *Session) recordOps(ops []WriteOp) {
	if s.cfg.Session == nil {
		return
	}
	for _, op := range ops {
		s.cfg.Session.RecordOp(op)
	}
}

// recordUsage 把本轮用量增量交给记录器：与计费、usage 事件同源（同一份增量）。
func (s *Session) recordUsage(u llm.Usage) {
	if s.cfg.Session == nil {
		return
	}
	s.cfg.Session.RecordUsage(u)
}

// charge 把本轮新增用量转交策略，并上报一次**增量** usage 事件。
//
// run.Usage 是累计值，而策略与事件要的都是增量，所以自己记水位：没有水位就会把累计值
// 反复当增量上报，预算会被自己的重报耗尽、平台也会把同一笔代价重复记账。增量在水位这一处
// 算出、**上报与计费同源**，不在别处再算一遍。
func (s *Session) charge(total llm.Usage) {
	in := total.InputTokens - s.charged.InputTokens
	out := total.OutputTokens - s.charged.OutputTokens
	cached := total.CachedInputTokens - s.charged.CachedInputTokens
	s.charged = total

	// 负增量（上游重报或乱序到达）按 0 处理，与策略侧口径一致。
	if in < 0 {
		in = 0
	}
	if out < 0 {
		out = 0
	}
	if cached < 0 {
		cached = 0
	}

	// 三个字段全零 = 本轮上游没有回报用量。**不发**：一条全零的 usage 会被读成"这轮不花钱"，
	// 那是假数字——用量不可得时不得估算、更不得报 0 装作有数。
	//
	// 同一判据（有任一非零增量）也决定收尾的 usage.reported：这里一旦见到非零就置真，收尾
	// 据此决定 reported 与是否发 degraded——**判据只有这一处**，不在收尾另算。
	if in != 0 || out != 0 || cached != 0 {
		s.usageReported = true
		s.emitUsage(in, out, cached)
		// 同一份增量也交给记录器：材料含用量、"每轮增量"因此只有一处计算。
		s.recordUsage(llm.Usage{InputTokens: in, OutputTokens: out, CachedInputTokens: cached})
	}

	if s.cfg.Policy == nil || (in <= 0 && out <= 0) {
		return
	}
	// 策略只吃输入／输出两项（它管的是预算，不关心缓存明细），且拿的是增量——语义不动。
	s.cfg.Policy.Charge(llm.Usage{InputTokens: in, OutputTokens: out})
}

// emitUsage 上报本轮用量的**增量**（配对的是这一轮）。字段与使用手册 §5 一致，每轮末发。
func (s *Session) emitUsage(in, out, cached int) {
	if s.cfg.Sink == nil {
		return
	}
	_ = s.cfg.Sink.Emit(ExternalEvent{Type: "usage", Payload: map[string]any{
		"input_tokens":        in,
		"output_tokens":       out,
		"cached_input_tokens": cached,
	}})
}

// Finalize 循环后一次（无论成败）：无产出判失败、交付提交、差异、材料落盘、清理、终态上报。
func (s *Session) Finalize(ctx context.Context, run *harness.Run) error {
	s.setPhase(PhaseFinalize)

	// 用量口径冻结一次：终态 hunt_end 事件与结果文件读的是**同一份**（各算一遍迟早会漂）。
	// Reported 的判据来自 charge（上游回报过用量），这里只搬运、不重算。
	s.usage = UsageReport{
		Reported:          s.usageReported,
		InputTokens:       run.Usage.InputTokens,
		OutputTokens:      run.Usage.OutputTokens,
		CachedInputTokens: run.Usage.CachedInputTokens,
		Turns:             run.Usage.Turns,
		ElapsedMS:         run.Usage.Elapsed.Milliseconds(),
	}

	// 会话增量冻结一次：**仅恢复时**定型（未恢复 → nil，结果文件不带该键）。turns_from / turns_to
	// 以"恢复的轮数"为基（按**记录条数**算，不取 turn no 的最大值——恢复后本趟轮号从 1 重新起计），
	// ops_count 是**本次运行**的写操作数（恢复的写操作不并入本次，工作区已由分支 tip 给出）。
	if s.resumed != nil {
		from := len(s.resumed.Turns) + 1
		s.delta = &SessionDelta{
			TurnsFrom: from,
			TurnsTo:   from - 1 + run.Usage.Turns,
			OpsCount:  len(s.ops),
		}
	}

	// 模型自陈优先于「无工具调用」的默认推断，但让位于机制性终止。
	//
	// 方向是单向的：引擎给的 succeeded 只是「没人声明时的默认值」，不是结论，所以可以被
	// 自陈改写；而预算耗尽、止损、取消是机制给的结论，不该被一句自陈抹掉。因此只在引擎
	// 给出 succeeded 时才采纳「## 需要补全」——这正是「机制性终止由引擎强制给出」的落点。
	//
	// 这一步必须排在下发 hunt_end 之前：否则事件里带的是覆盖前的 succeeded，与结果文件
	// 不一致，而平台正是按 status 分类处置的。
	if out := run.Outcome(); out.Status == harness.StatusSucceeded && len(s.declared.Needs) > 0 {
		run.SetTerminal(harness.Terminal{
			Status: harness.StatusBlocked, Reason: reasonNeedsInput, Code: harness.ExitOK,
		})
	}

	// 自陈如实上报：终态是 blocked 还是别的，与「说过什么」无关，两类清单都要发出去。
	s.emitDeclarations()

	// 材料在**交付提交之前**落盘：写在工作树里的记录若排在提交之后，就进不了交付提交，随后还会被
	// Clean 连同工作树一起删掉——末轮的用量/记录会因此"写对了但没交上去"。
	s.snapshot()

	// 交付提交：失败也尽力，补丁仍可单独产出。
	//
	// 这里刻意**不设"无产出即失败"的闸门**：任务不一定改代码——任务内容本身可能就是
	// "产出一份小结"，那份最终答复就是交付物；按写操作数量判失败会把它误杀。
	// 交付物是否存在，看结果文件里的改动清单与最终答复，而不是看有没有落盘。
	if len(s.ops) > 0 {
		cm, err := s.cfg.Git.Commit(ctx, s.cfg.Bounty.Repo, deliveryMessage(s.cfg.Bounty))
		if err != nil {
			s.logf("warn", "交付提交失败，将只产出补丁", "err", err.Error())
		} else {
			s.commit = &cm
		}
	}

	// 门禁补跑：主路径是模型自己调 `check`，兜底是收尾把**尚未跑过的** required 门禁跑一遍——
	// `required` 门禁从未运行意味着这次交付没有质量证据（FR-5.2f）。排在交付提交之后：那时
	// 工作区正是最终交付物，跑出来的结论才有针对性；门禁自身的产物也不会被提交进去。
	s.runRequiredGates(ctx, run)

	// 附带交付物在这里定型：Clean 之后就再也取不到了，而结果文件（FR-1.5、§6）
	// 要用它们。因此**不依赖事件出口**——没有 sink 时同样要能交出补丁与清单。
	if files, err := s.cfg.Git.Diff(ctx, s.cfg.Bounty.Repo); err != nil {
		s.logf("warn", "改动清单不可用，结果文件将缺 files_changed", "err", err.Error())
	} else {
		s.files = files
		if s.cfg.Sink != nil {
			_ = s.cfg.Sink.Emit(ExternalEvent{Type: "deliverable", Payload: map[string]any{"files": files}})
		}
	}
	if patch, err := s.cfg.Git.Patch(ctx, s.cfg.Bounty.Repo); err != nil {
		s.logf("warn", "补丁不可用，将只交付分支 tip", "err", err.Error())
	} else {
		s.patch = patch
	}

	if err := s.cfg.Git.Clean(ctx); err != nil {
		s.logf("warn", "工作区清理失败", "err", err.Error())
	}

	// 先把心跳停死（幂等、阻塞到心跳 goroutine 退出）：停完之后一条心跳也不会再发，
	// hunt_end 的"最后一条"因此是结构保证，不靠时序运气。
	s.stopHeartbeat()

	out := run.Outcome()

	// 门禁结论改写终态：**只在引擎给出 succeeded 时**才改写——与自陈同一条单向规则：
	// 机制性终止（预算、止损、取消）是引擎给的结论，不该被门禁抹掉。
	// 退出码仍是 0：对话正常走完，失败由证据给出（FR-5.2d），改动照常交付（FR-6.5）。
	if out.Status == harness.StatusSucceeded {
		if reason := s.gateVerdict(); reason != "" {
			run.SetTerminal(harness.Terminal{Status: harness.StatusFailed, Reason: reason, Code: harness.ExitOK})
			out = run.Outcome()
		}
	}

	// 用量不可得时如实标注：收尾只跑一次，因此**最多一条**。排在终态之前——平台读到终态前
	// 就该知道"各项为 0 不代表真的没用"。
	if !s.usage.Reported {
		s.emitUsageUnavailable()
	}

	// 失败才是错误：`error` 供程序按 kind 分流、按 retryable 决定是否重派。取消不算错误；
	// `blocked` 是模型的正常判断，它"缺什么"已由 needs_input 逐条报出——这两类都不发。
	if out.Status == harness.StatusFailed {
		s.emitError(out, run)
	}

	// 终态上报：保证「一定有结论发出」，否则调用方会一直等下去。**必须是最后一条**——
	// 它带累计用量（与结果文件同一份），平台读到它即可记账。
	if s.cfg.Sink != nil {
		_ = s.cfg.Sink.Emit(ExternalEvent{Type: "hunt_end", Payload: map[string]any{
			"status": string(out.Status), "reason": out.Reason, "usage": s.usage,
		}})
	}
	return nil
}

// reasonNeedsInput 是「缺条件停下」的固定原因，也是对应的事件名（使用手册 §5/§7 的
// 契约取值）——两处共用一个常量，避免改一处忘一处。
const reasonNeedsInput = "needs_input"

// emitUsageUnavailable 发一条 degraded：上游未回报用量。它是**非致命**的——任务照常收敛，
// 只是"花了多少"不可得。形状与使用手册 §5 的三字段一致（scope / subject / reason）。
func (s *Session) emitUsageUnavailable() {
	if s.cfg.Sink == nil {
		return
	}
	_ = s.cfg.Sink.Emit(ExternalEvent{Type: "degraded", Payload: map[string]any{
		"scope":   "usage",
		"subject": "upstream",
		"reason":  "上游未回报用量：各项为 0 不代表真的没用，本次实际用量不可得",
	}})
}

// emitError 发一条结构化错误：`kind` 供程序分支，`retryable` 与退出码同源，`context` 给人
// 定位（阶段由 kind 表达，这里给轮次位置）。只在终态失败时发。
func (s *Session) emitError(out harness.Outcome, run *harness.Run) {
	if s.cfg.Sink == nil {
		return
	}
	_ = s.cfg.Sink.Emit(ExternalEvent{Type: "error", Payload: map[string]any{
		"kind":      ErrorKind(out.Reason),
		"retryable": RetryableForExitCode(out.ExitCode),
		"context":   failureContext(run),
	}})
}

// failureContext 给失败一个给人看的定位线索：未进入对话时明确说"首轮之前"，否则给出轮次。
func failureContext(run *harness.Run) string {
	if run.Usage.Turns > 0 {
		return "turn " + strconv.Itoa(run.Usage.Turns)
	}
	return "首轮之前"
}

// emitDeclarations 上报模型自陈的两类清单。
//
// 逐条发而不是打包：`needs_input` / `assumption` 的载荷形状是 `{text}`（使用手册 §5），
// 平台按条转成待办与风险项。缺出口时是空操作——自陈是诊断，不是装配的必需件。
func (s *Session) emitDeclarations() {
	if s.cfg.Sink == nil {
		return
	}
	for _, n := range s.declared.Needs {
		_ = s.cfg.Sink.Emit(ExternalEvent{Type: reasonNeedsInput, Payload: map[string]any{"text": n}})
	}
	for _, a := range s.declared.Assumptions {
		_ = s.cfg.Sink.Emit(ExternalEvent{Type: "assumption", Payload: map[string]any{"text": a}})
	}
}

// ============================================================ 检查点意图与提交信息

// consumeCheckpoint 取走本轮的检查点意图，读过即清。一次意图只兑现一次。
func (s *Session) consumeCheckpoint() (intent string, requested bool) {
	intent, requested = s.checkpointSummary, s.checkpointRequested
	s.checkpointRequested, s.checkpointSummary = false, ""
	return intent, requested
}

// maxIntentRunes 是模型给出的理由进入提交信息时的长度上限。
const maxIntentRunes = 120

// checkpointMessage 合成检查点提交信息。它是业务层的产物：模型只提供素材，不提供格式。
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
// 净化不是洁癖，而是格式不可被素材伪造：只取首行、剥掉控制与格式字符、空白折叠、
// 按上限截断；清完为空就当作没给理由。
func sanitizeIntent(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	s = strings.Map(func(r rune) rune {
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

// runRequiredGates 收尾补跑尚未跑过的 required 门禁（FR-5.2f 的兜底）。
//
// 只补**没跑过**的：跑过但不通过的，模型已经看到了结论，再跑一遍只是多花墙钟；
// 而"跑过"这件事本身才是证据（结果文件按它区分"未通过"与"未运行"）。
//
// 预算已耗尽则**不再补跑**：那时补跑必然以超时收场，而"超时"会被读成环境问题，
// 反而掩盖"这次没有质量证据"这个真正的结论——于是按"未运行"判失败（FR-5.2f）。
func (s *Session) runRequiredGates(ctx context.Context, run *harness.Run) {
	if s.cfg.GateRunner == nil || s.root == "" || len(s.gates) == 0 {
		return
	}
	if s.cfg.Policy != nil {
		if exhausted, dim := s.cfg.Policy.Exhausted(TurnNo(run.Usage.Turns)); exhausted {
			s.logf("warn", "预算已耗尽，不再补跑门禁", "dim", dim)
			s.emitGateRunFailed("", "预算已耗尽（"+dim+"）：未补跑必需门禁，本次交付没有质量证据")
			return
		}
	}
	for _, g := range s.gates {
		if !g.Required {
			continue
		}
		if _, ran := s.latestGateResult(g.Name); ran {
			continue
		}
		res, err := s.cfg.GateRunner.Run(ctx, s.root, g, s.ChangeFingerprint())
		if err != nil {
			// 执行失败≠判过：命令起不来、超时，都是环境退化，不是"代码质量差"。
			// 这条按"未运行"计入终态，并如实上报——绝不能因为跑不起来就当成通过。
			s.logf("warn", "收尾补跑门禁失败", "gate", g.Name, "err", err.Error())
			s.emitGateRunFailed(g.Name, err.Error())
			continue
		}
		s.RecordGateResult(res)
	}
}

// latestGateResult 找同名门禁**最近一次**结论：模型可能反复调同一条门禁，
// 只有最后一次反映当前改动的状态。
func (s *Session) latestGateResult(name string) (GateResult, bool) {
	return LatestGateResult(s.gateResults, name)
}

// LatestGateResult 在一串结论里找同名门禁的**最近一次**。它是包级函数，因为终态判定与
// 结果文件（装配层）都要用同一份"最近结论"的口径——两处各找一遍迟早会选出不同的那一条。
func LatestGateResult(results []GateResult, name string) (GateResult, bool) {
	for i := len(results) - 1; i >= 0; i-- {
		if results[i].Name == name {
			return results[i], true
		}
	}
	return GateResult{}, false
}

// gateVerdict 给出"必需门禁是否构成交付失败"的结论：任一 required 门禁**未通过或从未运行**
// 即失败，返回终态原因；否则空串。
//
// 从未运行与未通过都要报失败：这次交付没有质量证据，与"证据显示不合格"同样不能算成功交付。
// 两者在结果文件里仍然分开表达（passed: null / false），这里只回答"算不算失败"。
func (s *Session) gateVerdict() string {
	var failed []string
	for _, g := range s.gates {
		if !g.Required {
			continue
		}
		res, ran := s.latestGateResult(g.Name)
		switch {
		case !ran:
			failed = append(failed, g.Name+"（从未运行）")
		case !res.Passed:
			failed = append(failed, g.Name)
		}
	}
	if len(failed) == 0 {
		return ""
	}
	return reasonGateFailed + "：" + strings.Join(failed, "、")
}

// reasonGateFailed 是「必需门禁未通过或未运行」的固定原因前缀（使用手册 §7 的一档）。
// 退出码为 0——对话正常走完，失败由证据给出。
const reasonGateFailed = "gate_failed"

// emitGateRunFailed 上报"门禁没能跑起来"：它是非致命的降级，但必须可见——
// 静默跳过会让平台以为这次没有门禁。
func (s *Session) emitGateRunFailed(gate, reason string) {
	if s.cfg.Sink == nil {
		return
	}
	_ = s.cfg.Sink.Emit(ExternalEvent{Type: "degraded", Payload: map[string]any{
		"scope": "gate", "subject": gate, "reason": reason,
	}})
}
