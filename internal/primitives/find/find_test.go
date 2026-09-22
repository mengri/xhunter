package find

import (
	"context"
	"strings"
	"testing"

	"xhunter/harness"
)

// find 一期只声明不实现：调用必须得到**明确的结构化错误**，而不是一个不完整的结果。
// 工具保持可见是有意的——从工具面摘掉它，模型既不知道少了什么，也无从调整策略。

func TestPlan_ReportsNotImplemented(t *testing.T) {
	// 事实一个都不需要：本阶段它不碰工作区。
	in := harness.PlanInput{
		Call:  harness.Call{ID: "c1", Primitive: Name, Selector: harness.Selector{Literal: "TODO"}},
		Route: harness.Route{Path: harness.PathText, Reason: "内容寻址选择器"},
	}

	got, err := impl{}.Plan(context.Background(), in)
	if err != nil {
		t.Fatalf("未实现不是执行失败，而是可解释的结果：%v", err)
	}
	if got.Result.Err == nil || got.Result.Err.Kind != "not_implemented" {
		t.Fatalf("应返回 not_implemented：%+v", got.Result)
	}
	// 不可重试：原样重试还是同一个结果，模型应当换个做法或绕开它。
	if got.Result.Err.Retryable {
		t.Error("尚未实现不该标为可重试")
	}
	if !strings.Contains(got.Result.Err.Message, "find") {
		t.Errorf("错误信息应指明是哪个原语：%q", got.Result.Err.Message)
	}
	if len(got.Edits) != 0 {
		t.Errorf("不得产出任何编辑：%+v", got.Edits)
	}
}

func TestTool_Shape(t *testing.T) {
	tool := Tool()
	if tool.Name != Name || tool.Decl.Name != string(Name) {
		t.Errorf("名字与声明必须一致：%+v", tool)
	}
	// 检索内容走文本、定位符号走符号路径：由选择器表达式决定。
	if tool.Address != harness.AddressedBySelector {
		t.Errorf("寻址性质 = %q，期望由选择器决定", tool.Address)
	}
	// 参数是"内容检索 或 符号定位"，两者给其一——required 因此落在 anyOf 的每个分支里。
	schema := string(tool.Decl.Schema)
	if !strings.Contains(schema, `"anyOf"`) {
		t.Errorf("两种用法应表达为 anyOf：%s", schema)
	}
	for _, want := range []string{`"literal"`, `"symbol"`, `"scope"`, `"path"`} {
		if !strings.Contains(schema, want) {
			t.Errorf("schema 缺少参数 %s：%s", want, schema)
		}
	}
}
