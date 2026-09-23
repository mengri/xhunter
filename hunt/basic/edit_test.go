package basic

import (
	"context"
	"errors"
	"strings"
	"testing"

	"xhunter/hunt"
	"xhunter/llm"
	"xhunter/workspace"
)

// editCall 造一个内容寻址的调用：文件已写好，字面量取自它的内容。
func editCall(t *testing.T, file, literal string) (hunt.Call, workspace.Storage) {
	t.Helper()
	st := newStore(t)
	seed(t, st, "a.go", file)
	return hunt.Call{
		ID: "c1", Primitive: Edit, Target: "a.go",
		Selector: hunt.Selector{Literal: literal},
		Content:  "NEW",
	}, st
}

// 唯一匹配时给出精确区间。用重复模板做夹具：精确命中的前提是"只有一处"。
func TestEdit_UniqueMatchProducesExactRange(t *testing.T) {
	file := "func a() {}\nfunc b() {}\nfunc c() {}\n"
	call, st := editCall(t, file, "func b() {}")

	res, edits, err := EditTool(st).Execute(context.Background(), call, newFacts())
	if err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	if res.Err != nil {
		t.Fatalf("唯一匹配不该带结构化错误：%+v", res.Err)
	}
	if len(edits) != 1 {
		t.Fatalf("编辑数 = %d，期望 1", len(edits))
	}
	e := edits[0]
	if e.NewContent != "NEW" || e.File != "a.go" {
		t.Errorf("编辑 = %+v", e)
	}
	// 区间必须正好覆盖"func b() {}"，且原文与该区间一致——
	// 改错位置是这类编辑最坏的失败（模型察觉不到）。
	want := strings.Index(file, "func b() {}")
	if e.ByteRange != (workspace.ByteRange{Start: want, End: want + len("func b() {}")}) {
		t.Errorf("区间 = %+v，期望 [%d,%d)", e.ByteRange, want, want+len("func b() {}"))
	}
	if file[e.ByteRange.Start:e.ByteRange.End] != "func b() {}" {
		t.Error("区间取值与目标字面量不一致")
	}
}

// 找不到就报找不到：不做模糊匹配、不做"最接近的一处"。
func TestEdit_NoMatchReportsNotFound(t *testing.T) {
	call, st := editCall(t, "func a() {}\n", "func z() {}")

	res, edits, err := EditTool(st).Execute(context.Background(), call, newFacts())
	if err != nil {
		t.Fatalf("未匹配不是执行失败：%v", err)
	}
	if res.Err == nil || res.Err.Kind != "not_found" {
		t.Fatalf("应返回 not_found：%+v", res)
	}
	if !res.Err.Retryable {
		t.Error("补足上下文后可以重试")
	}
	if len(edits) != 0 {
		t.Errorf("未匹配不得产出编辑：%+v", edits)
	}
}

// 匹配到多处也报错，并给出数量：取第一处会静默改错位置，全部替换会静默放大影响面。
func TestEdit_AmbiguousReportsCount(t *testing.T) {
	call, st := editCall(t, "x\nx\nx\n", "x")

	res, edits, err := EditTool(st).Execute(context.Background(), call, newFacts())
	if err != nil {
		t.Fatalf("多处匹配不是执行失败：%v", err)
	}
	if res.Err == nil || res.Err.Kind != "ambiguous" {
		t.Fatalf("应返回 ambiguous：%+v", res)
	}
	if !strings.Contains(res.Err.Message, "3") {
		t.Errorf("错误信息应报出匹配数量：%q", res.Err.Message)
	}
	if len(edits) != 0 {
		t.Errorf("歧义不得产出编辑：%+v", edits)
	}
}

// 内容寻址没给字面量：调用形状不成立，属执行错误而不是"没找到"。
func TestEdit_MissingLiteralIsError(t *testing.T) {
	call, st := editCall(t, "x\n", "")

	_, _, err := EditTool(st).Execute(context.Background(), call, newFacts())
	if err == nil {
		t.Fatal("缺字面量必须报错")
	}
	var fault *llm.Fault
	if !errors.As(err, &fault) || fault.Kind != "bad_selector" {
		t.Errorf("错误类型 = %v，期望 bad_selector", err)
	}
}

// 声明形状：`literal` **必须在 required 里**——实现缺它就返回 bad_selector，schema 漏了它
// 模型照 schema 省略就会拿到一个本可避免的错误（产品设计 §6 曾把这条列为"已知不一致"）。
func TestEdit_DeclShape(t *testing.T) {
	d := EditTool(nil).Decl()
	if d.Name != string(Edit) {
		t.Errorf("声明名 = %q，期望 %q", d.Name, Edit)
	}
	s := decodeSchema(t, d)
	if got := propNames(s.Properties); len(got) != 3 || got[0] != "content" || got[1] != "literal" || got[2] != "path" {
		t.Errorf("参数面 = %v，期望 [content literal path]", got)
	}
	required := strings.Join(s.Required, ",")
	for _, want := range []string{"path", "literal", "content"} {
		if !strings.Contains(required, want) {
			t.Errorf("required = %v，应含 %s（实现缺它即报错，schema 不能漏）", s.Required, want)
		}
	}
	if s.AdditionalProperties == nil || *s.AdditionalProperties {
		t.Errorf("必须禁止额外字段：%s", d.Schema)
	}
}
