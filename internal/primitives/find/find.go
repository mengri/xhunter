// Package find 实现 find 原语：在工作区里查找内容或符号。
//
// 一期只声明不实现：内容检索与符号检索都要等检索路径定型，先返回明确的结构化错误，
// 而不是悄悄给出一个不完整的结果。工具保持可见是有意的——直接从工具面摘掉，
// 模型既不知道少了什么，也无从调整策略。
package find

import (
	"context"

	"xhunter/harness"
)

// Name 是本原语对模型可见的名字。
const Name = harness.PrimitiveName("find")

// Tool 给出本原语的完整描述。寻址由选择器决定：检索内容走文本，定位符号走符号路径。
func Tool() harness.Tool {
	return harness.Tool{
		Name:    Name,
		Decl:    decl,
		Impl:    impl{},
		Address: harness.AddressedBySelector,
	}
}

var decl = harness.ToolDecl{
	Name:        string(Name),
	Description: "检索内容：在文件或目录范围内查找片段。",
	Schema: harness.ObjectSchemaAnyOf(`{
			"literal": {"type": "string", "description": "要检索的内容片段"},
			"symbol": {"type": "string", "description": "按名称定位符号定义"},
			"path": {"type": "string", "description": "限定到某个文件"},
			"scope": {"type": "string", "description": "限定范围：目录或包"}
		}`, []string{"literal"}, []string{"symbol"}),
}

type impl struct{}

func (impl) Plan(context.Context, harness.PlanInput) (harness.Plan, error) {
	return harness.Plan{Result: harness.Result{Err: &harness.ToolError{
		Kind:      "not_implemented",
		Message:   "本阶段尚未实现该原语：find",
		Retryable: false,
	}}}, nil
}
