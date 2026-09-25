package basic

import (
	"context"
	"fmt"
	"strings"

	"xhunter/hunt"
	"xhunter/llm"
	"xhunter/workspace"
)

// Find 是 find 原语对模型可见的名字（内容检索）。
const Find = hunt.PrimitiveName("find")

// 三个护栏：命中条数、单行显示宽度、参与检索的单文件大小上限。
// 超出的部分一律**如实标注**（不静默丢）——静默跳过会让模型把「没找到」当成「不存在」，
// 与读取截断同一条纪律。
const (
	findMaxHits      = 100
	findLineWidth    = 160
	findMaxFileBytes = 2 << 20
)

// FindTool 构造 find 原语：在工作区内容里检索字面量片段。
//
// 它只依赖两件已经实现的事——枚举（`Workspace.List`）与读取（`Workspace.Read`），
// 因此不需要任何外部能力。这跟符号原语（要扩展进程）与门禁原语（要门禁清单）不同：
// 那两个「不实现」有正当理由，这个没有。
//
// 枚举面与 glob **完全一致**（同一套 List、同一套默认排除与 include 参数）：
// 同一条面两种用法，避免出现「glob 看不到、find 找得到」这种要靠记忆区分的行为。
func FindTool(ws workspace.Workspace) hunt.Primitive {
	return findPrim{ws: ws}
}

type findPrim struct{ ws workspace.Workspace }

// Writes 报告本原语不写盘：读类原语，策略因此不必为它查路径边界。
func (findPrim) Writes() bool { return false }

func (findPrim) Decl() llm.ToolDecl {
	return llm.ToolDecl{
		Name:        string(Find),
		Description: "检索内容：在工作区里查找字面量片段，返回文件、行号与该行内容。可用 path 限定单个文件，用 scope 限定目录。",
		Schema: llm.ObjectSchemaAnyOf(`{
			"literal": {"type": "string", "description": "要检索的内容片段"},
			"path": {"type": "string", "description": "限定到某个文件"},
			"scope": {"type": "string", "description": "限定范围：目录或包"},
			"include": {"type": "array", "items": {"type": "string"}, "description": "额外放行的默认排除目录（如 node_modules、vendor）；[\"*\"] 表示全部放行"}
		}`, []string{"literal"}),
	}
}

func (p findPrim) Execute(_ context.Context, call hunt.Call, _ hunt.Facts) (hunt.Result, []workspace.FileEdit, error) {
	literal := call.Selector.Literal
	if literal == "" {
		return hunt.Result{}, nil, &llm.Fault{
			Kind: "bad_selector", Message: "内容检索需要给出要查找的字面量",
		}
	}
	res, scope, err := p.candidates(call)
	if err != nil {
		return hunt.Result{}, nil, err
	}
	files := res.Files

	var (
		hits       []string
		total      int
		hitFiles   = map[string]bool{}
		big        int
		unreadable int
	)
	for _, f := range files {
		info, err := p.ws.Stat(f)
		if err != nil {
			unreadable++
			continue
		}
		if info.Size > findMaxFileBytes {
			big++
			continue
		}
		fc, err := p.ws.Read(f, workspace.LineRange{})
		if err != nil {
			unreadable++
			continue
		}
		first := fc.FirstLine
		if first <= 0 {
			first = 1
		}
		for i, line := range strings.Split(fc.Raw, "\n") {
			if !strings.Contains(line, literal) {
				continue
			}
			total++
			hitFiles[f] = true
			if len(hits) < findMaxHits {
				hits = append(hits, fmt.Sprintf("%s:%d: %s", f, first+i, clipLine(line)))
			}
		}
	}

	// 「没找到」是**结论**而不是错误：模型据此判断"哪里都没有"，而不是"工具坏了"。
	return hunt.Result{Summary: findReport(literal, scope, hits, total, len(hitFiles), big, unreadable, len(files), res.Skipped)}, nil, nil
}

// candidates 给出要检索的文件清单与范围说明：path 指单文件；否则是整个工作区（可被 scope 收窄到某个目录）。
func (p findPrim) candidates(call hunt.Call) (workspace.ListResult, string, error) {
	if call.Target != "" {
		return workspace.ListResult{Files: []string{call.Target}}, "文件 " + call.Target, nil
	}
	res, err := p.ws.List("*", call.Selector.Include)
	if err != nil {
		return workspace.ListResult{}, "", err
	}
	scope := strings.Trim(strings.ReplaceAll(call.Selector.Scope, "\\", "/"), "/")
	if scope == "" {
		return res, "整个工作区", nil
	}
	var out []string
	for _, f := range res.Files {
		if f == scope || strings.HasPrefix(f, scope+"/") {
			out = append(out, f)
		}
	}
	return workspace.ListResult{Files: out, Skipped: res.Skipped}, "目录 " + scope, nil
}

// clipLine 把命中行压到可读宽度：去掉首尾空白（缩进对"找到没找到"没有信息量），超宽截断并标注。
func clipLine(line string) string {
	line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
	if r := []rune(line); len(r) > findLineWidth {
		return string(r[:findLineWidth]) + "…"
	}
	return line
}

func findReport(literal, scope string, hits []string, total, files, big, unreadable, scanned int, skipped []string) string {
	if total == 0 {
		return withNotes(fmt.Sprintf("未找到匹配：%s（范围：%s；扫描 %d 个文件）", literal, scope, scanned), big, unreadable, skipped)
	}
	head := fmt.Sprintf("找到 %d 处匹配（%d 个文件；范围：%s）", total, files, scope)
	if len(hits) < total {
		head += fmt.Sprintf("，只显示前 %d 处", len(hits))
	}
	report := head + "：\n" + strings.Join(hits, "\n")
	return withNotes(report, big, unreadable, skipped)
}

// withNotes 附上「没检索全」的事实。有跳过就必须说：否则"未找到"会被读成"不存在"。
func withNotes(report string, big, unreadable int, skipped []string) string {
	var notes []string
	if len(skipped) > 0 {
		notes = append(notes, "未枚举："+strings.Join(skipped, "、")+"，带 include 可放行")
	}
	if big > 0 {
		notes = append(notes, fmt.Sprintf("跳过 %d 个超过 %d KB 的文件", big, findMaxFileBytes>>10))
	}
	if unreadable > 0 {
		notes = append(notes, fmt.Sprintf("%d 个文件读取失败未检索", unreadable))
	}
	if len(notes) == 0 {
		return report
	}
	return report + "\n（" + strings.Join(notes, "；") + "）"
}
