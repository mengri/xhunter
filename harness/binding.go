package harness

import (
	"encoding/json"
	"fmt"
	"strings"

	"xhunter/llm"
)

// 绑定层：模型侧的中立词汇 ↔ 本场景的领域词汇。
//
// 这一步之所以存在，是因为两者回答的问题不同：
//   - 模型侧只回答"模型要调哪个名字、参数是什么"，它不认识任何工具集；
//   - 领域侧要回答"这次调用落到哪个原语、定位参数是什么"，这需要知道工具面。
//
// 把解释放在这里，协议实现才能被别的场景复用：换一套工具集只改本文件，
// 协议包一行都不用动。

// BindToolCall 把模型给出的一次原始调用翻译成领域调用。
//
// 它只做**形状**上的翻译（名字与参数各就各位），不判断"这个名字存不存在"——
// 有哪些原语由装配层决定，框架手里没有白名单，也不该有。名字不认识由运行时在
// 查表时挡下并列出可用的名字，那里才是清单所在。
//
// 参数不成立必须显式报错而不是"尽力而为"：参数不是合法 JSON、或含未知字段时，
// 尽力而为意味着模型以为它调用了一个工具、实际执行的是另一个意思——那种错误要等到
// 很久之后才以"任务做错了"的形式暴露，而那时已经无从归因。
func BindToolCall(tc llm.ToolCall) (Call, *llm.Fault) {
	call := Call{
		ID:        ToolCallID(tc.ID),
		Primitive: PrimitiveName(tc.Name),
	}

	var args callArgs
	// 空串与 null 都按"没有参数"处理：上游在无参数时未必给 {}。
	switch trimmed := strings.TrimSpace(string(tc.Arguments)); trimmed {
	case "", "null":
	default:
		dec := json.NewDecoder(strings.NewReader(trimmed))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&args); err != nil {
			return call, &llm.Fault{
				Kind:    "invalid_arguments",
				Message: fmt.Sprintf("%s 的参数不是合法 JSON 或含未知字段：%v", tc.Name, err),
			}
		}
	}

	call.Target = args.Path
	call.Selector = Selector{
		Literal:  args.Literal,
		Symbol:   args.Symbol,
		InSymbol: args.InSymbol,
		FileView: args.FileView,
		Scope:    args.Scope,
	}
	if args.Range != nil {
		call.Selector.Range = &LineRange{From: args.Range.From, To: args.Range.To}
	}
	call.Content = args.Content
	// 具名参数直接落到自己的槽位，不按原语分支、也不借用别的字段：
	// 参数形状与领域字段一对一（见 callArgs），绑定因此是纯搬运。
	call.NewName = args.NewName
	call.Gate = args.Name
	call.Summary = args.Summary
	return call, nil
}

// UnbindToolCall 把领域调用还原成原始调用，用于把历史里的助手消息送回模型。
//
// 只写非空字段：模型看到的应当是"当初发的那次调用"，而参数形状里"没给"与
// "给了一个空值"是两件事。
func UnbindToolCall(c Call) llm.ToolCall {
	args := map[string]any{}
	if c.Target != "" {
		args["path"] = c.Target
	}
	if c.Content != "" {
		args["content"] = c.Content
	}
	if c.Selector.Range != nil {
		args["range"] = lineRange{From: c.Selector.Range.From, To: c.Selector.Range.To}
	}
	if c.Selector.Literal != "" {
		args["literal"] = c.Selector.Literal
	}
	if c.Selector.Symbol != "" {
		args["symbol"] = c.Selector.Symbol
	}
	if c.Selector.InSymbol != "" {
		args["in_symbol"] = c.Selector.InSymbol
	}
	if c.Selector.Scope != "" {
		args["scope"] = c.Selector.Scope
	}
	if c.Selector.FileView {
		args["file_view"] = true
	}
	// 具名参数各自回灌到自己的参数名：模型看到的应当就是当初发的那次调用。
	if c.NewName != "" {
		args["new_name"] = c.NewName
	}
	if c.Gate != "" {
		args["name"] = c.Gate
	}
	if c.Summary != "" {
		args["summary"] = c.Summary
	}
	b, err := json.Marshal(args)
	if err != nil {
		// 参数全来自字符串与布尔，编码不会失败；真失败了也不能让一次历史回灌
		// 把整轮推理挡下来，给一个空参数对象即可。
		b = []byte("{}")
	}
	return llm.ToolCall{ID: string(c.ID), Name: string(c.Primitive), Arguments: b}
}

// UnbindToolResult 把执行结果翻译成回灌给模型的形态。
//
// 失败时保留"类型 + 是否可重试 + 说明"三要素：模型看不到日志，
// 这是它决定"换个做法"还是"原样重试"的唯一输入。
func UnbindToolResult(r Result) llm.ToolResult {
	return llm.ToolResult{
		CallID:  string(r.CallID),
		Output:  resultText(r),
		IsError: r.Err != nil,
	}
}

func resultText(r Result) string {
	if r.Err == nil {
		return r.Summary
	}
	retry := "不可重试"
	if r.Err.Retryable {
		retry = "可重试"
	}
	if r.Summary == "" {
		return fmt.Sprintf("工具执行失败[%s，%s]：%s", r.Err.Kind, retry, r.Err.Message)
	}
	return fmt.Sprintf("工具执行失败[%s，%s]：%s（%s）", r.Err.Kind, retry, r.Err.Message, r.Summary)
}

// toLLMMessages 把领域消息翻译成模型侧消息。
//
// 转换只发生在这一处：领域消息携带的是原语与选择器（编排与工具面的词汇），
// 模型要的是"名字 + 参数 JSON"（协议无关的词汇）。
func toLLMMessages(msgs []Message) []llm.Message {
	out := make([]llm.Message, 0, len(msgs))
	for _, m := range msgs {
		lm := llm.Message{Role: m.Role, Content: m.Content}
		for _, c := range m.Calls {
			lm.Calls = append(lm.Calls, UnbindToolCall(c))
		}
		for _, r := range m.Results {
			lm.Results = append(lm.Results, UnbindToolResult(r))
		}
		out = append(out, lm)
	}
	return out
}

// callArgs 是工具参数在本场景里的形状。
//
// 字段名与领域类型一一对应（调用与选择器的字段，小写下划线形式），因此绑定是纯粹的
// 搬运，不涉及"这个字段大概是什么意思"的猜测。这张表同时也是参数形状的**唯一**定义处：
// 工具声明里的 JSON Schema 必须与它一致（属性名都能被这里收下），否则模型照 schema
// 填的参数会被拒，而错在它看不到的地方。两者的一致性由测试守着。
type callArgs struct {
	Path     string     `json:"path"`
	Range    *lineRange `json:"range"`
	Literal  string     `json:"literal"`
	Symbol   string     `json:"symbol"`
	InSymbol string     `json:"in_symbol"`
	FileView bool       `json:"file_view"`
	Scope    string     `json:"scope"`
	Content  string     `json:"content"`
	Name     string     `json:"name"`
	NewName  string     `json:"new_name"`
	Summary  string     `json:"summary"`
}

type lineRange struct {
	From int `json:"from"`
	To   int `json:"to"`
}
