// Package agentsmd 是项目约定文件的默认读法——提示词扩展的一个实现。
//
// 约定文件的发现方式本身开放给插件（见 hunt.PromptPlugin），这个包给的是"按开放规范读
// AGENTS.md"的口径：文件名精确匹配、根级全文注入、嵌套场景下按操作位置附注路径链上
// 最近且尚未注入过的那一份。
//
// 它只依赖 PromptInput 给的**只读**工作区视图，因此不持有配置、不需要注册、不知道自己
// 被装在哪个流程里——装配层把它放进插件清单即可，替换或增减都不影响内核。
package agentsmd

import (
	"context"
	"fmt"
	"strings"

	"xhunter/hunt"
	"xhunter/workspace"
)

// name 是插件的自述名（稳定短名）：进生效配置快照供审计，唯一来源是插件自己。
const name = "agentsmd"

// 项目约定文件的规范文件名。
const fileName = "AGENTS.md"

// scope 是降级记录里的范围标识。
const scope = "prompt.agentsmd"

// maxBytes 是注入正文的字节上限。截断必须留痕，所以伴随一条降级记录。
const maxBytes = 32 << 10

// Plugin 把项目约定读成 system 段的一段正文。它持有工作区（只读视图）。
type Plugin struct {
	ws workspace.Workspace
}

// New 构造项目约定插件。
func New(ws workspace.Workspace) *Plugin { return &Plugin{ws: ws} }

var _ hunt.PromptPlugin = (*Plugin)(nil)

// Name 返回插件自述名。
func (p *Plugin) Name() string { return name }

// Build 读取根级约定文件，作为 system 段的一段正文返回。
func (p *Plugin) Build(_ context.Context, in hunt.PromptInput) (hunt.PromptPart, error) {
	info, err := p.ws.Stat(fileName)
	if err != nil {
		return hunt.PromptPart{}, err
	}
	if !info.Exists {
		return hunt.PromptPart{}, nil
	}

	fc, err := p.ws.Read(fileName, workspace.LineRange{})
	if err != nil {
		return hunt.PromptPart{}, err
	}
	body := strings.TrimRight(fc.Raw, "\n")
	if strings.TrimSpace(body) == "" {
		return hunt.PromptPart{}, nil
	}

	part := hunt.PromptPart{
		Sources: []string{"base:" + ref(in) + ":" + fileName},
	}
	if len(body) > maxBytes {
		cut := strings.LastIndexByte(body[:maxBytes], '\n')
		if cut <= 0 {
			cut = maxBytes
		} else {
			cut++
		}
		part.Notices = append(part.Notices, hunt.Notice{
			Scope:   scope,
			Subject: fileName,
			Reason:  fmt.Sprintf("内容超过上限 %d 字节，已截断（原始 %d 字节）", maxBytes, len(body)),
		})
		body = body[:cut]
	}

	part.Body = "以下是本仓库的项目约定（" + fileName + "），实现与修改时以它为准：\n\n" + body
	return part, nil
}

// ref 给出这次约定的来源标识：从哪个基线读来的。
func ref(in hunt.PromptInput) string {
	commit := in.Bounty.Repo.BaseCommit
	if commit == "" {
		return "worktree"
	}
	if len(commit) > 12 {
		commit = commit[:12]
	}
	return commit
}
