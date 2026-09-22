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
type PromptPlugin interface {
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
