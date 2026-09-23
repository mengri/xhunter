package providerconfig

import (
	"errors"
	"strings"
	"testing"
)

// fakeLookup 造一个环境来源，使用例不必摆弄进程环境。
func fakeLookup(kv map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := kv[name]
		return v, ok
	}
}

// 全量齐备：协议、模型、端点、上限都能解析出来。
func TestFromEnv_ResolvesFullFacts(t *testing.T) {
	r, err := FromEnv(fakeLookup(map[string]string{
		EnvProtocol:     ProtocolAnthropicMessages,
		EnvModel:        "claude-test",
		EnvBaseURL:      "https://api.example.com/v1",
		EnvAPIKey:       "sk-secret",
		EnvModelContext: "200000",
		EnvModelOutput:  "8192",
	}))
	if err != nil {
		t.Fatalf("齐备时不该报错：%v", err)
	}
	if r.Protocol != ProtocolAnthropicMessages || r.ModelID != "claude-test" {
		t.Errorf("协议/模型解析异常：%+v", r)
	}
	if r.BaseURL != "https://api.example.com/v1" || r.APIKey != "sk-secret" {
		t.Errorf("连接事实解析异常：%+v", r)
	}
	if r.MaxContextTokens != 200000 || r.OutputReserve != 8192 {
		t.Errorf("上限解析异常：%+v", r)
	}
}

// 未声明协议即默认协议：绝大多数端点说的就是 OpenAI 兼容那一套形状。
func TestFromEnv_ProtocolDefaults(t *testing.T) {
	r, err := FromEnv(fakeLookup(map[string]string{
		EnvModel:        "m",
		EnvBaseURL:      "https://gw.example/v1",
		EnvModelContext: "1000",
		EnvModelOutput:  "100",
	}))
	if err != nil {
		t.Fatalf("缺省协议应可解析：%v", err)
	}
	if r.Protocol != ProtocolOpenAIChat {
		t.Errorf("缺省协议 = %q，期望 %q", r.Protocol, ProtocolOpenAIChat)
	}
}

// 未知协议必须在启动期失败，并列出可用取值——"写错了协议名"要能当场改对。
func TestFromEnv_UnknownProtocolIsExplicit(t *testing.T) {
	_, err := FromEnv(fakeLookup(map[string]string{
		EnvProtocol:     "@vendor/gateway",
		EnvModel:        "m",
		EnvBaseURL:      "https://gw.example/v1",
		EnvModelContext: "1000",
		EnvModelOutput:  "100",
	}))
	if err == nil {
		t.Fatal("未知协议必须报错")
	}
	msg := err.Error()
	if !strings.Contains(msg, EnvProtocol) {
		t.Errorf("错误应指名变量：%s", msg)
	}
	for _, p := range Protocols() {
		if !strings.Contains(msg, p) {
			t.Errorf("错误应列出可用协议 %q：%s", p, msg)
		}
	}
}

// 上限类字段不得估算（FR-9.5）：缺失即显式失败，且**一次报出全部问题**。
func TestFromEnv_ReportsAllIssuesAtOnce(t *testing.T) {
	_, err := FromEnv(fakeLookup(map[string]string{}))
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want ValidationError, got %v", err)
	}
	for _, want := range []string{EnvModel, EnvBaseURL, EnvModelContext, EnvModelOutput} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("一次应报出 %s：%s", want, err.Error())
		}
	}
	if len(ve.Issues) < 4 {
		t.Errorf("问题条数 = %d，期望至少 4：%v", len(ve.Issues), ve.Error())
	}
}

// 取值写错（非数字 / 非正 / 输出预留不小于窗口）同样是显式问题。
func TestFromEnv_RejectsBadLimitValues(t *testing.T) {
	cases := map[string]map[string]string{
		"非数字": {EnvModelContext: "abc"},
		"零":   {EnvModelContext: "0"},
		"负数":  {EnvModelOutput: "-1"},
		"输出不小于窗口": {
			EnvModelContext: "1000", EnvModelOutput: "1000",
		},
	}
	for name, over := range cases {
		env := map[string]string{
			EnvModel:        "m",
			EnvBaseURL:      "https://gw.example/v1",
			EnvModelContext: "1000",
			EnvModelOutput:  "100",
		}
		for k, v := range over {
			env[k] = v
		}
		if _, err := FromEnv(fakeLookup(env)); err == nil {
			t.Errorf("%s：必须报错", name)
		}
	}
}

// 凭据缺省是合法的：有的网关不需要鉴权。未声明就不带鉴权头，而不是报错。
func TestFromEnv_APIKeyIsOptional(t *testing.T) {
	r, err := FromEnv(fakeLookup(map[string]string{
		EnvModel:        "m",
		EnvBaseURL:      "https://gw.example/v1",
		EnvModelContext: "1000",
		EnvModelOutput:  "100",
	}))
	if err != nil {
		t.Fatalf("未提供凭据不该报错：%v", err)
	}
	if r.APIKey != "" {
		t.Errorf("未提供时凭据应为空，实际 %q", r.APIKey)
	}
}

// 自定义请求头：JSON 对象，值支持带前缀的 {env:VAR} 引用——凭据仍可只存在于专用变量里。
func TestFromEnv_HeadersExpandEnvRefs(t *testing.T) {
	r, err := FromEnv(fakeLookup(map[string]string{
		EnvModel:        "m",
		EnvBaseURL:      "https://gw.example/v1",
		EnvModelContext: "1000",
		EnvModelOutput:  "100",
		EnvHeaders:      `{"X-Tenant":"acme","Authorization":"Bearer {env:GW_TOKEN}"}`,
		"GW_TOKEN":      "tok-123",
	}))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if r.Headers["X-Tenant"] != "acme" {
		t.Errorf("字面量头丢失：%v", r.Headers)
	}
	if r.Headers["Authorization"] != "Bearer tok-123" {
		t.Errorf("引用未展开：%q", r.Headers["Authorization"])
	}
}

// 写坏的请求头必须显式失败：静默忽略会让"鉴权没生效"拖到第一次调用才暴露。
func TestFromEnv_ReportsBadHeaders(t *testing.T) {
	base := map[string]string{
		EnvModel:        "m",
		EnvBaseURL:      "https://gw.example/v1",
		EnvModelContext: "1000",
		EnvModelOutput:  "100",
	}
	cases := map[string]string{
		"不是 JSON":   `not-json`,
		"不是对象":      `["a"]`,
		"值不是字符串":    `{"X-N":1}`,
		"引用指向未设置变量": `{"Authorization":"{env:ABSENT_TOKEN}"}`,
		"引用写法非法":    `{"Authorization":"{env:1BAD}"}`,
	}
	for name, raw := range cases {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		env[EnvHeaders] = raw
		_, err := FromEnv(fakeLookup(env))
		if err == nil {
			t.Errorf("%s：必须报错", name)
			continue
		}
		if !strings.Contains(err.Error(), EnvHeaders) {
			t.Errorf("%s：错误应指名 %s：%v", name, EnvHeaders, err)
		}
	}
}

// 未提供请求头即无额外头（合法），不返回空 map 以免调用方误以为是"声明了但为空"。
func TestFromEnv_NoHeadersIsNil(t *testing.T) {
	r, err := FromEnv(fakeLookup(map[string]string{
		EnvModel:        "m",
		EnvBaseURL:      "https://gw.example/v1",
		EnvModelContext: "1000",
		EnvModelOutput:  "100",
		EnvHeaders:      "   ",
	}))
	if err != nil {
		t.Fatalf("空白请求头应视同未提供：%v", err)
	}
	if r.Headers != nil {
		t.Errorf("期望 nil，实际 %v", r.Headers)
	}
}

func TestEnvRefName(t *testing.T) {
	cases := map[string]struct {
		name string
		ok   bool
	}{
		"{env:FOO}":       {"FOO", true},
		" {env:FOO_BAR} ": {"FOO_BAR", true},
		"sk-literal":      {"", false},
		"{env:}":          {"", false},
		"{env:1BAD}":      {"", false},
	}
	for in, want := range cases {
		name, ok := EnvRefName(in)
		if ok != want.ok || name != want.name {
			t.Fatalf("EnvRefName(%q) = (%q,%v), want (%q,%v)", in, name, ok, want.name, want.ok)
		}
	}
}

func TestExpandEnvRef(t *testing.T) {
	lookup := fakeLookup(map[string]string{"KEY": "v"})
	if got, err := ExpandEnvRef("Bearer {env:KEY}", lookup); err != nil || got != "Bearer v" {
		t.Fatalf("展开异常：(%q,%v)", got, err)
	}
	if _, err := ExpandEnvRef("{env:ABSENT}", lookup); err == nil {
		t.Fatal("引用未设置的变量必须报错")
	}
	if _, err := ExpandEnvRef("{env:1BAD}", lookup); err == nil {
		t.Fatal("非法引用写法必须报错")
	}
}
