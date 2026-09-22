package main

import (
	"xhunter/harness"
	"xhunter/prompt/agentsmd"
	"xhunter/prompt/skills"
)

// defaultPromptPlugins 给出首轮两段正文的默认插件清单。
//
// **顺序即执行顺序**：同一段的插件按这里的顺序拼接，因此这份清单就是"提示词由哪几段、
// 以什么次序拼成"的唯一定义处。选择与排序都留在装配期——内核不参与挑选，它只按段把
// 拼好的正文放到固定位置上。
//
// 换一家实现（比如技能清单改从别处拉）只改这里一行：插件既不认识 harness 的其余协作者，
// 也不认识自己被装在哪个流程里。
//
// user 段暂不接插件：这一轮先把 system 段的两件事（项目约定、可用技能清单）落到代码上。
func defaultPromptPlugins() (system, user []harness.PromptPlugin) {
	return []harness.PromptPlugin{
		agentsmd.New(), // 项目约定
		skills.New(),   // 可用 skill 清单
	}, nil
}
