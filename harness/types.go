// Package harness 提供「模型调用循环」的控制。
//
// 它只依赖两样东西：模型（llm.Provider）与三组 handler——Prepare（循环前）、OnTurn
// （每个轮边界）、Final（循环后）。上下文怎么组装、工具怎么执行、会话怎么记、事件怎么发，
// 全部在 handler 里由业务自己完成；harness 不认识这些协作者，也不声明它们的接口。
//
// 因此本包可被任何「模型调用工具」的产品复用：换一套 handler，就是换一套业务。
package harness

import "xhunter/llm"

// Status 是一次循环收敛后的结论。
type Status string

const (
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// ExitCode 是进程退出码。分成四档是为了让调用方能区分「该重试」和「不该重试」：
// 任务本身失败重跑还是失败，而环境问题修好后可以接着跑。
type ExitCode int

const (
	// ExitOK：模型正常完成对话（任务处于什么状态看 status）。
	ExitOK ExitCode = 0
	// ExitEnv：这一趟没走成——未进入对话，或对话中被上游/环境打断；**修好环境可重跑**。
	ExitEnv ExitCode = 1
	// ExitAborted：被引擎中止（预算耗尽、止损、轮数硬顶、引擎侧错误）；**重跑同样是这个结果**。
	ExitAborted ExitCode = 2
	// ExitCancelled：被外部取消（SIGTERM / SIGINT）。
	ExitCancelled ExitCode = 3
)

// Terminal 是循环收敛后的终态。机制性终止「首个生效、后续不覆盖」（见 Run.terminate）；
// handler 可在收尾阶段用 SetTerminal 覆盖（如「全程无产出 → 失败」）。
type Terminal struct {
	Status Status
	Reason string
	Code   ExitCode
}

// Outcome 是对外的结果：终态 + 用量。业务的交付物（分支、提交、文件清单）由 handler
// 自行上报，harness 不定义它们的形状。
type Outcome struct {
	Status   Status
	Reason   string
	ExitCode ExitCode
	Usage    llm.Usage
}
