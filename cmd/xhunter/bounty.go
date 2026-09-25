package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"xhunter/git"
	"xhunter/hunt"
)

// 投递形态：Bounty 文件只装任务正文，部署事实由环境变量给出。
const (
	envRepoURL    = "XHUNTER_REPO_URL"
	envRepoBranch = "XHUNTER_REPO_BRANCH"
	envRepoBase   = "XHUNTER_REPO_BASE_COMMIT"
	envBountyID   = "XHUNTER_BOUNTY_ID"
	envSessionID  = "XHUNTER_SESSION_ID"
	envTraceID    = "XHUNTER_TRACE_ID"

	// 预算上限：运行策略，与任务正文分开投递（同一台机器上往往固定）。
	// 未设置或 0 表示该维度不限（hunt.Budget 的口径）。
	envBudgetTurns     = "XHUNTER_BUDGET_TURNS"
	envBudgetTokens    = "XHUNTER_BUDGET_TOKENS"
	envBudgetWallClock = "XHUNTER_BUDGET_WALL_CLOCK"

	// 心跳间隔：任务级心跳的部署事实（可选）。未设置取 Session 的默认 30s；
	// 平台判断"卡死"的灵敏度由它定。
	envHeartbeatInterval = "XHUNTER_HEARTBEAT_INTERVAL"

	// 接收段不活动超时：运行段看护的部署事实（可选）。未设置取 harness 的默认 120s；
	// 平台判断"上游挂起"的灵敏度由它定。**只接受正 duration**——「不配」才是取默认，
	// 「关掉看门狗」（负数）只在程序内可达、不暴露给部署侧（关掉它 = 上游挂起永不中止）。
	envStreamIdleTimeout = "XHUNTER_STREAM_IDLE_TIMEOUT"

	// 外挂符号后端（MCP over stdio 子进程）：可选后端，不配即用内置语法级后端（FR-13.9）。
	// 配了命令就是"这次用外挂后端"；它起不来时符号原语给结构化错误（FR-13.4），
	// **不暗地退回内置**——静默退回会让精度档位（syntactic / semantic）悄悄变化而不上报。
	envExtCommand = "XHUNTER_EXT_COMMAND"
	// envExtArgs 是子进程入参：按空白切分（不支持引号转义——够用，且不会解析出意外的参数）。
	envExtArgs = "XHUNTER_EXT_ARGS"
	// envExtTimeout 是单次调用的上界（正 duration，缺省 30s）。
	envExtTimeout = "XHUNTER_EXT_TIMEOUT"
	// envExtLanguages 是后端覆盖的语言清单（逗号分隔）：能力指纹的素材。指纹在装配冻结
	// 时就读，那时还没问过后端，语言只能由部署侧声明（FR-13.7）。
	envExtLanguages = "XHUNTER_EXT_LANGUAGES"
	// envExtEnv 是**点名授予**的环境变量名（逗号分隔）：外挂进程默认一个变量都不继承
	// （XHUNTER_* 里可能有凭据，FR-13.6），只拿到这里点名、且当前环境里确实存在的那几项——
	// 点了一个不存在的名字即启动期失败：拼错不该在运行期才变成"扩展莫名其妙起不来"。
	envExtEnv = "XHUNTER_EXT_ENV"
)

// lookupEnv 让组装过程可在测试里替换环境来源。
type lookupEnv func(string) (string, bool)

// loadTask 读任务正文，不解析任何结构。
func loadTask(path string, readFile func(string) ([]byte, error)) (string, error) {
	raw, err := readFile(path)
	if err != nil {
		return "", fmt.Errorf("读取任务文件失败：%w", err)
	}
	task := strings.TrimSpace(string(raw))
	if task == "" {
		return "", fmt.Errorf("任务文件 %s 是空的：没有任何要做的内容", path)
	}
	return task, nil
}

// bountyFromEnv 从环境变量组装任务分派事实。
func bountyFromEnv(task string, lookup lookupEnv) (hunt.Bounty, error) {
	remote := readEnv(lookup, envRepoURL)
	if remote == "" {
		return hunt.Bounty{}, fmt.Errorf("缺少 %s：仓库地址是必填的部署事实", envRepoURL)
	}
	base := readEnv(lookup, envRepoBase)
	if base == "" {
		return hunt.Bounty{}, fmt.Errorf("缺少 %s：基线提交是必填的部署事实", envRepoBase)
	}

	id := readEnv(lookup, envBountyID)
	if id == "" {
		id = shortSHA(base)
	}

	bounty := hunt.Bounty{ID: hunt.BountyID(id), Task: task, TraceID: readEnv(lookup, envTraceID)}
	if bounty.TraceID == "" {
		// 追踪标识缺省回填：没有它，事件流就串不回平台上的那一次投递。
		bounty.TraceID = id
	}
	if session := readEnv(lookup, envSessionID); session != "" {
		bounty.Session = &hunt.SessionRef{ID: session}
	}

	branch := readEnv(lookup, envRepoBranch)
	if branch == "" {
		branch = branchFor(SessionID(bounty))
	}

	bounty.Repo = git.RepoRef{
		Remote: remote, Branch: branch, BaseCommit: base,
		// 材料目录是"本次运行的仓库事实"：交付 diff/patch 据此把它排除（FR-6.1）。路径的唯一来源
		// 是 materialDirFor —— 与落盘同源，不两处各拼一遍。
		MaterialDir: materialDirFor(SessionID(bounty)),
	}

	// 预算是策略层的输入：没有它，Policy.Exhausted 永远为假，任务只能靠引擎的
	// 轮数硬顶兜底——而"预算耗尽立即终止并上报耗尽维度"是产品需求（FR-9、AC-5）。
	budget, err := parseBudget(lookup)
	if err != nil {
		return hunt.Bounty{}, err
	}
	bounty.Budget = budget
	return bounty, nil
}

// parseBudget 从环境读三重预算上限。值必须是正数；写错即启动期失败（退出 1），
// 不静默当成"不限"——一个拼错的变量名会让预算悄悄失效。
func parseBudget(lookup lookupEnv) (hunt.Budget, error) {
	var b hunt.Budget

	if v := readEnv(lookup, envBudgetTurns); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return hunt.Budget{}, fmt.Errorf("%s 必须是正整数（当前 %q）", envBudgetTurns, v)
		}
		b.MaxTurns = n
	}
	if v := readEnv(lookup, envBudgetTokens); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return hunt.Budget{}, fmt.Errorf("%s 必须是正整数（当前 %q）", envBudgetTokens, v)
		}
		b.MaxTokens = n
	}
	if v := readEnv(lookup, envBudgetWallClock); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return hunt.Budget{}, fmt.Errorf("%s 必须是正的时间长度（如 90m、2h；当前 %q）", envBudgetWallClock, v)
		}
		b.MaxWallClock = d
	}
	return b, nil
}

// parseHeartbeatInterval 从环境读心跳间隔。取值非法即启动期失败（环境问题，退出 1）——
// 不留到运行期才发现、也不静默退回默认值：一个拼错的变量名会让间隔悄悄失效。
//
// 未设置 → 0（由 Session 取默认 30s）。显式写 0 / 负数一律非法：0 会被读成"不限"或"关掉"，
// 两种都不是我们承诺的语义——「不配」才是取默认。
func parseHeartbeatInterval(lookup lookupEnv) (time.Duration, error) {
	v := readEnv(lookup, envHeartbeatInterval)
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s 必须是正的时间长度（如 45s、2m；当前 %q）", envHeartbeatInterval, v)
	}
	return d, nil
}

// parseStreamIdleTimeout 从环境读接收段不活动超时。取值非法即启动期失败（环境问题，退出 1）——
// 不留到运行期才发现、也不静默退回默认值：一个拼错的变量名会让超时悄悄失效。
//
// 未设置 → 0（由 harness 取默认 120s）。非 duration / 0 / 负数一律非法：0 会被读成"不限"、
// 负数会被读成"关闭看门狗"，两种都不是我们承诺的语义——「不配」才是取默认。负值本可用于
// 关闭看门狗，但它只在程序内可达、**不暴露给部署侧**（部署侧关掉它 = 上游挂起永不中止）。
func parseStreamIdleTimeout(lookup lookupEnv) (time.Duration, error) {
	v := readEnv(lookup, envStreamIdleTimeout)
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s 必须是正的时间长度（如 90s、2m；当前 %q）", envStreamIdleTimeout, v)
	}
	return d, nil
}

// SessionID 返回这次执行所属的会话标识。取值口径定义在 hunt.Bounty 上，这里只是本包的
// 便利入口（结果文件与事件都经它取），实现委托过去——两处各写一遍，改一处忘一处就会漂。
func SessionID(b hunt.Bounty) string { return b.SessionID() }

// branchFor 给出任务分支名。它是**分支名规则的唯一来源**：环境投递路径（bountyFromEnv）
// 与本地驱动路径读同一份，两处各拼一遍迟早会漂。
func branchFor(sessionID string) string { return "xhunter/" + sessionID }

func readEnv(lookup lookupEnv, name string) string {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	v, _ := lookup(name)
	return strings.TrimSpace(v)
}

// shortSHA 取提交哈希的前 12 位作为缺省任务标识。
func shortSHA(sha string) string {
	const short = 12
	if len(sha) <= short {
		return sha
	}
	return sha[:short]
}
