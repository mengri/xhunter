package basic

import (
	"context"

	"xhunter/hunt"
	"xhunter/llm"
	"xhunter/workspace"
)

// Write 是 write 原语对模型可见的名字。
const Write = hunt.PrimitiveName("write")

// WriteTool 构造 write 原语。新建没有符号语义：指名要写的路径就是全部定位信息。
func WriteTool(ws workspace.Workspace) hunt.Primitive {
	return writePrim{ws: ws}
}

type writePrim struct{ ws workspace.Workspace }

func (writePrim) Decl() llm.ToolDecl {
	return llm.ToolDecl{
		Name:        string(Write),
		Description: "新建文件。目标已存在时会被拒绝，改用 edit 修改。",
		Schema: llm.ObjectSchema(`{
			"path": {"type": "string", "description": "工作区内相对路径"},
			"content": {"type": "string", "description": "新文件的完整内容"}
		}`, "path", "content"),
	}
}

func (p writePrim) Execute(_ context.Context, call hunt.Call, _ hunt.Facts) (hunt.Result, []workspace.FileEdit, error) {
	info, err := p.ws.Stat(call.Target)
	if err != nil {
		return hunt.Result{}, nil, err
	}
	if info.Exists {
		return hunt.Result{Err: &llm.Fault{
			Kind:      "file_exists",
			Message:   "目标已存在，请改用精确替换：" + call.Target,
			Retryable: true,
		}}, nil, nil
	}
	// 新建表达为「往空文件的开头插入」。
	return hunt.Result{}, []workspace.FileEdit{{
		File:       call.Target,
		ByteRange:  workspace.ByteRange{Start: 0, End: 0},
		NewContent: call.Content,
	}}, nil
}
