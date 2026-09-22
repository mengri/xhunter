package basic

import (
	"context"

	"xhunter/hunt"
	"xhunter/llm"
	"xhunter/workspace"
)

// Find 是 find 原语对模型可见的名字（内容检索）。
const Find = hunt.PrimitiveName("find")

// FindTool 构造 find 原语。内容检索在一期只声明不实现：检索路径待定型。
func FindTool(ws workspace.Workspace) hunt.Primitive {
	return findPrim{}
}

type findPrim struct{}

func (findPrim) Decl() llm.ToolDecl {
	return llm.ToolDecl{
		Name:        string(Find),
		Description: "检索内容：在文件或目录范围内查找片段。",
		Schema: llm.ObjectSchemaAnyOf(`{
			"literal": {"type": "string", "description": "要检索的内容片段"},
			"path": {"type": "string", "description": "限定到某个文件"},
			"scope": {"type": "string", "description": "限定范围：目录或包"}
		}`, []string{"literal"}),
	}
}

func (findPrim) Execute(context.Context, hunt.Call, hunt.Facts) (hunt.Result, []workspace.FileEdit, error) {
	return hunt.Result{Err: &llm.Fault{
		Kind:      "not_implemented",
		Message:   "本阶段尚未实现该原语：find",
		Retryable: false,
	}}, nil, nil
}
