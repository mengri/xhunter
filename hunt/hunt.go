// Package hunt 是「代码编辑 agent」的业务领域：任务输入、工具面、门禁、策略，以及
// 默认的工具执行器与生命周期钩子。
//
// 它依赖 harness（实现 ToolExecutor 与 Hooks）和三个基础能力包——workspace（文件读写）、
// git（版本控制）、ext（符号解析）。harness 不认识 hunt：业务的一切（Bounty、Call、Gate、
// 检查点、交付）都在这里，harness 只提供循环，业务通过钩子与工具执行器注入。
package hunt

import (
	"time"

	"xhunter/git"
)

// ============================================================ 标识

type (
	BountyID   string
	TurnNo     int
	ToolCallID string
)

// ============================================================ 任务输入

// Budget 是三重预算的上限；值为 0 表示该维度不限。
type Budget struct {
	MaxTurns     int
	MaxTokens    int
	MaxWallClock time.Duration
}

// SessionRef 指向同一任务的历次执行记录；为 nil 表示新任务。
type SessionRef struct {
	ID  string
	Ref string
}

// Bounty 是一次任务分派的完整输入。Repo 引用 git 基础包的仓库形状。
type Bounty struct {
	ID   BountyID
	Task string
	// TraceID 是贯穿平台侧记录的追踪标识（FR-11.3）：它进事件的信封，
	// 用来把"这一串事件"对回平台上的那一次投递。为空表示调用方未提供，
	// 由装配层回填（缺省取 BountyID）。
	TraceID string
	Repo    git.RepoRef
	Session *SessionRef
	Budget  Budget
}

// SessionID 返回这次执行所属的会话标识：有会话就取会话标识，否则取本次任务的标识。
//
// 它是「记账口径」的唯一来源——事件流与结果文件都按它取值，两处因此不会各写一遍、
// 也就不会漂。分支与记忆目录同样以它为准（`xhunter/<session_id>`、`.xhunter/<session_id>/`）。
func (b Bounty) SessionID() string {
	if b.Session != nil && b.Session.ID != "" {
		return b.Session.ID
	}
	return string(b.ID)
}

// ============================================================ 门禁

// Gate 是一个具名校验条目。
//
// 模型只知道这个名字，命令、参数、判据都在业务侧——所以模型无法通过改命令来让
// 自己更容易通过，也不可能借它把 shell 请回来。
type Gate struct {
	Name     string
	Argv     []string
	Required bool
}
