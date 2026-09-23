package main

import (
	"runtime"

	"xhunter/git"
	"xhunter/hunt"
	"xhunter/internal/git/cli"
	"xhunter/internal/policy"
	"xhunter/internal/workspace/osfs"
	"xhunter/workspace"
)

// checkpointBehaviour 是本期的检查点行为口径：只在结构完整点上自动产生（判据不可判定时
// 不提交）。它进生效配置快照——评审者据此知道本次运行的检查点策略，而不是靠记忆去猜。
const checkpointBehaviour = "on_structure"

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

// assemblyFacts 汇总「本次实际生效的规则」里只有装配层知道的那部分，值注入 Session。
//
// 策略口径来自策略实现自述（边界常量是它的知识，装配层不另抄一份）；检查点行为是本期固定
// 档；扩展指纹本期为空（扩展未接入，空数组如实表示"没有扩展"）；目标平台与内核环境事实
// 同源（GOOS/GOARCH）。
func assemblyFacts() hunt.AssemblyFacts {
	return hunt.AssemblyFacts{
		Policy:     policy.Facts(),
		Checkpoint: checkpointBehaviour,
		Platform:   runtime.GOOS + "/" + runtime.GOARCH,
	}
}
