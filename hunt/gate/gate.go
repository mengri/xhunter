// Package gate 是门禁领域：提供 check 原语与自动检查点钩子。
//
// 门禁不是一个普通原语，而是一个跨生命周期的领域：
//   - check 原语——模型显式运行一个具名门禁条目（工具面）；
//   - 自动检查点钩子——门禁通过后，在每轮结束的钩子里自动落检查点；
//   - 交付门禁钩子——收尾阶段补跑尚未通过的必需门禁。
//
// 命令、参数与判据都在业务侧，模型只给门禁名——因此无法通过改命令让自己更容易通过，
// 也不可能借它把 shell 请回来。
//
// 一期只声明不实现：门禁清单的加载与执行待补。
package gate

import (
	"context"

	"xhunter/hunt"
	"xhunter/llm"
	"xhunter/workspace"
)

// Check 是 check 原语对模型可见的名字。
const Check = hunt.PrimitiveName("check")

// CheckTool 构造 check 原语。门禁是具名条目，名字之外没有定位信息。
func CheckTool() hunt.Primitive {
	return checkPrim{}
}

type checkPrim struct{}

// Writes 报告 check 不写工作盘：它跑什么由任务侧清单决定、不经模型之手，因此不属于
// 「模型写工作区」这件事，策略不为它做路径裁决。
func (checkPrim) Writes() bool { return false }

func (checkPrim) Decl() llm.ToolDecl {
	return llm.ToolDecl{
		Name:        string(Check),
		Description: "运行一个具名校验条目（清单由任务侧提供），并把结果作为自检证据。",
		Schema: llm.ObjectSchema(`{
			"name": {"type": "string", "description": "清单里的门禁条目名"}
		}`, "name"),
	}
}

func (checkPrim) Execute(context.Context, hunt.Call, hunt.Facts) (hunt.Result, []workspace.FileEdit, error) {
	return hunt.Result{Err: &llm.Fault{
		Kind:      "not_implemented",
		Message:   "本阶段尚未实现该原语：check",
		Retryable: false,
	}}, nil, nil
}
