package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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

func testResolved(baseURL string) providerconfig.Resolved {
	return providerconfig.Resolved{
		ProviderID:       "vendor",
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
	for _, sdk := range []string{
		"", providerconfig.SDKBuiltin, providerconfig.SDKOpenAICompatible,
		providerconfig.SDKOpenAI, providerconfig.SDKAnthropic,
	} {
		r := testResolved("https://example.invalid/v1")
		r.SDK = sdk
		p, err := providerFor(vendorFactories(), r)
		if err != nil {
			t.Fatalf("SDK=%q 装配失败：%v", sdk, err)
		}
		if caps := p.Capabilities(); caps.MaxContextTokens != r.MaxContextTokens {
			t.Errorf("SDK=%q 的能力声明 = %+v，期望上限等于配置值", sdk, caps)
		}
	}
}

func TestProviderFor_UnknownSDKTellsWhereToAddAFactory(t *testing.T) {
	r := testResolved("https://example.invalid/v1")
	r.SDK = "@vendor/private-gateway"
	_, err := providerFor(vendorFactories(), r)
	if err == nil {
		t.Fatal("未装配的协议必须在启动期失败，而不是等到第一次推理")
	}
	// 错误信息要能直接告诉维护者该动哪里：协议实现不做注册，所以答案永远在组装层。
	if !strings.Contains(err.Error(), "组装层") || !strings.Contains(err.Error(), providerconfig.SDKOpenAICompatible) {
		t.Errorf("错误信息 = %q，应说明去组装层加一条并列出现已装配的项", err)
	}
}

func TestProviderFor_PropagatesMissingContextWindow(t *testing.T) {
	r := testResolved("https://example.invalid/v1")
	r.MaxContextTokens = 0
	_, err := providerFor(vendorFactories(), r)
	if !errors.Is(err, adapter.ErrNoContextWindow) {
		t.Fatalf("错误 = %v，期望可识别的上限缺失错误：上限是唯一不允许估算的能力", err)
	}
}

// 端点不预设：配置没给就必须在启动期失败，而不是悄悄用某个官方地址。
func TestProviderFor_RequiresEndpointFromConfig(t *testing.T) {
	for _, sdk := range []string{"", providerconfig.SDKOpenAI, providerconfig.SDKAnthropic} {
		r := testResolved("")
		r.SDK = sdk
		r.HasAPIKey = false
		if _, err := providerFor(vendorFactories(), r); err == nil {
			t.Errorf("SDK=%q 缺少 baseURL 时必须失败：本包不预设任何主机", sdk)
		}
	}
}

// apiKey 是便捷形式：补一个标准的 Authorization: Bearer。
func TestProviderFor_ResolvesAPIKeyIntoBearerHeader(t *testing.T) {
	up := &captureUpstream{}
	srv := httptest.NewServer(up)
	defer srv.Close()

	t.Setenv("XHUNTER_TEST_KEY", "secret-value")
	r := testResolved(srv.URL)
	r.HasAPIKey = true
	r.APIKeyEnv = "XHUNTER_TEST_KEY"

	p, err := providerFor(vendorFactories(), r)
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	inferOnce(t, p)

	header, path := up.last(t)
	if got := header.Get("Authorization"); got != "Bearer secret-value" {
		t.Errorf("鉴权头 = %q，期望由组装层从环境变量解析成 Bearer", got)
	}
	if path != "/chat/completions" {
		t.Errorf("路径 = %q，期望协议路径由协议实现拼接", path)
	}
}

func TestProviderFor_RejectsMissingCredentialValue(t *testing.T) {
	r := testResolved("https://example.invalid/v1")
	r.HasAPIKey = true
	r.APIKeyEnv = "XHUNTER_TEST_ABSENT_KEY"
	_, err := providerFor(vendorFactories(), r)
	if !errors.Is(err, errNoCredential) {
		t.Fatalf("错误 = %v，期望凭据缺失错误：它属启动期问题，不是运行中途的意外", err)
	}
}

// 鉴权方式可配置：任意鉴权形状都靠请求头表达，值支持 {env:VAR} 引用。
func TestProviderFor_ResolvesEnvRefInHeaders(t *testing.T) {
	up := &captureUpstream{}
	srv := httptest.NewServer(up)
	defer srv.Close()

	t.Setenv("XHUNTER_TEST_KEY", "sk-ant-raw")
	r := testResolved(srv.URL)
	r.SDK = providerconfig.SDKAnthropic
	// 本协议惯用 x-api-key（裸值）：这就是"非标准鉴权不需要改代码"。
	r.Headers = map[string]string{"x-api-key": "{env:XHUNTER_TEST_KEY}"}

	p, err := providerFor(vendorFactories(), r)
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	inferOnce(t, p)

	header, path := up.last(t)
	if path != "/messages" {
		t.Errorf("路径 = %q", path)
	}
	if got := header.Get("x-api-key"); got != "sk-ant-raw" {
		t.Errorf("凭据头 = %q，期望引用被解析成裸 key", got)
	}
	if header.Get("Authorization") != "" {
		t.Error("本协议不该出现 Authorization 头（配置没要求它）")
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

func TestProviderFor_RejectsMissingEnvRefValue(t *testing.T) {
	r := testResolved("https://example.invalid/v1")
	r.Headers = map[string]string{"x-api-key": "{env:XHUNTER_TEST_ABSENT_KEY}"}
	if _, err := providerFor(vendorFactories(), r); err == nil {
		t.Fatal("引用指向未设置的变量必须在启动期失败，而不是发一个空头")
	}
}

func TestProviderFor_OpenAIResponsesWiring(t *testing.T) {
	up := &captureUpstream{}
	srv := httptest.NewServer(up)
	defer srv.Close()

	t.Setenv("XHUNTER_TEST_KEY", "sk-openai")
	r := testResolved(srv.URL)
	r.SDK = providerconfig.SDKOpenAI
	r.HasAPIKey = true
	r.APIKeyEnv = "XHUNTER_TEST_KEY"

	p, err := providerFor(vendorFactories(), r)
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

// 针对性工厂的形状：同一份配置经由不同工厂得到不同的连线方式。
// 这里用一个假想的网关演示差异（鉴权头换名），证明"加一条即可"，
// 且完全不需要改协议实现——协议实现根本不知道有这么一个厂商。
func TestProviderFor_VendorSpecificFactoryIsJustAnotherEntry(t *testing.T) {
	up := &captureUpstream{}
	srv := httptest.NewServer(up)
	defer srv.Close()

	t.Setenv("XHUNTER_TEST_KEY", "secret-value")

	table := map[string]providerFactory{
		"@test/vendor-custom-auth": func(r providerconfig.Resolved) (llm.Provider, error) {
			headers, err := requestHeaders(r)
			if err != nil {
				return nil, err
			}
			// 该网关只认自己的鉴权头，且是裸值。
			delete(headers, "Authorization")
			headers["x-gateway-token"] = strings.TrimSpace(os.Getenv(r.APIKeyEnv))
			return openaichat.New(openaichat.Config{
				BaseURL:          r.BaseURL,
				Model:            r.ModelID,
				Headers:          headers,
				MaxContextTokens: r.MaxContextTokens,
			})
		},
	}

	r := testResolved(srv.URL)
	r.SDK = "@test/vendor-custom-auth"
	r.HasAPIKey = true
	r.APIKeyEnv = "XHUNTER_TEST_KEY"

	p, err := providerFor(table, r)
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	inferOnce(t, p)

	header, _ := up.last(t)
	if got := header.Get("x-gateway-token"); got != "secret-value" {
		t.Errorf("鉴权头 = %q，期望厂商特有的形状由组装层决定", got)
	}
	if header.Get("Authorization") != "" {
		t.Error("该厂商的工厂应当去掉默认的 Bearer 头")
	}
}

// 客户端标识默认注入：与厂商、协议、鉴权都无关，任何协议请求都带上。
func TestProviderFor_InjectsClientIdentity(t *testing.T) {
	up := &captureUpstream{}
	srv := httptest.NewServer(up)
	defer srv.Close()

	r := testResolved(srv.URL)
	p, err := providerFor(vendorFactories(), r)
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	inferOnce(t, p)

	header, _ := up.last(t)
	if got := header.Get("User-Agent"); got != "xhunter/"+version {
		t.Errorf("客户端标识 = %q，期望默认注入 xhunter/<version>", got)
	}
}

// 客户端标识不提供配置入口：配置里的 User-Agent 被忽略，恒为系统值。
func TestProviderFor_IgnoresConfiguredUserAgent(t *testing.T) {
	up := &captureUpstream{}
	srv := httptest.NewServer(up)
	defer srv.Close()

	r := testResolved(srv.URL)
	r.Headers = map[string]string{"User-Agent": "gateway-probe/1.0"}
	p, err := providerFor(vendorFactories(), r)
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	inferOnce(t, p)

	header, _ := up.last(t)
	if got := header.Get("User-Agent"); got != "xhunter/"+version {
		t.Errorf("客户端标识 = %q，期望恒为系统值 xhunter/<version>，配置里的同名头被忽略", got)
	}
}
