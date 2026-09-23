package openairesponses

import "encoding/json"

// 本文件描述上游的请求与响应形状，全部不导出：它们是协议细节，一旦被包外引用，
// 调用方就会开始依赖某一家的字段名。形状基线见 package 注释里的"协议版本基线"。

type wireRequest struct {
	Model        string     `json:"model"`
	Instructions string     `json:"instructions,omitempty"`
	Input        []wireItem `json:"input"`
	Tools        []wireTool `json:"tools,omitempty"`
	Stream       bool       `json:"stream"`
}

// wireItem 是输入条目。本协议把"对话"表示成一个类型化条目数组：消息、函数调用、
// 函数调用结果各是一种条目，顺序即时间顺序——而不是把调用塞进某条消息里。
type wireItem struct {
	Type    string     `json:"type,omitempty"`
	Role    string     `json:"role,omitempty"`
	Content []wirePart `json:"content,omitempty"`

	// function_call
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`

	// function_call_output
	Output string `json:"output,omitempty"`
}

type wirePart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// wireTool 是工具声明：形状是平的（没有"函数"外层包装），parameters 可选。
type wireTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// streamEvent 是流式语义事件。每种事件自带 type，只声明用得到的字段：
// 上游加事件类型或加字段都不该让解析失败。
type streamEvent struct {
	Type        string `json:"type"`
	Delta       string `json:"delta"`
	Arguments   string `json:"arguments"`
	ItemID      string `json:"item_id"`
	OutputIndex int    `json:"output_index"`

	// output_item.added / output_item.done 携带完整条目（函数调用在这里给出名字与标识）。
	Item *wireItem `json:"item"`

	// error 事件把编码与说明放在顶层。
	Code      string `json:"code"`
	Message   string `json:"message"`
	SequenceN int    `json:"sequence_number"`

	// completed / incomplete / failed 携带整个响应对象（用量在这里）。
	Response *eventResponse `json:"response"`
}

type eventResponse struct {
	Status string         `json:"status"`
	Usage  *eventUsage    `json:"usage"`
	Error  *upstreamError `json:"error"`
}

type eventUsage struct {
	InputTokens        int                `json:"input_tokens"`
	OutputTokens       int                `json:"output_tokens"`
	InputTokensDetails *inputTokenDetails `json:"input_tokens_details"`
}

// inputTokenDetails 是输入侧的明细。本协议里 input_tokens **已经包含**缓存部分，
// 缓存数只能是它的子集（与对话补全一致、与 Messages 协议相反）。
type inputTokenDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// upstreamError 出现在两处：流内的 error 事件，以及 response.failed 里的 response.error。
type upstreamError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *upstreamError) Error() string {
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}

// Retryable 按错误编码判断：限流与服务端故障重试有意义；
// 请求不合法或被内容策略拒绝则重试永远得到同一个答案。
func (e *upstreamError) Retryable() bool {
	switch e.Code {
	case "rate_limit_exceeded", "server_error", "internal_error", "timeout":
		return true
	default:
		return false
	}
}

// asError 把 error 事件的顶层字段归一成同一形状，便于统一分类与上报。
func (e *streamEvent) asError() *upstreamError {
	if e.Code == "" && e.Message == "" {
		return nil
	}
	return &upstreamError{Code: e.Code, Message: e.Message}
}
