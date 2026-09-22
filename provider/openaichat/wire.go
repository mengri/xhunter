package openaichat

import (
	"encoding/json"

	"xhunter/llm"
)

// 本文件描述上游的请求与响应形状，全部不导出：它们是协议细节，一旦被包外引用，
// 调用方就会开始依赖某一家的字段名。形状基线见 package 注释里的"协议版本基线"。

type wireRequest struct {
	Model    string        `json:"model"`
	Messages []wireMessage `json:"messages"`
	Tools    []wireTool    `json:"tools,omitempty"`
	Stream   bool          `json:"stream"`
	// StreamOptions 要求上游在流末尾回报用量。没有它就只能本地估算，
	// 而估算误差会直接进入预算维度——能用事实的地方不用估算。
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content,omitempty"`
	// 助手消息里的工具调用；参数是**字符串**形式的 JSON。
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireTool struct {
	Type     string       `json:"type"`
	Function wireToolDecl `json:"function"`
}

type wireToolDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// streamChunk 是流式分片。只声明用得到的字段：上游加字段不该让解析失败。
type streamChunk struct {
	Choices []chunkChoice  `json:"choices"`
	Usage   *chunkUsage    `json:"usage"`
	Error   *upstreamError `json:"error"`
}

type chunkChoice struct {
	Delta        chunkDelta `json:"delta"`
	FinishReason string     `json:"finish_reason"`
}

type chunkDelta struct {
	Content   string          `json:"content"`
	ToolCalls []toolCallDelta `json:"tool_calls"`
	// 思考块（各家字段名不一）刻意不接收：中立侧没有对应槽位，
	// 混进正文会把"模型的推理过程"变成"模型的回答"。要保留它，
	// 应先给中立事件加槽位，而不是在这里悄悄并进正文。
}

type toolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chunkUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

func (u chunkUsage) usage() llm.Usage {
	return llm.Usage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens}
}

// upstreamError 是流内错误（有些上游用 200 开头、再在流里报错）。
type upstreamError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Code    any    `json:"code"`
}

func (e *upstreamError) Error() string {
	if e.Type == "" {
		return e.Message
	}
	return e.Type + ": " + e.Message
}

// Retryable 按错误类型判断：限流与上游自身故障重试有意义，
// 请求不合法则重试永远得到同一个答案。
func (e *upstreamError) Retryable() bool {
	switch e.Type {
	case "rate_limit_error", "server_error", "overloaded_error", "api_error":
		return true
	default:
		return false
	}
}
