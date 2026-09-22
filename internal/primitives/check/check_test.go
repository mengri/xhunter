package check

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"xhunter/harness"
)

// check 一期只声明不实现。它将来是"自检 + 检查点触发源"，**不是任意命令执行**：
// 模型只给门禁名，命令、参数与判据都在引擎侧。这条边界在声明上就要看得出来。

func TestPlan_ReportsNotImplemented(t *testing.T) {
	in := harness.PlanInput{
		Call:  harness.Call{ID: "c1", Primitive: Name, Gate: "unit"},
		Route: harness.Route{Path: harness.PathText, Reason: "该原语只有文本路径"},
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
		t.Errorf("校验本身不产生编辑：%+v", got.Edits)
	}
}

func TestTool_Shape(t *testing.T) {
	tool := Tool()
	if tool.Name != Name || tool.Decl.Name != string(Name) {
		t.Errorf("名字与声明必须一致：%+v", tool)
	}
	// 门禁是具名条目，名字之外没有定位信息，因此只有文本路径。
	if tool.Address != harness.AddressedAsText {
		t.Errorf("寻址性质 = %q，期望只有文本路径", tool.Address)
	}
	schema := string(tool.Decl.Schema)
	// 唯一的参数是条目名：参数面里**没有任何**接受命令、参数或脚本的槽位——
	// 这是"不是任意命令执行"在模型可见面上的体现。按属性名精确断言，
	// 不用子串匹配（"description" 里就含 "script"）。
	var parsed struct {
		Properties           map[string]any `json:"properties"`
		Required             []string       `json:"required"`
		AdditionalProperties *bool          `json:"additionalProperties"`
	}
	if err := json.Unmarshal(tool.Decl.Schema, &parsed); err != nil {
		t.Fatalf("schema 不是合法 JSON：%v（%s）", err, schema)
	}
	if len(parsed.Properties) != 1 {
		t.Errorf("参数面只应有门禁名，实际 %v", keysOf(parsed.Properties))
	}
	for name := range parsed.Properties {
		if name != "name" {
			t.Errorf("参数面不得出现 %q（校验不接受任意命令）：%s", name, schema)
		}
	}
	if !reflect.DeepEqual(parsed.Required, []string{"name"}) {
		t.Errorf("required = %v，期望只有 name", parsed.Required)
	}
	if parsed.AdditionalProperties == nil || *parsed.AdditionalProperties {
		t.Errorf("必须禁止额外字段：%s", schema)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
