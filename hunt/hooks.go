package hunt

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"unicode"

	"xhunter/harness"
	"xhunter/llm"
	"xhunter/workspace"
)

// ============================================================ 三组 handler
//
// Session 以三组 handler 的形式接进循环：Prepare（循环前）、OnTurn（每个轮边界）、
// Finalize（循环后）。harness 不认识 Session，只认识这三个函数签名；上下文、会话材料、
// 事件出口、工具执行都由 Session 自己完成。

// Prepare 循环前一次：准备基线、打开工作区、定格工具面、构造首轮提示词。
//
// 工作区在这里就打开并检查——失败即返回错误，循环根本不会开始，因此不存在「跑到一半
// 才发现工作区不可用」的中间态。
//
// 装配缺件也在这里显式失败（FR-1.8：缺件在首轮推理之前失败，退出码 2）。此前
// Git / Opener 缺失是**调用即 panic**（被引擎收敛成一句 "panic: invalid memory
// address"），而 Policy 缺失会被静默跳过——同一份"装配校验"的说法，三种行为。
func (s *Session) Prepare(ctx context.Context, run *harness.Run) error {
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
	storage, err := s.cfg.Opener.Open(root)
	if err != nil {
		return err
	}
	s.storage = storage

	// 工作区就绪后构造原语并定格工具面：原语读文件需要工作区，声明与执行因此同源。
	s.buildTools(storage)
	run.Tools = s.decls()

	// 门禁清单从基线读取（一期暂空）。来源优先级「任务下发 > 仓库声明 > 没有」，
	// 但仓库声明要从基线读而不是工作区读——工作区里的清单模型能改，基线钉住、改不了。
	s.gates = nil

	// 构造首轮两段正文：插件给正文、Session 拼位置与顺序。取一次、整任务内冻结。
	in := PromptInput{Bounty: s.cfg.Bounty, Tools: run.Tools}
	system, err := buildPromptStage(ctx, s.systemPlugins(storage), in)
	if err != nil {
		return err
	}
	user, err := buildPromptStage(ctx, s.userPlugins(storage), in)
	if err != nil {
		return err
	}
	s.emitNotices(system.Notices, user.Notices)

	prompt := firstPrompt(system.Body, user.Body, s.cfg.Bounty)
	if system.Body == "" && user.Body == "" {
		s.logf("warn", "没有任何插件贡献正文：模型只会看到内核条款与环境事实，看不到项目约定与任务描述")
	}

	// 提示词交给上下文持有者，历史也归它——harness 只拿组装好的消息。
	if s.cfg.Context == nil {
		run.Messages = prompt
		return nil
	}
	s.cfg.Context.SetPrompt(prompt)
	run.Messages = s.cfg.Context.Assemble()
	return nil
}

// firstPrompt 把两段正文摆成首轮消息：system 在前、user 在后。
//
// 内核那两块不由插件贡献，位置也固定（FR-7.8）：system 段 = 插件正文 + **内核条款**
// （末尾追加），user 段 = **环境事实**（最前面）+ 插件正文。插件只交正文，插不进
// 第三段、也删不掉内核条款与环境事实——它们在这里生成，插件没有表达"删除"的途径。
// 插件正文为空时它不占位置（不留空行），内核那两块照旧。
func firstPrompt(system, user string, b Bounty) []llm.Message {
	msgs := make([]llm.Message, 0, 2)
	if body := joinBlocks(system, kernelClauses()); body != "" {
		msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: body})
	}
	if body := joinBlocks(environmentFacts(b), user); body != "" {
		msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: body})
	}
	return msgs
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
	// 工具调用由业务执行：harness 只从流里收齐「要调什么」，执行、落盘、结果回灌都在这里。
	//
	// 已经有结果的调用不再执行——排在前面 handler 可以直接答复某次调用（缓存命中、一眼
	// 可见的非法规格），把它从执行里摘出去，而不是被再执行一遍。
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
		if err := f(ctx, turn); err != nil {
			return false, err
		}
	}

	// 记录本轮：一份进上下文历史（下一轮组装的依据），一份进会话材料（崩溃后恢复）。
	if s.cfg.Context != nil {
		s.cfg.Context.Append(*turn)
	}
	if s.cfg.Session != nil {
		s.cfg.Session.RecordTurn(*turn)
	}
	s.snapshot()
	s.charge(run.Usage)

	s.checkpoint(ctx, turn)

	// 下一轮的输入 = 提示词 + 历史（含刚记下的本轮）。
	if s.cfg.Context != nil {
		run.Messages = s.cfg.Context.Assemble()
	}

	// 守卫：取消优先于预算。
	if ctx.Err() != nil {
		run.SetTerminal(harness.Terminal{Status: harness.StatusCancelled, Reason: "cancelled", Code: harness.ExitCancelled})
		return false, nil
	}
	if s.cfg.Policy != nil {
		if yes, dim := s.cfg.Policy.Exhausted(TurnNo(turn.No)); yes {
			run.SetTerminal(harness.Terminal{Status: harness.StatusFailed, Reason: "budget_exhausted:" + dim, Code: harness.ExitAborted})
			return false, nil
		}
	}
	return true, nil
}

// checkpoint 在本轮已应用的改动上产生检查点。触发条件只有两条：
//   - **模型显式请求**（`checkpoint` 原语的语义判断——它说「这里自洽」，那是它的判断）；
//   - 落在**结构完整点**上（语法自洽，改动封闭在符号区间内）。
//
// 刻意**没有「按轮提交」这一档**：语法残缺的中间态钉在分支上价值很低——残次品不是可用的
// 检查点，还会污染提交链。判据**不可判定时不提交**（语言未注册 / 扩展未接入）：宁可不留
// 检查点，也不留残次品；收尾的交付提交照常，改动不会因此丢失。
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
	if !requested && !s.structuralPoint() {
		s.logf("info", "本轮不产生自动检查点：未落在结构完整点", "turn", turn.No)
		return
	}

	cm, err := s.cfg.Git.Commit(ctx, s.cfg.Bounty.Repo, checkpointMessage(s.cfg.Bounty, TurnNo(turn.No), requested, intent))
	if err != nil {
		s.logf("warn", "阶段性提交失败，将在下一轮重试", "turn", turn.No, "err", err.Error())
		return
	}
	s.commit = &cm
	if cm.Created {
		s.logf("info", "已创建检查点", "turn", turn.No, "model_requested", requested)
	} else {
		// 本轮没有新改动：提交是空操作（FR-1.3c）。日志不能报"已创建"。
		s.logf("info", "本轮无新改动，未产生提交", "turn", turn.No)
	}
}

// structuralPoint 报告本轮改动是否落在结构完整点上（自动检查点的唯一判据）。
//
// 判据是「语法自洽（ParseOK）＋ 改动区间封闭在某个符号内」，两者都由符号扩展提供。
// 扩展未接入时**不可判定**——此时按上面的取舍**不提交**：不猜、也不放宽。
func (s *Session) structuralPoint() bool {
	return false
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

// charge 把本轮新增用量转交策略。run.Usage 是累计值，策略要的是增量，所以自己记水位：
// 没有水位就会把累计值反复当增量上报，预算会被自己的重报耗尽。
func (s *Session) charge(total llm.Usage) {
	in := total.InputTokens - s.charged.InputTokens
	out := total.OutputTokens - s.charged.OutputTokens
	s.charged = total
	if s.cfg.Policy == nil || (in <= 0 && out <= 0) {
		return
	}
	if in < 0 {
		in = 0
	}
	if out < 0 {
		out = 0
	}
	s.cfg.Policy.Charge(llm.Usage{InputTokens: in, OutputTokens: out})
}

// Finalize 循环后一次（无论成败）：无产出判失败、交付提交、差异、材料落盘、清理、终态上报。
func (s *Session) Finalize(ctx context.Context, run *harness.Run) error {
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

	// 附带交付物在这里定型：Clean 之后就再也取不到了，而结果文件（FR-1.5、§6）
	// 要用它们。因此**不依赖事件出口**——没有 sink 时同样要能交出补丁与清单。
	if files, err := s.cfg.Git.Diff(ctx, s.cfg.Bounty.Repo.BaseCommit); err != nil {
		s.logf("warn", "改动清单不可用，结果文件将缺 files_changed", "err", err.Error())
	} else {
		s.files = files
		if s.cfg.Sink != nil {
			_ = s.cfg.Sink.Emit(ExternalEvent{Type: "deliverable", Payload: map[string]any{"files": files}})
		}
	}
	if patch, err := s.cfg.Git.Patch(ctx, s.cfg.Bounty.Repo.BaseCommit); err != nil {
		s.logf("warn", "补丁不可用，将只交付分支 tip", "err", err.Error())
	} else {
		s.patch = patch
	}

	// 收尾前再落一次材料：交付提交的哈希只在提交之后才知道，早落的那份会缺它。
	s.snapshot()

	if err := s.cfg.Git.Clean(ctx); err != nil {
		s.logf("warn", "工作区清理失败", "err", err.Error())
	}

	// 终态上报：保证「一定有结论发出」，否则调用方会一直等下去。
	if s.cfg.Sink != nil {
		out := run.Outcome()
		_ = s.cfg.Sink.Emit(ExternalEvent{Type: "hunt_end", Payload: map[string]any{
			"status": string(out.Status), "reason": out.Reason,
		}})
	}
	return nil
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
