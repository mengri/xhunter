package anthropicmessages

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"xhunter/llm"
	"xhunter/provider/adapter"
)

type fakeUpstream struct {
	mu       sync.Mutex
	requests []recorded
	status   int
	body     string
	parts    []string
	hold     bool
}

type recorded struct {
	Path   string
	Header http.Header
	Body   []byte
}

func (f *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, recorded{Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
	f.mu.Unlock()

	if f.status != 0 && f.status != http.StatusOK {
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, f.body)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for _, p := range f.parts {
		_, _ = io.WriteString(w, p)
		if flusher != nil {
			flusher.Flush()
		}
	}
	if f.hold {
		<-r.Context().Done()
	}
}

func (f *fakeUpstream) last(t *testing.T) recorded {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("上游没有收到任何请求")
	}
	return f.requests[len(f.requests)-1]
}

func testConfig(baseURL string) Config {
	return Config{
		BaseURL:          baseURL,
		Model:            "claude-test",
		MaxContextTokens: 200000,
		MaxOutputTokens:  8192,
	}
}

func newAgainst(t *testing.T, f *fakeUpstream, tweak func(*Config)) *Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	cfg := testConfig(srv.URL)
	if tweak != nil {
		tweak(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("构造客户端失败：%v", err)
	}
	return c
}

func drain(t *testing.T, s llm.Session) []llm.Event {
	t.Helper()
	var out []llm.Event
	for ev := range s.Events() {
		out = append(out, ev)
	}
	return out
}

func kinds(evs []llm.Event) []llm.EventKind {
	out := make([]llm.EventKind, 0, len(evs))
	for _, ev := range evs {
		out = append(out, ev.Kind)
	}
	return out
}

func sse(payload string) string { return "data: " + payload + "\n\n" }

// ============================================================ 构造期检查

func TestNew_FailsAtStartupWhenFactsAreMissing(t *testing.T) {
	cases := []struct {
		name    string
		tweak   func(*Config)
		wantErr error
	}{
		{"端点缺失", func(c *Config) { c.BaseURL = "" }, nil},
		{"模型标识缺失", func(c *Config) { c.Model = "" }, nil},
		{"上下文上限缺失", func(c *Config) { c.MaxContextTokens = 0 }, adapter.ErrNoContextWindow},
		{"生成上限缺失", func(c *Config) { c.MaxOutputTokens = 0 }, errNoMaxOutput},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig("https://example.invalid/v1")
			tc.tweak(&cfg)
			_, err := New(cfg)
			if err == nil {
				t.Fatal("配置不成立时必须在构造期失败：跑到一半才发现，代价是已消耗的轮次与预算")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("错误 = %v，期望可识别的 %v", err, tc.wantErr)
			}
		})
	}
}

func TestNew_AppliesProtocolHeadersAndLetsConfigOverride(t *testing.T) {
	f := &fakeUpstream{parts: []string{sse(`{"type":"message_stop"}`)}}
	c := newAgainst(t, f, func(cfg *Config) {
		// 鉴权头由调用方给（本协议惯用 x-api-key，裸值）；版本头也可覆盖。
		cfg.Headers = map[string]string{"x-api-key": "sk-ant-raw", "anthropic-version": "2030-01-01"}
	})

	if !strings.HasSuffix(c.endpoint, "/messages") {
		t.Fatalf("端点 = %q，期望协议路径拼在基址之后", c.endpoint)
	}
	if _, err := c.Infer(context.Background(), llm.Request{}); err != nil {
		t.Fatalf("发起推理失败：%v", err)
	}

	got := f.last(t)
	if got.Path != "/messages" {
		t.Errorf("路径 = %q", got.Path)
	}
	if h := got.Header.Get("x-api-key"); h != "sk-ant-raw" {
		t.Errorf("凭据头 = %q，期望原样透传调用方给的值", h)
	}
	if h := got.Header.Get("anthropic-version"); h != "2030-01-01" {
		t.Errorf("协议版本头 = %q，期望配置可覆盖默认值", h)
	}
}

func TestNew_SendsDefaultProtocolVersion(t *testing.T) {
	f := &fakeUpstream{parts: []string{sse(`{"type":"message_stop"}`)}}
	c := newAgainst(t, f, nil)
	if _, err := c.Infer(context.Background(), llm.Request{}); err != nil {
		t.Fatalf("发起推理失败：%v", err)
	}
	if h := f.last(t).Header.Get("anthropic-version"); h != defaultVersion {
		t.Errorf("协议版本头 = %q，期望内置默认值 %q（缺了会被上游拒绝）", h, defaultVersion)
	}
}

// ============================================================ 请求形状

func TestInfer_SendsProtocolRequest(t *testing.T) {
	f := &fakeUpstream{parts: []string{sse(`{"type":"message_stop"}`)}}
	c := newAgainst(t, f, nil)

	_, err := c.Infer(context.Background(), llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "规则"},
			{Role: llm.RoleUser, Content: "任务"},
			{Role: llm.RoleAssistant, Calls: []llm.ToolCall{
				{ID: "toolu_1", Name: "read", Arguments: json.RawMessage(`{"path":"a.go"}`)},
			}},
			{Role: llm.RoleTool, Results: []llm.ToolResult{
				{CallID: "toolu_1", Output: "已读取"},
				{CallID: "toolu_2", Output: "工具执行失败[not_found，不可重试]：没找到", IsError: true},
			}},
		},
		Tools: []llm.ToolDecl{
			{Name: "read", Description: "读文件", Schema: json.RawMessage(`{"type":"object"}`)},
			{Name: "glob", Description: "匹配文件名"},
		},
	})
	if err != nil {
		t.Fatalf("发起推理失败：%v", err)
	}

	var req struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		System    string `json:"system"`
		Stream    bool   `json:"stream"`
		Messages  []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string          `json:"type"`
				Text      string          `json:"text"`
				ID        string          `json:"id"`
				Name      string          `json:"name"`
				Input     json.RawMessage `json:"input"`
				ToolUseID string          `json:"tool_use_id"`
				Content   string          `json:"content"`
				IsError   bool            `json:"is_error"`
			} `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(f.last(t).Body, &req); err != nil {
		t.Fatalf("请求体不是合法 JSON：%v", err)
	}

	if req.Model != "claude-test" || req.MaxTokens != 8192 {
		t.Errorf("model/max_tokens = %q/%d，本协议要求两者都在请求体里", req.Model, req.MaxTokens)
	}
	if !req.Stream {
		t.Error("必须显式开启流式")
	}
	// 系统提示是顶层字段，不是一条消息。
	if req.System != "规则" {
		t.Errorf("system = %q，期望提到顶层字段", req.System)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("消息数 = %d，期望 3（系统提示不算消息）", len(req.Messages))
	}
	// 助手消息：工具调用是内容块，参数是**对象**而不是字符串。
	assistant := req.Messages[1]
	if assistant.Role != "assistant" || len(assistant.Content) != 1 {
		t.Fatalf("助手消息 = %+v", assistant)
	}
	if assistant.Content[0].Type != "tool_use" || assistant.Content[0].Name != "read" {
		t.Errorf("工具调用块 = %+v", assistant.Content[0])
	}
	var args map[string]any
	if err := json.Unmarshal(assistant.Content[0].Input, &args); err != nil || args["path"] != "a.go" {
		t.Errorf("工具调用参数 = %s，期望是对象且带 path", assistant.Content[0].Input)
	}
	// 工具结果挂在**用户消息**下，失败的要带 is_error。
	toolMsg := req.Messages[2]
	if toolMsg.Role != "user" || len(toolMsg.Content) != 2 {
		t.Fatalf("工具结果消息 = %+v，期望用户角色下两个 tool_result 块", toolMsg)
	}
	if toolMsg.Content[0].Type != "tool_result" || toolMsg.Content[0].ToolUseID != "toolu_1" {
		t.Errorf("结果块 = %+v", toolMsg.Content[0])
	}
	if !toolMsg.Content[1].IsError || !strings.Contains(toolMsg.Content[1].Content, "not_found") {
		t.Errorf("失败结果块 = %+v，期望标出 is_error", toolMsg.Content[1])
	}
	// 工具声明用 input_schema；中立侧未声明形状时给最小合法形状（本字段必填）。
	if len(req.Tools) != 2 || string(req.Tools[0].InputSchema) != `{"type":"object"}` {
		t.Fatalf("工具声明 = %+v", req.Tools)
	}
	if string(req.Tools[1].InputSchema) != `{"type":"object"}` {
		t.Errorf("未声明形状时应给最小合法形状，实得 %s", req.Tools[1].InputSchema)
	}
}

func TestInfer_RejectsUnknownRole(t *testing.T) {
	c := newAgainst(t, &fakeUpstream{}, nil)
	_, err := c.Infer(context.Background(), llm.Request{Messages: []llm.Message{{Role: llm.Role("human"), Content: "x"}}})
	if err == nil {
		t.Fatal("未知角色必须显式报错：角色决定消息被当成什么，透传会让拼错的名字变成对端的静默行为")
	}
}

// ============================================================ 流翻译

func TestInfer_TranslatesTextUsageAndEnd(t *testing.T) {
	f := &fakeUpstream{parts: []string{
		"event: message_start\n" + sse(`{"type":"message_start","message":{"usage":{"input_tokens":25,"output_tokens":0}}}`),
		sse(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		sse(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"第一段"}}`),
		sse(`{"type":"ping"}`),
		sse(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"第二段"}}`),
		sse(`{"type":"content_block_stop","index":0}`),
		sse(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":15}}`),
		sse(`{"type":"message_stop"}`),
	}}
	c := newAgainst(t, f, nil)

	s, err := c.Infer(context.Background(), llm.Request{})
	if err != nil {
		t.Fatalf("发起推理失败：%v", err)
	}
	evs := drain(t, s)

	if got := kinds(evs); len(got) != 5 ||
		got[0] != llm.EvUsage || got[1] != llm.EvText || got[2] != llm.EvText ||
		got[3] != llm.EvUsage || got[4] != llm.EvEnd {
		t.Fatalf("事件序列 = %v，期望 用量/文本/文本/用量/结束（ping 忽略）", got)
	}
	if evs[0].Usage.InputTokens != 25 || evs[3].Usage.OutputTokens != 15 {
		t.Errorf("用量 = %+v / %+v：输入在起始事件、输出在收尾事件", evs[0].Usage, evs[3].Usage)
	}
	if evs[1].Text != "第一段" || evs[2].Text != "第二段" {
		t.Errorf("正文增量 = %q / %q", evs[1].Text, evs[2].Text)
	}
}

func TestInfer_AssemblesRawToolUseFromJSONDeltas(t *testing.T) {
	f := &fakeUpstream{parts: []string{
		sse(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"我来看看"}}`),
		sse(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"read","input":{}}}`),
		sse(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"a.go\",\"range\":{"}}`),
		sse(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"from\":1,\"to\":10}}"}}`),
		sse(`{"type":"content_block_stop","index":1}`),
		sse(`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":20}}`),
		sse(`{"type":"message_stop"}`),
	}}
	c := newAgainst(t, f, nil)

	s, err := c.Infer(context.Background(), llm.Request{})
	if err != nil {
		t.Fatalf("发起推理失败：%v", err)
	}
	evs := drain(t, s)

	if got := kinds(evs); len(got) != 4 ||
		got[0] != llm.EvText || got[1] != llm.EvUsage ||
		got[2] != llm.EvToolUse || got[3] != llm.EvEnd {
		t.Fatalf("事件序列 = %v，期望 文本/用量/调用/结束", got)
	}
	// 协议层不解释参数：断言原始形态。
	call := evs[2].Call
	if call.Name != "read" || call.ID != "toolu_1" {
		t.Errorf("调用 = %+v", call)
	}
	if !strings.Contains(string(call.Arguments), `"path":"a.go"`) ||
		!strings.Contains(string(call.Arguments), `"to":10`) {
		t.Errorf("参数 = %s，期望分片拼成完整 JSON", call.Arguments)
	}
}

func TestInfer_UsesCompleteInputWhenNotStreamed(t *testing.T) {
	// 有些上游在起始事件里就给完整参数、之后不再逐片吐。
	f := &fakeUpstream{parts: []string{
		sse(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_9","name":"glob","input":{"path":"**/*.go"}}}`),
		sse(`{"type":"message_stop"}`),
	}}
	c := newAgainst(t, f, nil)

	s, err := c.Infer(context.Background(), llm.Request{})
	if err != nil {
		t.Fatalf("发起推理失败：%v", err)
	}
	evs := drain(t, s)
	if len(evs) != 2 || evs[0].Err != nil {
		t.Fatalf("事件 = %+v", evs)
	}
	if evs[0].Call.Name != "glob" || !strings.Contains(string(evs[0].Call.Arguments), `"**/*.go"`) {
		t.Errorf("调用 = %+v / %s，期望用起始事件里的完整参数", evs[0].Call, evs[0].Call.Arguments)
	}
}

func TestInfer_ErrorEventIsClassified(t *testing.T) {
	cases := []struct {
		name      string
		payload   string
		retryable bool
	}{
		{"上游过载", `{"type":"error","error":{"type":"overloaded_error","message":"过载"}}`, true},
		{"限流", `{"type":"error","error":{"type":"rate_limit_error","message":"太频繁"}}`, true},
		{"请求不合法", `{"type":"error","error":{"type":"invalid_request_error","message":"参数错"}}`, false},
		{"鉴权失败", `{"type":"error","error":{"type":"authentication_error","message":"凭据无效"}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeUpstream{parts: []string{sse(tc.payload)}}
			c := newAgainst(t, f, nil)

			s, err := c.Infer(context.Background(), llm.Request{})
			if err != nil {
				t.Fatalf("发起推理失败：%v", err)
			}
			last := drain(t, s)[0]
			if last.Kind != llm.EvError || last.Err == nil {
				t.Fatalf("最后一条事件 = %+v，期望显式错误", last)
			}
			if last.Err.Retryable != tc.retryable {
				t.Errorf("可重试性 = %v，期望 %v（错误 = %q）", last.Err.Retryable, tc.retryable, last.Err.Message)
			}
		})
	}
}

func TestInfer_TruncatedWithoutMessageStopIsExplicit(t *testing.T) {
	f := &fakeUpstream{parts: []string{
		sse(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"半句"}}`),
	}}
	c := newAgainst(t, f, nil)

	s, err := c.Infer(context.Background(), llm.Request{})
	if err != nil {
		t.Fatalf("发起推理失败：%v", err)
	}
	evs := drain(t, s)
	last := evs[len(evs)-1]
	if last.Kind != llm.EvError || last.Err.Kind != "stream_truncated" || !last.Err.Retryable {
		t.Fatalf("最后一条事件 = %+v，期望显式且可重试的截断", last)
	}
}

func TestInfer_MalformedChunkIsExplicit(t *testing.T) {
	f := &fakeUpstream{parts: []string{sse(`{不是 JSON}`)}}
	c := newAgainst(t, f, nil)

	s, err := c.Infer(context.Background(), llm.Request{})
	if err != nil {
		t.Fatalf("发起推理失败：%v", err)
	}
	evs := drain(t, s)
	if last := evs[len(evs)-1]; last.Kind != llm.EvError || last.Err.Kind != "stream_decode_error" {
		t.Fatalf("最后一条事件 = %+v", last)
	}
}

// ============================================================ 错误与取消

func TestInfer_StatusErrorsAreClassified(t *testing.T) {
	for _, tc := range []struct {
		status    int
		retryable bool
	}{
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			f := &fakeUpstream{status: tc.status, body: "上游拒绝了这次请求"}
			c := newAgainst(t, f, nil)

			_, err := c.Infer(context.Background(), llm.Request{})
			var se *adapter.StatusError
			if !errors.As(err, &se) {
				t.Fatalf("错误 = %v，期望带状态码的结构化错误", err)
			}
			if se.Retryable != tc.retryable {
				t.Errorf("可重试性 = %v，期望 %v", se.Retryable, tc.retryable)
			}
		})
	}
}

func TestInfer_CancelUnblocksTheStream(t *testing.T) {
	f := &fakeUpstream{
		parts: []string{
			sse(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"一"}}`),
		},
		hold: true,
	}
	c := newAgainst(t, f, nil)

	s, err := c.Infer(context.Background(), llm.Request{})
	if err != nil {
		t.Fatalf("发起推理失败：%v", err)
	}
	select {
	case <-s.Events():
	case <-time.After(2 * time.Second):
		t.Fatal("没有收到任何事件")
	}
	if err := s.Cancel(); err != nil {
		t.Fatalf("取消失败：%v", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-s.Events():
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("取消之后流没有关闭：挂起的流必须能被解开")
		}
	}
}

// 本协议的 usage 是累计值：message_start 给起始输出用量、message_delta 给最终值。
// 消费端把事件相加，因此实现只能交增量——否则每轮都多算一次。
func TestInfer_UsageIsNotDoubleCounted(t *testing.T) {
	up := &fakeUpstream{parts: []string{
		"event: message_start\n" + sse(`{"type":"message_start","message":{"usage":{"input_tokens":100,"output_tokens":1}}}`),
		"event: content_block_delta\n" + sse(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`),
		"event: message_delta\n" + sse(`{"type":"message_delta","usage":{"output_tokens":57}}`),
		"event: message_stop\n" + sse(`{"type":"message_stop"}`),
	}}
	c := newAgainst(t, up, nil)
	sess, err := c.Infer(context.Background(), llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatalf("Infer 失败：%v", err)
	}
	var in, out int
	for _, ev := range drain(t, sess) {
		if ev.Kind == llm.EvUsage {
			in += ev.Usage.InputTokens
			out += ev.Usage.OutputTokens
		}
	}
	if in != 100 {
		t.Errorf("输入用量 = %d，期望 100", in)
	}
	if out != 57 {
		t.Errorf("输出用量 = %d，期望 57（累计值要差分成增量，而不是 1+57）", out)
	}
}
