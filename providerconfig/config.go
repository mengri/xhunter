// Package providerconfig 定义 Xhunter 的 Provider 配置模型。
//
// 结构参考 opencode 的 opencode.json（provider / options / models / limit），
// 字段与内部契约的对应关系：
//
//	limit.context → MaxContextTokens（上限类字段必须预置配置，不得估算）
//	limit.output  → 输出预留，参与可用输入预算计算
//	options.apiKey → 只允许 {env:VAR} 引用，凭据本身不落配置
//	options.baseURL / npm → 自建网关与 OpenAI 兼容端点，使供应商成为部署期配置
//
// 本包是**公开契约**：它是"接入一家供应商"在配置侧的全部约定，由组装层消费——
// 组装层把这里解析出的连接事实（Resolved）翻译成某个协议客户端能接受的构造参数。
//
// 本包不读环境变量、不发网络请求、不依赖模型：只做配置的解析、校验与解析（resolve）。
package providerconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Config 是配置文件的根，也是内置模型目录的根——两者结构相同，因此可以合并（见 Merge）。
type Config struct {
	Provider map[string]Provider `json:"provider"`
}

// Provider 是一个供应商（或一个自建网关）。
type Provider struct {
	// Name 是显示名，仅用于诊断输出。
	Name string `json:"name,omitempty"`
	// SDK 指定用哪套适配器（对应 opencode 的 npm 字段）。
	// 约定值见 SDKOpenAICompatible 等常量；为空表示使用内置适配器。
	SDK string `json:"npm,omitempty"`
	// Options 是连接参数。
	Options Options `json:"options"`
	// Models 是该供应商可用的模型。
	Models map[string]Model `json:"models"`
}

// Options 是连接参数。凭据只以引用形式出现。
type Options struct {
	BaseURL string            `json:"baseURL,omitempty"`
	APIKey  string            `json:"apiKey,omitempty"` // 必须是 {env:VAR}
	Headers map[string]string `json:"headers,omitempty"`
}

// Model 是单个模型的配置。
type Model struct {
	Name string `json:"name,omitempty"`
	// Limit 是模型能力上限。这两个字段是 FR-9.5 与 FR-9.6 的唯一来源。
	Limit Limit `json:"limit"`
	// Cost 是价格，用于 FR-9.1 的费用预算维度。缺省表示不计费用。
	Cost *Cost `json:"cost,omitempty"`
}

// Limit 是模型上限。
type Limit struct {
	// Context 是模型接受的最大输入 token 数（FR-9.5）。必须配置，不得估算。
	Context int `json:"context"`
	// Output 是模型可生成的最大 token 数，用作输出预留（FR-9.6）。
	Output int `json:"output"`
	// OutputIsPolicyDefault 表示 Output 不是上游声明的模型能力，而是策略默认值。
	//
	// 出现场景：上游只给出"总窗口"一个数（max_input_tokens == max_output_tokens ==
	// max_tokens），没有区分输入与输出。此时**上下文上限仍取上游事实值**，而输出预留
	// 是预算参数而非模型事实，取目录构建期的策略默认值——不猜模型，但也不假装知道。
	OutputIsPolicyDefault bool `json:"output_policy_default,omitempty"`
}

// Cost 是每 token 单价，单位**纳美元（1e-9 USD）**，与 harness.Money 一致。
// 换算来源见 modelcatalog：LiteLLM 目录以「美元/token」浮点给出价格。
type Cost struct {
	InputNanoUSDPerToken  int64 `json:"input_nano_usd_per_token,omitempty"`
	OutputNanoUSDPerToken int64 `json:"output_nano_usd_per_token,omitempty"`
}

// SDK 约定值。它们指的是**协议**，不是厂商：同一个协议可以由多家提供。
// 自建网关与大多数第三方端点都应落在 OpenAI 兼容适配器上。
const (
	// SDKOpenAICompatible 是"按角色平铺的消息列表 + 分片流式"那一类协议，
	// 端点必须由配置给出（协议本身没有默认主机）。
	SDKOpenAICompatible = "@ai-sdk/openai-compatible"
	// SDKOpenAI 是 OpenAI 的 Responses 协议（类型化条目 + 语义事件流）。
	SDKOpenAI = "@ai-sdk/openai"
	// SDKAnthropic 是 Anthropic 的 Messages 协议（顶层系统提示 + 内容块）。
	SDKAnthropic = "@ai-sdk/anthropic"
	// SDKBuiltin 表示使用内置适配器，等价于不声明协议。
	SDKBuiltin = "builtin"
)

// envRefRe 匹配完整的 {env:VAR} 引用。凭据值本身不得出现在配置里（FR-8.4）。
var envRefRe = regexp.MustCompile(`^\{env:([A-Za-z_][A-Za-z0-9_]*)\}$`)

// envRefAnyRe 匹配值中**任意位置**的 {env:VAR} 引用。请求头的值允许带前缀，
// 例如 `Authorization: Bearer {env:KEY}`——那仍然是引用，凭据值依然不在配置里。
var envRefAnyRe = regexp.MustCompile(`\{env:([A-Za-z_][A-Za-z0-9_]*)\}`)

// Issue 是一条配置问题。校验一次报出全部问题，而不是逐个发现。
type Issue struct {
	Path    string // 如 provider.anthropic.models.claude.limit.context
	Message string
}

func (i Issue) String() string { return i.Path + ": " + i.Message }

// ValidationError 汇总全部配置问题。
type ValidationError struct {
	Issues []Issue
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Issues))
	for _, is := range e.Issues {
		parts = append(parts, is.String())
	}
	return "provider 配置无效：" + strings.Join(parts, "; ")
}

// Parse 解析配置并做**结构校验**。
//
// 注意校验的层次：配置文件既可以是完整定义，也可以只是**覆盖片段**（例如只给
// baseURL 与 apiKey，模型上限由内置目录提供——与 opencode 的 provider 段语义一致）。
// 因此这里只校验"与是否被覆盖无关"的错误：引用形式、必填连接参数、已给出的 limit
// 必须自洽。**limit 是否齐备属于"生效校验"**，在合并（Merge）之后判定（见 ValidateResolved
// 与 Resolve）。
//
// 未知字段视为错误——拼错的字段名不得被静默忽略。
func Parse(r io.Reader) (Config, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("解析 provider 配置失败：%w", err)
	}
	if err := c.ValidateShape(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// LoadFile 从文件加载配置。
func LoadFile(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("打开 provider 配置失败：%w", err)
	}
	defer f.Close()
	return Parse(f)
}

// ValidateShape 做结构校验：报出与"是否被上层覆盖"无关的问题。
// 一次报出全部问题，而不是逐个发现。
func (c Config) ValidateShape() error {
	var issues []Issue

	if len(c.Provider) == 0 {
		issues = append(issues, Issue{Path: "provider", Message: "至少需要一个供应商"})
	}

	for pid, p := range c.Provider {
		base := "provider." + pid

		if p.SDK == SDKOpenAICompatible && p.Options.BaseURL == "" {
			issues = append(issues, Issue{Path: base + ".options.baseURL",
				Message: "OpenAI 兼容适配器必须提供 baseURL"})
		}

		if p.Options.APIKey != "" {
			if _, ok := EnvRefName(p.Options.APIKey); !ok {
				issues = append(issues, Issue{Path: base + ".options.apiKey",
					Message: "必须是 {env:VAR} 引用；凭据值不得写入配置（FR-1.2、FR-8.4）"})
			}
		}

		// 请求头里的引用写法必须全部合法：拼错的引用会被当成字面量发出去，
		// 而"发了一个叫 {env:XXX} 的头"这件事要到对端拒绝时才会暴露。
		for name, value := range p.Options.Headers {
			if !strings.Contains(value, "{env:") {
				continue
			}
			rest := envRefAnyRe.ReplaceAllString(value, "")
			if strings.Contains(rest, "{env:") {
				issues = append(issues, Issue{Path: base + ".options.headers." + name,
					Message: "环境变量引用写法非法；正确形式是 {env:VAR}，可带前缀（如 Bearer {env:VAR}）"})
			}
		}

		for mid, m := range p.Models {
			mbase := base + ".models." + mid
			if m.Limit.Context < 0 || m.Limit.Output < 0 {
				issues = append(issues, Issue{Path: mbase + ".limit",
					Message: "上限必须为正；未提供请省略该字段而非填 0 或负数"})
				continue
			}
			// 两个都给出了才检查自洽性；只给其一是合法的覆盖片段。
			if m.Limit.Context > 0 && m.Limit.Output > 0 && m.Limit.Output >= m.Limit.Context {
				issues = append(issues, Issue{Path: mbase + ".limit.output",
					Message: fmt.Sprintf("输出预留 %d 不得大于等于上下文上限 %d",
						m.Limit.Output, m.Limit.Context)})
			}
		}
	}

	if len(issues) > 0 {
		return &ValidationError{Issues: issues}
	}
	return nil
}

// ValidateResolved 做**生效校验**：合并（目录 + 用户）之后调用。
// 它落实 FR-9.5：上限类字段必须齐备——缺失即显式失败，不得估算或回退隐式默认。
func (c Config) ValidateResolved() error {
	var issues []Issue
	for pid, p := range c.Provider {
		for mid, m := range p.Models {
			mbase := "provider." + pid + ".models." + mid
			if m.Limit.Context <= 0 {
				issues = append(issues, Issue{Path: mbase + ".limit.context",
					Message: "上限类字段必须配置，不得估算（FR-9.5）"})
			}
			if m.Limit.Output <= 0 {
				issues = append(issues, Issue{Path: mbase + ".limit.output",
					Message: "必须给出输出预留，用于计算可用输入预算（FR-9.6）"})
			}
		}
	}
	if len(issues) > 0 {
		return &ValidationError{Issues: issues}
	}
	return nil
}

// EnvRefName 解析 {env:VAR} 引用，返回变量名。
// 非引用形式返回 ok=false。
func EnvRefName(ref string) (string, bool) {
	m := envRefRe.FindStringSubmatch(strings.TrimSpace(ref))
	if m == nil {
		return "", false
	}
	return m[1], true
}

// ExpandEnvRef 展开值里所有 {env:VAR} 引用，得到可直接发送的值。
//
// lookup 由调用方给出（装配期用 os.LookupEnv），因此本包依旧不读环境变量。
// 变量缺失即报错，不静默变成空头：凭据没配好属启动期问题，而不是运行到一半才发现
// "请求被 401 拒绝"。值里若残留形似引用的片段（写法不合法），同样显式报错。
func ExpandEnvRef(value string, lookup func(string) (string, bool)) (string, error) {
	var missing []string
	out := envRefAnyRe.ReplaceAllStringFunc(value, func(match string) string {
		name := envRefAnyRe.FindStringSubmatch(match)[1]
		v, ok := lookup(name)
		if !ok {
			missing = append(missing, name)
			return ""
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("这些环境变量未设置：%s", strings.Join(missing, ", "))
	}
	if strings.Contains(out, "{env:") {
		return "", fmt.Errorf("非法的环境变量引用写法：%s", value)
	}
	return out, nil
}

// ProviderIDs 返回已配置的供应商标识（有序，便于诊断输出稳定）。
func (c Config) ProviderIDs() []string {
	ids := make([]string, 0, len(c.Provider))
	for id := range c.Provider {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// ErrNotConfigured 表示所需供应商或模型未在配置（含内置目录）中定义。
var ErrNotConfigured = errors.New("provider 或 model 未配置")
