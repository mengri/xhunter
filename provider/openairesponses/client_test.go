package openairesponses

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
		Model:            "gpt-test",
		MaxContextTokens: 400000,
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
				t.Fatal("配置不成立时必须在构造期失败")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("错误 = %v，期望可识别的 %v", err, tc.wantErr)
			}
		})
	}
}

func TestNew_BuildsEndpointAndPassesHeadersThrough(t *testing.T) {
	f := &fakeUpstream{parts: []string{sse(`{"type":"response.completed","response":{}}`)}}
	c := newAgainst(t, f, func(cfg *Config) {
		cfg.Headers = map[string]string{"Authorization": "Bearer secret-value"}
	})

	if !strings.HasSuffix(c.endpoint, "/responses") {
		t.Fatalf("端点 = %q，期望协议路径拼在基址之后", c.endpoint)
	}
	if _, err := c.Infer(context.Background(), llm.Request{}); err != nil {
		t.Fatalf("发起推理失败：%v", err)
	}
	got := f.last(t)
	if got.Path != "/responses" {
		t.Errorf("路径 = %q", got.Path)
	}
	if h := got.Header.Get("Authorization"); h != "Bearer secret-value" {
		t.Errorf("鉴权头 = %q，期望原样透传调用方给的值", h)
	}
}

// ============================================================ 请求形状

func TestInfer_SendsProtocolRequest(t *testing.T) {
	f := &fakeUpstream{parts: []string{sse(`{"type":"response.completed","response":{}}`)}}
	c := newAgainst(t, f, nil)

	_, err := c.Infer(context.Background(), llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "规则"},
			{Role: llm.RoleUser, Content: "任务"},
			{Role: llm.RoleAssistant, Content: "我来看看", Calls: []llm.ToolCall{
				{ID: "call_1", Name: "read", Arguments: json.RawMessage(`{"path":"a.go"}`)},
			}},
			{Role: llm.RoleTool, Results: []llm.ToolResult{
				{CallID: "call_1", Output: "已读取"},
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
		Model        string `json:"model"`
		Instructions string `json:"instructions"`
		Stream       bool   `json:"stream"`
		Input        []struct {
			Type      string `json:"type"`
			Role      string `json:"role"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Output    string `json:"output"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"input"`
		Tools []struct {
			Type       string          `json:"type"`
			Name       string          `json:"name"`
			Parameters json.RawMessage `json:"parameters"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(f.last(t).Body, &req); err != nil {
		t.Fatalf("请求体不是合法 JSON：%v", err)
	}

	if req.Model != "gpt-test" || !req.Stream {
		t.Errorf("model/stream = %q/%v", req.Model, req.Stream)
	}
	// 系统提示是顶层字段，不是一条消息。
	if req.Instructions != "规则" {
		t.Errorf("instructions = %q，期望提到顶层字段", req.Instructions)
	}
	// 对话是一个类型化条目数组：消息、函数调用、函数调用结果各占一条。
	if len(req.Input) != 4 {
		t.Fatalf("条目数 = %d，期望 4（用户消息 / 助手消息 / 函数调用 / 结果）", len(req.Input))
	}
	if req.Input[0].Type != "message" || req.Input[0].Content[0].Type != "input_text" {
		t.Errorf("用户条目 = %+v", req.Input[0])
	}
	if req.Input[1].Type != "message" || req.Input[1].Content[0].Type != "output_text" {
		t.Errorf("助手条目 = %+v，助手正文要用 output_text 部件", req.Input[1])
	}
	if req.Input[2].Type != "function_call" || req.Input[2].Name != "read" ||
		req.Input[2].CallID != "call_1" || req.Input[2].Arguments != `{"path":"a.go"}` {
		t.Errorf("函数调用条目 = %+v", req.Input[2])
	}
	if req.Input[3].Type != "function_call_output" || req.Input[3].CallID != "call_1" {
		t.Errorf("结果条目 = %+v，期望按调用标识与调用配对", req.Input[3])
	}
	// 工具声明是平的：没有"函数"外层包装；无形状时不发 parameters。
	if len(req.Tools) != 2 || req.Tools[0].Type != "function" || req.Tools[0].Name != "read" {
		t.Fatalf("工具声明 = %+v", req.Tools)
	}
	if string(req.Tools[0].Parameters) != `{"type":"object"}` {
		t.Errorf("参数形状应原样搬运，实得 %s", req.Tools[0].Parameters)
	}
	if len(req.Tools[1].Parameters) != 0 {
		t.Errorf("没有形状时不该发空壳，实得 %s", req.Tools[1].Parameters)
	}
}

func TestInfer_RejectsUnknownRole(t *testing.T) {
	c := newAgainst(t, &fakeUpstream{}, nil)
	_, err := c.Infer(context.Background(), llm.Request{Messages: []llm.Message{{Role: llm.Role("human"), Content: "x"}}})
	if err == nil {
		t.Fatal("未知角色必须显式报错：角色决定消息被当成什么")
	}
}

// ============================================================ 流翻译

func TestInfer_TranslatesTextUsageAndCompleted(t *testing.T) {
	f := &fakeUpstream{parts: []string{
		sse(`{"type":"response.created","response":{"status":"in_progress"}}`),
		sse(`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","role":"assistant"}}`),
		sse(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"第一段"}`),
		sse(`{"type":"response.reasoning_summary_text.delta","delta":"思考"}`),
		sse(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"第二段"}`),
		sse(`{"type":"response.output_text.done","output_index":0,"content_index":0,"text":"第一段第二段"}`),
		sse(`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":25,"output_tokens":15}}}`),
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
		t.Fatalf("事件序列 = %v，期望 文本/文本/用量/结束（其余事件忽略）", got)
	}
	if evs[0].Text != "第一段" || evs[1].Text != "第二段" {
		t.Errorf("正文增量 = %q / %q", evs[0].Text, evs[1].Text)
	}
	if evs[2].Usage.InputTokens != 25 || evs[2].Usage.OutputTokens != 15 {
		t.Errorf("用量 = %+v，期望取自 completed 事件", evs[2].Usage)
	}
}

func TestInfer_AssemblesRawFunctionCallFromDeltas(t *testing.T) {
	f := &fakeUpstream{parts: []string{
		sse(`{"type":"response.output_text.delta","delta":"我来看看"}`),
		sse(`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_1","name":"read","arguments":""}}`),
		sse(`{"type":"response.function_call_arguments.delta","output_index":1,"item_id":"fc_1","delta":"{\"path\":\"a.go\","}`),
		sse(`{"type":"response.function_call_arguments.delta","output_index":1,"item_id":"fc_1","delta":"\"range\":{\"from\":1,\"to\":10}}"}`),
		sse(`{"type":"response.function_call_arguments.done","output_index":1,"item_id":"fc_1","arguments":"{\"path\":\"a.go\",\"range\":{\"from\":1,\"to\":10}}"}`),
		sse(`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":20}}}`),
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
	if evs[2].Call.Name != "read" || evs[2].Call.ID != "call_1" {
		t.Errorf("调用 = %+v", evs[2].Call)
	}
	if !strings.Contains(string(evs[2].Call.Arguments), `"a.go"`) ||
		!strings.Contains(string(evs[2].Call.Arguments), `"to":10`) {
		t.Errorf("参数 = %s，期望分片拼成完整 JSON", evs[2].Call.Arguments)
	}
}

func TestInfer_FallsBackToItemArguments(t *testing.T) {
	// 不逐片吐参数的上游：只在条目事件里给完整参数。
	f := &fakeUpstream{parts: []string{
		sse(`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_9","name":"glob","arguments":"{\"path\":\"**/*.go\"}"}}`),
		sse(`{"type":"response.completed","response":{"status":"completed"}}`),
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
	if evs[0].Call.Name != "glob" || string(evs[0].Call.Arguments) != `{"path":"**/*.go"}` {
		t.Errorf("调用 = %+v / %s，期望用条目里的完整参数", evs[0].Call, evs[0].Call.Arguments)
	}
}

func TestInfer_FailedResponseIsReported(t *testing.T) {
	cases := []struct {
		name      string
		payload   string
		retryable bool
	}{
		{
			name:      "服务端故障",
			payload:   `{"type":"response.failed","response":{"status":"failed","error":{"code":"server_error","message":"内部错误"}}}`,
			retryable: true,
		},
		{
			name:      "内容被拒",
			payload:   `{"type":"response.failed","response":{"status":"failed","error":{"code":"invalid_prompt","message":"拒绝"}}}`,
			retryable: false,
		},
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
				t.Fatalf("事件 = %+v，期望显式错误", last)
			}
			if last.Err.Retryable != tc.retryable {
				t.Errorf("可重试性 = %v，期望 %v（%q）", last.Err.Retryable, tc.retryable, last.Err.Message)
			}
		})
	}
}

func TestInfer_ErrorEventIsReported(t *testing.T) {
	f := &fakeUpstream{parts: []string{
		sse(`{"type":"error","code":"rate_limit_exceeded","message":"太频繁","sequence_number":1}`),
	}}
	c := newAgainst(t, f, nil)

	s, err := c.Infer(context.Background(), llm.Request{})
	if err != nil {
		t.Fatalf("发起推理失败：%v", err)
	}
	last := drain(t, s)[0]
	if last.Kind != llm.EvError || last.Err == nil || !last.Err.Retryable {
		t.Fatalf("事件 = %+v，期望可重试的显式错误", last)
	}
	if !strings.Contains(last.Err.Message, "rate_limit_exceeded") {
		t.Errorf("错误信息 = %q，应带上上游的编码", last.Err.Message)
	}
}

func TestInfer_IncompleteIsNormalEnd(t *testing.T) {
	// 输出不完整（例如达到上限）仍是一次完整的响应，只是内容被截断：
	// 当错误上报会把它变成环境问题，而真正该做的是调高上限。
	f := &fakeUpstream{parts: []string{
		sse(`{"type":"response.output_text.delta","delta":"半句"}`),
		sse(`{"type":"response.incomplete","response":{"status":"incomplete"}}`),
	}}
	c := newAgainst(t, f, nil)

	s, err := c.Infer(context.Background(), llm.Request{})
	if err != nil {
		t.Fatalf("发起推理失败：%v", err)
	}
	if got := kinds(drain(t, s)); len(got) != 2 || got[1] != llm.EvEnd {
		t.Fatalf("事件序列 = %v，期望正常结束", got)
	}
}

func TestInfer_TruncatedWithoutCompletedIsExplicit(t *testing.T) {
	f := &fakeUpstream{parts: []string{sse(`{"type":"response.output_text.delta","delta":"半句"}`)}}
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
		{http.StatusBadGateway, true},
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
		parts: []string{sse(`{"type":"response.output_text.delta","delta":"一"}`)},
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
