package hunt

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"xhunter/llm"
	"xhunter/workspace"
)

// BindToolCall 把模型给出的一次原始调用翻译成业务调用。
//
// 它只做形状上的翻译（名字与参数各就各位），不判断「这个名字存不存在」——
// 有哪些原语由装配层决定，查表时挡下并列出可用名字。参数不成立必须显式报错而不是
// 尽力而为：模型以为它调了一个工具、实际执行的是另一个意思，那种错误要到很久之后
// 才以「任务做错了」的形式暴露。
func BindToolCall(tc llm.ToolCall) (Call, *llm.Fault) {
	call := Call{
		ID:        ToolCallID(tc.ID),
		Primitive: PrimitiveName(tc.Name),
	}

	var args callArgs
	// 空串与 null 都按「没有参数」处理：上游在无参数时未必给 {}。
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
		// 对象之后还有内容（`{...}{...}`、`{...} 后面跟半截文本`）说明这不是一次
		// 完整调用：只看第一个对象会静默丢掉后半段，而模型以为整段都被采用了。
		// 判据是"再解一个值、必须恰好读到流尾"，语义不含糊（不依赖 More 的边界用法）。
		var extra json.RawMessage
		if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
			return call, &llm.Fault{
				Kind:    "invalid_arguments",
				Message: tc.Name + " 的参数在 JSON 对象之后还有多余内容",
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
		Include:  args.Include,
	}
	if args.Range != nil {
		call.Selector.Range = &workspace.LineRange{From: args.Range.From, To: args.Range.To}
	}
	call.Content = args.Content
	call.NewName = args.NewName
	call.Gate = args.Name
	call.Summary = args.Summary
	return call, nil
}

// UnbindToolCall 把业务调用还原成原始调用，用于把历史里的助手消息送回模型。
// 只写非空字段：模型看到的应当是「当初发的那次调用」。
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
	if len(c.Selector.Include) > 0 {
		args["include"] = c.Selector.Include
	}
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
		b = []byte("{}")
	}
	return llm.ToolCall{ID: string(c.ID), Name: string(c.Primitive), Arguments: b}
}

// UnbindToolResult 把执行结果翻译成回灌给模型的形态。
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

// callArgs 是工具参数在本场景里的形状，字段与领域类型一一对应。
type callArgs struct {
	Path     string     `json:"path"`
	Range    *lineRange `json:"range"`
	Literal  string     `json:"literal"`
	Symbol   string     `json:"symbol"`
	InSymbol string     `json:"in_symbol"`
	FileView bool       `json:"file_view"`
	Scope    string     `json:"scope"`
	Include  []string   `json:"include"`
	Content  string     `json:"content"`
	Name     string     `json:"name"`
	NewName  string     `json:"new_name"`
	Summary  string     `json:"summary"`
}

type lineRange struct {
	From int `json:"from"`
	To   int `json:"to"`
}
