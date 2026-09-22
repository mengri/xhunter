// Package llm 是"与模型说话"的中立契约：对话的形状、流式事件的形状、以及 Provider 接口。
//
// 它刻意不认识任何场景概念——没有工具语义、没有编排、没有任务、没有路径与选择器。
// 工具调用在这里是**原样呈现**的：模型说了要调哪个名字、参数是什么（一段 JSON），
// 至于这个名字在某个场景里意味着什么，由认识工具集的一侧去解释。
//
// 因此本包可以被任何 Go 程序复用：只要实现 Provider，就能把任何一个模型接进来；
// 反过来说，任何协议实现也只依赖本包，不依赖任何使用方。
package llm

import (
	"context"
	"encoding/json"
	"time"
)

// ============================================================ 对话

// Role 是消息角色，中立词汇：适配器据此翻译成各家协议字段。未知角色必须显式报错，
// 原样透传会让一个拼错的角色名变成对端的静默行为差异（例如被当成 user）。
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolCall 是模型要求的一次调用，原样呈现。
//
// Arguments 是模型给出的参数 JSON，本包不做任何解释——不解析字段含义、不校验工具名。
// 那两件事需要知道"有哪些工具"，而这是使用方的知识，不是协议层的知识。
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// ToolResult 是把一次调用的结果送回给模型的形态。
//
// Output 是回灌文本（由使用方决定怎么组织，例如带上错误类型与可重试性）；
// IsError 只有一部分协议有对应字段，支持的协议会如实标注，其余按普通文本发送。
type ToolResult struct {
	CallID  string
	Output  string
	IsError bool
}

// ToolDecl 是工具对模型可见的声明。
//
// 它必须只有一份——上下文里的工具说明层与发给供应商的注册面读同一份，
// 各自推导就会出现"说明了一层、实际注册了另一层"的漂移。
type ToolDecl struct {
	Name        string
	Description string
	// Schema 是参数形状的 JSON Schema。为 nil 表示参数形状尚未定格：
	// 此时不向供应商声明参数（协议要求该字段必填的除外，那里给最小的合法形状）。
	Schema json.RawMessage
}

// Message 是上下文里的一条消息。Role 取 Role* 常量。
type Message struct {
	Role    Role
	Content string
	Calls   []ToolCall
	Results []ToolResult
}

// Usage 是 token 用量与轮级统计。协议实现只填 token 两项；轮数与耗时由循环
// （harness.Engine）在收尾时补齐——它才知道一共跑了几轮、花了多久。
type Usage struct {
	InputTokens  int
	OutputTokens int
	Turns        int
	Elapsed      time.Duration
}

// ============================================================ 事件与错误

type EventKind string

const (
	EvText    EventKind = "text"
	EvToolUse EventKind = "tool_use"
	EvUsage   EventKind = "usage"
	EvError   EventKind = "error"
	EvEnd     EventKind = "end"
)

// Event 是协议实现交给使用方的唯一东西：一轮推理就是一条事件序列。
//
// 流式与一次性因此不需要两套接口：多次 EvText 就是流式，一次也算流式。
type Event struct {
	Kind  EventKind
	Text  string
	Call  ToolCall
	Usage Usage
	// Err 承载"响应本身有问题"的事实，不得静默吞掉：流中断、上游报错，
	// 以及形状不成立的调用（参数不是合法 JSON 之类，由使用方在绑定时发现）。
	Err *Fault
}

// Fault 是结构化错误。三要素缺一不可：Kind 供程序分支，Message 供模型或人理解，
// Retryable 决定该换策略还是原样重试——无人值守时，这是自我纠错的唯一输入。
type Fault struct {
	Kind      string
	Message   string
	Retryable bool
}

func (f *Fault) Error() string { return f.Kind + ": " + f.Message }

// ============================================================ 契约

// Caps 是 Provider 声明的能力。上限类字段必须来自预置配置，不得估算；
// 行为类字段缺失即由使用方降级。
type Caps struct {
	MaxContextTokens  int
	ParallelToolCalls bool
}

// Request 是一次推理的输入。
type Request struct {
	Turn     int
	Messages []Message
	Tools    []ToolDecl
}

// Session 是一轮推理的进行中状态。
//
// Events 与 Cancel 分离：取消必须在消费之外可达，否则"中止一次挂起的流"
// 就只能靠消费端读到某个特殊事件——那等于把控制面塞进了数据面。
type Session interface {
	Events() <-chan Event
	Cancel() error
}

// Provider 是使用方看到的模型侧全部能力：发起一次推理，以及询问能力。
//
// 刻意没有执行方法：工具执行不委托给 Provider，否则"什么时候真正动手"就变成了
// 供应商行为，换一家模型就会换掉语义。
type Provider interface {
	Infer(ctx context.Context, req Request) (Session, error)
	Capabilities() Caps
}
