package main

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"xhunter/llm"
	"xhunter/provider/anthropicmessages"
	"xhunter/provider/openaichat"
	"xhunter/provider/openairesponses"
	"xhunter/providerconfig"
)

// 组装层：把环境变量解析出的接入事实，变成某个协议实现的客户端。
//
// 这里是**唯一**知道"有哪些协议、哪个取值对应哪一个"的地方。协议实现只说协议——
// 不做注册、也不提供工厂——因此针对已知厂商的那些差异全部落在这一层，而且**以接入
// 事实为源**：端点来自 XHUNTER_BASE_URL（协议侧不预设任何主机），鉴权形状来自请求头
// （用哪个头、加不加前缀由 XHUNTER_HEADERS / XHUNTER_API_KEY 决定）。
//
// 接一家**厂商** = 设一组环境变量（协议不变、代码不动）。
// 接一种**协议** = 写一个协议包，并在这里加一行绑定。

// protocolFactory 是组装层的针对性工厂：给定已解析的接入事实，构造协议实现。
//
// 所有工厂在签名上完全一致，差异全在内部——这正是"针对性"的落点：同一份事实经由
// 不同工厂可以得到不同的连线方式，而调用方不必知道差别在哪。
type protocolFactory func(providerconfig.Resolved) (llm.Provider, error)

// protocolFactories 返回组装层的协议表：协议取值 → 工厂。
//
// 每次返回一张新表，而不是复用包级变量：装配是一次性动作，而"可用的协议有哪些"
// 不该成为全局可变状态（否则它会随测试或调用顺序漂移）。
func protocolFactories() map[string]protocolFactory {
	return map[string]protocolFactory{
		providerconfig.ProtocolOpenAIChat:        openAIChat,
		providerconfig.ProtocolOpenAIResponses:   openAIResponses,
		providerconfig.ProtocolAnthropicMessages: anthropicMessages,
	}
}

// openAIChat 构造对话补全协议的客户端。
func openAIChat(r providerconfig.Resolved) (llm.Provider, error) {
	return openaichat.New(openaichat.Config{
		BaseURL:          r.BaseURL,
		Model:            r.ModelID,
		Headers:          requestHeaders(r),
		MaxContextTokens: r.MaxContextTokens,
	})
}

// openAIResponses 构造 Responses 协议的客户端。
func openAIResponses(r providerconfig.Resolved) (llm.Provider, error) {
	return openairesponses.New(openairesponses.Config{
		BaseURL:          r.BaseURL,
		Model:            r.ModelID,
		Headers:          requestHeaders(r),
		MaxContextTokens: r.MaxContextTokens,
	})
}

// anthropicMessages 构造 Messages 协议的客户端。
//
// 本协议额外要求生成上限（请求体必填 max_tokens），因此把输出预留转进去——
// 两者口径一致，都是"留给模型生成的空间"。
func anthropicMessages(r providerconfig.Resolved) (llm.Provider, error) {
	return anthropicmessages.New(anthropicmessages.Config{
		BaseURL:          r.BaseURL,
		Model:            r.ModelID,
		Headers:          requestHeaders(r),
		MaxContextTokens: r.MaxContextTokens,
		MaxOutputTokens:  r.OutputReserve,
	})
}

// requestHeaders 把接入事实里的请求头整理成可直接发送的样子。
//
// 三条约定合起来就是"鉴权方式由环境变量决定"：
//   - XHUNTER_HEADERS 里的头原样保留，头名先归一（`http.Header` 的规范形式）——
//     否则 `authorization` 与下面补出的 `Authorization` 会成为两个 key，而 map 迭代
//     顺序决定谁赢，同一份环境两次运行可能发出不同的鉴权头；
//   - XHUNTER_API_KEY 是**便捷形式**：请求头里没有 Authorization 时补一个标准的
//     Authorization: Bearer。想要别的形状（裸 x-api-key、租户头、网关签名）就在
//     XHUNTER_HEADERS 里显式写，无需改代码；
//   - 客户端标识 User-Agent: xhunter/<version> 由系统注入，不提供入口：环境里写了
//     同名头也被忽略（任意大小写）。它不是一个可改写的身份。
func requestHeaders(r providerconfig.Resolved) map[string]string {
	headers := make(map[string]string, len(r.Headers)+2)
	for name, value := range r.Headers {
		canonical := http.CanonicalHeaderKey(name)
		if strings.EqualFold(canonical, "User-Agent") {
			continue // 客户端标识不提供入口
		}
		headers[canonical] = value
	}
	if _, ok := headers["Authorization"]; !ok && r.APIKey != "" {
		headers["Authorization"] = "Bearer " + r.APIKey
	}
	// 客户端标识恒为系统值：环境里的同名头已在上方跳过，此处不存在被覆盖的路径。
	headers["User-Agent"] = "xhunter/" + version
	return headers
}

// openProvider 按接入事实装配 Provider：协议取值 → 针对性工厂 → 协议客户端。
func openProvider(r providerconfig.Resolved) (llm.Provider, error) {
	return providerFor(protocolFactories(), r)
}

// providerFor 按协议取值从表里取针对性工厂并构造协议实现。
//
// 表由调用方给出，而不是在函数里读一个包级变量：装配是一次性动作，测试也可以塞一张
// 只含自己那条协议的表。
func providerFor(table map[string]protocolFactory, r providerconfig.Resolved) (llm.Provider, error) {
	f, ok := table[r.Protocol]
	if !ok {
		return nil, fmt.Errorf(
			"组装层没有协议 %q 的适配器（已装配：%s）：为它写一个协议包并在组装层加一行绑定，"+
				"协议实现本身不做注册",
			r.Protocol, strings.Join(sortedProtocols(table), ", "))
	}
	return f(r)
}

func sortedProtocols(table map[string]protocolFactory) []string {
	out := make([]string, 0, len(table))
	for p := range table {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
