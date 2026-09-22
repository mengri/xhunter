package main

import (
	"xhunter/hunt"
	"xhunter/prompt/agentsmd"
	"xhunter/prompt/skills"
	"xhunter/prompt/task"
	"xhunter/workspace"
)

// defaultSystemPlugins / defaultUserPlugins 给出首轮两段正文的默认插件工厂。
// 顺序即拼接顺序；插件读约定、扫技能清单都需要工作区，因此是工厂。
//
// 两段的分工按内容稳定性划：system 段放跨任务稳定的内容（项目约定、技能清单），
// user 段放每次投递都不同的内容（任务陈述）——稳定前缀才可能长期命中提示词缓存。
func defaultSystemPlugins(ws workspace.Workspace) []hunt.PromptPlugin {
	return []hunt.PromptPlugin{
		agentsmd.New(ws), // 项目约定
		skills.New(ws),   // 可用 skill 清单
	}
}

// defaultUserPlugins 给 user 段接上任务陈述。任务正文只存在于 Bounty 里，
// 没有这一段模型就看不到要做什么（FR-7.1：user 段 = 任务描述）。
func defaultUserPlugins(workspace.Workspace) []hunt.PromptPlugin {
	return []hunt.PromptPlugin{
		task.New(),
	}
}
