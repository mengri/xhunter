package rename

import (
	"context"
	"strings"
	"testing"

	"xhunter/harness"
)

// rename 一期只声明不实现；而它的**寻址性质**本身就是一条必须钉住的语义：
// 仅符号路径、没有可用的降级形态——环境不支持时返回结构化错误，不悄悄改走文本路径
// （那会产出一个改了声明、没改调用点的半完成重命名）。

func TestPlan_ReportsNotImplemented(t *testing.T) {
	in := harness.PlanInput{
		Call: harness.Call{
			ID: "c1", Primitive: Name,
			Selector: harness.Selector{Symbol: "pkg.Fn"},
			NewName:  "Fn2",
		},
		Route: harness.Route{Path: harness.PathSymbol, Reason: "该原语仅支持符号路径"},
	}

	got, err := impl{}.Plan(context.Background(), in)
	if err != nil {
		t.Fatalf("未实现不是执行失败：%v", err)
	}
	if got.Result.Err == nil || got.Result.Err.Kind != "not_implemented" {
		t.Fatalf("应返回 not_implemented：%+v", got.Result)
	}
	if got.Result.Err.Retryable {
		t.Error("尚未实现不该标为可重试")
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
	// 这条是语义断言，不只是元数据：仅符号寻址 = 环境不支持时没有降级形态。
	if tool.Address != harness.AddressedAsSymbol {
		t.Errorf("寻址性质 = %q，重命名必须仅走符号路径（无降级形态）", tool.Address)
	}
	schema := string(tool.Decl.Schema)
	for _, want := range []string{`"symbol"`, `"new_name"`, `"path"`} {
		if !strings.Contains(schema, want) {
			t.Errorf("schema 缺少参数 %s：%s", want, schema)
		}
	}
	if !strings.Contains(schema, `"required":["symbol","new_name"]`) {
		t.Errorf("目标符号与新名称都是必填：%s", schema)
	}
}
