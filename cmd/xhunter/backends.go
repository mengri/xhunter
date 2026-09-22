package main

import (
	"xhunter/git"
	"xhunter/hunt"
	"xhunter/internal/git/cli"
	"xhunter/internal/policy"
	"xhunter/internal/workspace/osfs"
	"xhunter/workspace"
)

// defaultWorkspaces 给出文件操作的实现：本地文件系统。
func defaultWorkspaces() workspace.WorkspaceOpener {
	return osfs.Opener{}
}

// defaultGit 给出 git 操作的实现：命令行。
func defaultGit() git.GitWorktree {
	return cli.New(cli.Config{})
}

// defaultPolicy 给出策略引擎的实现：默认拒绝 + 路径边界 + 预算。
func defaultPolicy(budget hunt.Budget) hunt.Policy {
	return policy.New(policy.Config{Budget: budget})
}
