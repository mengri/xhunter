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

// Policy 是无人类场景下唯一顶替人的位置：路径边界、破坏性操作、预算与止损都由它裁决。
// 默认应当是拒绝——放行需要一条明确的理由，而不是反过来。
type Policy interface {
	Decide(ctx context.Context, call Call) (Decision, error)
	Charge(u llm.Usage)
	Exhausted(turn TurnNo) (bool, string)
}
