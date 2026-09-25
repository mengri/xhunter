// Package skills 是技能清单的默认扫描方式——提示词扩展的一个实现。
//
// 渐进披露有三步，这个包只管第一步：**发现**。它只把 name 与 description 交给 system 段，
// 清单之外一个字都不进上下文。SKILL.md 全文与 references/ 从不经过这里——那是模型按需调
// read 原语取的东西。因此零新原语，skill 也不会多出一条绕过策略与截断的通道。
package skills

import (
	"context"
	"fmt"
	"path"
	"strings"

	"xhunter/hunt"
	"xhunter/workspace"
)

const (
	name       = "skills" // 插件自述名（进生效配置快照供审计，唯一来源是插件自己）
	dir        = ".xhunter/skills/"
	skillFile  = "SKILL.md"
	scope      = "prompt.skills"
	maxEntries = 50
)

// Plugin 把可用 skill 清单读成 system 段的一段正文。它持有工作区（只读视图）。
type Plugin struct {
	ws workspace.Workspace
}

// New 构造技能清单插件。
func New(ws workspace.Workspace) *Plugin { return &Plugin{ws: ws} }

var _ hunt.PromptPlugin = (*Plugin)(nil)

// Name 返回插件自述名。
func (p *Plugin) Name() string { return name }

// entry 是清单里的一条：只有名字、说明与入口路径。
type entry struct {
	name        string
	description string
	path        string
}

// Build 扫描技能目录，把清单作为 system 段的一段正文返回。
func (p *Plugin) Build(_ context.Context, in hunt.PromptInput) (hunt.PromptPart, error) {
	res, err := p.ws.List(skillFile, nil)
	if err != nil {
		return hunt.PromptPart{}, err
	}

	var notices []hunt.Notice
	entries := make([]entry, 0, len(res.Files))
	for _, file := range res.Files {
		if !strings.HasPrefix(file, dir) {
			continue
		}
		e, notice := readEntry(p.ws, file)
		if notice != nil {
			notices = append(notices, *notice)
			continue
		}
		entries = append(entries, e)
	}

	if len(entries) > maxEntries {
		notices = append(notices, hunt.Notice{
			Scope:   scope,
			Subject: dir,
			Reason:  fmt.Sprintf("共 %d 条，超过上限 %d，已截断", len(entries), maxEntries),
		})
		entries = entries[:maxEntries]
	}
	if len(entries) == 0 {
		return hunt.PromptPart{Notices: notices}, nil
	}

	return hunt.PromptPart{
		Body:    render(entries),
		Sources: []string{fmt.Sprintf("base:%s:skills(%d)", ref(in), len(entries))},
		Notices: notices,
	}, nil
}

// readEntry 读一条技能并解析出清单需要的字段；失败返回降级记录而不是错误。
func readEntry(ws workspace.Workspace, file string) (entry, *hunt.Notice) {
	skip := func(reason string) (entry, *hunt.Notice) {
		return entry{}, &hunt.Notice{Scope: scope, Subject: file, Reason: reason}
	}

	fc, err := ws.Read(file, workspace.LineRange{})
	if err != nil {
		return skip("读取失败：" + err.Error())
	}
	meta, err := parseFrontmatter(fc.Raw)
	if err != nil {
		return skip(err.Error())
	}
	if want := path.Base(path.Dir(file)); meta.name != want {
		return skip(fmt.Sprintf("name 与目录名不一致：目录 %q，frontmatter 写的是 %q", want, meta.name))
	}
	return entry{name: meta.name, description: meta.description, path: file}, nil
}

// render 把清单渲染成一段正文。
func render(entries []entry) string {
	var b strings.Builder
	b.WriteString("可用的技能（判断与当前任务相关时，先用 read 原语读取对应的 SKILL.md 全文，再按其说明执行）：\n")
	for _, e := range entries {
		b.WriteString("- ")
		b.WriteString(e.name)
		b.WriteString("：")
		b.WriteString(e.description)
		b.WriteString("（")
		b.WriteString(e.path)
		b.WriteString("）\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// ref 给出清单的来源标识：从哪个基线读来的，共几条。
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
