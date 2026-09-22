// Package glob 实现 glob 原语：按文件名模式匹配工作区内的文件。
//
// 它不读内容、不认识符号，纯粹是"工作区里有哪些文件"的枚举入口。
package glob

import (
	"context"
	"fmt"
	"strings"

	"xhunter/harness"
)

// Name 是本原语对模型可见的名字。
const Name = harness.PrimitiveName("glob")

// Tool 给出本原语的完整描述。枚举没有符号语义。
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
	Description: "按文件名模式匹配工作区内的文件。",
	Schema: harness.ObjectSchema(`{
			"scope": {"type": "string", "description": "文件名模式，如 *.go"}
		}`, "scope"),
}

type impl struct{}

func (impl) Plan(_ context.Context, in harness.PlanInput) (harness.Plan, error) {
	pattern := in.Call.Selector.Scope
	if pattern == "" {
		pattern = in.Call.Target
	}
	files, err := in.Facts.Workspace().List(pattern)
	if err != nil {
		return harness.Plan{}, err
	}
	return harness.Plan{Result: harness.Result{
		Summary: fmt.Sprintf("%d 个匹配：%s", len(files), strings.Join(files, ", ")),
	}}, nil
}
