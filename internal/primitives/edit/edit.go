// Package edit 实现 edit 原语：修改文件。
//
// 两种定位方式：内容寻址（写字面量片段）与符号寻址（写限定名，交给扩展定位区间）。
//
// 内容寻址的匹配语义刻意不做猜测：找不到就报找不到，找到多处就报多处。取第一处会
// 静默改错位置，全部替换会静默放大影响面——两者都比报错更糟，因为模型无从察觉，
// 而报错能让它补足上下文重新定位。
package edit

import (
	"context"
	"fmt"
	"strings"

	"xhunter/harness"
)

// Name 是本原语对模型可见的名字。
const Name = harness.PrimitiveName("edit")

// Tool 给出本原语的完整描述。寻址由选择器表达式决定：字面量走文本，限定名走符号。
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
	Description: "修改文件：按内容精确匹配后替换。匹配不到或匹配到多处都显式报错，不做猜测。",
	Schema: harness.ObjectSchema(`{
			"path": {"type": "string", "description": "工作区内相对路径"},
			"literal": {"type": "string", "description": "内容寻址：待替换的字面量片段"},
			"symbol": {"type": "string", "description": "符号寻址：限定名"},
			"in_symbol": {"type": "string", "description": "符号内相对定位"},
			"content": {"type": "string", "description": "替换成的新内容"}
		}`, "path", "content"),
}

type impl struct{}

func (impl) Plan(_ context.Context, in harness.PlanInput) (harness.Plan, error) {
	if in.Route.Path == harness.PathSymbol {
		// 符号路径：区间与内容都由扩展给出，这里只把它转交给唯一的写入入口。
		return harness.Plan{Edits: []harness.FileEdit{{
			File:       in.Prepared.File,
			ByteRange:  in.Prepared.ByteRange,
			NewContent: in.Call.Content,
		}}}, nil
	}

	literal := in.Call.Selector.Literal
	if literal == "" {
		return harness.Plan{}, &harness.ToolError{
			Kind: "bad_selector", Message: "内容寻址需要给出待替换的字面量",
		}
	}
	fc, err := in.Facts.Workspace().Read(in.Call.Target, harness.LineRange{})
	if err != nil {
		return harness.Plan{}, err
	}

	switch n := strings.Count(fc.Raw, literal); {
	case n == 0:
		return harness.Plan{Result: harness.Result{Err: &harness.ToolError{
			Kind: "not_found", Message: "未找到匹配内容：" + in.Call.Target, Retryable: true}}}, nil
	case n > 1:
		return harness.Plan{Result: harness.Result{Err: &harness.ToolError{
			Kind:      "ambiguous",
			Message:   fmt.Sprintf("匹配到 %d 处，请给更长的上下文以唯一定位", n),
			Retryable: true,
		}}}, nil
	}

	idx := strings.Index(fc.Raw, literal)
	return harness.Plan{Edits: []harness.FileEdit{{
		File:       in.Call.Target,
		ByteRange:  harness.ByteRange{Start: idx, End: idx + len(literal)},
		NewContent: in.Call.Content,
	}}}, nil
}
