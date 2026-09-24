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
	"fmt"
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

// 止损两段式的阈值（连续同类失败）。它们是策略自己的知识：没有第二个取值，因此是导出常量
// 而不是可配字段。两个数分开，是为了"达阈值先换策略、超上限才终止"这一段式不被压成一行。
//
// MaxSameKindSwitch 取 2：第一次失败还只说明"这一步不成立"，第二次同类失败说明"这条路走不通"——
// 到这里回灌一条换做法的提示，比一路撞到上限才报错多给模型一次自我纠偏的机会。
const MaxSameKindSwitch = 2

// MaxSameKindStreak 取 3：换策略提示给过一次仍不奏效，说明模型换不动；再喂提示只会继续烧预算。
const MaxSameKindStreak = 3

// MaxDeniedStreak 是「连续策略拒绝达阈值即终止」的阈值，取 3。
//
// 为什么不取更大：拒绝也会让 turn.Failed=true，而机制止损在第 3 轮就收敛（harness.Config.MaxFailStreak
// 默认 3、装配层未覆盖）。业务阈值若大于 3，「每轮 1 次拒绝」这种最常见形态永远由机制止损抢先上报
// 通用原因，stop_loss_denied 就成了不可达的死规则；取 3 则业务在 OnTurn 守卫里先判并胜出，平台拿到
// 的是能说出「撞的是哪堵墙」的原因（两者退出码同为 2，处置不变、诊断变好）。
const MaxDeniedStreak = 3

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

	// failKind / failStreak 是"连续同类失败"的观测状态：failKind 是当前连续的那一类失败，
	// failStreak 是同类连续出现的次数（出现一次成功或换一类，重新起计）。
	failKind   string
	failStreak int
	// deniedStreak 是"连续策略拒绝"的计数：出现一次 Allow 即归零（在 Decide 里喂）。
	deniedStreak int
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
// write_exception 是唯一允许模型写入的控制子目录，max_denied_streak 与 max_same_kind_streak
// 是两个止损阈值——它们同样由策略自述（`config_snapshot` 事件据此向平台解释机制性终止）。
func Facts() map[string]any {
	return map[string]any{
		"default":              "deny",
		"write_protected":      controlDir,
		"write_exception":      skillsDraftDir,
		"max_denied_streak":    MaxDeniedStreak,
		"max_same_kind_streak": MaxSameKindStreak,
	}
}

// Decide 裁决一次调用能否放行。只对写操作做路径裁决。
//
// 输入是「目标路径 ＋ 这次调用是否写盘」——**是否写盘由原语自述**（`Call.Writes`，执行体
// 查表后回填）。策略不自己维护一张原语分类表：那张表与实现分处两个包，漂开之后不会以编译
// 错误的形式暴露，只会让某个写原语被当成只读、绕开路径边界。
//
// 名字不认识的原语到不了这里——执行体在查表处就挡下了（`unknown_tool`）。
//
// 每一次裁决同时驱动「连续策略拒绝」这根轴：放行即归零、拒绝即累加（DeniedCount 据此判定）。
// 计数只有这一处——ObserveFailure 忽略 policy_denied，拒绝不占同类失败那条轴。三条放行路径
// （只读、符号级写、工作区内写）都要归零，漏一条就会把"夹在拒绝之间的正常调用"误判成连拒。
func (e *engine) Decide(_ context.Context, call hunt.Call) (hunt.Decision, error) {
	if !call.Writes {
		// 只读与门禁不走路径边界：越界路径根本表达不出来（工作区层只接受相对路径且拒绝
		// 逃逸），而 `.xhunter/**` 的**读必须放行**——skill 正文正是靠 read 按需加载的。
		e.deniedStreak = 0
		return allow("只读或门禁，无路径约束"), nil
	}
	if call.Target == "" {
		// 符号级写操作（如不指定文件的符号重命名）没有目标路径可查：它的改动面由定位
		// 结果决定，而定位发生在裁决之后。刻意不按改动规模设阈值——判据来自证据（门禁、
		// 读回校验），不来自"改得多就保守拒绝"。
		e.deniedStreak = 0
		return allow("符号级写操作"), nil
	}
	if reason, blocked := blockPath(call.Target); blocked {
		e.deniedStreak++
		return deny(reason), nil
	}
	e.deniedStreak = 0
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

// ObserveFailure 上报一次工具调用结局并给出处置（止损两段式的第一段）。
//
// 约定（见 hunt.Policy 的同名注释）：
//   - failKind == "" 表示**一次成功调用**：它把「连续同类失败」归零（失败与成功混在一根轴上，
//     "连续"才有意义）；
//   - "同类" = 同一个 failKind 字符串；出现另一种 kind 即从 1 重新起计；
//   - `policy_denied` 由实现**忽略**：拒绝是另一条轴（连续拒绝），它由 DeniedCount 单独管，
//     其计数在 Decide 里喂——同一次拒绝不占这里的同类失败计数。
//
// 连续同类失败达 MaxSameKindSwitch → StopSwitch（回灌"换一种做法"提示，不终止）；
// 达 MaxSameKindStreak → StopTerminate（退出 2）。
func (e *engine) ObserveFailure(failKind string) (hunt.StopLoss, string) {
	if failKind == "" {
		// 一次成功调用：同类连续归零。
		e.failKind = ""
		e.failStreak = 0
		return hunt.StopContinue, ""
	}
	if failKind == "policy_denied" {
		// 拒绝走 DeniedCount 那条轴：这里不重复计数，也不改变同类失败的状态。
		return hunt.StopContinue, ""
	}
	if failKind != e.failKind {
		// 换了一类失败：从 1 重新起计（不同类的失败不是"同一条路走不通"）。
		e.failKind = failKind
		e.failStreak = 1
	} else {
		e.failStreak++
	}
	switch {
	case e.failStreak >= MaxSameKindStreak:
		// 详情尾巴接在 stop_loss_same_kind 之后：说得出"哪一类、错了几次"。
		return hunt.StopTerminate, fmt.Sprintf("连续 %d 次同类失败（%s）", e.failStreak, e.failKind)
	case e.failStreak >= MaxSameKindSwitch:
		// switch 提示带 kind 与计数：模型据此知道"哪一类、错了几次"，比一句笼统提示有用。
		return hunt.StopSwitch, fmt.Sprintf(
			"上一步连续 %d 次因同一类原因失败（%s）。换一种做法：不要重复刚才的动作，先根据失败信息调整参数或改用别的工具。",
			e.failStreak, e.failKind)
	default:
		return hunt.StopContinue, ""
	}
}

// DeniedCount 报告连续策略拒绝的累积情况（达阈值即终止）。
//
// "连续"的口径：出现一次 Allow（放行）即归零、拒绝即累加——计数在 Decide 里喂，这里只读。
func (e *engine) DeniedCount() (int, bool) {
	return e.deniedStreak, e.deniedStreak >= MaxDeniedStreak
}
