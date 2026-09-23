package hunt

import (
	"context"
	"strings"

	"xhunter/llm"
	"xhunter/workspace"
)

// PromptInput 是构造首轮消息时能看到的任务事实。只给事实、不给权限：提供方能读到
// 任务与工具面，但拿不到写入口、拿不到凭据——它交付的是文本，不是行为。
type PromptInput struct {
	Bounty Bounty         // 任务描述、仓库、基线、会话标识
	Tools  []llm.ToolDecl // 已定格的工具面，供提供方按需陈述
}

// PromptPart 是首轮一段正文：由单个插件产出，是拼接前的最小单位。
type PromptPart struct {
	Body    string
	Sources []string // 素材来源，进生效配置快照供审计
	Notices []Notice // 构造过程中的降级记录，进事件流
}

// Notice 是一次降级记录：发生了什么、在哪个对象上、为什么。
type Notice struct {
	Scope   string
	Subject string
	Reason  string
}

// PromptPlugin 构造首轮的一段正文。插件只产出正文，不产出消息序列：
// 两段的位置、顺序、内核条款都由 Session 拼，插件插不进第三段、也删不掉内核条款。
//
// Name 是插件的自述名（稳定短名）：装配顺序由 Session 掌管，但"这一段是谁贡献的"只有
// 插件自己知道——名字的唯一来源是插件本身，装配层不维护平行的名字表（与 Primitive 的
// 名字只来自 Decl().Name 同一原则）。自述名进生效配置快照供审计。
type PromptPlugin interface {
	Name() string
	Build(ctx context.Context, in PromptInput) (PromptPart, error)
}

// PromptPluginFactory 把已就绪的工作区变成一段正文的构造插件清单。
// 插件读约定文件、扫技能清单，都需要工作区，而工作区是运行期产物。
type PromptPluginFactory func(ws workspace.Workspace) []PromptPlugin

// buildPromptStage 按装配顺序依次调用插件，把产出的正文拼成一段。
func buildPromptStage(ctx context.Context, plugins []PromptPlugin, in PromptInput) (PromptPart, error) {
	parts := make([]PromptPart, 0, len(plugins))
	for _, p := range plugins {
		part, err := p.Build(ctx, in)
		if err != nil {
			return PromptPart{}, err
		}
		parts = append(parts, part)
	}
	return joinPromptParts(parts), nil
}

// pluginNames 取两段插件各自的自述名，顺序不变。名字只从插件本身读，不看类型：
// 生效配置快照要报的是"这次实际装了哪几个插件"，而不是一张平行的身份表。
func pluginNames(plugins []PromptPlugin) []string {
	names := make([]string, 0, len(plugins))
	for _, p := range plugins {
		names = append(names, p.Name())
	}
	return names
}

// joinPromptParts 按给定顺序拼接各段正文。没有贡献正文的插件整段跳过，
// 但降级记录不随正文一起被跳过——正文为空不等于无事发生。
func joinPromptParts(parts []PromptPart) PromptPart {
	var bodies, sources []string
	var notices []Notice
	for _, p := range parts {
		notices = append(notices, p.Notices...)
		body := strings.TrimSpace(p.Body)
		if body == "" {
			continue
		}
		bodies = append(bodies, body)
		sources = append(sources, p.Sources...)
	}
	return PromptPart{Body: strings.Join(bodies, "\n\n"), Sources: sources, Notices: notices}
}
