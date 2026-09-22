package main

import (
	"xhunter/ext"
	"xhunter/hunt"
	"xhunter/hunt/basic"
	"xhunter/hunt/gate"
	"xhunter/hunt/symbolic"
	"xhunter/workspace"
)

// defaultTools 组装本产品的原语清单，按模型看到的顺序排列。
//
// **这条清单就是业务边界**。harness 只提供循环，不认识任何原语；「有哪些原语、叫
// 什么名字、以什么顺序暴露」全在这里定。工作区是原语的构造参数（读文件需要它），
// 因此这里是工厂、由 Session 在工作区就绪后调用。
func defaultTools(ws workspace.Workspace, ex ext.ExtHost) []hunt.Primitive {
	return []hunt.Primitive{
		basic.ReadTool(ws),
		basic.WriteTool(ws),
		basic.EditTool(ws),
		basic.FindTool(ws),
		basic.GlobTool(ws),
		symbolic.SymbolReadTool(ws, ex),
		symbolic.SymbolEditTool(ws, ex),
		symbolic.SymbolRenameTool(ws, ex),
		gate.CheckTool(),
	}
}
