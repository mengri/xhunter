package harness

import (
	"context"
	"strings"
	"testing"

	"xhunter/llm"
)

// 控制原语 checkpoint 的绑定与执行语义。它是框架自带的一件东西——"请求检查点"
// 属于循环自身的词汇，与业务工具集无关（那些由装配层给）。

// 绑定：checkpoint 的 summary 有自己的槽位（早期借用 content，形状定格后归位）；
// 回灌时也回自己的参数名，不带上任何借用的痕迹。
func TestBind_CheckpointSummarySlot(t *testing.T) {
	tc := llm.ToolCall{ID: "c1", Name: "checkpoint",
		Arguments: []byte(`{"summary":"结构完整：重构已完成且门禁通过"}`)}
	call, fault := BindToolCall(tc)
	if fault != nil {
		t.Fatalf("绑定失败：%+v", fault)
	}
	if call.Primitive != PrimCheckpoint {
		t.Errorf("原语 = %q", call.Primitive)
	}
	if call.Summary != "结构完整：重构已完成且门禁通过" {
		t.Errorf("意图说明 = %q（应进 summary 槽位）", call.Summary)
	}
	if call.Content != "" || call.Target != "" {
		t.Errorf("不得再借用别的槽位：Content=%q Target=%q", call.Content, call.Target)
	}

	back := UnbindToolCall(call)
	if !strings.Contains(string(back.Arguments), `"summary"`) {
		t.Errorf("回灌参数应还原为 summary：%s", back.Arguments)
	}
	if strings.Contains(string(back.Arguments), `"content"`) || strings.Contains(string(back.Arguments), `"path"`) {
		t.Errorf("回灌参数不得带上借用的槽位名：%s", back.Arguments)
	}
}

// 执行：首次表达 → 记录意图并确认；重复表达 → 确认而非错误（长轮次里正常会做的事）。
func TestExecute_CheckpointRecordsIntentOnce(t *testing.T) {
	rt, err := NewRuntime(&stubPolicyForCp{}, stubExt{}, testTools())
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}
	facts := &stubFacts{storage: testFS(t), ledger: NewLedger()}
	in := ExecInput{
		Call:  Call{ID: "cp1", Primitive: PrimCheckpoint, Summary: "重构完成"},
		Facts: facts,
	}

	res := rt.Execute(context.Background(), in)
	if !res.OK || res.Err != nil {
		t.Fatalf("首次表达应成功：%+v", res)
	}
	if !strings.Contains(res.Summary, "已记录检查点意图：重构完成") {
		t.Errorf("确认应带上意图说明：%q", res.Summary)
	}
	if res.Route.Path != PathControl {
		t.Errorf("路径 = %q，控制原语应走 control", res.Route.Path)
	}
	if len(res.Ops) != 0 {
		t.Errorf("控制原语不得产生写操作：%+v", res.Ops)
	}

	res2 := rt.Execute(context.Background(), in)
	if !res2.OK {
		t.Fatalf("重复表达不是错误：%+v", res2)
	}
	if !strings.Contains(res2.Summary, "已请求过检查点") {
		t.Errorf("重复表达应得到确认：%q", res2.Summary)
	}
}

// 名字不认识：挡在运行时并**列出可用的名字**——模型换个名字就能继续，
// 而不是对着同一堵墙重试。清单在运行时手上，因此这条判断也只能在这里做。
func TestExecute_UnknownToolListsAvailable(t *testing.T) {
	rt, err := NewRuntime(&stubPolicyForCp{}, stubExt{}, testTools())
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}
	facts := &stubFacts{storage: testFS(t), ledger: NewLedger()}

	res := rt.Execute(context.Background(), ExecInput{
		Call:  Call{ID: "c9", Primitive: "deploy"},
		Facts: facts,
	})
	if res.Err == nil || res.Err.Kind != "unknown_tool" {
		t.Fatalf("未知工具应返回结构化错误：%+v", res)
	}
	if !strings.Contains(res.Err.Message, "write") {
		t.Errorf("错误信息应列出可用的名字：%q", res.Err.Message)
	}
}

// 构造期只校验**结构自洽**（机制），不校验业务合法性（那是装配层的事）。
func TestNewRuntime_RejectsStructuralProblems(t *testing.T) {
	cases := []struct {
		name  string
		tools []Tool
		want  string
	}{
		{"缺少名字", []Tool{{Decl: ToolDecl{Name: "x"}, Impl: writeStub{}}}, "缺少名字"},
		{"缺少实现", []Tool{{Name: "x", Decl: ToolDecl{Name: "x"}}}, "没有实现"},
		{"声明名字不一致", []Tool{{Name: "x", Decl: ToolDecl{Name: "y"}, Impl: writeStub{}}}, "必须一致"},
		{"重复", []Tool{
			{Name: "x", Decl: ToolDecl{Name: "x"}, Impl: writeStub{}},
			{Name: "x", Decl: ToolDecl{Name: "x"}, Impl: writeStub{}},
		}, "两次"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewRuntime(&stubPolicyForCp{}, stubExt{}, tc.tools)
			if err == nil {
				t.Fatal("应在装配期被拒绝")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("拒绝原因 = %q，期望包含 %q", err.Error(), tc.want)
			}
		})
	}
}

// 声明面由清单推出，控制原语殿后；清单为空时仍只剩控制原语——
// 框架不替业务决定"至少要有几个工具"。
func TestDecls_ControlLastAndOrderPreserved(t *testing.T) {
	rt, err := NewRuntime(&stubPolicyForCp{}, stubExt{}, testTools())
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}
	decls := rt.Decls()
	if len(decls) != 2 {
		t.Fatalf("声明数 = %d，期望 2（1 个工具 + 控制原语）", len(decls))
	}
	if decls[0].Name != "write" || decls[len(decls)-1].Name != "checkpoint" {
		t.Errorf("声明顺序不对：%+v", decls)
	}

	empty, err := NewRuntime(&stubPolicyForCp{}, stubExt{}, nil)
	if err != nil {
		t.Fatalf("空清单不该构造失败：%v", err)
	}
	if got := empty.Decls(); len(got) != 1 || got[0].Name != "checkpoint" {
		t.Errorf("空清单只应剩控制原语：%+v", got)
	}
}

// 策略桩：全部放行（控制原语当前不经策略裁决——它不产生任何副作用）。
type stubPolicyForCp struct{}

func (stubPolicyForCp) Decide(context.Context, Call, Route) (Decision, error) {
	return Decision{Verdict: VerdictAllow}, nil
}
func (stubPolicyForCp) Charge(Usage)                    {}
func (stubPolicyForCp) Exhausted(TurnNo) (bool, string) { return false, "" }
