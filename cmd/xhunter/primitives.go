package main

import (
	"xhunter/ext"
	"xhunter/hunt"
	"xhunter/hunt/basic"
	"xhunter/hunt/gate"
	"xhunter/workspace"
)

// defaultTools 组装本产品的原语清单，按模型看到的顺序排列。
//
// **这条清单就是业务边界**。harness 只提供循环，不认识任何原语；「有哪些原语、叫
// 什么名字、以什么顺序暴露」全在这里定。工作区是原语的构造参数（读文件需要它），
// 因此这里是工厂、由 Session 在工作区就绪后调用。
//
// 它同时承担**期次口径**：哪一期把哪些原语接进来，也由这条清单表达。工具面的口径是
// **同一构建内恒定、随能力接入而扩展**——环境能力不决定注册（扩展不可用、语言未注册
// 只改变「调用时会发生什么」：退回文本行为 / 返回结构化错误），尚未实现、且不在本次
// 构建范围内的原语则不注册（模型看不到它），实现落地后再注册。
//
// 一期工具面 = 6 + 1：read / write / edit / find / glob / check，加执行体殿后追加的
// checkpoint。三个符号原语（symbol_read / symbol_edit / symbol_rename）本期**不在此处
// 构造、也不交给模型**——模型看不到这三个工具，而不是「看得到但调用失败」；它们随
// MS-8 接入时再注册。符号原语的**构造器与实现都保留在 hunt/symbolic**（本包只是不再
// 调用它们），MS-8 恢复注册时直接在这里补回即可。
//
// 因此装配层现在传进来的 ext.Unimplemented{}（panic 哨兵）在一期内**不可达**：它的
// 用途就是 MS-8 的接入位，届时符号原语复活、才真正走到扩展宿主。ext.ExtHost 形参也
// 因此保留——它是给 MS-8 的构造参数，一期虽未用到、形参不动。
//
// check 的工具名一直在工具面上：一期它注册但返回 not_implemented（那是刻意的不对称——
// 门禁是检查点的触发源）。现在它接上真正的执行器：跑什么、按什么算通过，都来自清单。
//
// 执行器由装配层传进来、与执行体的收尾补跑**共用同一份**：同一条门禁在两处跑出不同结论
// 是不可接受的。
func defaultTools(ws workspace.Workspace, ex ext.ExtHost, runner hunt.GateRunner) []hunt.Primitive {
	return []hunt.Primitive{
		basic.ReadTool(ws),
		basic.WriteTool(ws),
		basic.EditTool(ws),
		basic.FindTool(ws),
		basic.GlobTool(ws),
		gate.CheckTool(runner),
	}
}
