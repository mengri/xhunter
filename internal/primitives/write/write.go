// Package write 实现 write 原语：新建文件。
//
// 已有文件不给重写：整文件重写会顺手抹掉没有注意到的内容，而"精确替换"要求先找到
// 目标——这个约束本身就是一种保护。所以撞上已有文件时返回的是"改用 edit"，
// 而不是一个更宽松的写入模式。
package write

import (
	"context"

	"xhunter/harness"
)

// Name 是本原语对模型可见的名字。
const Name = harness.PrimitiveName("write")

// Tool 给出本原语的完整描述。新建没有符号语义：指名要写的路径就是全部定位信息。
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
	Description: "新建文件。目标已存在时会被拒绝，改用 edit 修改。",
	Schema: harness.ObjectSchema(`{
			"path": {"type": "string", "description": "工作区内相对路径"},
			"content": {"type": "string", "description": "新文件的完整内容"}
		}`, "path", "content"),
}

type impl struct{}

func (impl) Plan(_ context.Context, in harness.PlanInput) (harness.Plan, error) {
	info, err := in.Facts.Workspace().Stat(in.Call.Target)
	if err != nil {
		return harness.Plan{}, err
	}
	if info.Exists {
		return harness.Plan{Result: harness.Result{Err: &harness.ToolError{
			Kind:      "file_exists",
			Message:   "目标已存在，请改用精确替换：" + in.Call.Target,
			Retryable: true,
		}}}, nil
	}
	// 新建表达为"往空文件的开头插入"。
	return harness.Plan{Edits: []harness.FileEdit{{
		File:       in.Call.Target,
		ByteRange:  harness.ByteRange{Start: 0, End: 0},
		NewContent: in.Call.Content,
	}}}, nil
}
