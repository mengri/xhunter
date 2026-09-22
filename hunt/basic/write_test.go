package basic

import (
	"context"
	"testing"

	"xhunter/hunt"
	"xhunter/workspace"
)

// 新建表达为「往空文件的开头插入」：写入语义因此收敛在一处。
func TestWrite_NewFileInsertsAtHead(t *testing.T) {
	st := newStore(t)
	res, edits, err := WriteTool(st).Execute(context.Background(),
		hunt.Call{ID: "c1", Primitive: Write, Target: "note.txt", Content: "hello"}, newFacts())
	if err != nil {
		t.Fatalf("新建不该报错：%v", err)
	}
	if res.Err != nil {
		t.Fatalf("新建不该带结构化错误：%+v", res.Err)
	}
	if len(edits) != 1 {
		t.Fatalf("编辑数 = %d，期望 1", len(edits))
	}
	e := edits[0]
	if e.File != "note.txt" || e.NewContent != "hello" {
		t.Errorf("编辑 = %+v", e)
	}
	if e.ByteRange != (workspace.ByteRange{Start: 0, End: 0}) {
		t.Errorf("区间应为 [0,0)，实际 %+v", e.ByteRange)
	}
}

// 已有文件一律拒绝，并指向正确的做法：整文件重写会顺手抹掉没注意到的内容，
// 而"精确替换"要求先找到目标——这个约束本身就是保护。
func TestWrite_ExistingFileIsRejectedWithGuidance(t *testing.T) {
	st := newStore(t)
	seed(t, st, "note.txt", "已有内容")

	res, edits, err := WriteTool(st).Execute(context.Background(),
		hunt.Call{ID: "c1", Primitive: Write, Target: "note.txt", Content: "hello"}, newFacts())
	if err != nil {
		t.Fatalf("撞上已有文件是可自愈的拒绝，不是执行失败：%v", err)
	}
	if res.Err == nil || res.Err.Kind != "file_exists" {
		t.Fatalf("应返回结构化错误：%+v", res)
	}
	if !res.Err.Retryable {
		t.Error("换成 edit 就能继续，所以是可重试的")
	}
	if len(edits) != 0 {
		t.Errorf("被拒绝时不得产出编辑计划：%+v", edits)
	}
}

// 空内容合法：建一个占位文件是正当动作，不该被拦。
func TestWrite_EmptyContentIsAllowed(t *testing.T) {
	_, edits, err := WriteTool(newStore(t)).Execute(context.Background(),
		hunt.Call{ID: "c1", Primitive: Write, Target: "placeholder", Content: ""}, newFacts())
	if err != nil {
		t.Fatalf("空内容不该报错：%v", err)
	}
	if len(edits) != 1 || edits[0].NewContent != "" {
		t.Errorf("应产出建空文件的编辑计划：%+v", edits)
	}
}

// 路径不合法由工作区拒绝：原语不自己判断路径，边界只有一处实现。
func TestWrite_InvalidPathPropagates(t *testing.T) {
	if _, _, err := WriteTool(newStore(t)).Execute(context.Background(),
		hunt.Call{ID: "c1", Primitive: Write, Target: "../escape.txt", Content: "x"}, newFacts()); err == nil {
		t.Fatal("越界路径必须被拒绝")
	}
}

// 声明形状：参数面精确等于 {path, content}，两者都必填。
func TestWrite_DeclShape(t *testing.T) {
	d := WriteTool(nil).Decl()
	if d.Name != string(Write) {
		t.Errorf("声明名 = %q，期望 %q", d.Name, Write)
	}
	s := decodeSchema(t, d)
	if got := propNames(s.Properties); len(got) != 2 || got[0] != "content" || got[1] != "path" {
		t.Errorf("参数面 = %v，期望 [content path]", got)
	}
	if len(s.Required) != 2 || s.Required[0] != "path" || s.Required[1] != "content" {
		t.Errorf("required = %v，期望 path 与 content", s.Required)
	}
	if s.AdditionalProperties == nil || *s.AdditionalProperties {
		t.Errorf("必须禁止额外字段：%s", d.Schema)
	}
}
