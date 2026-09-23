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

	// ObserveFailure 上报一次工具失败，返回处置（三态见 StopLoss）。
	//
	// failKind 是失败类别；「同类失败」「连续」「阈值 / 上限」的**判据本次不定义**——那是 MS-3 的
	// 实现工作，且文档要求"先用测试把它定死"。现在没有调用方（MS-3 才会接进 OnTurn 守卫）；
	// 默认实现 `internal/policy` 里它是 panic 哨兵（未冻结期口径：先定契约、后填实现）。
	ObserveFailure(failKind string) (StopLoss, string)

	// DeniedCount 报告连续策略拒绝的累积情况，第二个返回值表示是否已达阈值（达即终止）。
	//
	// 与 ObserveFailure 同理："连续"的口径与阈值由 MS-3 定；现在没有调用方。
	DeniedCount() (int, bool)
}
