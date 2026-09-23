// Package providerconfig 定义「模型接入事实」的唯一来源：环境变量。
//
// 接入一个模型需要的事实只有几件：用哪套协议说话、端点在哪、怎么鉴权、模型的窗口上限
// 是多少。它们**全部由 XHUNTER_* 环境变量给出**——没有配置文件、没有内置模型目录、
// 没有合并与回退。理由：这些是**部署事实**（同一台机器上往往固定），而任务正文每个
// 任务都不同；两者分开之后，部署事实只有一处定义，也就不存在"配置里写的是这个、
// 运行时用的是那个"的漂移，更没有"哪一层优先"需要解释。
//
// 本包是**公开契约**：它给出环境变量的名字、解析与校验规则，以及解析结果 Resolved。
// 组装层把 Resolved 翻译成某个协议客户端能接受的构造参数（端点、模型、鉴权头、上限）。
//
// 本包不发网络请求、不读文件、不依赖模型，也不做任何注册。
package providerconfig

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// 环境变量名。它们是外部契约的一部分：驱动者按这些名字投递模型接入事实，
// 字段清单与形态见 xhunter-usage.md §3。
const (
	// EnvProtocol 是**协议**取值（见 Protocol* 常量），不是厂商——同一个协议可以由
	// 多家提供，端点与鉴权形状另由 EnvBaseURL / EnvAPIKey / EnvHeaders 给出。
	// 为空表示默认协议。
	EnvProtocol = "XHUNTER_PROTOCOL"
	// EnvModel 是模型标识，原样进入请求体。
	EnvModel = "XHUNTER_MODEL"
	// EnvBaseURL 是端点。协议实现不预设任何主机，因此它是必填的。
	EnvBaseURL = "XHUNTER_BASE_URL"
	// EnvAPIKey 是凭据值：声明了它、且请求头里没有鉴权头时，补一个标准的
	// Authorization: Bearer。想要别的形状就用 EnvHeaders 显式写。
	EnvAPIKey = "XHUNTER_API_KEY"
	// EnvHeaders 是自定义请求头，JSON 对象。值里可以写 {env:VAR} 引用（可带前缀，
	// 如 "Bearer {env:K}"），引用在启动期展开——因此凭据值仍然可以只存在于
	// 专用的环境变量里，而不必写进这一串 JSON。
	EnvHeaders = "XHUNTER_HEADERS"
	// EnvModelContext 是模型接受的输入上限（FR-9.5）。必填：上限类字段**不得估算**。
	EnvModelContext = "XHUNTER_MODEL_CONTEXT_TOKENS"
	// EnvModelOutput 是留给模型生成的空间，用作输出预留（FR-9.6）。必填。
	EnvModelOutput = "XHUNTER_MODEL_OUTPUT_TOKENS"
)

// 协议取值。它们指的是**协议**，不是厂商：自建网关与大多数第三方端点说的都是
// OpenAI 兼容那一套形状。
const (
	// ProtocolOpenAIChat 是"按角色平铺的消息列表 + 分片流式"那一类协议（默认）。
	ProtocolOpenAIChat = "openaichat"
	// ProtocolOpenAIResponses 是 OpenAI 的 Responses 协议（类型化条目 + 语义事件流）。
	ProtocolOpenAIResponses = "openairesponses"
	// ProtocolAnthropicMessages 是 Anthropic 的 Messages 协议（顶层系统提示 + 内容块）。
	ProtocolAnthropicMessages = "anthropicmessages"
)

// protocolSet 是已装配的协议集合：取值只在这里定义，诊断输出与校验都从它导出，
// 因此不存在"常量加了一条、清单忘了加"的漂移。
var protocolSet = map[string]bool{
	ProtocolOpenAIChat:        true,
	ProtocolOpenAIResponses:   true,
	ProtocolAnthropicMessages: true,
}

// Protocols 返回全部协议取值（有序，便于诊断输出稳定）。
func Protocols() []string {
	out := make([]string, 0, len(protocolSet))
	for p := range protocolSet {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Resolved 是解析后的运行期事实：组装层据此构造协议客户端，Harness 据此推导水位线。
//
// 它承载凭据值（APIKey）：那份值只在装配期存在于内存，绝不进入事件、日志、patch
// 与会话材料（FR-8.4）。
type Resolved struct {
	// Protocol 决定用哪套适配器（Protocol* 常量之一）。
	Protocol string
	// ModelID 原样进请求体。
	ModelID string
	// BaseURL 是端点：协议侧不预设任何主机。
	BaseURL string
	// Headers 是自定义请求头，引用已展开。
	Headers map[string]string
	// APIKey 是凭据值；为空表示未提供（有的网关不需要鉴权）。
	APIKey string

	// MaxContextTokens 是模型接受的输入上限（FR-9.5）。一定 > 0：缺失即报错。
	MaxContextTokens int
	// OutputReserve 是生成预留，参与可用输入预算（FR-9.6）。一定 > 0。
	OutputReserve int
}

// Issue 是一条接入事实问题，Path 是**环境变量名**。
type Issue struct {
	Path    string
	Message string
}

func (i Issue) String() string { return i.Path + "：" + i.Message }

// ValidationError 汇总全部问题。一次报出全部，而不是逐个发现。
type ValidationError struct {
	Issues []Issue
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Issues))
	for _, is := range e.Issues {
		parts = append(parts, is.String())
	}
	return "模型接入事实无效：" + strings.Join(parts, "；")
}

// FromEnv 从环境变量解析出模型接入事实。lookup 由调用方给出（生产用 os.LookupEnv），
// 因此本包依旧不自己读环境，测试也不必摆弄进程环境。
//
// 校验一次报出全部问题：驱动者需要的是"这几个变量缺了或写错了"，一次就能改完。
// 缺一项就写一项的报法，在无人值守场景下意味着反复重派去逐个发现——那是最贵的失败方式。
//
// 未提供 EnvProtocol 时取默认协议；未提供 EnvAPIKey / EnvHeaders 是合法的（有的网关
// 不需要鉴权，也不是每个网关都要求额外请求头）。其余四项缺一不可。
func FromEnv(lookup func(string) (string, bool)) (Resolved, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	var issues []Issue

	protocol := readEnv(lookup, EnvProtocol)
	if protocol == "" {
		protocol = ProtocolOpenAIChat
	}
	if !protocolSet[protocol] {
		issues = append(issues, Issue{Path: EnvProtocol,
			Message: fmt.Sprintf("未知协议 %q；可用的是：%s", protocol, strings.Join(Protocols(), "、"))})
	}

	model := readEnv(lookup, EnvModel)
	if model == "" {
		issues = append(issues, Issue{Path: EnvModel, Message: "必须给出模型标识"})
	}

	baseURL := readEnv(lookup, EnvBaseURL)
	if baseURL == "" {
		issues = append(issues, Issue{Path: EnvBaseURL,
			Message: "必须给出端点：协议不预设任何主机（换一家端点只改这个变量）"})
	}

	context, contextOK := positiveInt(lookup, EnvModelContext, &issues,
		"必须给出模型输入上限：上限类字段不得估算（FR-9.5）")
	output, outputOK := positiveInt(lookup, EnvModelOutput, &issues,
		"必须给出输出预留，用于计算可用输入预算（FR-9.6）")
	if contextOK && outputOK && output >= context {
		issues = append(issues, Issue{Path: EnvModelOutput,
			Message: fmt.Sprintf("输出预留 %d 不得大于等于上下文上限 %d", output, context)})
	}

	headers, headerIssues := parseHeaders(readEnv(lookup, EnvHeaders), lookup)
	issues = append(issues, headerIssues...)

	if len(issues) > 0 {
		return Resolved{}, &ValidationError{Issues: issues}
	}
	return Resolved{
		Protocol:         protocol,
		ModelID:          model,
		BaseURL:          baseURL,
		Headers:          headers,
		APIKey:           readEnv(lookup, EnvAPIKey),
		MaxContextTokens: context,
		OutputReserve:    output,
	}, nil
}

// positiveInt 读一个必须为正整数的变量；缺失、非法、非正都是问题，一次记进 issues。
func positiveInt(lookup func(string) (string, bool), name string, issues *[]Issue, missing string) (int, bool) {
	raw := readEnv(lookup, name)
	if raw == "" {
		*issues = append(*issues, Issue{Path: name, Message: missing})
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		*issues = append(*issues, Issue{Path: name,
			Message: fmt.Sprintf("必须是正整数（当前 %q）", raw)})
		return 0, false
	}
	if n <= 0 {
		*issues = append(*issues, Issue{Path: name,
			Message: fmt.Sprintf("必须为正数（当前 %d）", n)})
		return 0, false
	}
	return n, true
}

// parseHeaders 解析自定义请求头。
//
// 形状不成立（不是 JSON 对象、值不是字符串）即显式报错——静默忽略一串写错的请求头，
// 会让"鉴权没生效"变成运行到第一次调用才发现的 401。值里的 {env:VAR} 引用在这里
// 就展开：变量没设置属启动期问题，不是运行中途的意外。
func parseHeaders(raw string, lookup func(string) (string, bool)) (map[string]string, []Issue) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var parsed map[string]string
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, []Issue{{Path: EnvHeaders,
			Message: fmt.Sprintf("必须是一个 JSON 对象（{\"头名\":\"值\"}）：%v", err)}}
	}

	var issues []Issue
	out := make(map[string]string, len(parsed))
	for name, value := range parsed {
		if strings.TrimSpace(name) == "" {
			issues = append(issues, Issue{Path: EnvHeaders, Message: "请求头名不得为空"})
			continue
		}
		expanded, err := ExpandEnvRef(value, lookup)
		if err != nil {
			issues = append(issues, Issue{Path: EnvHeaders,
				Message: fmt.Sprintf("请求头 %s：%v", name, err)})
			continue
		}
		out[name] = expanded
	}
	if len(issues) > 0 {
		return nil, issues
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// envRefRe 匹配完整的 {env:VAR} 引用。
var envRefRe = regexp.MustCompile(`^\{env:([A-Za-z_][A-Za-z0-9_]*)\}$`)

// envRefAnyRe 匹配值中**任意位置**的 {env:VAR} 引用。请求头的值允许带前缀，
// 例如 `Bearer {env:KEY}`——那仍然是引用，凭据值依然不在这一串 JSON 里。
var envRefAnyRe = regexp.MustCompile(`\{env:([A-Za-z_][A-Za-z0-9_]*)\}`)

// EnvRefName 解析 {env:VAR} 引用，返回变量名；非引用形式返回 ok=false。
func EnvRefName(ref string) (string, bool) {
	m := envRefRe.FindStringSubmatch(strings.TrimSpace(ref))
	if m == nil {
		return "", false
	}
	return m[1], true
}

// ExpandEnvRef 展开值里所有 {env:VAR} 引用，得到可直接发送的值。
//
// lookup 由调用方给出，因此本包不自己读环境。变量缺失即报错，不静默变成空头：
// 凭据没配好属启动期问题，而不是运行到一半才发现"请求被 401 拒绝"。值里若残留
// 形似引用的片段（写法不合法），同样显式报错。
func ExpandEnvRef(value string, lookup func(string) (string, bool)) (string, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
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

func readEnv(lookup func(string) (string, bool), name string) string {
	v, _ := lookup(name)
	return strings.TrimSpace(v)
}
