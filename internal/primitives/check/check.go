// Package check 实现 check 原语：运行一个具名的质量门禁条目。
//
// 它是自检手段，也是检查点的触发源（门禁通过 → 检查点）。**不是任意命令执行**：
// 模型只给门禁名，命令、参数与判据都在引擎侧——所以模型无法通过改命令来让自己
// 更容易通过，也不可能借它把 shell 请回来。
//
// 一期只声明不实现（门禁清单的加载与执行待补）。
package check

import (
	"context"

	"xhunter/harness"
)

// Name 是本原语对模型可见的名字。
const Name = harness.PrimitiveName("check")

// Tool 给出本原语的完整描述。门禁是具名条目，名字之外没有定位信息，因此只有文本路径。
func Tool() harness.Tool {
	return harness.Tool{
		Name:    Name,
		Decl:    decl,
		Impl:    impl{},
		Address: harness.AddressedAsText,
	}
}

var decl = harness.ToolDecl{
	Name:        string(Name),
	Description: "运行一个具名校验条目（清单由任务侧提供），并把结果作为自检证据。",
	Schema: harness.ObjectSchema(`{
			"name": {"type": "string", "description": "清单里的门禁条目名"}
		}`, "name"),
}

type impl struct{}

func (impl) Plan(context.Context, harness.PlanInput) (harness.Plan, error) {
	return harness.Plan{Result: harness.Result{Err: &harness.ToolError{
		Kind:      "not_implemented",
		Message:   "本阶段尚未实现该原语：check",
		Retryable: false,
	}}}, nil
}
