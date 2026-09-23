package hunt

import (
	"strings"

	"xhunter/harness"
)

// 用量与失败原因的对外口径：终态事件（`hunt_end` / `error`）与结果文件读的是**同一份**。
// 两处各写一个形状就是下一个漂点，所以形状与提取函数都收在这里，事件与文件都引用它。

// UsageReport 是用量的对外口径。
//
// Reported 表示**上游回报过用量**：为 false 时各项为 0 **不代表真的没用**，只是不可得。
// 判据是同一个事实（本轮增量有任一非零）——它只在 `Session.charge` 那一处判定，不在这里
// 另算一遍。**绝不估算**：不回报就不报数字，也不得拿 0 去触发或不触发 token 预算。
type UsageReport struct {
	Reported          bool  `json:"reported"`
	InputTokens       int   `json:"input_tokens"`
	OutputTokens      int   `json:"output_tokens"`
	CachedInputTokens int   `json:"cached_input_tokens"`
	Turns             int   `json:"turns"`
	ElapsedMS         int64 `json:"elapsed_ms"`
}

// ErrorKind 取失败原因的首段（`prepare_failed: …` → `prepare_failed`）：它稳定、可供程序
// 分支；完整原因留在 `reason` 里给人看。无冒号的短原因整体作 kind，空串给 `unknown`。
//
// 它是 `kind` 提取口径的**唯一一处**：终态 `error` 事件与结果文件 `error.kind` 都读它。
func ErrorKind(reason string) string {
	if i := strings.IndexByte(reason, ':'); i > 0 {
		return strings.TrimSpace(reason[:i])
	}
	if reason == "" {
		return "unknown"
	}
	return reason
}

// RetryableForExitCode 报告某个退出码是否代表"环境问题、可重试"。它是 `retryable` 的**唯一
// 判据**：终态 `error` 事件与结果文件 `error.retryable` 都读它，不各写一遍。
func RetryableForExitCode(code harness.ExitCode) bool { return code == harness.ExitEnv }
