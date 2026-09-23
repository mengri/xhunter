// Package symbolic 提供符号读写原语：按限定名定位代码符号后读写。
//
// 它依赖基本读写（hunt/basic）复用区间读写，依赖符号解析（xhunter/ext）做定位。
// 定位 → 字节区间 → 复用基本读写，是这里唯一的新增职责；「符号化」与「降级」因此
// 收在本包内，harness 与 basic 都不感知符号。
//
// 一期只声明不实现：符号定位依赖扩展的引用解析，尚未接入。
package symbolic

import (
	"context"

	"xhunter/ext"
	"xhunter/hunt"
	"xhunter/llm"
	"xhunter/workspace"
)

// 符号原语对模型可见的名字。
const (
	SymbolRead   = hunt.PrimitiveName("symbol_read")
	SymbolEdit   = hunt.PrimitiveName("symbol_edit")
	SymbolRename = hunt.PrimitiveName("symbol_rename")
)

// SymbolReadTool 构造符号读原语：定位符号后读其所在区间。
func SymbolReadTool(ws workspace.Workspace, ex ext.ExtHost) hunt.Primitive {
	return symbolicPrim{name: SymbolRead, ws: ws, ex: ex, decl: llm.ToolDecl{
		Name:        string(SymbolRead),
		Description: "按限定名定位符号并读取其定义。",
		Schema: llm.ObjectSchema(`{
			"symbol": {"type": "string", "description": "符号限定名"},
			"path": {"type": "string", "description": "限定到某个文件（可选）"}
		}`, "symbol"),
	}}
}

// SymbolEditTool 构造符号编辑原语：定位符号后替换其定义区间。
func SymbolEditTool(ws workspace.Workspace, ex ext.ExtHost) hunt.Primitive {
	return symbolicPrim{name: SymbolEdit, ws: ws, ex: ex, decl: llm.ToolDecl{
		Name:        string(SymbolEdit),
		Description: "按限定名定位符号并替换其定义。",
		Schema: llm.ObjectSchema(`{
			"symbol": {"type": "string", "description": "符号限定名"},
			"content": {"type": "string", "description": "替换成的新内容"},
			"path": {"type": "string", "description": "限定到某个文件（可选）"}
		}`, "symbol", "content"),
	}}
}

// SymbolRenameTool 构造符号重命名原语：跨文件重命名，一次完成全部改动。
func SymbolRenameTool(ws workspace.Workspace, ex ext.ExtHost) hunt.Primitive {
	return symbolicPrim{name: SymbolRename, ws: ws, ex: ex, decl: llm.ToolDecl{
		Name:        string(SymbolRename),
		Description: "跨文件重命名符号，一次调用完成全部改动。当前环境不支持时返回明确错误。",
		Schema: llm.ObjectSchema(`{
			"symbol": {"type": "string", "description": "要重命名的符号限定名"},
			"new_name": {"type": "string", "description": "重命名后的名称"},
			"path": {"type": "string", "description": "限定到某个文件（可选）"}
		}`, "symbol", "new_name"),
	}}
}

type symbolicPrim struct {
	name hunt.PrimitiveName
	ws   workspace.Workspace
	ex   ext.ExtHost
	decl llm.ToolDecl
}

func (p symbolicPrim) Decl() llm.ToolDecl { return p.decl }

// Writes 报告本原语是否写盘：符号读不写，符号编辑与重命名写。
// 按名字分而不是按某次调用的参数分——写不写盘是原语的性质，不是这一次调用的性质。
func (p symbolicPrim) Writes() bool { return p.name != SymbolRead }

func (p symbolicPrim) Execute(context.Context, hunt.Call, hunt.Facts) (hunt.Result, []workspace.FileEdit, error) {
	return hunt.Result{Err: &llm.Fault{
		Kind:      "not_implemented",
		Message:   "本阶段尚未实现该原语：" + string(p.name),
		Retryable: false,
	}}, nil, nil
}
