// Package task 把任务正文渲染成 user 段的一段正文——提示词扩展的一个实现。
//
// 它与其它插件的分工是"素材来源"上的分工：约定文件与技能清单来自工作区，任务陈述
// 来自 Bounty（投递事实）。三者在提示词里的位置由执行体决定（system 段放稳定内容、
// user 段放每次不同的内容），本插件只把正文交出来。
//
// 为什么它必须存在：任务正文只出现在 Bounty 里，而 Bounty 不是模型可见的形状。没有
// 这一段，模型拿到的是"项目约定 + 技能清单"，看不到要做什么——任务会在第一轮就退化
// 成"无工具调用、无产出"。
//
// 它不产出消息序列、不读工作区、不需要注册：装配层把它放进 user 段插件清单即可。
package task

import (
	"context"
	"strings"

	"xhunter/hunt"
)

// name 是插件的自述名（稳定短名）：进生效配置快照供审计，唯一来源是插件自己。
const name = "task"

// Plugin 把任务正文渲染成 user 段的一段正文。它没有构造参数：任务正文来自 PromptInput。
type Plugin struct{}

// New 构造任务陈述插件。
func New() *Plugin { return &Plugin{} }

var _ hunt.PromptPlugin = (*Plugin)(nil)

// Name 返回插件自述名。
func (p *Plugin) Name() string { return name }

// Build 渲染任务陈述。任务正文为空时不贡献正文（空段不占位置），也不报错——
// "没有任务"该由投递侧拦下（空任务文件在启动期即失败），到不了这里。
func (p *Plugin) Build(_ context.Context, in hunt.PromptInput) (hunt.PromptPart, error) {
	body := strings.TrimSpace(in.Bounty.Task)
	if body == "" {
		return hunt.PromptPart{}, nil
	}
	return hunt.PromptPart{
		Body:    "以下是本次任务。按它执行，任务之外不做额外改动：\n\n" + body,
		Sources: []string{"bounty:" + string(in.Bounty.ID)},
	}, nil
}
