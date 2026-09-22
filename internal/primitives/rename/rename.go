// Package rename 实现 rename 原语：跨文件重命名符号。
//
// 它是唯一"没有有意义降级形态"的原语——无法解析引用时，单文件文本替换本质上就是
// edit 而不是 rename。因此环境不支持时**不退回文本路径**，而是返回结构化错误：
// 悄悄降级会产出一个改了声明、没改调用点的半完成重命名，那比直接失败更糟。
//
// 一期只声明不实现（重命名依赖符号扩展的引用解析）。
package rename

import (
	"context"

	"xhunter/harness"
)

// Name 是本原语对模型可见的名字。
const Name = harness.PrimitiveName("rename")

// Tool 给出本原语的完整描述。仅符号寻址：没有符号路径就没有这个原语的意义。
func Tool() harness.Tool {
	return harness.Tool{
		Name:    Name,
		Decl:    decl,
		Impl:    impl{},
		Address: harness.AddressedAsSymbol,
	}
}

var decl = harness.ToolDecl{
	Name:        string(Name),
	Description: "跨文件重命名符号，一次调用完成全部改动。当前环境不支持时返回明确错误。",
	Schema: harness.ObjectSchema(`{
			"symbol": {"type": "string", "description": "要重命名的符号限定名"},
			"new_name": {"type": "string", "description": "重命名后的名称"},
			"path": {"type": "string", "description": "限定到某个文件（可选）"}
		}`, "symbol", "new_name"),
}

type impl struct{}

func (impl) Plan(context.Context, harness.PlanInput) (harness.Plan, error) {
	return harness.Plan{Result: harness.Result{Err: &harness.ToolError{
		Kind:      "not_implemented",
		Message:   "本阶段尚未实现该原语：rename",
		Retryable: false,
	}}}, nil
}
