package main

import (
	"fmt"
	"os"
	"strings"

	"xhunter/harness"
)

// 投递形态：**Bounty 文件只装任务正文，部署事实由环境变量给出**。
//
// 这条分工的理由是"谁最了解什么"：
//   - 任务正文是**每个任务各不相同**的东西，来自平台的任务分派，因此放在文件里（可读、可 diff、可存档）；
//   - 仓库地址、分支、基线提交、续跑标识这些是**这次跑在哪儿**的事实，属于部署环境，
//     同一台机器上的多次执行往往是同一套值，因此走环境变量，不必每个任务重复写一遍。
//
// 环境变量一律带 `XHUNTER_` 前缀（与凭据的约定一致）。凭据本身仍然只在
// provider 配置的 `{env:VAR}` 引用里出现，不经过本文件。

// 环境变量名。缺省值见 README 与使用手册 §3。
const (
	envRepoURL      = "XHUNTER_REPO_URL"
	envRepoBranch   = "XHUNTER_REPO_BRANCH"
	envRepoBase     = "XHUNTER_REPO_BASE_COMMIT"
	envBountyID     = "XHUNTER_BOUNTY_ID"
	envSessionID    = "XHUNTER_SESSION_ID"
	envProvider     = "XHUNTER_PROVIDER"
	envModel        = "XHUNTER_MODEL"
	envProviderFile = "XHUNTER_PROVIDER_CONFIG"
)

// lookupEnv 让组装过程可在测试里替换环境来源。
type lookupEnv func(string) (string, bool)

// loadTask 读任务正文。
//
// 不解析任何结构：任务描述是自然语言（可含验收标准），解析只会逼着写的人去迁就格式。
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
//
// 必填的只有两项：仓库地址与基线提交——没有它们连工作区都建不起来。
// 分支可省略（按会话标识生成），任务标识可省略（按基线推导），
// 但省略后者只适合本地试跑：同一基线上的并行任务会撞到同一个分支名。
//
// **两个标识的分工**（见 SessionID）：`XHUNTER_BOUNTY_ID` 是**本次投递**的标识，
// `XHUNTER_SESSION_ID` 是**会话**的标识——同一个会话可以有多次投递，它们
// **共享记忆（`.xhunter/<session_id>/`）与分支（`xhunter/<session_id>`）**。
// 不传会话标识时，本次投递自成一次新会话（会话标识即本任务的 id）。
func bountyFromEnv(task string, lookup lookupEnv) (harness.Bounty, error) {
	remote := readEnv(lookup, envRepoURL)
	if remote == "" {
		return harness.Bounty{}, fmt.Errorf("缺少 %s：仓库地址是必填的部署事实", envRepoURL)
	}
	base := readEnv(lookup, envRepoBase)
	if base == "" {
		return harness.Bounty{}, fmt.Errorf("缺少 %s：基线提交是必填的部署事实", envRepoBase)
	}

	// id 缺省取自基线：可复现、可与任务对上。同一基线上的并行任务需显式给 id。
	id := readEnv(lookup, envBountyID)
	if id == "" {
		id = shortSHA(base)
	}

	bounty := harness.Bounty{ID: harness.BountyID(id), Task: task}
	// 显式给出会话标识即加入一个既有会话：分支与记忆都落在它那一份上，
	// 于是同一个会话的第 N 次投递能接着前几次的成果与历史继续。
	if session := readEnv(lookup, envSessionID); session != "" {
		bounty.Session = &harness.SessionRef{ID: session}
	}

	branch := readEnv(lookup, envRepoBranch)
	if branch == "" {
		branch = "xhunter/" + SessionID(bounty)
	}

	bounty.Repo = harness.RepoRef{Remote: remote, Branch: branch, BaseCommit: base}
	return bounty, nil
}

// SessionID 返回这次执行所属的会话标识。
//
// 规则：显式给出 `XHUNTER_SESSION_ID` 就用它，否则取本任务的 id。
// **会话记忆的目录与分支名都以它为准**（`.xhunter/<session_id>/`、`xhunter/<session_id>`），
// 因此同一个会话的多次投递天然共享记忆与工作分支，不需要各自指定分支；
// 反过来，不同会话之间不会互相看到对方的记忆。
func SessionID(b harness.Bounty) string {
	if b.Session != nil && b.Session.ID != "" {
		return b.Session.ID
	}
	return string(b.ID)
}

// providerSelection 是从环境得到的模型接入选择。
//
// 它对应"用哪个 provider + 哪个模型"这一部署决策：同一台机器上通常固定，
// 因此与仓库事实一样走环境变量，而不是塞进每个任务的文件里。
type providerSelection struct {
	ProviderID string
	ModelID    string
	ConfigPath string
}

// selectionFromEnv 读模型接入选择。三者都可为空：为空表示本次不构造 Provider
// （例如只想单独跑 `models` 这类环境侧动作）。
func selectionFromEnv(lookup lookupEnv) providerSelection {
	return providerSelection{
		ProviderID: readEnv(lookup, envProvider),
		ModelID:    readEnv(lookup, envModel),
		ConfigPath: readEnv(lookup, envProviderFile),
	}
}

// validate 检查三要素是否齐备。缺一项都必须显式失败：跑起来才发现
// "不知道该用哪个模型"是最没有价值的失败方式。
func (s providerSelection) validate() error {
	var missing []string
	if s.ProviderID == "" {
		missing = append(missing, envProvider)
	}
	if s.ModelID == "" {
		missing = append(missing, envModel)
	}
	if s.ConfigPath == "" {
		missing = append(missing, envProviderFile)
	}
	if len(missing) > 0 {
		return fmt.Errorf("缺少模型接入配置：%s", strings.Join(missing, "、"))
	}
	return nil
}

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
