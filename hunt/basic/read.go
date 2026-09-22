// Package basic 提供基本读写原语：按路径、行范围、内容定位的读写，纯文本寻址。
//
// 符号化不是本包的事——它由符号读写领域（hunt/symbolic）提供，内部先定位到字节区间
// 再复用本包的读写能力。本包只认「路径 + 区间 + 内容」，不感知符号、不感知扩展。
package basic

import (
	"context"
	"fmt"
	"strings"

	"xhunter/hunt"
	"xhunter/llm"
	"xhunter/workspace"
)

// Read 是 read 原语对模型可见的名字。
const Read = hunt.PrimitiveName("read")

// ReadTool 构造 read 原语。工作区是构造参数——读文件必须知道读哪个目录。
func ReadTool(ws workspace.Workspace) hunt.Primitive {
	return readPrim{ws: ws}
}

type readPrim struct{ ws workspace.Workspace }

func (readPrim) Decl() llm.ToolDecl {
	return llm.ToolDecl{
		Name:        string(Read),
		Description: "读文件：按路径与行范围读取内容，越界处标注截断。",
		Schema: llm.ObjectSchema(`{
			"path": {"type": "string", "description": "工作区内相对路径"},
			"range": {"type": "object", "description": "按行读取的区间（1 起，闭区间），省略则读全文",
				"properties": {"from": {"type": "integer"}, "to": {"type": "integer"}}}
		}`, "path"),
	}
}

func (p readPrim) Execute(_ context.Context, call hunt.Call, facts hunt.Facts) (hunt.Result, []workspace.FileEdit, error) {
	fc, err := p.ws.Read(call.Target, rangeOf(call))
	if err != nil {
		return hunt.Result{}, nil, err
	}
	// 读的同时登记台账——「改前必读」的校验因此建立在「这个文件的当前内容模型是否见过」上。
	facts.Ledger().Mark(call.Target, fc.Fingerprint)
	return hunt.Result{Summary: render(fc)}, nil, nil
}

func rangeOf(call hunt.Call) workspace.LineRange {
	if call.Selector.Range != nil {
		return *call.Selector.Range
	}
	return workspace.LineRange{}
}

// readFullByteLimit 是单次全文读取进入上下文的字节预算，护栏而非能力上限。
const readFullByteLimit = 64 << 10

// render 把读取结果渲染成回灌给模型的文本，逐行带行号前缀。
func render(fc workspace.FileContent) string {
	if fc.Truncated {
		return fmt.Sprintf("%s（已按行范围截断，共 %d 行）\n%s", fc.Path, fc.TotalLines, numberLines(fc.Raw, 1))
	}
	if len(fc.Raw) <= readFullByteLimit {
		return fmt.Sprintf("%s（%d 行）\n%s", fc.Path, fc.TotalLines, numberLines(fc.Raw, 1))
	}
	cut := strings.LastIndexByte(fc.Raw[:readFullByteLimit], '\n')
	if cut <= 0 {
		cut = readFullByteLimit
	} else {
		cut++
	}
	shown := fc.Raw[:cut]
	shownLines := strings.Count(shown, "\n")
	return fmt.Sprintf("%s（已截断：显示第 1–%d 行，共 %d 行；请从第 %d 行续读）\n%s",
		fc.Path, shownLines, fc.TotalLines, shownLines+1, numberLines(shown, 1))
}

// numberLines 给每行加上右对齐的行号前缀（"    7→原文"）。
func numberLines(raw string, first int) string {
	lines := strings.Split(raw, "\n")
	if n := len(lines); n > 1 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	width := len(fmt.Sprintf("%d", first+len(lines)-1))
	var b strings.Builder
	for i, line := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		num := fmt.Sprintf("%d", first+i)
		for pad := len(num); pad < width; pad++ {
			b.WriteByte(' ')
		}
		b.WriteString(num)
		b.WriteString("→")
		b.WriteString(strings.TrimSuffix(line, "\r"))
	}
	return b.String()
}
