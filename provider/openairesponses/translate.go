package openairesponses

import (
	"fmt"
	"strings"

	"xhunter/llm"
)

// 中立消息与工具声明 → 上游请求体。
//
// 本协议的重点在"对话是一个类型化条目数组"：消息、函数调用、函数调用结果各占一个条目，
// 顺序即时间顺序。把它当成"按角色平铺的消息列表"来处理，工具调用就会丢失。

// minimalParameters 说明：本协议里 parameters 是可选的，因此中立侧尚未声明形状时
// **不补空壳**——省略字段比给一个假的形状诚实（那会让模型以为参数已被声明）。

func toWire(model string, req llm.Request) (wireRequest, error) {
	wire := wireRequest{
		Model:  model,
		Input:  make([]wireItem, 0, len(req.Messages)),
		Tools:  toWireTools(req.Tools),
		Stream: true,
	}

	// 系统提示在本协议里是顶层字段，而不是一条消息。
	var instructions []string
	for _, m := range req.Messages {
		switch m.Role {
		case llm.RoleSystem:
			if strings.TrimSpace(m.Content) != "" {
				instructions = append(instructions, m.Content)
			}

		case llm.RoleUser:
			if m.Content == "" {
				// 空消息在本协议里没有意义（文本部分不能为空），跳过而不是发一条空条目。
				continue
			}
			wire.Input = append(wire.Input, wireItem{
				Type: "message", Role: "user",
				Content: []wirePart{{Type: "input_text", Text: m.Content}},
			})

		case llm.RoleAssistant:
			if m.Content != "" {
				wire.Input = append(wire.Input, wireItem{
					Type: "message", Role: "assistant",
					Content: []wirePart{{Type: "output_text", Text: m.Content}},
				})
			}
			// 工具调用是独立条目：本协议不把它挂在助手消息的内容里。
			for _, c := range m.Calls {
				wire.Input = append(wire.Input, wireItem{
					Type:   "function_call",
					CallID: c.ID,
					Name:   c.Name,
					// 参数是**字符串**形式的 JSON（与对话补全一致）。
					Arguments: string(c.Arguments),
				})
			}

		case llm.RoleTool:
			// 结果同样是独立条目，按调用标识与调用配对。
			for _, r := range m.Results {
				wire.Input = append(wire.Input, wireItem{
					Type:   "function_call_output",
					CallID: r.CallID,
					Output: r.Output,
				})
			}

		default:
			return wireRequest{}, fmt.Errorf("未知的消息角色 %q：角色决定消息被当成什么，因此不做透传", m.Role)
		}
	}

	wire.Instructions = strings.Join(instructions, "\n\n")
	return wire, nil
}

// toWireTools 把中立声明翻译成上游的工具声明：形状是平的（没有"函数"外层包装），
// 参数形状原样搬运；没有声明形状时不发 parameters。
func toWireTools(decls []llm.ToolDecl) []wireTool {
	out := make([]wireTool, 0, len(decls))
	for _, d := range decls {
		out = append(out, wireTool{
			Type:        "function",
			Name:        d.Name,
			Description: d.Description,
			Parameters:  d.Schema,
		})
	}
	return out
}
