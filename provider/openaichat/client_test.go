package openaichat

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

// fakeUpstream 是一个可控的上游：记录收到的请求，按脚本回放 SSE 分片。
// 测试因此不依赖网络，也不依赖任何真实模型——协议实现的行为是可复现的。
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
		Model:            "test-model",
		MaxContextTokens: 128000,
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
		{"上下文上限缺失", func(c *Config) { c.MaxContextTokens = 0 }, adapter.ErrNoContextWindow},
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

func TestNew_BuildsEndpointAndDeclaresWindow(t *testing.T) {
	for _, base := range []string{"https://gateway.example/v1", "https://gateway.example/v1/"} {
		c, err := New(Config{BaseURL: base, Model: "m", MaxContextTokens: 128000})
		if err != nil {
			t.Fatalf("构造失败：%v", err)
		}
		if c.endpoint != "https://gateway.example/v1/chat/completions" {
			t.Errorf("端点 = %q，期望协议路径被拼到配置的基址之后（尾斜杠不重复）", c.endpoint)
		}
		if caps := c.Capabilities(); caps.MaxContextTokens != 128000 {
			t.Errorf("MaxContextTokens = %d，期望等于配置值：上限不得探测或估算", caps.MaxContextTokens)
		}
	}
}

// ============================================================ 请求形状

func TestInfer_SendsProtocolRequest(t *testing.T) {
	f := &fakeUpstream{parts: []string{sse(`{"choices":[{"delta":{"content":"好"},"finish_reason":"stop"}]}`), "data: [DONE]\n\n"}}
	c := newAgainst(t, f, func(cfg *Config) {
		// 鉴权形状由调用方决定：本协议只管把给到的头发出去。
		cfg.Headers = map[string]string{"Authorization": "Bearer secret-value", "x-tenant": "t-1"}
	})

	_, err := c.Infer(context.Background(), llm.Request{
		Turn: 1,
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "规则"},
			{Role: llm.RoleUser, Content: "任务"},
			{Role: llm.RoleAssistant, Content: "我来看看", Calls: []llm.ToolCall{
				{ID: "c1", Name: "read", Arguments: json.RawMessage(`{"path":"a.go"}`)},
			}},
			{Role: llm.RoleTool, Results: []llm.ToolResult{
				{CallID: "c1", Output: "已读取"},
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

	got := f.last(t)
	if got.Path != "/chat/completions" {
		t.Errorf("路径 = %q", got.Path)
	}
	if h := got.Header.Get("Authorization"); h != "Bearer secret-value" {
		t.Errorf("鉴权头 = %q，期望原样透传调用方给的值", h)
	}
	if h := got.Header.Get("x-tenant"); h != "t-1" {
		t.Errorf("附加头 = %q，期望一并透传", h)
	}

	var req struct {
		Model         string `json:"model"`
		Stream        bool   `json:"stream"`
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
		Messages []struct {
			Role       string `json:"role"`
			Content    string `json:"content"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(got.Body, &req); err != nil {
		t.Fatalf("请求体不是合法 JSON：%v", err)
	}

	if req.Model != "test-model" || !req.Stream || !req.StreamOptions.IncludeUsage {
		t.Errorf("model/stream/include_usage = %q/%v/%v；用量回报不打开就只剩本地估算",
			req.Model, req.Stream, req.StreamOptions.IncludeUsage)
	}
	if len(req.Messages) != 4 {
		t.Fatalf("消息数 = %d，期望 4（工具结果按调用配对，不合并）", len(req.Messages))
	}
	if req.Messages[2].ToolCalls[0].Function.Name != "read" ||
		req.Messages[2].ToolCalls[0].Function.Arguments != `{"path":"a.go"}` {
		t.Errorf("历史里的调用未被还原：%+v，参数应当是字符串形式的 JSON", req.Messages[2].ToolCalls)
	}
	if req.Messages[3].Role != "tool" || req.Messages[3].ToolCallID != "c1" {
		t.Errorf("工具结果消息 = %+v，期望按调用标识配对", req.Messages[3])
	}
	if len(req.Tools) != 2 || req.Tools[0].Function.Name != "read" {
		t.Fatalf("工具声明 = %+v", req.Tools)
	}
	if string(req.Tools[0].Function.Parameters) != `{"type":"object"}` {
		t.Errorf("参数形状应原样搬运，实得 %s", req.Tools[0].Function.Parameters)
	}
	if len(req.Tools[1].Function.Parameters) != 0 {
		t.Errorf("没有形状的声明不得补空壳，实得 %s", req.Tools[1].Function.Parameters)
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
		sse(`{"choices":[{"delta":{"content":"第一段"}}]}`),
		sse(`{"choices":[{"delta":{"content":"第二段"},"finish_reason":"stop"}]}`),
		sse(`{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7}}`),
		"data: [DONE]\n\n",
	}}
	c := newAgainst(t, f, nil)

	s, err := c.Infer(context.Background(), llm.Request{})
	if err != nil {
		t.Fatalf("发起推理失败：%v", err)
	}
	evs := drain(t, s)

	if got := kinds(evs); len(got) != 4 ||
		got[0] != llm.EvText || got[1] != llm.EvText ||
		got[2] != llm.EvUsage || got[3] != llm.EvEnd {
		t.Fatalf("事件序列 = %v，期望 文本/文本/用量/结束", got)
	}
	if evs[0].Text != "第一段" || evs[1].Text != "第二段" {
		t.Errorf("正文增量未原样交出：%q %q", evs[0].Text, evs[1].Text)
	}
	if evs[2].Usage.InputTokens != 11 || evs[2].Usage.OutputTokens != 7 {
		t.Errorf("用量 = %+v，期望上游回报的值", evs[2].Usage)
	}
}

func TestInfer_AssemblesRawToolCallsAcrossChunks(t *testing.T) {
	f := &fakeUpstream{parts: []string{
		// 参数是流式吐出的 JSON 文本：这里刻意切成两段，中间不加任何分隔。
		sse(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read","arguments":"{\"pa"}}]}}]}`),
		sse(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"a.go\"}"}}]}}]}`),
		// 第二个调用与前一个交错出现，位置下标是唯一稳定的配对键。
		sse(`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_2","function":{"name":"glob","arguments":"{\"path\":\"**/*.go\"}"}}]},"finish_reason":"tool_calls"}]}`),
		"data: [DONE]\n\n",
	}}
	c := newAgainst(t, f, nil)

	s, err := c.Infer(context.Background(), llm.Request{})
	if err != nil {
		t.Fatalf("发起推理失败：%v", err)
	}
	evs := drain(t, s)

	if got := kinds(evs); len(got) != 3 ||
		got[0] != llm.EvToolUse || got[1] != llm.EvToolUse || got[2] != llm.EvEnd {
		t.Fatalf("事件序列 = %v，期望 两次调用 + 结束", got)
	}
	// 协议层**不解释**参数：这里断言的是原始形态，含义由绑定层解释。
	if evs[0].Call.Name != "read" || evs[0].Call.ID != "call_1" {
		t.Errorf("第一次调用 = %+v", evs[0].Call)
	}
	if string(evs[0].Call.Arguments) != `{"path":"a.go"}` {
		t.Errorf("第一次调用的参数 = %s，期望分片按位置拼成完整 JSON", evs[0].Call.Arguments)
	}
	if evs[1].Call.Name != "glob" || string(evs[1].Call.Arguments) != `{"path":"**/*.go"}` {
		t.Errorf("第二次调用 = %+v / %s", evs[1].Call, evs[1].Call.Arguments)
	}
}

func TestInfer_DoesNotInterpretArguments(t *testing.T) {
	// 参数不合法也照原样交出：协议层不认识工具集，解释与拒绝是绑定层的事。
	f := &fakeUpstream{parts: []string{
		sse(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"shell","arguments":"{\"path\":"}}]},"finish_reason":"tool_calls"}]}`),
		"data: [DONE]\n\n",
	}}
	c := newAgainst(t, f, nil)

	s, err := c.Infer(context.Background(), llm.Request{})
	if err != nil {
		t.Fatalf("发起推理失败：%v", err)
	}
	evs := drain(t, s)
	if len(evs) != 2 || evs[0].Kind != llm.EvToolUse {
		t.Fatalf("事件序列 = %v", kinds(evs))
	}
	if evs[0].Err != nil {
		t.Errorf("协议层不该对参数下判断，实得错误 = %+v", evs[0].Err)
	}
	if evs[0].Call.Name != "shell" || string(evs[0].Call.Arguments) != `{"path":` {
		t.Errorf("原始调用 = %+v / %s", evs[0].Call, evs[0].Call.Arguments)
	}
}

func TestInfer_TruncatedStreamIsExplicit(t *testing.T) {
	// 没有结束原因、也没有结束标记就断了：半截响应绝不能被当成完整回答。
	f := &fakeUpstream{parts: []string{sse(`{"choices":[{"delta":{"content":"半句"}}]}`)}}
	c := newAgainst(t, f, nil)

	s, err := c.Infer(context.Background(), llm.Request{})
	if err != nil {
		t.Fatalf("发起推理失败：%v", err)
	}
	evs := drain(t, s)
	last := evs[len(evs)-1]
	if last.Kind != llm.EvError || last.Err == nil || last.Err.Kind != "stream_truncated" {
		t.Fatalf("最后一条事件 = %+v，期望显式的流中断", last)
	}
	if !last.Err.Retryable {
		t.Error("流中断属环境问题，重跑有意义")
	}
}

func TestInfer_MissingDoneMarkerButFinishedIsNormalEnd(t *testing.T) {
	// 相当一部分网关省略末尾标记：见过结束原因就不算截断，否则它们全都不可用。
	f := &fakeUpstream{parts: []string{sse(`{"choices":[{"delta":{"content":"完了"},"finish_reason":"stop"}]}`)}}
	c := newAgainst(t, f, nil)

	s, err := c.Infer(context.Background(), llm.Request{})
	if err != nil {
		t.Fatalf("发起推理失败：%v", err)
	}
	if got := kinds(drain(t, s)); len(got) != 2 || got[1] != llm.EvEnd {
		t.Fatalf("事件序列 = %v，期望正常结束", got)
	}
}

func TestInfer_UpstreamErrorInsideStream(t *testing.T) {
	f := &fakeUpstream{parts: []string{
		sse(`{"choices":[{"delta":{"content":"开始"}}]}`),
		sse(`{"error":{"type":"server_error","message":"上游内部错误"}}`),
	}}
	c := newAgainst(t, f, nil)

	s, err := c.Infer(context.Background(), llm.Request{})
	if err != nil {
		t.Fatalf("发起推理失败：%v", err)
	}
	evs := drain(t, s)
	last := evs[len(evs)-1]
	if last.Kind != llm.EvError || last.Err == nil {
		t.Fatalf("最后一条事件 = %+v，期望流内错误被上报", last)
	}
	if !strings.Contains(last.Err.Message, "上游内部错误") || !last.Err.Retryable {
		t.Errorf("错误 = %+v，应保留上游说法且标为可重试", last.Err)
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
		parts: []string{sse(`{"choices":[{"delta":{"content":"一"}}]}`)},
		hold:  true,
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
