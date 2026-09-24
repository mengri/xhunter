package hunt

import (
	"context"

	"xhunter/llm"
)

// Verdict 是策略裁决的结果。
type Verdict string

const (
	VerdictAllow Verdict = "allow"
	VerdictDeny  Verdict = "deny"
)

// Decision 必须携带原因：拒绝时原因会回灌给模型，让它换个做法，而不是对着同一堵墙
// 反复尝试。
type Decision struct {
	Verdict Verdict
	Reason  string
}

// StopLoss 是止损的处置三态（FR-9.4 的两段式：达阈值先换策略、超上限才终止）。
type StopLoss string

const (
	// StopContinue 照常继续。
	StopContinue StopLoss = "continue"
	// StopSwitch 达阈值：**切换策略**（回灌提示让模型换个做法），不终止。
	StopSwitch StopLoss = "switch"
	// StopTerminate 超上限：终止。
	StopTerminate StopLoss = "terminate"
)

// Policy 是无人类场景下唯一顶替人的位置：路径边界、破坏性操作、预算与止损都由它裁决。
// 默认应当是拒绝——放行需要一条明确的理由，而不是反过来。
type Policy interface {
	Decide(ctx context.Context, call Call) (Decision, error)
	Charge(u llm.Usage)
	Exhausted(turn TurnNo) (bool, string)

	// ObserveFailure 上报一次工具调用结局，返回处置（三态见 StopLoss）：达阈值先换策略、
	// 超上限才终止（两段式）。
	//
	// failKind 的约定：
	//   - **空串表示一次成功调用**——它把「连续同类失败」归零（失败与成功混在一根轴上，
	//     "连续"才有意义）；
	//   - "同类" = 同一个 failKind 字符串；出现另一种 kind 即从 1 重新起计；
	//   - `policy_denied` 由实现**忽略**：拒绝是另一条轴（连续拒绝），它由 DeniedCount 单独管，
	//     其计数在 Decide 里喂——同一次拒绝不占这里的同类失败计数。
	ObserveFailure(failKind string) (StopLoss, string)

	// DeniedCount 报告连续策略拒绝的累积情况，第二个返回值表示是否已达阈值（达即终止）。
	//
	// "连续"的口径：出现一次 Allow（放行）即归零；拒绝由 Decide 累加——计数只有这一处。
	DeniedCount() (int, bool)
}
