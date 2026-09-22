// Package modelcatalog 维护内置模型目录——FR-9.5 的来源①。
//
// 数据源是 LiteLLM 的模型价格与上下文窗口目录（见 DefaultSource）。它同时给出
// 上下文上限、输出上限与每 token 单价，正好覆盖三件事：
//
//	FR-9.5 上下文窗口上限（不得估算，必须预置）
//	FR-9.6 输出预留（参与可用输入预算）
//	FR-9.1 费用预算维度（每 token 单价）
//
// 使用方式：安装时拉取一份快照到 ~/.xhunter/models.json，**运行期只读本地快照、不联网**；
// 手动刷新用 CLI `xhunter models update`。
//
// 解析策略与用户配置相反：用户配置是我们定义的契约，未知字段一律拒绝；
// 上游目录是别人的数据且字段持续膨胀，因此**宽松解析**，只取需要的字段。
package modelcatalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"xhunter/providerconfig"
)

// DefaultSource 是上游目录地址（LiteLLM）。
const DefaultSource = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

// MaxUpstreamBytes 是上游响应体上限，防止误配地址导致把磁盘写满。
const MaxUpstreamBytes = 64 << 20

// defaultModes 是保留的模式。chat 与 responses 都是对话型 LLM 接口，
// 其余（embedding / image_generation / rerank / audio_* / ocr …）与 Xhunter 无关。
var defaultModes = map[string]bool{"chat": true, "responses": true}

// Entry 是上游条目中我们关心的字段。其余字段一律忽略。
type Entry struct {
	LitellmProvider    string   `json:"litellm_provider"`
	Mode               string   `json:"mode"`
	MaxInputTokens     OptInt   `json:"max_input_tokens"`
	MaxOutputTokens    OptInt   `json:"max_output_tokens"`
	MaxTokens          OptInt   `json:"max_tokens"`
	InputCostPerToken  OptFloat `json:"input_cost_per_token"`
	OutputCostPerToken OptFloat `json:"output_cost_per_token"`
}

// OptInt 是"可能是数字"的整数字段。
//
// 上游文件里 sample_spec 条目的数值字段放的是**说明文案**（"max input tokens, if the
// provider specifies it"），一条说明就能让整份目录解析失败。因此这里容错：非数字视为
// "未提供"。真正的格式变更由 Convert 兜底——转换结果为零即显式报错，不会静默产出空目录。
type OptInt struct {
	Value int
	Set   bool
}

func (o *OptInt) UnmarshalJSON(b []byte) error {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	var n int
	if err := json.Unmarshal(trimmed, &n); err != nil {
		return nil // 允许字符串占位
	}
	o.Value, o.Set = n, true
	return nil
}

// OptFloat 是"可能是数字"的浮点字段，容错规则同 OptInt。
type OptFloat struct {
	Value float64
	Set   bool
}

func (o *OptFloat) UnmarshalJSON(b []byte) error {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	var f float64
	if err := json.Unmarshal(trimmed, &f); err != nil {
		return nil
	}
	o.Value, o.Set = f, true
	return nil
}

// DefaultOutputReserve 是"上游未区分输入输出"时的输出预留默认值。
//
// 为什么需要它：上游有 900+ 条对话模型的三个字段都等于同一个数（总窗口），即没有区分
// 输入与输出预算。丢弃它们会平白损失大批模型（xai / mistral / bedrock 等重度出现）。
// 而**输出预留是预算参数、不是模型事实**，因此这里用策略默认值补齐，并把该条目标记为
// `OutputIsPolicyDefault`——不猜模型能力，但也不假装知道。
const DefaultOutputReserve = 8192

// Report 是一次转换的统计，用于 CLI 输出与快照元数据。
type Report struct {
	UpstreamEntries   int `json:"upstream_entries"`
	Converted         int `json:"converted"`
	SkippedSampleSpec int `json:"skipped_sample_spec"`
	SkippedMode       int `json:"skipped_mode"`
	SkippedNoLimits   int `json:"skipped_no_limits"`
	SkippedIncoherent int `json:"skipped_incoherent"`
	Collisions        int `json:"collisions"`
	Providers         int `json:"providers"`
	// UsedPolicyDefault 记录多少条目的输出预留取自策略默认值（可审计）。
	UsedPolicyDefault int `json:"used_policy_default"`
	// PolicyDefaultReserve 是本次转换使用的默认值，写入快照元数据。
	PolicyDefaultReserve int `json:"policy_default_reserve"`
}

// sampleSpecKey 是上游文件里描述字段含义的条目，不是模型。
const sampleSpecKey = "sample_spec"

// Convert 把上游目录转换为 Xhunter 的 provider 配置形态。
//
// 规则：
//   - 跳过 sample_spec 与无关模式；
//   - 上下文上限取 max_input_tokens，回退 max_tokens；输出上限取 max_output_tokens，回退 max_tokens；
//   - 上限缺失的条目跳过——它们无法满足 FR-9.5；
//   - **输出 >= 上下文**的条目不丢弃：上游只有"总窗口"一个数时，上下文取该事实值，
//     输出预留取 defaultOutputReserve 并标记为策略默认（outputReserve<=0 时此类条目跳过）；
//   - 模型 id 去掉与 provider 同名的前缀（上游键名不统一：既有裸名也有 gemini/xxx 这种）；
//   - 同名冲突按"信息更全者胜"取舍，并以键名排序保证结果确定。
func Convert(raw map[string]Entry, outputReserve int) (providerconfig.Config, Report, error) {
	var rep Report
	rep.UpstreamEntries = len(raw)
	rep.PolicyDefaultReserve = outputReserve

	providers := map[string]providerconfig.Provider{}
	models := map[string]map[string]providerconfig.Model{}
	origins := map[string]map[string]string{} // provider → modelID → 上游键名（用于确定性取舍）

	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		if key == sampleSpecKey {
			rep.SkippedSampleSpec++
			continue
		}
		e := raw[key]
		if e.LitellmProvider == "" {
			rep.SkippedMode++
			continue
		}
		if !defaultModes[e.Mode] {
			rep.SkippedMode++
			continue
		}

		ctx := firstPositive(e.MaxInputTokens, e.MaxTokens)
		out := firstPositive(e.MaxOutputTokens, e.MaxTokens)
		if ctx <= 0 || out <= 0 {
			rep.SkippedNoLimits++
			continue
		}
		policyDefault := false
		if out >= ctx {
			// 上游未区分输入输出：上下文用事实值，输出预留用策略默认值。
			if outputReserve <= 0 || outputReserve >= ctx {
				rep.SkippedIncoherent++
				continue
			}
			out = outputReserve
			policyDefault = true
			rep.UsedPolicyDefault++
		}

		id := stripProviderPrefix(key, e.LitellmProvider)
		m := providerconfig.Model{Limit: providerconfig.Limit{
			Context:               ctx,
			Output:                out,
			OutputIsPolicyDefault: policyDefault,
		}}
		if c := convertCost(e); c != nil {
			m.Cost = c
		}

		if models[e.LitellmProvider] == nil {
			models[e.LitellmProvider] = map[string]providerconfig.Model{}
			origins[e.LitellmProvider] = map[string]string{}
		}
		if prevKey, exists := origins[e.LitellmProvider][id]; exists {
			rep.Collisions++
			prev := models[e.LitellmProvider][id]
			// 信息更全者胜；平手时保留先出现的（键名已排序，结果确定）。
			if prev.Cost == nil && m.Cost != nil {
				models[e.LitellmProvider][id] = m
				origins[e.LitellmProvider][id] = key
			}
			_ = prevKey
			continue
		}
		models[e.LitellmProvider][id] = m
		origins[e.LitellmProvider][id] = key
		rep.Converted++
	}

	for pid, ms := range models {
		providers[pid] = providerconfig.Provider{
			Name:   pid,
			SDK:    providerconfig.SDKOpenAICompatible,
			Models: ms,
		}
	}
	rep.Providers = len(providers)

	if rep.Converted == 0 {
		return providerconfig.Config{}, rep, fmt.Errorf(
			"目录转换后无可用模型（上游 %d 条）：请检查数据源格式是否变化", rep.UpstreamEntries)
	}
	return providerconfig.Config{Provider: providers}, rep, nil
}

// stripProviderPrefix 去掉与 provider 同名的前缀：gemini/gemini-2.0-flash → gemini-2.0-flash。
// 上游键名不统一（既有裸名也有带前缀的），去前缀后同一模型在不同来源下仍映射到同一 id。
func stripProviderPrefix(key, provider string) string {
	prefix := provider + "/"
	if strings.HasPrefix(key, prefix) {
		return strings.TrimPrefix(key, prefix)
	}
	return key
}

// convertCost 把「美元/token」换算为纳美元/token（harness.Money 的单位）。
// 零价与负价一律视为"未提供"，避免把免费额度误当成价格。
func convertCost(e Entry) *providerconfig.Cost {
	var c providerconfig.Cost
	any := false
	if v := e.InputCostPerToken; v.Set && v.Value > 0 {
		c.InputNanoUSDPerToken = int64(math.Round(v.Value * 1e9))
		any = true
	}
	if v := e.OutputCostPerToken; v.Set && v.Value > 0 {
		c.OutputNanoUSDPerToken = int64(math.Round(v.Value * 1e9))
		any = true
	}
	if !any {
		return nil
	}
	return &c
}

func firstPositive(vals ...OptInt) int {
	for _, v := range vals {
		if v.Set && v.Value > 0 {
			return v.Value
		}
	}
	return 0
}
