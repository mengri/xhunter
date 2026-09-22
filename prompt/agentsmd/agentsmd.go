// Package agentsmd 是项目约定文件的默认读法——提示词扩展的一个实现。
//
// 约定文件的发现方式本身开放给插件（见 harness.PromptPlugin），这个包给的是"按开放
// 规范读 AGENTS.md"的口径：文件名精确匹配、根级全文注入、嵌套场景下按操作位置附注
// 路径链上最近且尚未注入过的那一份。
//
// 它只依赖 PromptInput 给的**只读**工作区视图，因此不持有配置、不需要注册、不知道自己
// 被装在哪个流程里——装配层把它放进插件清单即可，替换或增减都不影响内核。
package agentsmd

import (
	"context"
	"fmt"
	"strings"

	"xhunter/harness"
)

// 项目约定文件的规范文件名。大小写不匹配的变体（如 agents.md）不算规范文件：
// 跨平台大小写敏感度不同，认下来会让"注入了什么"随文件系统而变。
const fileName = "AGENTS.md"

// scope 是降级记录里的范围标识：消费方据此筛选"提示词构造阶段的哪一步降了级"。
const scope = "prompt.agentsmd"

// maxBytes 是注入正文的字节上限。
//
// 约定文件是给人读的，正常体量在几 KB；超过这个量级多半是把无关文档塞进来了。
// 截断而不是整份注入，是为了不让一份失控的文件把上下文预算吃光——但它**必须留痕**，
// 所以截断会伴随一条降级记录（静默截断会让模型以为约定只有这些）。
const maxBytes = 32 << 10

// Plugin 把项目约定读成 system 段的一段正文。
type Plugin struct{}

// New 构造项目约定插件。
func New() *Plugin { return &Plugin{} }

var _ harness.PromptPlugin = (*Plugin)(nil)

// Build 读取根级约定文件，作为 system 段的一段正文返回。
//
// 三种结果都是"正常"的，只有读取本身失败才是错误：
//   - 文件不存在 → 空正文。仓库没有约定文件是合法状态，不是错误，更不该让任务失败；
//   - 内容为空白 → 空正文（与不存在同义，注入一段空白只会白占预算）；
//   - 超出上限 → 截断 + 降级记录。
//
// 读取走 in.Workspace：它是只读视图，入参是工作区相对路径，读到的就是基线的内容。
// 于是模型改不了本次注入的内容——它改的只是交付 diff 里的那一份（书写与生效分离）。
func (p *Plugin) Build(_ context.Context, in harness.PromptInput) (harness.PromptPart, error) {
	info, err := in.Workspace.Stat(fileName)
	if err != nil {
		return harness.PromptPart{}, err
	}
	if !info.Exists {
		return harness.PromptPart{}, nil
	}

	fc, err := in.Workspace.Read(fileName, harness.LineRange{})
	if err != nil {
		return harness.PromptPart{}, err
	}
	body := strings.TrimRight(fc.Raw, "\n")
	if strings.TrimSpace(body) == "" {
		return harness.PromptPart{}, nil
	}

	part := harness.PromptPart{
		Sources: []string{"base:" + ref(in) + ":" + fileName},
	}
	if len(body) > maxBytes {
		// 在行边界收刀：把一个句子劈成两半，读起来比少一段更糟。
		// 收刀点含换行符本身，因此截断后的正文以完整行收尾。
		cut := strings.LastIndexByte(body[:maxBytes], '\n')
		if cut <= 0 {
			cut = maxBytes
		} else {
			cut++
		}
		part.Notices = append(part.Notices, harness.Notice{
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
// 它进生效配置快照，事后要能回答"这次用的是哪一版约定"——工作区随任务改动，
// 只有基线提交能把"当时读到的"钉住。
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
