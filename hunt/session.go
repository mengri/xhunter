package hunt

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"xhunter/git"
	"xhunter/harness"
	"xhunter/llm"
	"xhunter/workspace"
)

// ToolFactory 把已就绪的工作区变成有序的原语清单。工作区是运行期产物，原语读文件
// 需要它，因此装配层交工厂、Session 在工作区就绪后调用。顺序即工具面顺序。
type ToolFactory func(ws workspace.Workspace) []Primitive

// Config 是 Session 的装配参数。上下文、会话、事件出口都是业务自己的协作者
// （harness 不认识它们）；原语清单、策略、工作区与 git 的实现同样由装配层注入。
// Filters 是本轮的**结果加工链**，按装配顺序在「工具已执行、还没落历史」之间生效。
// SystemPlugins / UserPlugins 是首轮两段正文的构造插件工厂，顺序即拼接顺序；为 nil
// 表示该段不接插件。
type Config struct {
	Bounty  Bounty
	Tools   ToolFactory
	Policy  Policy
	Opener  workspace.WorkspaceOpener
	Git     git.GitWorktree
	Context ContextBuilder
	Session SessionRecorder
	Sink    EventSink

	Filters []ResultFilter

	SystemPlugins PromptPluginFactory
	UserPlugins   PromptPluginFactory
}

// Session 是「代码编辑 agent」的默认执行体：向 harness 提供三组 handler
// （Prepare / OnTurn / Finalize），同时是原语看到的 Facts（门禁、台账、检查点意图）。
//
// 它自己持有上下文、会话、事件出口与工具——harness 只认识 llm 与 handler，其余一概
// 不认识；工具调用也由它在 OnTurn 里执行。
type Session struct {
	cfg     Config
	storage workspace.Storage
	tools   map[PrimitiveName]Primitive
	order   []PrimitiveName
	ledger  *Ledger
	gates   []Gate
	commit  *git.Commit
	ops     []WriteOp

	checkpointRequested bool
	checkpointSummary   string

	// charged 是已转交策略的累计用量水位：run 上的用量是累计值，策略要的是增量。
	charged llm.Usage
}

// NewSession 构造 Session。
func NewSession(cfg Config) *Session {
	return &Session{cfg: cfg, ledger: &Ledger{}}
}

// CheckpointDecl 是检查点原语的声明。它由业务层自带，装配工具面时殿后追加。
var CheckpointDecl = llm.ToolDecl{
	Name: string(PrimCheckpoint),
	Description: "请求在此刻创建一个检查点。每轮至多生效一次；何时真正提交、提交信息如何组织由系统决定，" +
		"你只需给出 summary：一句话说明为什么这里值得留检查点（做了什么、为什么自洽）。",
	Schema: llm.ObjectSchema(`{
		"summary": {"type": "string", "description": "为什么这里值得留检查点（一句话）"}
	}`, "summary"),
}

// ============================================================ Facts

func (s *Session) Gates() []Gate   { return s.gates }
func (s *Session) Ledger() *Ledger { return s.ledger }

func (s *Session) RequestCheckpoint(summary string) {
	s.checkpointRequested = true
	s.checkpointSummary = summary
}

func (s *Session) CheckpointRequested() bool { return s.checkpointRequested }

// buildTools 用装配层给的工厂构造原语清单，记录顺序（顺序即工具面顺序）。
func (s *Session) buildTools(ws workspace.Workspace) {
	if s.cfg.Tools == nil {
		return
	}
	prims := s.cfg.Tools(ws)
	s.tools = make(map[PrimitiveName]Primitive, len(prims))
	s.order = make([]PrimitiveName, 0, len(prims))
	for _, p := range prims {
		name := PrimitiveName(p.Decl().Name)
		s.tools[name] = p
		s.order = append(s.order, name)
	}
}

// decls 按顺序给出工具声明，检查点原语殿后。
func (s *Session) decls() []llm.ToolDecl {
	out := make([]llm.ToolDecl, 0, len(s.order)+1)
	for _, name := range s.order {
		if p, ok := s.tools[name]; ok {
			out = append(out, p.Decl())
		}
	}
	return append(out, CheckpointDecl)
}

// ============================================================ 工具调用（OnTurn 里执行）

// executeCall 执行一次工具调用：绑定参数 → 查表 → 裁决 → 原语执行 → 统一落盘 → 回灌结果。
//
// **每一次调用都恰好留下一条 tool_result 事件**（AC-28）：绑定失败、名字不认识、
// 策略拒绝、执行报错、落盘失败都算"模型提了一次调用、系统给了一个结果"，一律如实上报。
// 事件里必须带 `call_id`——并行或多调用时它是外部消费者唯一的配对依据（IA-1.5）。
// 用 defer 统一收口，是因为"每条 return 都记得发事件"这种纪律迟早会被漏掉一条。
func (s *Session) executeCall(ctx context.Context, turn *harness.Turn, tc llm.ToolCall) llm.ToolResult {
	started := time.Now()
	ev := toolResultEvent{callID: tc.ID}
	defer func() { s.emitToolResult(ev, time.Since(started)) }()

	call, fault := BindToolCall(tc)
	ev.tool = string(call.Primitive)
	if fault != nil {
		ev.fault = fault
		return llm.ToolResult{CallID: tc.ID, IsError: true,
			Output: resultText(Result{CallID: call.ID, Err: fault})}
	}

	// 检查点原语不读写工作区，单独接走：只把意图转交，真正提交由 OnTurn 完成。
	if call.Primitive == PrimCheckpoint {
		res := s.executeCheckpoint(call)
		ev.summary = res.Output
		return res
	}

	prim, ok := s.tools[call.Primitive]
	if !ok {
		f := &llm.Fault{Kind: "unknown_tool",
			Message: fmt.Sprintf("没有名为 %q 的工具；可用的是：%s", call.Primitive, s.available())}
		ev.fault = f
		return llm.ToolResult{CallID: tc.ID, IsError: true, Output: f.Message}
	}

	if s.cfg.Policy != nil {
		d, err := s.cfg.Policy.Decide(ctx, call)
		if err != nil {
			f := &llm.Fault{Kind: "policy_error", Message: "策略裁决失败：" + err.Error(), Retryable: true}
			ev.fault = f
			return llm.ToolResult{CallID: tc.ID, IsError: true, Output: f.Message}
		}
		if d.Verdict != VerdictAllow {
			// 拒绝要留两条痕迹：回灌给模型的原因（让它换做法），以及外部可见的
			// policy_denied（平台据此看出"模型在撞哪堵墙"）。
			f := &llm.Fault{Kind: "policy_denied", Message: d.Reason}
			ev.fault = f
			s.emitPolicyDenied(call, d.Reason)
			return llm.ToolResult{CallID: tc.ID, IsError: true, Output: d.Reason}
		}
	}

	res, edits, err := prim.Execute(ctx, call, s)
	if err != nil {
		f := &llm.Fault{Kind: "execute_failed", Message: "工具执行失败：" + err.Error(), Retryable: true}
		ev.fault = f
		return llm.ToolResult{CallID: tc.ID, IsError: true, Output: f.Message}
	}
	if res.Err != nil {
		res.CallID = call.ID
		ev.summary = res.Summary
		ev.fault = res.Err
		return UnbindToolResult(res)
	}

	// 落盘统一在这里：原语只产出编辑计划，写盘唯一入口由 Committer 保证。
	if len(edits) > 0 {
		ops, err := newCommitter(s.storage, s.ledger).Commit(edits)
		if err != nil {
			f := &llm.Fault{Kind: "commit_failed", Message: "落盘失败：" + err.Error(), Retryable: true}
			ev.fault = f
			return llm.ToolResult{CallID: tc.ID, IsError: true, Output: f.Message}
		}
		for i := range ops {
			ops[i].Turn = TurnNo(turn.No)
			ops[i].Primitive = call.Primitive
		}
		res.Ops = ops
		s.ops = append(s.ops, ops...)
	}

	res.CallID = call.ID
	res.OK = true
	ev.summary = res.Summary
	return llm.ToolResult{CallID: tc.ID, Output: res.Summary}
}

func (s *Session) executeCheckpoint(call Call) llm.ToolResult {
	summary := call.Summary
	if s.CheckpointRequested() {
		return llm.ToolResult{CallID: string(call.ID),
			Output: "本轮已请求过检查点，无需重复；系统会在合适的时机提交"}
	}
	s.RequestCheckpoint(summary)
	msg := "已记录检查点意图"
	if summary != "" {
		msg += "：" + summary
	}
	msg += "；系统会在合适的时机提交"
	return llm.ToolResult{CallID: string(call.ID), Output: msg}
}

func (s *Session) available() string {
	names := make([]string, 0, len(s.tools))
	for n := range s.tools {
		names = append(names, string(n))
	}
	sort.Strings(names)
	return strings.Join(names, "、")
}

// toolResultEvent 是一次调用要上报的事实：即使失败也必须有 call_id 与工具名。
type toolResultEvent struct {
	callID  string
	tool    string
	summary string
	fault   *llm.Fault
}

// emitToolResult 上报一次工具调用结果。载荷字段与使用手册 §5 的事件契约一一对应：
// `call_id` 用于配对，`tool`/`ok`/`summary` 描述结局，失败再补 `error`/`message`，
// `duration_ms` 供平台侧看单次调用的代价。
func (s *Session) emitToolResult(ev toolResultEvent, elapsed time.Duration) {
	if s.cfg.Sink == nil {
		return
	}
	payload := map[string]any{
		"call_id":     ev.callID,
		"tool":        ev.tool,
		"ok":          ev.fault == nil,
		"summary":     ev.summary,
		"duration_ms": elapsed.Milliseconds(),
	}
	if ev.fault != nil {
		payload["error"] = ev.fault.Kind
		payload["message"] = ev.fault.Message
	}
	_ = s.cfg.Sink.Emit(ExternalEvent{Type: "tool_result", Payload: payload})
}

// emitPolicyDenied 上报一次策略拒绝：平台据此看出模型在撞哪堵墙，而不是只看到
// 一串失败结果（使用手册 §5 的 policy_denied）。
func (s *Session) emitPolicyDenied(call Call, reason string) {
	if s.cfg.Sink == nil {
		return
	}
	payload := map[string]any{
		"call_id": string(call.ID),
		"action":  string(call.Primitive),
		"reason":  reason,
	}
	if call.Target != "" {
		payload["target"] = call.Target
	}
	_ = s.cfg.Sink.Emit(ExternalEvent{Type: "policy_denied", Payload: payload})
}

var _ Facts = (*Session)(nil)
