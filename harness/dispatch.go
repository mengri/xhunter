package harness

// 分发是纯函数：不读文件、不看时钟、不依赖全局状态，因此可脱离模型与 IO 单测。
//
// 它在框架里，但**不认识任何具体原语**：路径由"原语的寻址性质 + 选择器表达式 +
// 环境可用性"三者决定，而寻址性质是装配层随原语一起给的数据。
//
//	可用符号路径 = 目标语言已注册（扩展可用） 且 语法可解析
//	走符号路径   = 原语支持符号寻址 且 选择器为符号寻址 且 可用符号路径
//	否则         → 文本路径（记录降级原因并随结果上报）
//
// 三条硬约束：
//  1. 路径由"选择器语法 + 语言可用性"共同决定——模型用表达方式选路径，而非猜工具名；
//  2. 内部路径不同**不得放宽匹配语义**——两条路径的定位失败都必须显式报错；
//  3. 每次调用上报 resolved_mode 与降级原因。

// Dispatch 是分发的参考实现，纯函数。
func Dispatch(call Call, addr Addressing, caps ExtCaps, st FileState) Route {
	switch addr {
	case AddressedAsText:
		// 没有符号语义的原语：选择器写成什么都不改变路径。
		return Route{Path: PathText, Reason: "该原语只有文本路径"}

	case AddressedAsSymbol:
		// 仅符号寻址，且没有可用的降级形态：环境不支持时仍然落在符号路径上，
		// 由运行时返回结构化错误——悄悄改走文本路径会产出一个改了声明、
		// 却没改调用点的半完成结果，比直接失败更糟。
		return Route{Path: PathSymbol, Reason: "该原语仅支持符号路径"}

	default:
		if !selectorIsSymbol(call) {
			return Route{Path: PathText, Reason: "内容寻址选择器"}
		}
		if !symbolAvailable(caps, st) {
			return Route{Path: PathText, Reason: degradeReason(caps, st)}
		}
		return Route{Path: PathSymbol, Reason: "符号寻址 + 语言已注册且语法可解析"}
	}
}

func symbolAvailable(caps ExtCaps, st FileState) bool {
	return caps.Available && st.Registered && st.ParseOK
}

// selectorIsSymbol 判断模型是否用**表达方式**要求了符号寻址。
func selectorIsSymbol(call Call) bool {
	return call.Selector.Symbol != "" ||
		call.Selector.InSymbol != "" ||
		call.Selector.FileView
}

func degradeReason(caps ExtCaps, st FileState) string {
	switch {
	case !caps.Available:
		return "扩展不可用，降级文本路径"
	case !st.Registered:
		return "语言未注册，降级文本路径"
	case !st.ParseOK:
		return "语法不可解析，降级文本路径"
	default:
		return "降级文本路径"
	}
}
