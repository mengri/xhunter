package basic

import (
	"context"
	"fmt"
	"strings"

	"xhunter/hunt"
	"xhunter/llm"
	"xhunter/workspace"
)

// Edit 是 edit 原语对模型可见的名字。
const Edit = hunt.PrimitiveName("edit")

// EditTool 构造 edit 原语。内容寻址：写字面量片段，匹配到后替换。
func EditTool(ws workspace.Workspace) hunt.Primitive {
	return editPrim{ws: ws}
}

type editPrim struct{ ws workspace.Workspace }

// Writes 报告本原语会写盘：写类原语，策略据此做路径边界裁决。
func (editPrim) Writes() bool { return true }

func (editPrim) Decl() llm.ToolDecl {
	return llm.ToolDecl{
		Name:        string(Edit),
		Description: "修改文件：按内容精确匹配后替换。匹配不到或匹配到多处都显式报错，不做猜测。",
		Schema: llm.ObjectSchema(`{
			"path": {"type": "string", "description": "工作区内相对路径"},
			"literal": {"type": "string", "description": "内容寻址：待替换的字面量片段"},
			"content": {"type": "string", "description": "替换成的新内容"}
		}`, "path", "literal", "content"),
	}
}

func (p editPrim) Execute(_ context.Context, call hunt.Call, _ hunt.Facts) (hunt.Result, []workspace.FileEdit, error) {
	literal := call.Selector.Literal
	if literal == "" {
		return hunt.Result{}, nil, &llm.Fault{
			Kind: "bad_selector", Message: "内容寻址需要给出待替换的字面量",
		}
	}
	fc, err := p.ws.Read(call.Target, workspace.LineRange{})
	if err != nil {
		return hunt.Result{}, nil, err
	}

	switch n := strings.Count(fc.Raw, literal); {
	case n == 0:
		return hunt.Result{Err: &llm.Fault{
			Kind: "not_found", Message: "未找到匹配内容：" + call.Target, Retryable: true,
		}}, nil, nil
	case n > 1:
		return hunt.Result{Err: &llm.Fault{
			Kind:      "ambiguous",
			Message:   fmt.Sprintf("匹配到 %d 处，请给更长的上下文以唯一定位", n),
			Retryable: true,
		}}, nil, nil
	}

	idx := strings.Index(fc.Raw, literal)
	return hunt.Result{}, []workspace.FileEdit{{
		File:       call.Target,
		ByteRange:  workspace.ByteRange{Start: idx, End: idx + len(literal)},
		NewContent: call.Content,
	}}, nil
}
