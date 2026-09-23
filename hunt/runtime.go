package hunt

import (
	"xhunter/harness"
	"xhunter/llm"
)

// 本文件是业务侧的「协作者」接口：harness 只认识 llm 与三组 handler，上下文怎么组装、
// 会话怎么记、事件怎么发都由业务自己定义并由装配层注入。

// ContextBuilder 组装发给模型的完整消息，并累积历史。
type ContextBuilder interface {
	// SetPrompt 设定首轮提示词（Prepare 调一次，此后不变）。
	SetPrompt(msgs []llm.Message)
	// Assemble 给出「提示词 + 历史」的完整消息。
	Assemble() []llm.Message
	// Append 累积本轮产生的 message。
	Append(rec harness.Turn)
}

// SessionRecorder 记录会话材料（供崩溃后恢复）。
//
// 方法集**一次加齐**（不留二次扩公开接口）：材料是恢复的唯一状态源，而**写操作序列是恢复的唯一
// 刚需**——它此前没有通道（`Session` 攥着 `ops` 却交不出来），所以这一批一起加。刻意**不加**
// `Delta` / `Fingerprint` 一类：没有消费方就不加（同 `llm.Caps` 的原则）。
type SessionRecorder interface {
	// Open 绑定材料存放位置：工作区根由 PrepareBaseline 在运行期给出，所以在工作区就绪后调用一次。
	// 失败只降级、不阻断（材料丢了最多是崩溃后从头跑）。
	Open(root string) error
	// RecordTurn 记录一轮（供恢复时回灌上下文）。
	RecordTurn(rec harness.Turn)
	// RecordOp 记录一次写操作：它是恢复的唯一刚需（读操作与 check 不复放）。
	RecordOp(op WriteOp)
	// RecordUsage 记录本轮用量增量（与计费、usage 事件同源）。
	RecordUsage(u llm.Usage)
	// Ops 给出已记录的写操作序列。
	Ops() []WriteOp
	// Snapshot 把尚未落盘的记录 flush 出去（材料落盘的周期点）。
	Snapshot() error
}

// ExternalEvent 是外部事件；事件类型只增不改。
type ExternalEvent struct {
	Type    string
	Payload map[string]any
}

// NamedFilter 给结果过滤器一个名字：生效配置快照要报出「用的是哪条链」。
//
// 过滤器是匿名函数组成的链，本身说不出自己是谁；没有名字，评审者就无从对比两次运行的
// 过滤行为，也看不出"这次到底挂没挂脱敏"。因此名字与过滤器一起装配，不另维护一张表。
type NamedFilter struct {
	Name string
	Run  ResultFilter
}

// Phase 是心跳携带的阶段。
type Phase string

const (
	PhaseBootstrap Phase = "bootstrap"
	PhaseAssemble  Phase = "assemble"
	PhaseInfer     Phase = "infer"
	PhaseTools     Phase = "tools"
	PhaseFinalize  Phase = "finalize"
)

// EventSink 是外部事件与日志的出口：事件流给程序消费，日志给人看，两条通道分开。
//
// Failed 是出口的**自述健康状态**：它记着第一次写失败的原因。事件与心跳走同一个出口，因此
// 心跳写失败也被它覆盖（FR-10.4「事件（含心跳）写入失败时记录并终止」）。业务据此在轮边界
// 早停——通道断了，剩余轮次只是在烧预算地自说自话。它是契约的一部分、不是可选能力：写进接口，
// 缺实现即编译错误；用「可选接口＋类型断言」会让没实现的出口静默失效，与「缺件必须在装配期
// 显式失败」的口径相反。
type EventSink interface {
	Emit(ev ExternalEvent) error
	Log(level, msg string, kv ...any)
	Heartbeat(p Phase) error
	// Failed 报告出口是否断过（第一次写失败的原因）；它同时覆盖事件与心跳两条写路径。
	Failed() error
}
