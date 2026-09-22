package openaichat

import (
	"fmt"

	"xhunter/llm"
)

// 中立消息与工具声明 → 上游请求体。
//
// 参数词汇、结果的文本渲染都不在这里：工具名与参数含义属于使用方，
// 本包只按协议把"名字 + 参数 JSON"原样摆到对应字段上。

// toWireMessages 把中立消息翻译成上游消息。
//
// 未知角色显式报错而不是原样透传：角色决定这条消息被当成什么，透传会让一个拼错的
// 角色名变成对端的静默行为差异（例如被当成 user），这类错误在无人值守时几乎不可察觉。
func toWireMessages(msgs []llm.Message) ([]wireMessage, error) {
	out := make([]wireMessage, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case llm.RoleSystem, llm.RoleUser:
			out = append(out, wireMessage{Role: string(m.Role), Content: m.Content})

		case llm.RoleAssistant:
			wm := wireMessage{Role: string(m.Role), Content: m.Content}
			for _, c := range m.Calls {
				wm.ToolCalls = append(wm.ToolCalls, wireToolCall{
					ID:   c.ID,
					Type: "function",
					Function: wireFunction{
						Name: c.Name,
						// 本协议的参数是**字符串**（JSON 文本），不是对象。
						Arguments: string(c.Arguments),
					},
				})
			}
			out = append(out, wm)

		case llm.RoleTool:
			// 结果依附于具体某次调用，因此一次调用一条消息：上游按调用标识配对，
			// 把多条结果合并进一条消息会让配对退化成按顺序猜。
			for _, r := range m.Results {
				out = append(out, wireMessage{
					Role:       string(llm.RoleTool),
					ToolCallID: r.CallID,
					Content:    r.Output,
				})
			}

		default:
			return nil, fmt.Errorf("未知的消息角色 %q：角色决定消息被当成什么，因此不做透传", m.Role)
		}
	}
	return out, nil
}

// toWireTools 把中立声明翻译成上游的工具声明。
//
// 参数形状原样搬过去（本来就是 JSON Schema）；没有形状时不下发 parameters——
// 本协议里它是可选的，补一个空壳会让模型以为"这个工具不需要参数"。
func toWireTools(decls []llm.ToolDecl) []wireTool {
	out := make([]wireTool, 0, len(decls))
	for _, d := range decls {
		out = append(out, wireTool{
			Type: "function",
			Function: wireToolDecl{
				Name:        d.Name,
				Description: d.Description,
				Parameters:  d.Schema,
			},
		})
	}
	return out
}
