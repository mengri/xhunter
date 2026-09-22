package basic

import (
	"context"
	"strings"
	"testing"

	"xhunter/hunt"
)

// find 一期只声明不实现：调用必须得到**明确的结构化错误**，而不是一个不完整的结果。
// 工具保持可见是有意的——从工具面摘掉它，模型既不知道少了什么，也无从调整策略。
func TestFind_ReportsNotImplemented(t *testing.T) {
	res, edits, err := FindTool(nil).Execute(context.Background(),
		hunt.Call{ID: "c1", Primitive: Find, Selector: hunt.Selector{Literal: "TODO"}}, newFacts())
	if err != nil {
		t.Fatalf("未实现不是执行失败，而是可解释的结果：%v", err)
	}
	if res.Err == nil || res.Err.Kind != "not_implemented" {
		t.Fatalf("应返回 not_implemented：%+v", res)
	}
	// 不可重试：原样重试还是同一个结果，模型应当换个做法或绕开它。
	if res.Err.Retryable {
		t.Error("尚未实现不该标为可重试")
	}
	if !strings.Contains(res.Err.Message, "find") {
		t.Errorf("错误信息应指明是哪个原语：%q", res.Err.Message)
	}
	if len(edits) != 0 {
		t.Errorf("不得产出任何编辑：%+v", edits)
	}
}

// 声明形状：参数是"检索片段 + 可选范围"，必填落在 anyOf 的分支里。
func TestFind_DeclShape(t *testing.T) {
	d := FindTool(nil).Decl()
	if d.Name != string(Find) {
		t.Errorf("声明名 = %q，期望 %q", d.Name, Find)
	}
	s := decodeSchema(t, d)
	if got := propNames(s.Properties); len(got) != 3 || got[0] != "literal" || got[1] != "path" || got[2] != "scope" {
		t.Errorf("参数面 = %v，期望 [literal path scope]", got)
	}
	schema := string(d.Schema)
	if !strings.Contains(schema, `"anyOf"`) {
		t.Errorf("检索片段应表达为 anyOf 分支：%s", schema)
	}
	if !strings.Contains(schema, `"literal"`) {
		t.Errorf("schema 必须声明检索片段：%s", schema)
	}
}
