package main

import (
	"fmt"
	"os"
	"strings"

	"xhunter/git"
	"xhunter/hunt"
)

// 投递形态：Bounty 文件只装任务正文，部署事实由环境变量给出。
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

	bounty := hunt.Bounty{ID: hunt.BountyID(id), Task: task}
	if session := readEnv(lookup, envSessionID); session != "" {
		bounty.Session = &hunt.SessionRef{ID: session}
	}

	branch := readEnv(lookup, envRepoBranch)
	if branch == "" {
		branch = "xhunter/" + SessionID(bounty)
	}

	bounty.Repo = git.RepoRef{Remote: remote, Branch: branch, BaseCommit: base}
	return bounty, nil
}

// SessionID 返回这次执行所属的会话标识。
func SessionID(b hunt.Bounty) string {
	if b.Session != nil && b.Session.ID != "" {
		return b.Session.ID
	}
	return string(b.ID)
}

// providerSelection 是从环境得到的模型接入选择。
type providerSelection struct {
	ProviderID string
	ModelID    string
	ConfigPath string
}

func selectionFromEnv(lookup lookupEnv) providerSelection {
	return providerSelection{
		ProviderID: readEnv(lookup, envProvider),
		ModelID:    readEnv(lookup, envModel),
		ConfigPath: readEnv(lookup, envProviderFile),
	}
}

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
