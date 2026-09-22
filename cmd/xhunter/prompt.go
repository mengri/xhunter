package main

import (
	"xhunter/hunt"
	"xhunter/prompt/agentsmd"
	"xhunter/prompt/skills"
	"xhunter/workspace"
)

// defaultSystemPlugins / defaultUserPlugins 给出首轮两段正文的默认插件工厂。
// 顺序即拼接顺序；插件读约定、扫技能清单都需要工作区，因此是工厂。
func defaultSystemPlugins(ws workspace.Workspace) []hunt.PromptPlugin {
	return []hunt.PromptPlugin{
		agentsmd.New(ws), // 项目约定
		skills.New(ws),   // 可用 skill 清单
	}
}

func defaultUserPlugins(workspace.Workspace) []hunt.PromptPlugin {
	return nil
}
