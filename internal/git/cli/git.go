// Package cli 是「git 操作」这件事的实现落点：通过 git 命令行完成基线获取、任务分支、
// 提交与差异。它是 xhunter/git 里 GitWorktree 契约的一个实现。
//
// 为什么用命令行而不是库：契约要的是少数几个语义明确的动作，而这些动作的语义与 git
// 版本无关、与实现无关；用命令行换来零依赖与「人和工具跑的是同一条命令」，出问题时
// 可以照着日志手工复现。
package cli

import (
	"context"
	"os"
	"strings"

	"xhunter/git"
	"xhunter/llm"
)

// Config 是装配层给出的部署事实，只装「与环境有关、与任务无关」的部分。
type Config struct {
	WorkDir string // 工作区父目录，空则用系统临时目录
	Remote  string // 远端名，默认 origin
}

// Git 是命令行实现的 git 工作树。
type Git struct {
	workDir string
	remote  string
}

// New 构造 git 实现。它只做参数归一，不执行任何命令。
func New(cfg Config) *Git {
	workDir := cfg.WorkDir
	if strings.TrimSpace(workDir) == "" {
		workDir = os.TempDir()
	}
	remote := cfg.Remote
	if remote == "" {
		remote = "origin"
	}
	return &Git{workDir: workDir, remote: remote}
}

var _ git.GitWorktree = (*Git)(nil)

// PrepareBaseline 获取基线、建立任务分支，并返回工作树根。
//
// **实现待补**：在 workDir 下建临时工作树 → 按 BaseCommit 取基线 → 创建并推送任务分支
// → checkout 分支 tip。失败一律是环境问题。
func (g *Git) PrepareBaseline(_ context.Context, repo git.RepoRef) (string, error) {
	return "", notImplemented("PrepareBaseline", repo)
}

// Commit 提交自上一个检查点以来的全部改动并推送。**实现待补**。
func (g *Git) Commit(_ context.Context, repo git.RepoRef, _ string) (git.Commit, error) {
	return git.Commit{}, notImplemented("Commit", repo)
}

// Diff 产出相对基线的改动文件清单。
func (g *Git) Diff(_ context.Context, _ string) ([]string, error) {
	return nil, notImplemented("Diff", git.RepoRef{})
}

// Clean 回收临时工作树。**实现待补**：删目录即可。
func (g *Git) Clean(_ context.Context) error {
	return notImplemented("Clean", git.RepoRef{})
}

// notImplemented 给出明确的结构化失败，而不是静默返回零值。
func notImplemented(op string, repo git.RepoRef) error {
	target := repo.Remote
	if target == "" {
		target = "仓库"
	}
	return &llm.Fault{
		Kind:      "not_implemented",
		Message:   "git 操作尚未实现：" + op + "（" + target + "）",
		Retryable: false,
	}
}
