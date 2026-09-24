package hunt

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
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
// Filters 是本轮的**结果加工链**，按装配顺序在「工具已执行、还没落历史」之间生效；
// 每个过滤器带名字，名字进生效配置快照。SystemPlugins / UserPlugins 是首轮两段正文的
// 构造插件工厂，顺序即拼接顺序；为 nil 表示该段不接插件。Assembly 是「只有装配层知道」
// 的生效事实（策略口径、检查点行为、扩展指纹、目标平台），值注入、Session 不猜。
// Heartbeat 是任务级心跳的间隔；0 表示取默认（`defaultHeartbeatInterval`），不是「关掉」。
type Config struct {
	Bounty  Bounty
	Tools   ToolFactory
	Policy  Policy
	Opener  workspace.WorkspaceOpener
	Git     git.GitWorktree
	Context ContextBuilder
	Session SessionRecorder
	Sink    EventSink
	// Gates 是门禁清单的来源（承担「Bounty 下发 > 仓库声明 > 无」里的后两档）。它是**装配槽**——
	// 契约见 `GateSource`；**调用点（Prepare 的来源裁决）在 MS-5 接上**，本次不接。
	Gates GateSource

	Filters []NamedFilter

	SystemPlugins PromptPluginFactory
	UserPlugins   PromptPluginFactory

	Assembly  AssemblyFacts
	Heartbeat time.Duration
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

	// effective 是本次 Hunt 的生效配置快照，装配完成后冻结一次（见 Prepare）。起飞事件与
	// 结果文件读的是它同一份；Prepare 未成功时是零值，结果文件据此省略该字段。
	effective EffectiveConfig

	// files / patch 是收尾时定型的附带交付物：工作树一旦回收就再也取不到，
	// 因此必须在此之前取出来（见 Finalize）。
	files []string
	patch string

	// lastText 是模型最后一轮的**非空**正文：它就是"最终答复"（任务可能就是要产出一份小结，
	// 那份答复本身即交付物）。每轮正文非空时覆盖，收尾随 Delivery 交给装配层写结果文件
	// summary——放在这里，是因为它和 files / patch 同属"收尾定型的交付事实"。
	lastText string

	checkpointRequested bool
	checkpointSummary   string

	// commitFailStreak 是**连续**检查点提交失败数（成功即归零，含 Created=false 的空操作）；达
	// checkpointFailStreakLimit 时由 OnTurn 守卫统一收敛——不在这里设终态（SetTerminal 是覆盖式
	// 的，两处都设会让终态取决于执行顺序而不是事实本身）。commitLastErr 保留最后一次失败原因，
	// 收敛时带上以便远程定位。
	commitFailStreak int
	commitLastErr    error

	// charged 是已转交策略的累计用量水位：run 上的用量是累计值，策略与 usage 事件要的都是
	// 增量。
	charged llm.Usage

	// usage 是收尾定型的用量对外口径（含 reported）：终态 hunt_end 事件与结果文件读的**同一
	// 份**（见 Finalize 与 Delivery）。
	usage UsageReport
	// usageReported 记录"上游回报过用量"这一事实：charge 里一旦见到非零增量即置真，收尾据此
	// 决定 usage.reported 与是否发 degraded。判据只有这一处。
	usageReported bool

	// phase 是当前阶段的并发安全快照：心跳 goroutine 读、业务 goroutine 写（`--race` 必须干净），
	// 因此用 atomic.Value 而不是普通字段。见 Phase 与 setPhase。
	phase atomic.Value
	// stopHeartbeat 停掉本次运行的心跳（幂等、阻塞到 goroutine 退出）：Prepare 成功后才有真值，
	// 默认是空操作——所以 Prepare 未成功时 Finalize 照常调用一次也不会炸，也不会漏停。
	stopHeartbeat func()

	// structuralJudge 给出本轮改动的**结构判据三态**；它是判据的**包内可替换位置**——符号扩展
	// 接入时替换它即可（见 structuralVerdict）。NewSession 默认"不可判定"：扩展未接入时我们
	// 并**没有判过**，只是没法判，那与"判过但没通过"是两句话。刻意不给 hunt.Config 加一个
	// 只有一个取值的公开字段。
	structuralJudge func() structuralVerdict
	// structuralDegradedReported 保证"不可判定"的 degraded **每次运行最多一条**。
	structuralDegradedReported bool

	// stopLoss / stopMsg 记录本轮工具调用的止损处置（两段式的观测结果）：executeCall 每次调用
	// 后把结局喂给策略，这里"只升不降"地记下最重的一档（continue < switch < terminate）。OnTurn
	// 每轮开工先复位——处置是本轮的，上一轮的失败不该把后面每一轮都判成终止。
	stopLoss StopLoss
	stopMsg  string

	// declared 是模型在正文里自陈的两类清单（缺什么条件、采取了哪些默认）：轮边界登记、
	// 收尾据此收敛终态——见 AppendDeclared 与 Finalize。
	declared Declared
}

// Delivery 是收尾后可读的交付事实：主交付是分支 tip（Commit），附带交付是改动文件清单、
// 补丁、最终答复与累计用量（FR-6.1）。仓库实现的四个动作只到 Clean 为止，交付物因此在
// Finalize 里定型，并由装配层拿去写结果文件——结果文件与 hunt_end 事件因此读同一份用量。
type Delivery struct {
	Commit  *git.Commit
	Files   []string
	Patch   string
	Summary string
	Usage   UsageReport
}

// Delivery 返回本次执行的交付事实（Finalize 之后调用才有内容）。
func (s *Session) Delivery() Delivery {
	return Delivery{Commit: s.commit, Files: s.files, Patch: s.patch, Summary: s.lastText, Usage: s.usage}
}

// EffectiveConfig 返回本次 Hunt 的生效配置快照（收尾后由装配层读走写结果文件）。
//
// Prepare 尚未成功时是**零值**——没进入对话就没有快照，结果文件据此省略该字段，
// 而不是摆一个空壳（空切片会被读成"没有原语、没有插件"）。
func (s *Session) EffectiveConfig() EffectiveConfig { return s.effective }

// Phase 返回当前阶段（心跳据此上报，诊断也用得上）。并发安全：心跳 goroutine 读它。
func (s *Session) Phase() Phase {
	if p, ok := s.phase.Load().(Phase); ok {
		return p
	}
	return PhaseBootstrap
}

// setPhase 记录当前阶段。业务 goroutine 写、心跳 goroutine 读，故走 atomic.Value。
func (s *Session) setPhase(p Phase) { s.phase.Store(p) }

// observeOutcome 把一次工具调用的结局喂给策略止损（两段式的观测点），并记下处置。
//
// f == nil 表示这次调用成功——喂空串，让策略把"连续同类失败"归零；否则喂失败的 kind。处置
// "只升不降"地记进 stopLoss（continue < switch < terminate）：一轮内可能多次调用，最重的处置
// 胜出——同轮内先 switch 后 terminate 时，不能因为处理顺序反过来把终止降级成换策略。
func (s *Session) observeOutcome(f *llm.Fault) {
	if s.cfg.Policy == nil {
		return
	}
	kind := ""
	if f != nil {
		kind = f.Kind
	}
	sl, msg := s.cfg.Policy.ObserveFailure(kind)
	if stopLossRank(sl) > stopLossRank(s.stopLoss) {
		s.stopLoss, s.stopMsg = sl, msg
	}
}

// stopLossRank 给出处置的严重度秩，供"只升不降"地收敛一轮内的多次观测。
func stopLossRank(sl StopLoss) int {
	switch sl {
	case StopTerminate:
		return 2
	case StopSwitch:
		return 1
	default:
		return 0
	}
}

// NewSession 构造 Session。
func NewSession(cfg Config) *Session {
	return &Session{
		cfg:           cfg,
		ledger:        &Ledger{},
		stopHeartbeat: func() {},
		// 默认判据：不可判定（符号扩展未接入）。三态见 structuralVerdict。
		structuralJudge: func() structuralVerdict { return structuralUndecidable },
		// 本轮处置从"继续"起步；OnTurn 每轮开工再复位一次。
		stopLoss: StopContinue,
	}
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

// AppendDeclared 登记本轮正文里的自陈清单。
//
// 为什么在轮边界登记、而不是收尾时再从上下文里捞：压缩一旦接入，被下压掉的轮次里的
// 自陈就再也取不到（架构 §7.2「在模型说出的当轮就登记」）。现在做这一步零成本，等压缩
// 落地再补就要返工。
func (s *Session) AppendDeclared(text string) {
	d := ParseDeclared(text)
	s.declared.Needs = append(s.declared.Needs, d.Needs...)
	s.declared.Assumptions = append(s.declared.Assumptions, d.Assumptions...)
	s.declared.Unverified = append(s.declared.Unverified, d.Unverified...)
}

// Declared 给出本次运行累积的自陈清单（收尾后可读，供事件与结果文件使用）。
func (s *Session) Declared() Declared { return s.declared }

// buildTools 用装配层给的工厂构造原语清单，记录顺序（顺序即工具面顺序），并校验工具面
// 自身是否成立。
//
// 校验只认「工具面自洽」，不认业务名字——有哪些原语、叫什么，是装配层的知识。之所以由
// 框架兜这一道而不是全押给装配层：重复或匿名的声明会让供应商直接拒收整个请求，而错误
// 现场在工具面，不在这里报就只剩一条无从追起的上游错误。
//
// 返回错误由 Prepare 传播：工具面不成立与缺 git / 缺策略同类——装配期缺件，循环不开始。
func (s *Session) buildTools(ws workspace.Workspace) error {
	if s.cfg.Tools == nil {
		return nil
	}
	prims := s.cfg.Tools(ws)
	s.tools = make(map[PrimitiveName]Primitive, len(prims))
	s.order = make([]PrimitiveName, 0, len(prims))
	for i, p := range prims {
		if p == nil {
			return fmt.Errorf("原语清单第 %d 项没有实现", i+1)
		}
		name := PrimitiveName(p.Decl().Name)
		if name == "" {
			return fmt.Errorf("原语清单第 %d 项没有名字（声明里 Name 为空）", i+1)
		}
		// checkpoint 由执行体自带、由其后的 decls 殿后追加：工厂再给一个就会发出两条同名声明。
		if name == PrimCheckpoint {
			return fmt.Errorf("原语清单不能包含 %q：它由执行体自带、由 decls 殿后追加", PrimCheckpoint)
		}
		if _, dup := s.tools[name]; dup {
			return fmt.Errorf("原语清单里 %q 出现两次", name)
		}
		s.tools[name] = p
		s.order = append(s.order, name)
	}
	return nil
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

// executeCall 执行一次工具调用：发「提出调用」事件 → 绑定参数 → 查表 → 裁决 → 原语执行
// → 统一落盘 → 回灌结果。
//
// **每一次调用都恰好留下一条 tool_result 事件**（AC-28）：绑定失败、名字不认识、
// 策略拒绝、执行报错、落盘失败都算"模型提了一次调用、系统给了一个结果"，一律如实上报。
// 事件里必须带 `call_id`——并行或多调用时它是外部消费者唯一的配对依据（IA-1.5）。
// 用 defer 统一收口，是因为"每条 return 都记得发事件"这种纪律迟早会被漏掉一条。
//
// 执行前先发一条 `tool_call`（同一个 `call_id`），与随后的 `tool_result` 成对——平台据此
// 看到"模型想调什么、参数是什么"，而不只是结果。只有真正进入执行的调用才发：被前面
// handler 答复过的调用不经过这里（见 OnTurn），它没有"被系统执行"这件事。
func (s *Session) executeCall(ctx context.Context, turn *harness.Turn, tc llm.ToolCall) llm.ToolResult {
	started := time.Now()
	ev := toolResultEvent{callID: tc.ID}
	defer func() {
		s.emitToolResult(ev, time.Since(started))
		// 止损观测点：每条出口（绑定失败、未知工具、策略拒绝、执行报错、原语自身错误、落盘
		// 失败）都在这里喂一次结局——f == nil 即一次成功（喂空串归零同类连续），否则喂失败类别。
		// 它排在 emitToolResult 之后，且必然早于 OnTurn 把本轮记进 turn / 历史：处置读到的是
		// 这一轮已经定格的结局。
		s.observeOutcome(ev.fault)
	}()
	s.emitToolCall(tc)

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
		// 未知原语在这里就被挡下：策略因此不必再兜一道「名字不认识就拒绝」，它只看写不写盘。
		f := &llm.Fault{Kind: "unknown_tool",
			Message: fmt.Sprintf("没有名为 %q 的工具；可用的是：%s", call.Primitive, s.available())}
		ev.fault = f
		return llm.ToolResult{CallID: tc.ID, IsError: true, Output: f.Message}
	}
	// 写盘性质由原语自述，回填给这一次调用——策略只认这一个事实。
	call.Writes = prim.Writes()

	if s.cfg.Policy == nil {
		// 未装配策略 = **默认拒绝**（INV-4、hunt/policy.go 的开篇约定）。这里绝不
		// "跳过裁决"：忘装配策略不该让执行体变成无边界的写入者。
		f := &llm.Fault{Kind: "policy_missing",
			Message: "未装配策略：拒绝执行任何工具调用（装配期缺件）"}
		ev.fault = f
		return llm.ToolResult{CallID: tc.ID, IsError: true, Output: f.Message}
	}
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
		s.recordOps(ops)
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

// available 列出模型可用的工具名，供"名字不认识"时回灌。**必须包含 checkpoint**：
// 它由业务层自带、不经过原语表，漏掉它会让可用清单与实际工具面不一致。
func (s *Session) available() string {
	seen := make(map[string]bool, len(s.tools)+1)
	names := make([]string, 0, len(s.tools)+1)
	for _, name := range s.order {
		names = append(names, string(name))
		seen[string(name)] = true
	}
	if !seen[string(PrimCheckpoint)] {
		names = append(names, string(PrimCheckpoint))
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

// emitToolCall 上报一次「模型提出了调用」，在执行**之前**发。
//
// `tool` 是模型原始给出的名字（不是绑定后的原语名）——平台据此看到"模型想调什么"，
// 即使这个名字后来解析失败也照报。它与随后的 `tool_result` 用同一个 `call_id` 配对。
func (s *Session) emitToolCall(tc llm.ToolCall) {
	if s.cfg.Sink == nil {
		return
	}
	_ = s.cfg.Sink.Emit(ExternalEvent{Type: "tool_call", Payload: map[string]any{
		"call_id": tc.ID,
		"tool":    tc.Name,
		"args":    serializableArgs(tc.Arguments),
	}})
}

// serializableArgs 把模型给的原始参数转成可安全进事件的形状。
//
// 参数合法时原样作为 JSON 对象嵌入；非法时退化成字符串——参数不合法是 BindToolCall 要报的
// **结构化错误**，不是"事件通道故障"，绝不能让 Emit 因它报错（否则会被通道健康检查误判成
// 环境问题、把一次普通的参数错误升级成进程终止）。空参数按契约以空对象 `{}` 表达。
func serializableArgs(raw json.RawMessage) any {
	switch {
	case len(raw) == 0:
		return json.RawMessage("{}")
	case json.Valid(raw):
		return raw
	default:
		return string(raw)
	}
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
