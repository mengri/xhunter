package adapter

import (
	"encoding/json"
	"strings"

	"xhunter/llm"
)

// CallBuilder 按位置拼装工具调用。
//
// 各家协议都把一次调用拆成多条增量：标识与函数名先到，参数后到、且常常分成好几片。
// 位置下标是唯一稳定的配对键——同一个响应里可能有多个调用，按到达顺序拼接会在多调用时串台。
//
// 分片细节各协议不同（有的把参数当 JSON 文本逐片吐出，有的先给一个完整对象），
// 但"按位置配对 + 参数拼接 + 收尾整批交出"是共同的，因此在这里实现一次。
//
// 它**不解释参数**：吐出的是一段原样 JSON。哪个名字对应什么含义，需要知道工具集，
// 那是使用方（绑定层）的事——协议层不认识任何工具集，这正是它能被别的场景复用的前提。
type CallBuilder struct {
	byIndex map[int]*pendingCall
	order   []int
}

type pendingCall struct {
	id   string
	name string
	// streamed 是流式分片拼起来的参数；full 是某条事件一次性给出的完整参数。
	// 两者都可能是空的，也可能同时存在（见 Events 的取舍规则）。
	streamed strings.Builder
	full     string
}

func NewCallBuilder() *CallBuilder {
	return &CallBuilder{byIndex: map[int]*pendingCall{}}
}

// Add 追加一片参数。id 与 name 传空表示本片没带——它们通常只在第一片出现。
func (b *CallBuilder) Add(index int, id, name, argsFragment string) {
	pc := b.at(index)
	if id != "" {
		pc.id = id
	}
	if name != "" {
		pc.name = name
	}
	pc.streamed.WriteString(argsFragment)
}

// SetFull 记下一个一次性给出的完整参数。
//
// 它不覆盖流式拼装的结果，只是备选：有些上游在"该调用已结束"的事件里给出完整参数，
// 有些在起始事件里给一个空对象再逐片补。两种都要能处理，取舍见 Events。
func (b *CallBuilder) SetFull(index int, args string) {
	b.at(index).full = args
}

// Len 返回已记录的位置数。
func (b *CallBuilder) Len() int { return len(b.order) }

// Events 按出现顺序把拼好的调用翻译成中立事件。
//
// 参数的取舍规则是"流式拼装优先，没有再用完整值"：两者通常一致，而在只有其一到达时
// 这条规则能覆盖全部情况——只按完整值取，会把逐片吐参数的常用形态全丢掉。
func (b *CallBuilder) Events() []llm.Event {
	out := make([]llm.Event, 0, len(b.order))
	for _, idx := range b.order {
		pc := b.byIndex[idx]
		args := pc.streamed.String()
		if args == "" {
			args = pc.full
		}
		var raw json.RawMessage
		if strings.TrimSpace(args) != "" {
			raw = json.RawMessage(args)
		}
		out = append(out, llm.Event{Kind: llm.EvToolUse, Call: llm.ToolCall{
			ID:        pc.id,
			Name:      pc.name,
			Arguments: raw,
		}})
	}
	return out
}

func (b *CallBuilder) at(index int) *pendingCall {
	pc, ok := b.byIndex[index]
	if !ok {
		pc = &pendingCall{}
		b.byIndex[index] = pc
		b.order = append(b.order, index)
	}
	return pc
}
