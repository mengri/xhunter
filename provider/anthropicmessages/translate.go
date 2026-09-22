package anthropicmessages

import (
	"encoding/json"
	"fmt"
	"strings"

	"xhunter/llm"
)

// 中立消息与工具声明 → 上游请求体。
//
// 本协议的消息形状与"按角色平铺"的形态差别较大，翻译的重点全在这里：
// 系统提示在顶层字段里，工具调用与工具结果都是**内容块**，且工具结果挂在用户消息下。

// minimalInputSchema 是中立侧尚未声明参数形状时使用的形状。
//
// 本协议要求 input_schema 必填，省略会让请求直接不合法。给一个"未声明任何属性"的
// 最小合法对象，与"伪造一组参数"是两件事：前者诚实（就是不声明），后者会让模型
// 以为某些参数存在。
var minimalInputSchema = json.RawMessage(`{"type":"object"}`)

func toWire(model string, maxOutput int, req llm.Request) (wireRequest, error) {
	wire := wireRequest{
		Model:     model,
		MaxTokens: maxOutput,
		Messages:  make([]wireMessage, 0, len(req.Messages)),
		Tools:     toWireTools(req.Tools),
		Stream:    true,
	}

	// 系统提示在本协议里不是一条消息，而是顶层字段：放错位置会被当成普通对话内容。
	var system []string
	for _, m := range req.Messages {
		switch m.Role {
		case llm.RoleSystem:
			if strings.TrimSpace(m.Content) != "" {
				system = append(system, m.Content)
			}

		case llm.RoleUser:
			blocks := textBlocks("text", m.Content)
			if len(blocks) == 0 {
				continue
			}
			wire.Messages = append(wire.Messages, wireMessage{Role: "user", Content: blocks})

		case llm.RoleAssistant:
			blocks := textBlocks("text", m.Content)
			for _, call := range m.Calls {
				blocks = append(blocks, wireBlock{
					Type: "tool_use",
					ID:   call.ID,
					Name: call.Name,
					// 参数在本协议里是**对象**，因此原样放入中立侧的 JSON。
					Input: rawObject(call.Arguments),
				})
			}
			if len(blocks) == 0 {
				// 空助手消息在本协议里没有表达方式（内容块数组不能为空）。
				continue
			}
			wire.Messages = append(wire.Messages, wireMessage{Role: "assistant", Content: blocks})

		case llm.RoleTool:
			// 工具结果不是独立角色，而是**用户消息里的内容块**：角色用错，
			// 模型会把它当成用户说的话。
			blocks := make([]wireBlock, 0, len(m.Results))
			for _, r := range m.Results {
				blocks = append(blocks, wireBlock{
					Type:      "tool_result",
					ToolUseID: r.CallID,
					Content:   r.Output,
					IsError:   r.IsError,
				})
			}
			if len(blocks) == 0 {
				continue
			}
			wire.Messages = append(wire.Messages, wireMessage{Role: "user", Content: blocks})

		default:
			return wireRequest{}, fmt.Errorf("未知的消息角色 %q：角色决定消息被当成什么，因此不做透传", m.Role)
		}
	}

	wire.System = strings.Join(system, "\n\n")
	return wire, nil
}

// textBlocks 把一段文本变成一个内容块；空文本不产生块（空块会让请求不合法）。
func textBlocks(blockType, text string) []wireBlock {
	if text == "" {
		return nil
	}
	return []wireBlock{{Type: blockType, Text: text}}
}

// rawObject 把参数 JSON 当作对象放置，并保证空参数不是一个裸的空值。
func rawObject(args json.RawMessage) json.RawMessage {
	if trimmed := strings.TrimSpace(string(args)); trimmed == "" || trimmed == "null" {
		return json.RawMessage(`{}`)
	}
	return args
}

// toWireTools 把中立声明翻译成上游的工具声明：形状字段是 input_schema，
// 没有"函数"外层包装，参数形状原样搬运。
func toWireTools(decls []llm.ToolDecl) []wireTool {
	out := make([]wireTool, 0, len(decls))
	for _, d := range decls {
		schema := d.Schema
		if len(schema) == 0 {
			schema = minimalInputSchema
		}
		out = append(out, wireTool{Name: d.Name, Description: d.Description, InputSchema: schema})
	}
	return out
}
