package main

import (
	"xhunter/harness"
	"xhunter/llm"
)

// contextBuilder 持有首轮提示词与历史，并按需组装「提示词 + 历史」的完整消息。
//
// 「历史只住在这里」是刻意的——轮级状态（harness.Turn）只装「本轮那段」，handler
// 因此物理上碰不到历史；发给模型的东西一律由 Assemble 现拼，不靠某处维护的一份可变副本。
type contextBuilder struct {
	prompt []llm.Message
	turns  []harness.Turn
}

// SetPrompt 设定首轮提示词（Prepare 调一次，此后不变）。
func (c *contextBuilder) SetPrompt(msgs []llm.Message) { c.prompt = msgs }

// Assemble 给出「提示词 + 历史」的完整消息。
func (c *contextBuilder) Assemble() []llm.Message {
	msgs := make([]llm.Message, 0, len(c.prompt)+2*len(c.turns))
	msgs = append(msgs, c.prompt...)
	for _, t := range c.turns {
		msgs = append(msgs, produced(t)...)
	}
	return msgs
}

// Append 累积本轮产生的 message。
func (c *contextBuilder) Append(rec harness.Turn) { c.turns = append(c.turns, rec) }

// produced 把一轮翻译成它留在上下文里的消息：先是模型这一轮说了什么（含它要调的工具），
// 再是这些调用的结果。下一轮因此能看到「自己刚做过什么、得到了什么」。
//
// 空轮不占位置：既没正文也没调用，就没有什么可带进历史的。
func produced(t harness.Turn) []llm.Message {
	if t.Text == "" && len(t.Calls) == 0 {
		return nil
	}
	msgs := []llm.Message{{Role: llm.RoleAssistant, Content: t.Text, Calls: t.Calls}}
	if len(t.Results) > 0 {
		msgs = append(msgs, llm.Message{Role: llm.RoleTool, Results: t.Results})
	}
	return msgs
}
