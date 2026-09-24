package main

import (
	"runtime"

	"xhunter/ext"
	"xhunter/ext/syntax"
	"xhunter/git"
	"xhunter/harness"
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

// defaultExt 给出符号后端：内置**语法级**后端（进程内，零外部依赖、零子进程）。
//
// 它是首发后端也是兜底：外挂进程（MCP）是给第三方后端的通道，核心只认 `ext.ExtHost` 接口，
// 换后端不改任何业务代码。构造需要工作区（定位要读源码），因此是工厂形态。
func defaultExt(ws workspace.Workspace) ext.ExtHost { return syntax.New(ws) }

// defaultPolicy 给出策略引擎的实现：默认拒绝 + 路径边界 + 预算。
func defaultPolicy(budget hunt.Budget) hunt.Policy {
	return policy.New(policy.Config{Budget: budget})
}

// assemblyFacts 汇总「本次实际生效的规则」里只有装配层知道的那部分，值注入 Session。
//
// 策略口径来自策略实现自述（边界常量是它的知识，装配层不另抄一份）；检查点行为是本期固定
// 档；扩展指纹来自实际装配的符号后端（没有后端时是空数组，如实表示"没有"）；目标平台与内核环境事实
// 同源（GOOS/GOARCH）；机制硬顶取自实际生效的 harness 配置，供 config_snapshot 如实报出。
func assemblyFacts(hcfg harness.Config) hunt.AssemblyFacts {
	return hunt.AssemblyFacts{
		Policy:     policy.Facts(),
		Checkpoint: checkpointBehaviour,
		// 符号能力是本次装配的真实事实：指纹与 `defaultExt` 装的后端**同源**
		// （同一份静态函数，不另抄一份字符串）。
		Ext:      syntax.FingerprintTokens(),
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
		// 机制硬顶从实际生效的 harness 配置里取（与 harness.New 用的是同一份），这里不另抄
		// 3/1000——否则改了 withDefaults、快照报的还是旧数。
		MaxFailStreak: hcfg.MaxFailStreak,
		MaxTurnsHard:  hcfg.MaxTurns,
		// 接收段不活动超时的生效值（毫秒；0 = 不限/关闭）。与上面两条硬顶**同源同通道**上报，
		// 只进 config_snapshot、不进 EffectiveConfig。
		StreamIdleTimeoutMS: int(hcfg.StreamIdleTimeout.Milliseconds()),
	}
}
