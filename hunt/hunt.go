// Package hunt 是「代码编辑 agent」的业务领域：任务输入、工具面、门禁、策略，以及
// 默认的工具执行器与生命周期钩子。
//
// 它依赖 harness（实现 ToolExecutor 与 Hooks）和三个基础能力包——workspace（文件读写）、
// git（版本控制）、ext（符号解析）。harness 不认识 hunt：业务的一切（Bounty、Call、Gate、
// 检查点、交付）都在这里，harness 只提供循环，业务通过钩子与工具执行器注入。
package hunt

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
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
	// Gates 是任务侧**直接下发**的门禁清单（nil 表示没下发，走仓库声明）。下发可以覆盖或
	// 禁用仓库声明——验收标准归平台说了算，不由仓库自己决定。
	Gates []Gate
	// GatesSource 是清单来源档位（见 GateSource* 常量）。空值或 `bounty` 表示用下发的清单；
	// `working_tree` 是豁免档：让工作区里新写的清单本次就生效。它只存在于 Bounty——模型
	// 拿不到这个开关，否则"改判据让自己通过"就有了一条正当路径。
	GatesSource string
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
	Name string
	// Argv 是**数组直启**的命令与参数：不经 shell 解释——`;` / `&&` / `$()` 都不是语法。
	Argv []string
	// Required 表示"不通过就不算成功交付"。
	Required bool
	// Timeout 是单条门禁的超时（0 表示不限）。它独立于墙钟预算：门禁跑得久不是模型该背的账。
	Timeout time.Duration
	// Dir 是相对工作区根的执行目录（monorepo 用）；空表示工作区根。
	Dir string
	// Env 是清单显式声明的额外环境变量（**追加**在最小集之上）。
	Env []string
	// Expect 是按什么算通过。**判据在清单里**，引擎不内置默认。
	Expect Expect
}

// Validate 在清单生效前校验这一条**是否可判定**（元门禁的一环）：名字与命令齐备、没有把
// shell 请回来、超时不为负、判据可判定。
//
// 它**不查命令是否存在**——那要问 PATH，是运行期事实，由加载侧在清单真正生效前做。
func (g Gate) Validate() error {
	if strings.TrimSpace(g.Name) == "" {
		return errors.New("门禁条目缺少 name")
	}
	if len(g.Argv) == 0 {
		return fmt.Errorf("门禁 %q 缺少 argv", g.Name)
	}
	if IsShellCommand(g.Argv[0]) {
		return fmt.Errorf("门禁 %q 的 argv[0] 是 shell（%s）：门禁必须数组直启，不得把 shell 请回来", g.Name, g.Argv[0])
	}
	if g.Timeout < 0 {
		return fmt.Errorf("门禁 %q 的 timeout 不得为负：%s", g.Name, g.Timeout)
	}
	if err := g.Expect.Validate(); err != nil {
		return fmt.Errorf("门禁 %q 的判据不可用：%w", g.Name, err)
	}
	return nil
}

// shellCommands 是明确拒掉的 argv[0]。门禁命令自己内部用 shell 我们管不着（那已经是"执行
// 仓库配置的构建脚本"的性质），但**显式把 shell 请回来**必须堵死——那等于给了一条绕过
// "数组直启"的路。
var shellCommands = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true}

// IsShellCommand 报告 argv[0] 是不是 shell 本身（按基名比较：`/bin/sh` 与 `sh` 同罪）。
func IsShellCommand(argv0 string) bool { return shellCommands[filepath.Base(argv0)] }
