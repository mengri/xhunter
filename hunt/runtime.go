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
type SessionRecorder interface {
	RecordTurn(rec harness.Turn)
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
type EventSink interface {
	Emit(ev ExternalEvent) error
	Log(level, msg string, kv ...any)
	Heartbeat(p Phase) error
}
