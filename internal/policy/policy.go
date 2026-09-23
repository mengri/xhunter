// Package policy 实现策略引擎：无人类场景下唯一顶替人的位置。
//
// 它裁决三件事：**路径边界**（写到哪里合法）、**预算**（token / 轮数 / 墙钟）与
// 止损。默认应当是拒绝——放行需要一条明确的理由，而不是反过来。
//
// 它是 hunt.Policy 的默认实现，由装配层注入。它与工作区层（osfs）的分工是「业务」与
// 「机制」：工作区层管路径怎么解析（绝对路径、..、符号链接逃逸），这里写的是业务边界
// ——哪些路径是引擎自有的材料、写进那里等于篡改自己的会话记录或验收标准。
package policy

import (
	"context"
	"path"
	"strings"
	"time"

	"xhunter/hunt"
	"xhunter/llm"
)

// controlDir 是引擎在仓库里的控制目录。
//
// 它只放**引擎自己的**材料与冻结配置（会话材料、skill 清单）。**不是"引擎用到的文件都放这里"**：
// 仓库级、别的工具也会读的配置（如仓库根的 `gates.yml` 门禁清单）不进这个目录——放进来就等于
// 连带套上"模型禁写"，而它们本来就该是可改、可 review 的普通仓库文件。
const controlDir = ".xhunter"

// skillsDraftDir 是唯一允许模型写入的控制子目录。
const skillsDraftDir = ".xhunter/skills.draft"

// Config 是策略的装配参数。
type Config struct {
	Budget hunt.Budget
	Now    func() time.Time
}

type engine struct {
	budget  hunt.Budget
	now     func() time.Time
	started time.Time
	tokens  int
}

// New 构造策略引擎。策略在一轮 Hunt 里是单例。
func New(cfg Config) hunt.Policy {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &engine{budget: cfg.Budget, now: now, started: now()}
}

var _ hunt.Policy = (*engine)(nil)

// Facts 自述这套策略实际生效的口径，进「生效配置快照」供远程诊断与 MR 评审。
//
// 边界常量是策略自己的知识：快照因此由策略给出，装配层不必另抄一份边界清单，也就不会
// 因为改了一处而漂开。键的含义：default 是默认裁决，write_protected 是禁写的控制目录，
// write_exception 是唯一允许模型写入的控制子目录。
func Facts() map[string]any {
	return map[string]any{
		"default":         "deny",
		"write_protected": controlDir,
		"write_exception": skillsDraftDir,
	}
}

// Decide 裁决一次调用能否放行。只对写操作做路径裁决。
//
// 输入是「目标路径 ＋ 这次调用是否写盘」——**是否写盘由原语自述**（`Call.Writes`，执行体
// 查表后回填）。策略不自己维护一张原语分类表：那张表与实现分处两个包，漂开之后不会以编译
// 错误的形式暴露，只会让某个写原语被当成只读、绕开路径边界。
//
// 名字不认识的原语到不了这里——执行体在查表处就挡下了（`unknown_tool`）。
func (e *engine) Decide(_ context.Context, call hunt.Call) (hunt.Decision, error) {
	if !call.Writes {
		// 只读与门禁不走路径边界：越界路径根本表达不出来（工作区层只接受相对路径且拒绝
		// 逃逸），而 `.xhunter/**` 的**读必须放行**——skill 正文正是靠 read 按需加载的。
		return allow("只读或门禁，无路径约束"), nil
	}
	if call.Target == "" {
		// 符号级写操作（如不指定文件的符号重命名）没有目标路径可查：它的改动面由定位
		// 结果决定，而定位发生在裁决之后。刻意不按改动规模设阈值——判据来自证据（门禁、
		// 读回校验），不来自"改得多就保守拒绝"。
		return allow("符号级写操作"), nil
	}
	if reason, blocked := blockPath(call.Target); blocked {
		return deny(reason), nil
	}
	return allow("工作区内写操作"), nil
}

// Charge 累计 token 用量。
func (e *engine) Charge(u llm.Usage) {
	e.tokens += u.InputTokens + u.OutputTokens
}

// Exhausted 报告是否有某一维预算耗尽，并指明是哪一个维度。
func (e *engine) Exhausted(turn hunt.TurnNo) (bool, string) {
	if e.budget.MaxTokens > 0 && e.tokens >= e.budget.MaxTokens {
		return true, "tokens"
	}
	if e.budget.MaxTurns > 0 && int(turn) >= e.budget.MaxTurns {
		return true, "turns"
	}
	if e.budget.MaxWallClock > 0 && e.now().Sub(e.started) >= e.budget.MaxWallClock {
		return true, "wall_clock"
	}
	return false, ""
}

// blockPath 判断一个写目标是否越界。
func blockPath(rel string) (string, bool) {
	slash := strings.ReplaceAll(rel, "\\", "/")
	if path.IsAbs(slash) || (len(slash) >= 2 && slash[1] == ':') {
		return "拒绝绝对路径：" + rel, true
	}
	clean := path.Clean(slash)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "路径逃逸：" + rel, true
	}
	if underControlDir(clean) && !underSkillsDraft(clean) {
		return "禁止写入引擎控制目录：" + rel, true
	}
	return "", false
}

func underControlDir(rel string) bool {
	return rel == controlDir || strings.HasPrefix(rel, controlDir+"/")
}

func underSkillsDraft(rel string) bool {
	return rel == skillsDraftDir || strings.HasPrefix(rel, skillsDraftDir+"/")
}

func allow(reason string) hunt.Decision {
	return hunt.Decision{Verdict: hunt.VerdictAllow, Reason: reason}
}

func deny(reason string) hunt.Decision {
	return hunt.Decision{Verdict: hunt.VerdictDeny, Reason: reason}
}
