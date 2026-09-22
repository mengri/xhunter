package providerconfig

import (
	"errors"
	"strings"
	"testing"
)

// 参考 opencode 的真实配置形态（opencode.json 的 provider 段）。
const opencodeStyle = `{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "myprovider": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "My AI Provider Display Name",
      "options": {
        "baseURL": "https://api.myprovider.com/v1",
        "apiKey": "{env:MY_PROVIDER_API_KEY}",
        "headers": {"Authorization": "Bearer custom-token"}
      },
      "models": {
        "my-model-name": {
          "name": "My Model Display Name",
          "limit": {"context": 200000, "output": 65536}
        }
      }
    }
  }
}`

func TestParse_OpencodeStyleConfig(t *testing.T) {
	// $schema 是 opencode 的字段；Xhunter 不采用，未知字段应被拒绝——先去掉再解析。
	body := strings.Replace(opencodeStyle, `  "$schema": "https://opencode.ai/config.json",`+"\n", "", 1)

	c, err := Parse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if got := c.ProviderIDs(); len(got) != 1 || got[0] != "myprovider" {
		t.Fatalf("provider 列表异常：%v", got)
	}
	p := c.Provider["myprovider"]
	if p.SDK != SDKOpenAICompatible {
		t.Fatalf("npm 字段未映射到 SDK：%q", p.SDK)
	}
	if p.Options.Headers["Authorization"] != "Bearer custom-token" {
		t.Fatal("自定义 header 丢失")
	}
	if p.Models["my-model-name"].Limit.Context != 200000 {
		t.Fatal("limit.context 丢失")
	}
}

// FR-1.2 / FR-8.4：凭据只能是 {env:VAR} 引用，字面量必须被拒绝。
func TestValidateShape_RejectsLiteralAPIKey(t *testing.T) {
	body := `{"provider":{"p":{"options":{"apiKey":"sk-literal-123"},"models":{"m":{"limit":{"context":1000,"output":100}}}}}}`
	_, err := Parse(strings.NewReader(body))
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want ValidationError, got %v", err)
	}
	if !strings.Contains(ve.Error(), "apiKey") {
		t.Fatalf("必须指出 apiKey 问题：%s", ve.Error())
	}
}

// 覆盖片段是合法的：只给 context 不给 output，解析阶段必须放行（与 opencode 的语义一致）。
func TestParse_AcceptsPartialOverlay(t *testing.T) {
	body := `{"provider":{"p":{"options":{"apiKey":"{env:K}"},"models":{"m":{"limit":{"context":2000}}}}}}`
	c, err := Parse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("覆盖片段不应在解析阶段被拒：%v", err)
	}
	if c.Provider["p"].Models["m"].Limit.Output != 0 {
		t.Fatal("未提供的字段应保持零值")
	}
}

// FR-9.5：生效校验（合并之后）必须要求上限齐备——缺失即显式失败，不得估算。
func TestValidateResolved_RejectsMissingLimits(t *testing.T) {
	partial := mustParse(t, `{"provider":{"p":{"models":{"m":{"limit":{"context":2000}}}}}}`)
	merged := Merge(Config{}, partial)
	err := merged.ValidateResolved()
	if err == nil || !strings.Contains(err.Error(), "limit.output") {
		t.Fatalf("want limit.output 报错，got %v", err)
	}

	// 目录补齐后即通过。
	catalog := mustParse(t, `{"provider":{"p":{"models":{"m":{"limit":{"context":2000,"output":200}}}}}}`)
	if err := Merge(catalog, partial).ValidateResolved(); err != nil {
		t.Fatalf("补齐后应通过：%v", err)
	}
}

func TestValidateShape_RejectsOutputGEContext(t *testing.T) {
	body := `{"provider":{"p":{"models":{"m":{"limit":{"context":100,"output":100}}}}}}`
	_, err := Parse(strings.NewReader(body))
	if err == nil || !strings.Contains(err.Error(), "输出预留") {
		t.Fatalf("want 输出预留报错，got %v", err)
	}
}

// 一次报出全部结构问题，而不是逐个发现。
func TestValidateShape_ReportsAllIssues(t *testing.T) {
	body := `{"provider":{
		"p":{"npm":"@ai-sdk/openai-compatible","options":{"apiKey":"sk-literal"},"models":{
			"m":{"limit":{"context":100,"output":100}}}}}}`
	_, err := Parse(strings.NewReader(body))
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want ValidationError, got %v", err)
	}
	if len(ve.Issues) < 3 {
		t.Fatalf("应一次报出多条问题，实际 %d：%v", len(ve.Issues), ve.Error())
	}
}

// 未知字段（例如把 limit 拼成 limt）必须被拒绝，而不是静默忽略。
func TestParse_RejectsUnknownField(t *testing.T) {
	body := `{"provider":{"p":{"models":{"m":{"limt":{"context":1,"output":1}}}}}}`
	_, err := Parse(strings.NewReader(body))
	if err == nil || !strings.Contains(err.Error(), "解析") {
		t.Fatalf("未知字段应导致解析失败，got %v", err)
	}
}

func TestParse_RequiresBaseURLForOpenAICompatible(t *testing.T) {
	body := `{"provider":{"p":{"npm":"@ai-sdk/openai-compatible","models":{"m":{"limit":{"context":1000,"output":100}}}}}}`
	_, err := Parse(strings.NewReader(body))
	if err == nil || !strings.Contains(err.Error(), "baseURL") {
		t.Fatalf("want baseURL 报错，got %v", err)
	}
}

func TestResolve_UserConfigWinsOverCatalog(t *testing.T) {
	user := mustParse(t, `{"provider":{"anthropic":{"options":{"apiKey":"{env:ANTHROPIC_API_KEY}"},"models":{"claude":{"limit":{"context":1000,"output":100}}}}}}`)
	catalog := mustParse(t, `{"provider":{"anthropic":{"models":{"claude":{"limit":{"context":200000,"output":8192}},"opus":{"limit":{"context":200000,"output":8192}}}}}}`)

	r, err := user.Resolve(catalog, "anthropic", "claude")
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if r.MaxContextTokens != 1000 {
		t.Fatalf("用户配置应覆盖目录，实际 context=%d", r.MaxContextTokens)
	}
	if r.APIKeyEnv != "ANTHROPIC_API_KEY" {
		t.Fatalf("凭据引用解析异常：%q", r.APIKeyEnv)
	}

	// 用户未定义的模型由目录兜底。
	r2, err := user.Resolve(catalog, "anthropic", "opus")
	if err != nil {
		t.Fatalf("目录兜底失败：%v", err)
	}
	if r2.MaxContextTokens != 200000 {
		t.Fatalf("目录项未生效：%d", r2.MaxContextTokens)
	}
	// 连接参数仍来自用户配置。
	if r2.APIKeyEnv != "ANTHROPIC_API_KEY" {
		t.Fatalf("连接参数应沿用用户配置：%q", r2.APIKeyEnv)
	}
}

// FR-9.5：未命中时不采用保守默认，显式失败。
func TestResolve_NotConfiguredIsExplicit(t *testing.T) {
	user := mustParse(t, `{"provider":{"p":{"models":{"m":{"limit":{"context":1000,"output":100}}}}}}`)
	_, err := user.Resolve(Config{}, "unknown", "m")
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
	_, err = user.Resolve(Config{}, "p", "unknown-model")
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
}

// FR-9.6：可用输入预算 = 窗口上限 − 固定开销 − 输出预留。
func TestInputBudget(t *testing.T) {
	r := Resolved{MaxContextTokens: 200000, OutputReserve: 8192}
	got, err := r.InputBudget(5000)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if want := 200000 - 5000 - 8192; got != want {
		t.Fatalf("want %d, got %d", want, got)
	}

	tight := Resolved{MaxContextTokens: 1000, OutputReserve: 900}
	if _, err := tight.InputBudget(200); err == nil {
		t.Fatal("预算非正必须显式失败")
	}
}

// 水位线以可用输入预算为基数，而不是裸窗口（FR-9.6、FR-14.1）。
func TestWatermarks(t *testing.T) {
	r := Resolved{MaxContextTokens: 100000, OutputReserve: 10000}
	warn, target, hard, err := r.Watermarks(0, 0.70, 0.50, 0.90)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if warn != 63000 || target != 45000 || hard != 81000 {
		t.Fatalf("水位计算异常：warn=%d target=%d hard=%d", warn, target, hard)
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

func TestMerge_FieldLevelOverride(t *testing.T) {
	catalog := mustParse(t, `{"provider":{"p":{"name":"Catalog","models":{"m":{"limit":{"context":1000,"output":100}}}}}}`)
	user := mustParse(t, `{"provider":{"p":{"options":{"apiKey":"{env:K}"},"models":{"m":{"name":"User","limit":{"context":2000}}}}}}`)

	merged := Merge(catalog, user)
	p := merged.Provider["p"]
	if p.Name != "Catalog" {
		t.Fatalf("未覆盖字段应保留目录值：%q", p.Name)
	}
	if p.Options.APIKey != "{env:K}" {
		t.Fatal("用户 options 未生效")
	}
	m := p.Models["m"]
	if m.Limit.Context != 2000 {
		t.Fatalf("context 应被覆盖：%d", m.Limit.Context)
	}
	if m.Limit.Output != 100 {
		t.Fatalf("output 应保留目录值：%d", m.Limit.Output)
	}
}

func mustParse(t *testing.T, s string) Config {
	t.Helper()
	c, err := Parse(strings.NewReader(s))
	if err != nil {
		t.Fatalf("fixture 解析失败：%v", err)
	}
	return c
}
