package gate

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"xhunter/hunt"
)

// check 一期只声明不实现。它将来是"自检 + 检查点触发源"，**不是任意命令执行**：
// 模型只给门禁名，命令、参数与判据都在业务侧。这条边界在声明上就要看得出来。

func TestCheck_ReportsNotImplemented(t *testing.T) {
	res, edits, err := CheckTool().Execute(context.Background(),
		hunt.Call{ID: "c1", Primitive: Check, Gate: "unit"}, nil)
	if err != nil {
		t.Fatalf("未实现不是执行失败：%v", err)
	}
	if res.Err == nil || res.Err.Kind != "not_implemented" {
		t.Fatalf("应返回 not_implemented：%+v", res)
	}
	if res.Err.Retryable {
		t.Error("尚未实现不该标为可重试")
	}
	if len(edits) != 0 {
		t.Errorf("校验本身不产生编辑：%+v", edits)
	}
}

// 参数面精确等于 `{name}`：里面**没有任何**接受命令、参数或脚本的槽位——
// 这是"不是任意命令执行"在模型可见面上的体现。按属性名精确断言，
// 不用子串匹配（"description" 里就含 "script"）。
func TestCheck_DeclSurfaceIsExactlyTheGateName(t *testing.T) {
	d := CheckTool().Decl()
	if d.Name != string(Check) {
		t.Errorf("声明名 = %q，期望 %q", d.Name, Check)
	}
	if d.Description == "" {
		t.Error("声明必须带说明：模型只能靠它决定什么时候用")
	}
	if !json.Valid(d.Schema) {
		t.Fatalf("参数形状不是合法 JSON：%s", d.Schema)
	}

	var parsed struct {
		Properties           map[string]any `json:"properties"`
		Required             []string       `json:"required"`
		AdditionalProperties *bool          `json:"additionalProperties"`
	}
	if err := json.Unmarshal(d.Schema, &parsed); err != nil {
		t.Fatalf("解不开参数形状：%v（%s）", err, d.Schema)
	}
	if len(parsed.Properties) != 1 {
		t.Errorf("参数面只应有门禁名，实际 %v", parsed.Properties)
	}
	for name := range parsed.Properties {
		if name != "name" {
			t.Errorf("参数面不得出现 %q（校验不接受任意命令）：%s", name, d.Schema)
		}
	}
	if !reflect.DeepEqual(parsed.Required, []string{"name"}) {
		t.Errorf("required = %v，期望只有 name", parsed.Required)
	}
	if parsed.AdditionalProperties == nil || *parsed.AdditionalProperties {
		t.Errorf("必须禁止额外字段：%s", d.Schema)
	}
}
