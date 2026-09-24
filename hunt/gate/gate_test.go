package gate

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"xhunter/hunt"
)

// check 不是「任意命令执行」：模型的参数面只有一个门禁名，命令、参数与判据都在业务侧
// 清单里。这条边界在声明上就要看得出来，也在下面的用例里被钉住。

// stubFacts 是原语能看到的事实的最小替身：清单、工作区根、改动指纹，以及把结论交回
// 执行体的口子（门禁结论只有那一个来源）。
type stubFacts struct {
	gates       []hunt.Gate
	root        string
	fingerprint string
	results     []hunt.GateResult
	intent      string
}

func (f *stubFacts) Gates() []hunt.Gate               { return f.gates }
func (f *stubFacts) Ledger() *hunt.Ledger             { return &hunt.Ledger{} }
func (f *stubFacts) RequestCheckpoint(summary string) { f.intent = summary }
func (f *stubFacts) CheckpointRequested() bool        { return f.intent != "" }
func (f *stubFacts) WorkRoot() string                 { return f.root }
func (f *stubFacts) ChangeFingerprint() string        { return f.fingerprint }
func (f *stubFacts) RecordGateResult(r hunt.GateResult) {
	f.results = append(f.results, r)
}

// 执行器缺失是**装配缺件**，如实报成可重试的环境问题。
//
// 它不是「尚未实现」：后者会让平台以为等下个版本就好，而实际是这一次装配漏了东西。
func TestCheck_UnavailableRunnerIsRetryableEnvFault(t *testing.T) {
	facts := &stubFacts{gates: []hunt.Gate{{Name: "unit", Argv: []string{"true"}}}}
	res, edits, err := CheckTool(nil).Execute(context.Background(),
		hunt.Call{ID: "c1", Primitive: Check, Gate: "unit"}, facts)
	if err != nil {
		t.Fatalf("执行器缺失不是执行失败：%v", err)
	}
	if res.Err == nil || res.Err.Kind != "gate_unavailable" {
		t.Fatalf("应返回 gate_unavailable：%+v", res.Err)
	}
	if !res.Err.Retryable {
		t.Error("执行器缺失是可修复的装配问题，应标为可重试")
	}
	if len(edits) != 0 {
		t.Errorf("校验本身不产生编辑：%+v", edits)
	}
}

// 名字不在清单里是**结论**（这次没有这条门禁），不是环境问题——重试也不会有。
func TestCheck_UnknownNameIsAVerdictNotAnEnvFault(t *testing.T) {
	facts := &stubFacts{gates: []hunt.Gate{{Name: "unit", Argv: []string{"true"}}}}
	res, _, err := CheckTool(NewRunner()).Execute(context.Background(),
		hunt.Call{ID: "c1", Primitive: Check, Gate: "coverage"}, facts)
	if err != nil {
		t.Fatalf("未知门禁名不该上抛成执行失败：%v", err)
	}
	if res.Err == nil || res.Err.Kind != "unknown_gate" {
		t.Fatalf("应返回 unknown_gate：%+v", res.Err)
	}
	if res.Err.Retryable {
		t.Error("清单里没有就是没有，不该标为可重试")
	}
	if len(facts.results) != 0 {
		t.Errorf("没跑成不该留下结论：%+v", facts.results)
	}
}

// 门禁通过是一个语义自洽点：这一刻的改动是验证过的，值得钉在分支上——所以通过后
// 请求检查点；结论同时交回执行体（结果文件与终态都读那一份）。
func TestCheck_PassedGateRequestsCheckpoint(t *testing.T) {
	facts := &stubFacts{
		gates: []hunt.Gate{{Name: "unit", Argv: []string{"true"},
			Expect: hunt.Expect{Kind: hunt.ExpectExitZero}}},
		fingerprint: "fp1",
	}
	res, _, err := CheckTool(NewRunner()).Execute(context.Background(),
		hunt.Call{ID: "c1", Primitive: Check, Gate: "unit"}, facts)
	if err != nil {
		t.Fatalf("门禁执行失败：%v", err)
	}
	if res.Err != nil {
		t.Fatalf("通过的门禁不该带错误：%+v", res.Err)
	}
	if len(facts.results) != 1 || !facts.results[0].Passed {
		t.Fatalf("结论应交回执行体且为通过：%+v", facts.results)
	}
	if facts.intent == "" {
		t.Error("门禁通过后应请求检查点：验证过的改动值得钉在分支上")
	}
	if res.Summary == "" {
		t.Error("应给模型一句可复核的结论")
	}
}

// 未通过**不**请求检查点：不合格的中间态钉在分支上没有价值。
func TestCheck_FailedGateDoesNotRequestCheckpoint(t *testing.T) {
	facts := &stubFacts{
		gates: []hunt.Gate{{Name: "unit", Argv: []string{"false"},
			Expect: hunt.Expect{Kind: hunt.ExpectExitZero}}},
		fingerprint: "fp1",
	}
	res, _, err := CheckTool(NewRunner()).Execute(context.Background(),
		hunt.Call{ID: "c1", Primitive: Check, Gate: "unit"}, facts)
	if err != nil {
		t.Fatalf("判定不通过不是执行失败（那是质量结论）：%v", err)
	}
	if res.Err != nil {
		t.Fatalf("质量结论不该走错误通道：%+v", res.Err)
	}
	if len(facts.results) != 1 || facts.results[0].Passed {
		t.Fatalf("结论应记为未通过：%+v", facts.results)
	}
	if facts.intent != "" {
		t.Errorf("未通过不该请求检查点：%q", facts.intent)
	}
}

// 参数面精确等于 `{name}`：里面**没有任何**接受命令、参数或脚本的槽位——
// 这是"不是任意命令执行"在模型可见面上的体现。按属性名精确断言，
// 不用子串匹配（"description" 里就含 "script"）。
func TestCheck_DeclSurfaceIsExactlyTheGateName(t *testing.T) {
	d := CheckTool(nil).Decl()
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
