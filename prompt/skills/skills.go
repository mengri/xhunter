// Package skills 是技能清单的默认扫描方式——提示词扩展的一个实现。
//
// 渐进披露有三步，这个包只管第一步：**发现**。它只把 name 与 description 交给
// system 段，清单之外一个字都不进上下文。SKILL.md 全文与 references/ 从不经过这里——
// 那是模型按需调 read 原语取的东西（"激活"与"执行"两步）。因此零新原语，skill 也不会
// 多出一条绕过策略与截断的通道。
package skills

import (
	"context"
	"fmt"
	"path"
	"strings"

	"xhunter/harness"
)

const (
	// 技能目录与入口文件的默认位置。它们是本插件的约定，不是内核的知识——
	// 换一个插件就等于换一套约定。
	dir       = ".xhunter/skills/"
	skillFile = "SKILL.md"

	// scope 是降级记录里的范围标识。
	scope = "prompt.skills"

	// maxEntries 是清单条数上限：清单本身也会把预算吃光（每条约百 token）。
	// 溢出截断并上报，而不是让清单无界增长。
	maxEntries = 50
)

// Plugin 把可用 skill 清单读成 system 段的一段正文。
type Plugin struct{}

// New 构造技能清单插件。
func New() *Plugin { return &Plugin{} }

var _ harness.PromptPlugin = (*Plugin)(nil)

// entry 是清单里的一条：只有名字、说明与入口路径。
type entry struct {
	name        string
	description string
	path        string
}

// Build 扫描技能目录，把清单作为 system 段的一段正文返回。
//
// 逐条读、逐条判：单条素材非法（frontmatter 写坏、name 与目录名不一致、超长）
// 只跳过这一条并留下降级记录，不影响其余条目，更不让任务失败——一份坏文件不该让
// 整个任务起不来。
//
// 清单里**必须带上入口路径**：模型据此用 read 原语取全文（渐进披露的"激活"一步）。
// 只给名字等于给了把钥匙却不给锁的位置。
func (p *Plugin) Build(_ context.Context, in harness.PromptInput) (harness.PromptPart, error) {
	paths, err := in.Workspace.List(skillFile)
	if err != nil {
		return harness.PromptPart{}, err
	}

	var notices []harness.Notice
	entries := make([]entry, 0, len(paths))
	for _, file := range paths {
		if !strings.HasPrefix(file, dir) {
			continue // 只认约定目录：别处的同名文件不是技能，交给模型自己去读
		}
		e, notice := readEntry(in.Workspace, file)
		if notice != nil {
			notices = append(notices, *notice)
			continue
		}
		entries = append(entries, e)
	}

	if len(entries) > maxEntries {
		notices = append(notices, harness.Notice{
			Scope:   scope,
			Subject: dir,
			Reason:  fmt.Sprintf("共 %d 条，超过上限 %d，已截断", len(entries), maxEntries),
		})
		entries = entries[:maxEntries]
	}
	if len(entries) == 0 {
		return harness.PromptPart{Notices: notices}, nil
	}

	return harness.PromptPart{
		Body:    render(entries),
		Sources: []string{fmt.Sprintf("base:%s:skills(%d)", ref(in), len(entries))},
		Notices: notices,
	}, nil
}

// readEntry 读一条技能并解析出清单需要的字段；失败返回降级记录而不是错误。
func readEntry(ws harness.Workspace, file string) (entry, *harness.Notice) {
	skip := func(reason string) (entry, *harness.Notice) {
		return entry{}, &harness.Notice{Scope: scope, Subject: file, Reason: reason}
	}

	fc, err := ws.Read(file, harness.LineRange{})
	if err != nil {
		return skip("读取失败：" + err.Error())
	}
	meta, err := parseFrontmatter(fc.Raw)
	if err != nil {
		return skip(err.Error())
	}
	// 规范要求 name 与目录名一致：不一致时无法判断该信哪个，跳过比猜更安全。
	if want := path.Base(path.Dir(file)); meta.name != want {
		return skip(fmt.Sprintf("name 与目录名不一致：目录 %q，frontmatter 写的是 %q", want, meta.name))
	}
	return entry{name: meta.name, description: meta.description, path: file}, nil
}

// render 把清单渲染成一段正文。
//
// 措辞里点明"需要时读全文再照做"，因为这正是渐进披露的用法：清单只负责让模型知道
// 有什么可用，正文由它自己按需取。
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
func ref(in harness.PromptInput) string {
	commit := in.Bounty.Repo.BaseCommit
	if commit == "" {
		return "worktree"
	}
	if len(commit) > 12 {
		commit = commit[:12]
	}
	return commit
}
