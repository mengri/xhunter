package main

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

	"xhunter/llm"
	"xhunter/provider/adapter"
	"xhunter/provider/openaichat"
	"xhunter/providerconfig"
)

// captureUpstream 记录收到的请求，并按协议路径回一个最简的合法流。
type captureUpstream struct {
	mu     sync.Mutex
	header http.Header
	path   string
	body   []byte
}

func (c *captureUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	c.header, c.path, c.body = r.Header.Clone(), r.URL.Path, body
	c.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	switch r.URL.Path {
	case "/messages":
		_, _ = io.WriteString(w, `data: {"type":"message_stop"}`+"\n\n")
	case "/responses":
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{}}`+"\n\n")
	default:
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
}

func (c *captureUpstream) last(t *testing.T) (http.Header, string) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.header == nil {
		t.Fatal("上游没有收到任何请求")
	}
	return c.header, c.path
}

func (c *captureUpstream) requestBody(t *testing.T) []byte {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.body == nil {
		t.Fatal("上游没有收到任何请求")
	}
	return c.body
}

// testResolved 是"环境变量已解析完"的接入事实：组装层从这里开始。
func testResolved(baseURL string) providerconfig.Resolved {
	return providerconfig.Resolved{
		Protocol:         providerconfig.ProtocolOpenAIChat,
		ModelID:          "m1",
		BaseURL:          baseURL,
		MaxContextTokens: 128000,
		OutputReserve:    8192,
	}
}

// inferOnce 走完一次最小推理，用来观察装配结果落在请求上的样子。
func inferOnce(t *testing.T, p llm.Provider) {
	t.Helper()
	s, err := p.Infer(context.Background(), llm.Request{})
	if err != nil {
		t.Fatalf("发起推理失败：%v", err)
	}
	for range s.Events() {
	}
}

func TestProviderFor_BuildsEveryKnownProtocol(t *testing.T) {
	for _, protocol := range providerconfig.Protocols() {
		r := testResolved("https://example.invalid/v1")
		r.Protocol = protocol
		p, err := providerFor(protocolFactories(), r)
		if err != nil {
			t.Fatalf("协议=%q 装配失败：%v", protocol, err)
		}
		if caps := p.Capabilities(); caps.MaxContextTokens != r.MaxContextTokens {
			t.Errorf("协议=%q 的能力声明 = %+v，期望上限等于接入事实", protocol, caps)
		}
	}
}

// 协议取值未知时必须在启动期失败，并列出已装配的项——"环境变量写错了协议名"
// 与"组装层还没接这种协议"是两种故障，错误信息要能直接区分。
func TestProviderFor_UnknownProtocolTellsWhereToAddOne(t *testing.T) {
	r := testResolved("https://example.invalid/v1")
	r.Protocol = "some-new-protocol"
	_, err := providerFor(protocolFactories(), r)
	if err == nil {
		t.Fatal("未装配的协议必须在启动期失败，而不是等到第一次推理")
	}
	if !strings.Contains(err.Error(), "组装层") ||
		!strings.Contains(err.Error(), providerconfig.ProtocolOpenAIChat) {
		t.Errorf("错误信息 = %q，应说明去组装层加一行并列出现已装配的项", err)
	}
}

func TestProviderFor_PropagatesMissingContextWindow(t *testing.T) {
	r := testResolved("https://example.invalid/v1")
	r.MaxContextTokens = 0
	_, err := providerFor(protocolFactories(), r)
	if !errors.Is(err, adapter.ErrNoContextWindow) {
		t.Fatalf("错误 = %v，期望可识别的上限缺失错误：上限是唯一不允许估算的能力", err)
	}
}

// 端点不预设：接入事实没给就必须在启动期失败，而不是悄悄用某个官方地址。
func TestProviderFor_RequiresEndpoint(t *testing.T) {
	for _, protocol := range providerconfig.Protocols() {
		r := testResolved("")
		r.Protocol = protocol
		if _, err := providerFor(protocolFactories(), r); err == nil {
			t.Errorf("协议=%q 缺少端点时必须失败：核心不预设任何主机", protocol)
		}
	}
}

// XHUNTER_API_KEY 是便捷形式：补一个标准的 Authorization: Bearer。
func TestProviderFor_APIKeyBecomesBearerHeader(t *testing.T) {
	up := &captureUpstream{}
	srv := httptest.NewServer(up)
	defer srv.Close()

	r := testResolved(srv.URL)
	r.APIKey = "secret-value"

	p, err := providerFor(protocolFactories(), r)
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	inferOnce(t, p)

	header, path := up.last(t)
	if got := header.Get("Authorization"); got != "Bearer secret-value" {
		t.Errorf("鉴权头 = %q，期望由组装层从凭据值补成 Bearer", got)
	}
	if path != "/chat/completions" {
		t.Errorf("路径 = %q，期望协议路径由协议实现拼接", path)
	}
}

// 未提供凭据不是错误：有的网关不需要鉴权。此时请求不带鉴权头。
func TestProviderFor_NoCredentialMeansNoAuthHeader(t *testing.T) {
	up := &captureUpstream{}
	srv := httptest.NewServer(up)
	defer srv.Close()

	p, err := providerFor(protocolFactories(), testResolved(srv.URL))
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	inferOnce(t, p)

	header, _ := up.last(t)
	if got := header.Get("Authorization"); got != "" {
		t.Errorf("未提供凭据时不该带鉴权头，实际 %q", got)
	}
}

// 鉴权方式由环境变量决定：非标准形状（裸 x-api-key）靠 XHUNTER_HEADERS 表达，
// 不需要改代码。引用展开发生在解析阶段，因此这里收到的是成品值。
func TestProviderFor_CustomAuthHeaderPassesThrough(t *testing.T) {
	up := &captureUpstream{}
	srv := httptest.NewServer(up)
	defer srv.Close()

	r := testResolved(srv.URL)
	r.Protocol = providerconfig.ProtocolAnthropicMessages
	r.Headers = map[string]string{"x-api-key": "sk-ant-raw"}

	p, err := providerFor(protocolFactories(), r)
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	inferOnce(t, p)

	header, path := up.last(t)
	if path != "/messages" {
		t.Errorf("路径 = %q", path)
	}
	if got := header.Get("x-api-key"); got != "sk-ant-raw" {
		t.Errorf("凭据头 = %q，期望原样发出", got)
	}
	if header.Get("Authorization") != "" {
		t.Error("本协议不该出现 Authorization 头（接入事实没要求它）")
	}

	var body struct {
		MaxTokens int `json:"max_tokens"`
	}
	if err := json.Unmarshal(up.requestBody(t), &body); err != nil {
		t.Fatalf("请求体不是合法 JSON：%v", err)
	}
	if body.MaxTokens != r.OutputReserve {
		t.Errorf("max_tokens = %d，期望取输出预留 %d：本协议该字段必填",
			body.MaxTokens, r.OutputReserve)
	}
}

func TestProviderFor_OpenAIResponsesWiring(t *testing.T) {
	up := &captureUpstream{}
	srv := httptest.NewServer(up)
	defer srv.Close()

	r := testResolved(srv.URL)
	r.Protocol = providerconfig.ProtocolOpenAIResponses
	r.APIKey = "sk-openai"

	p, err := providerFor(protocolFactories(), r)
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	inferOnce(t, p)

	header, path := up.last(t)
	if path != "/responses" {
		t.Errorf("路径 = %q，期望走 Responses 协议", path)
	}
	if got := header.Get("Authorization"); got != "Bearer sk-openai" {
		t.Errorf("鉴权头 = %q，期望 Bearer 前缀由组装层补上", got)
	}
}

// 针对性工厂的形状：接一种**新协议**只需在组装层加一行，协议实现根本不知道
// 自己被哪个取值选中。这里用一个假想协议演示差异（鉴权头换名）也一并成立。
func TestProviderFor_NewProtocolIsJustAnotherEntry(t *testing.T) {
	up := &captureUpstream{}
	srv := httptest.NewServer(up)
	defer srv.Close()

	const fakeProtocol = "vendor-gateway"
	table := map[string]protocolFactory{
		fakeProtocol: func(r providerconfig.Resolved) (llm.Provider, error) {
			headers := requestHeaders(r)
			// 该网关只认自己的鉴权头，且是裸值。
			delete(headers, "Authorization")
			headers["x-gateway-token"] = r.APIKey
			return openaichat.New(openaichat.Config{
				BaseURL:          r.BaseURL,
				Model:            r.ModelID,
				Headers:          headers,
				MaxContextTokens: r.MaxContextTokens,
			})
		},
	}

	r := testResolved(srv.URL)
	r.Protocol = fakeProtocol
	r.APIKey = "secret-value"

	p, err := providerFor(table, r)
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	inferOnce(t, p)

	header, _ := up.last(t)
	if got := header.Get("x-gateway-token"); got != "secret-value" {
		t.Errorf("鉴权头 = %q，期望该协议的形状由组装层决定", got)
	}
	if header.Get("Authorization") != "" {
		t.Error("该协议的工厂应当去掉默认的 Bearer 头")
	}
}

// 客户端标识默认注入：与协议、鉴权都无关，任何协议请求都带上。
func TestProviderFor_InjectsClientIdentity(t *testing.T) {
	up := &captureUpstream{}
	srv := httptest.NewServer(up)
	defer srv.Close()

	p, err := providerFor(protocolFactories(), testResolved(srv.URL))
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	inferOnce(t, p)

	header, _ := up.last(t)
	if got := header.Get("User-Agent"); got != "xhunter/"+version {
		t.Errorf("客户端标识 = %q，期望默认注入 xhunter/<version>", got)
	}
}

// 客户端标识不提供入口：接入事实里的 User-Agent 被忽略，恒为系统值。
func TestProviderFor_IgnoresConfiguredUserAgent(t *testing.T) {
	up := &captureUpstream{}
	srv := httptest.NewServer(up)
	defer srv.Close()

	r := testResolved(srv.URL)
	r.Headers = map[string]string{"User-Agent": "gateway-probe/1.0"}
	p, err := providerFor(protocolFactories(), r)
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	inferOnce(t, p)

	header, _ := up.last(t)
	if got := header.Get("User-Agent"); got != "xhunter/"+version {
		t.Errorf("客户端标识 = %q，期望恒为系统值 xhunter/<version>，接入事实里的同名头被忽略", got)
	}
}

// 头名大小写不该决定鉴权结果：`authorization` 与凭据值派生出的 `Authorization`
// 必须被当成同一个头，否则 map 迭代顺序会决定发出去哪一个。
func TestProviderFor_HeaderNamesAreCaseInsensitive(t *testing.T) {
	r := testResolved("https://gw.example/v1")
	r.Headers = map[string]string{"authorization": "Bearer from-env"}
	r.APIKey = "from-api-key"

	headers := requestHeaders(r)
	if got := headers["Authorization"]; got != "Bearer from-env" {
		t.Errorf("显式写的鉴权头应生效且只发一份：%q", got)
	}
	// 不该同时留下小写那份。
	for name := range headers {
		if name != http.CanonicalHeaderKey(name) {
			t.Errorf("头名应归一为规范形式：%q", name)
		}
	}
}

// User-Agent 由系统注入：接入事实里怎么写都不会覆盖它（任意大小写）。
func TestProviderFor_UserAgentAlwaysSystemValue(t *testing.T) {
	r := testResolved("https://gw.example/v1")
	r.Headers = map[string]string{"user-agent": "custom/1.0", "X-Keep": "v"}

	headers := requestHeaders(r)
	if got := headers["User-Agent"]; got != "xhunter/"+version {
		t.Errorf("User-Agent = %q，期望系统值", got)
	}
	if headers["X-Keep"] != "v" {
		t.Errorf("其它头应原样保留：%v", headers)
	}
}
