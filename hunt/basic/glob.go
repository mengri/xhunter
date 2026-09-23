package basic

import (
	"context"
	"fmt"
	"strings"

	"xhunter/hunt"
	"xhunter/llm"
	"xhunter/workspace"
)

// Glob 是 glob 原语对模型可见的名字。
const Glob = hunt.PrimitiveName("glob")

// GlobTool 构造 glob 原语。枚举没有符号语义，纯粹是「工作区里有哪些文件」的入口。
func GlobTool(ws workspace.Workspace) hunt.Primitive {
	return globPrim{ws: ws}
}

type globPrim struct{ ws workspace.Workspace }

// Writes 报告本原语不写盘：读类原语，策略因此不必为它查路径边界。
func (globPrim) Writes() bool { return false }

func (globPrim) Decl() llm.ToolDecl {
	return llm.ToolDecl{
		Name:        string(Glob),
		Description: "按模式列出工作区内的文件。不含 / 的模式按文件名匹配（任意深度）；含 / 的模式按路径匹配，整段 ** 表示零到多层目录。",
		Schema: llm.ObjectSchema(`{
			"scope": {"type": "string", "description": "模式，如 *.go、internal/*.go、internal/**/*.go"}
		}`, "scope"),
	}
}

func (p globPrim) Execute(_ context.Context, call hunt.Call, _ hunt.Facts) (hunt.Result, []workspace.FileEdit, error) {
	pattern := call.Selector.Scope
	if pattern == "" {
		pattern = call.Target
	}
	files, err := p.ws.List(pattern)
	if err != nil {
		return hunt.Result{}, nil, err
	}
	return hunt.Result{
		Summary: fmt.Sprintf("%d 个匹配：%s", len(files), strings.Join(files, ", ")),
	}, nil, nil
}
