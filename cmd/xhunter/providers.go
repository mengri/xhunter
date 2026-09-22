package main

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"xhunter/harness"
	"xhunter/internal/modelcatalog"
	"xhunter/provider/anthropicmessages"
	"xhunter/provider/openaichat"
	"xhunter/provider/openairesponses"
	"xhunter/providerconfig"
)

// 组装层：把配置里解析出的连接事实，变成某个协议实现的客户端。
//
// 这里是**唯一**知道"有哪些协议、哪个配置值对应哪一个"的地方。协议实现只说协议——
// 不做注册、也不提供工厂——因此针对已知厂商的那些差异全部落在这一层，
// 而且**以配置为源**：端点来自 baseURL（协议侧不预设任何主机），
// 鉴权形状来自配置里的请求头（用哪个头、加不加前缀都由配置决定）。
//
// 接一家新厂商 = 在配置里写一条 provider 条目（协议 + 端点 + 鉴权头 + 模型上限）。
// 只有当它说的是**另一种协议**时，才需要在 vendorFactories 里加一行绑定。

// errNoCredential 表示配置声明了凭据引用，但对应环境变量为空。
var errNoCredential = errors.New("凭据环境变量为空或未设置")

// providerFactory 是组装层的针对性工厂：给定已解析的连接事实，构造协议实现。
//
// 所有工厂在签名上完全一致，差异全在内部——这正是"针对性"的落点：
// 同一份配置经由不同工厂可以得到不同的连线方式，而调用方不必知道差别在哪。
type providerFactory func(providerconfig.Resolved) (harness.Provider, error)

// vendorFactories 返回组装层的协议表：SDK 值（指协议，不指厂商）→ 工厂。
//
// 每次返回一张新表，而不是复用包级变量：装配是一次性动作，而"可用协议有哪些"
// 不该成为全局可变状态（否则它会随测试或调用顺序漂移）。
func vendorFactories() map[string]providerFactory {
	return map[string]providerFactory{
		// 通用形态：对话补全。空值与 builtin 都落在这里——绝大多数自建网关与
		// 第三方托管端说的就是这套形状，把它当作"没声明协议"时的默认更符合实际。
		"":                                 openAICompatible,
		providerconfig.SDKBuiltin:          openAICompatible,
		providerconfig.SDKOpenAICompatible: openAICompatible,

		// OpenAI 的 Responses 协议与 Anthropic 的 Messages 协议各占一条。
		providerconfig.SDKOpenAI:    openAIResponses,
		providerconfig.SDKAnthropic: anthropicMessages,
	}
}

// openAICompatible 构造对话补全协议的客户端。
func openAICompatible(r providerconfig.Resolved) (harness.Provider, error) {
	headers, err := requestHeaders(r)
	if err != nil {
		return nil, err
	}
	return openaichat.New(openaichat.Config{
		BaseURL:          r.BaseURL,
		Model:            r.ModelID,
		Headers:          headers,
		MaxContextTokens: r.MaxContextTokens,
	})
}

// openAIResponses 构造 Responses 协议的客户端。
func openAIResponses(r providerconfig.Resolved) (harness.Provider, error) {
	headers, err := requestHeaders(r)
	if err != nil {
		return nil, err
	}
	return openairesponses.New(openairesponses.Config{
		BaseURL:          r.BaseURL,
		Model:            r.ModelID,
		Headers:          headers,
		MaxContextTokens: r.MaxContextTokens,
	})
}

// anthropicMessages 构造 Messages 协议的客户端。
//
// 本协议额外要求生成上限（请求体必填 max_tokens），因此把输出预留转进去——
// 两者口径一致，都是"留给模型生成的空间"。
func anthropicMessages(r providerconfig.Resolved) (harness.Provider, error) {
	headers, err := requestHeaders(r)
	if err != nil {
		return nil, err
	}
	return anthropicmessages.New(anthropicmessages.Config{
		BaseURL:          r.BaseURL,
		Model:            r.ModelID,
		Headers:          headers,
		MaxContextTokens: r.MaxContextTokens,
		MaxOutputTokens:  r.OutputReserve,
	})
}

// requestHeaders 把配置里的请求头解析成可直接发送的值。
//
// 两条约定合起来就是"鉴权方式可配置"：
//   - headers 里可以写任意头，值支持 {env:VAR} 引用（含带前缀的写法），
//     因此非标准鉴权（x-api-key、租户头、网关签名）都不需要改代码；
//   - apiKey 是**便捷形式**：声明了它、且 headers 里没有同名头时，
//     补一个标准的 Authorization: Bearer。想要别的形状就用 headers 显式写。
//
// 客户端标识 User-Agent: xhunter/<version> 由系统注入，不提供配置入口：配置里的
// 同名头被忽略（任意大小写），上游看到的恒为 xhunter 自身——它不是一个可改写的身份。
func requestHeaders(r providerconfig.Resolved) (map[string]string, error) {
	headers := make(map[string]string, len(r.Headers)+2)
	for name, value := range r.Headers {
		if strings.EqualFold(name, "User-Agent") {
			continue // 客户端标识不提供配置入口
		}
		expanded, err := providerconfig.ExpandEnvRef(value, os.LookupEnv)
		if err != nil {
			return nil, fmt.Errorf("请求头 %s：%w", name, err)
		}
		headers[name] = expanded
	}

	if _, ok := r.Headers["Authorization"]; !ok && r.HasAPIKey {
		key := strings.TrimSpace(os.Getenv(r.APIKeyEnv))
		if key == "" {
			return nil, fmt.Errorf("%w: %s", errNoCredential, r.APIKeyEnv)
		}
		headers["Authorization"] = "Bearer " + key
	}

	// 客户端标识恒为系统值：配置同名头已在上方跳过，此处不存在被覆盖的路径。
	headers["User-Agent"] = "xhunter/" + version
	return headers, nil
}

// openProvider 按部署选择装配 Provider：读配置文件 → 与本地模型目录合并 → 解析出
// (provider, model) 的连接事实与上限 → 建协议客户端。
//
// 目录快照缺失不是错误：配置文件本身可以给出全部事实（自建网关、新模型）。
// 真正缺事实的情况由解析与构造两处显式报出。
func openProvider(sel providerSelection) (harness.Provider, error) {
	user, err := providerconfig.LoadFile(sel.ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("%s：%w", envProviderFile, err)
	}

	catalog := providerconfig.Config{}
	if snap, err := modelcatalog.Load(""); err == nil {
		catalog = snap.Catalog
	} else if !errors.Is(err, modelcatalog.ErrNoSnapshot) {
		return nil, fmt.Errorf("读取模型目录失败：%w", err)
	}

	resolved, err := user.Resolve(catalog, sel.ProviderID, sel.ModelID)
	if err != nil {
		return nil, err
	}
	return providerFor(vendorFactories(), resolved)
}

// providerFor 按配置声明的 SDK 从表里取针对性工厂并构造协议实现。
//
// 表由调用方给出，而不是在函数里读一个包级变量：装配是一次性动作，测试也可以塞一张
// 只含自己那条协议的表。
func providerFor(table map[string]providerFactory, r providerconfig.Resolved) (harness.Provider, error) {
	f, ok := table[r.SDK]
	if !ok {
		return nil, fmt.Errorf(
			"组装层没有 SDK %q 的针对性工厂（已装配：%s）：为它加一条即可，协议实现本身不做注册",
			r.SDK, strings.Join(sortedSDKs(table), ", "))
	}
	return f(r)
}

func sortedSDKs(table map[string]providerFactory) []string {
	out := make([]string, 0, len(table))
	for sdk := range table {
		out = append(out, sdk)
	}
	sort.Strings(out)
	return out
}
