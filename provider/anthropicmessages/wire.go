package anthropicmessages

import "encoding/json"

// 本文件描述上游的请求与响应形状，全部不导出：它们是协议细节，一旦被包外引用，
// 调用方就会开始依赖某一家的字段名。形状基线见 package 注释里的"协议版本基线"。

type wireRequest struct {
	Model     string        `json:"model"`
	MaxTokens int           `json:"max_tokens"`
	System    string        `json:"system,omitempty"`
	Messages  []wireMessage `json:"messages"`
	Tools     []wireTool    `json:"tools,omitempty"`
	Stream    bool          `json:"stream"`
}

type wireMessage struct {
	Role    string      `json:"role"`
	Content []wireBlock `json:"content"`
}

// wireBlock 是内容块。各类型的字段放在同一个结构里而不是分成多种类型：
// 严格区分只会让翻译层多一层断言，而字段名本身已经能自证属于哪种块。
type wireBlock struct {
	Type string `json:"type"`

	// 文本块。
	Text string `json:"text,omitempty"`

	// 工具调用块（助手产出）。参数是**对象**，不是字符串。
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// 工具结果块（随后由用户消息送回）。
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

// wireTool 是工具声明：形状字段叫 input_schema，且没有"函数"外层包装。
type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// streamEvent 是流式事件。事件名既出现在 SSE 的 event 行、也出现在载荷的 type 字段，
// 这里以后者为准——event 行缺失也不影响解析。
type streamEvent struct {
	Type    string         `json:"type"`
	Index   int            `json:"index"`
	Message *eventStart    `json:"message"`
	Block   *wireBlock     `json:"content_block"`
	Delta   *eventDelta    `json:"delta"`
	Usage   *eventUsage    `json:"usage"`
	Error   *upstreamError `json:"error"`
}

type eventStart struct {
	Usage *eventUsage `json:"usage"`
}

// eventDelta 承载三种增量：文本、工具参数的 JSON 片段、以及收尾时的停止原因。
type eventDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	PartialJSON string `json:"partial_json"`
	StopReason  string `json:"stop_reason"`
}

// eventUsage 分两处出现：输入用量在 message_start，输出用量在 message_delta。
//
// 本协议的输入三项是**并列**关系、不是包含关系：`input_tokens` 只算最后一个缓存
// 断点之后的 token，缓存读与缓存写各占一个字段。因此"全部输入"必须三项相加——
// 少加一项，缓存命中越多、上报的输入越小，预算与账目都会偏低。
type eventUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// totalInput 是本次请求的全部输入（含缓存读与缓存写）。
func (u eventUsage) totalInput() int {
	return u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

// upstreamError 是流内错误（有些上游用 200 开头、再在流里报错）。
type upstreamError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func (e *upstreamError) Error() string {
	if e.Type == "" {
		return e.Message
	}
	return e.Type + ": " + e.Message
}

// Retryable 按错误类型判断：限流与上游自身故障重试有意义，
// 请求不合法（参数、鉴权、权限、找不到资源）则重试永远得到同一个答案。
func (e *upstreamError) Retryable() bool {
	switch e.Type {
	case "rate_limit_error", "api_error", "overloaded_error":
		return true
	default:
		return false
	}
}
