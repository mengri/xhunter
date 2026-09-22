package providerconfig

import (
	"fmt"
	"math"
	"sort"
)

// Resolved 是解析后的运行期事实：Provider Adapter 据此声明 Caps，Harness 据此推导压缩水位线。
type Resolved struct {
	ProviderID string
	ModelID    string
	SDK        string
	BaseURL    string
	Headers    map[string]string

	// APIKeyEnv 是凭据所在的环境变量名。凭据值本身不进入本结构（FR-8.4）。
	APIKeyEnv string
	// HasAPIKey 表示配置声明了凭据引用；未声明时由适配器回退到内置约定变量。
	HasAPIKey bool

	// MaxContextTokens 是模型接受的输入上限（FR-9.5）。一定 > 0：缺失即报错。
	MaxContextTokens int
	// OutputReserve 是生成预留，参与可用输入预算（FR-9.6）。
	OutputReserve int
}

// Resolve 按"用户配置优先、内置目录兜底"的顺序解析 (providerID, modelID)。
//
// catalog 可为空（未内置目录）。两者都未命中时返回 ErrNotConfigured——
// 不采用保守默认值：无人值守场景下，静默默认会静默拉低所有同类任务的效率（FR-9.5）。
func (user Config) Resolve(catalog Config, providerID, modelID string) (Resolved, error) {
	p, ok := user.Provider[providerID]
	if !ok {
		if p, ok = catalog.Provider[providerID]; !ok {
			return Resolved{}, fmt.Errorf("%w: provider %q", ErrNotConfigured, providerID)
		}
	}
	m, ok := p.Models[modelID]
	if !ok {
		if cp, found := catalog.Provider[providerID]; found {
			if cm, found := cp.Models[modelID]; found {
				m = cm
				ok = true
				// 目录提供模型上限，连接参数仍取用户配置（若有）。
				if m.Limit.Context <= 0 || m.Limit.Output <= 0 {
					if um, ufound := p.Models[modelID]; ufound {
						m = um
					}
				}
			}
		}
	}
	if !ok {
		return Resolved{}, fmt.Errorf("%w: model %q @ %q", ErrNotConfigured, modelID, providerID)
	}
	if m.Limit.Context <= 0 || m.Limit.Output <= 0 {
		return Resolved{}, fmt.Errorf("%w: model %q 缺少 limit.context / limit.output（FR-9.5）",
			ErrNotConfigured, modelID)
	}

	r := Resolved{
		ProviderID:       providerID,
		ModelID:          modelID,
		SDK:              p.SDK,
		BaseURL:          p.Options.BaseURL,
		Headers:          cloneHeaders(p.Options.Headers),
		MaxContextTokens: m.Limit.Context,
		OutputReserve:    m.Limit.Output,
	}
	if name, ok := EnvRefName(p.Options.APIKey); ok {
		r.APIKeyEnv = name
		r.HasAPIKey = true
	}
	return r, nil
}

// InputBudget 计算可用输入预算（FR-9.6）：
//
//	可用输入预算 = 窗口上限 − 固定开销（内置层 + 工具 schema） − 输出预留
//
// 结果必须为正：非正说明配置本身不成立（上下文太小或输出预留过大），
// 按配置错误显式失败，而不是在运行时靠"压到很小"苟活。
func (r Resolved) InputBudget(fixedOverhead int) (int, error) {
	budget := r.MaxContextTokens - fixedOverhead - r.OutputReserve
	if budget <= 0 {
		return 0, fmt.Errorf(
			"可用输入预算非正：context=%d − 固定开销=%d − 输出预留=%d = %d（FR-9.6）",
			r.MaxContextTokens, fixedOverhead, r.OutputReserve, budget)
	}
	return budget, nil
}

// Watermarks 按比例给出三档水位（FR-14.1）。基数是可用输入预算，而不是裸窗口。
// 四舍五入而非截断：截断会让 0.7×90000 算成 62999，与人工核算结果对不上。
func (r Resolved) Watermarks(fixedOverhead int, warn, target, hard float64) (warnAt, targetAt, hardAt int, err error) {
	budget, err := r.InputBudget(fixedOverhead)
	if err != nil {
		return 0, 0, 0, err
	}
	return int(math.Round(float64(budget) * warn)),
		int(math.Round(float64(budget) * target)),
		int(math.Round(float64(budget) * hard)),
		nil
}

// Merge 合并内置目录与用户配置：用户同名项覆盖目录项（字段级）。
// 合并后的配置是"生效配置"，仍应过一次 Validate。
func Merge(catalog, user Config) Config {
	out := Config{Provider: make(map[string]Provider, len(catalog.Provider)+len(user.Provider))}
	for id, p := range catalog.Provider {
		out.Provider[id] = cloneProvider(p)
	}
	for id, up := range user.Provider {
		if cp, ok := out.Provider[id]; ok {
			out.Provider[id] = mergeProvider(cp, up)
			continue
		}
		out.Provider[id] = cloneProvider(up)
	}
	return out
}

func mergeProvider(base, over Provider) Provider {
	out := cloneProvider(base)
	if over.Name != "" {
		out.Name = over.Name
	}
	if over.SDK != "" {
		out.SDK = over.SDK
	}
	if over.Options.BaseURL != "" {
		out.Options.BaseURL = over.Options.BaseURL
	}
	if over.Options.APIKey != "" {
		out.Options.APIKey = over.Options.APIKey
	}
	if len(over.Options.Headers) > 0 {
		if out.Options.Headers == nil {
			out.Options.Headers = map[string]string{}
		}
		for k, v := range over.Options.Headers {
			out.Options.Headers[k] = v
		}
	}
	if out.Models == nil {
		out.Models = map[string]Model{}
	}
	for mid, om := range over.Models {
		if bm, ok := out.Models[mid]; ok {
			merged := bm
			if om.Name != "" {
				merged.Name = om.Name
			}
			if om.Limit.Context > 0 {
				merged.Limit.Context = om.Limit.Context
			}
			if om.Limit.Output > 0 {
				merged.Limit.Output = om.Limit.Output
			}
			out.Models[mid] = merged
			continue
		}
		out.Models[mid] = om
	}
	return out
}

func cloneProvider(p Provider) Provider {
	c := p
	c.Options.Headers = cloneHeaders(p.Options.Headers)
	c.Models = make(map[string]Model, len(p.Models))
	for k, v := range p.Models {
		c.Models[k] = v
	}
	return c
}

func cloneHeaders(h map[string]string) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out[k] = h[k]
	}
	return out
}
