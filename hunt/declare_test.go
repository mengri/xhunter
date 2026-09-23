package hunt

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"xhunter/harness"
)

// 本文件补的是「澄清回路的采集端」：模型在正文固定小节里自陈的两类清单（FR-6.3）——
// 怎么切出来、什么时候登记、收尾怎么据此收敛终态。

// ============================================================ 解析

func TestParseDeclared_SectionsAndEntries(t *testing.T) {
	text := strings.Join([]string{
		"我改完了两处，但有两件事需要确认。",
		"",
		"## 需要补全",
		"- 数据库迁移脚本用哪个版本（任务描述与约定都没有给出）",
		"2、目标环境的 `GOOS`（不能取默认：跨平台编译结果不同）",
		"（3）是否需要同步更新 CHANGELOG",
		"",
		"## 假设",
		"* 假定 `go test ./...` 就是验收命令",
		"1. 假定分支命名沿用仓库既有惯例",
		"",
		"## 其他",
		"- 这一条不属于上面两类",
	}, "\n")

	got := ParseDeclared(text)
	wantNeeds := []string{
		"数据库迁移脚本用哪个版本（任务描述与约定都没有给出）",
		"目标环境的 `GOOS`（不能取默认：跨平台编译结果不同）",
		"是否需要同步更新 CHANGELOG",
	}
	wantAssumptions := []string{
		"假定 `go test ./...` 就是验收命令",
		"假定分支命名沿用仓库既有惯例",
	}
	if !reflect.DeepEqual(got.Needs, wantNeeds) {
		t.Errorf("needs = %q，期望 %q", got.Needs, wantNeeds)
	}
	if !reflect.DeepEqual(got.Assumptions, wantAssumptions) {
		t.Errorf("assumptions = %q，期望 %q", got.Assumptions, wantAssumptions)
	}
}

// 三态：未提供必须是 nil（不是空切片）。序列化时只有 nil 才是 null——「没提供」与
// 「提供了但一条都没有」是两句话。
func TestParseDeclared_AbsentSectionIsNilNotEmpty(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
	}{
		{"完全没有小节", "我改完了，没有别的话。"},
		{"小节在但一条都没有", "## 需要补全\n\n## 假设\n"},
		{"空正文", ""},
	} {
		got := ParseDeclared(tc.text)
		if got.Needs != nil || got.Assumptions != nil {
			t.Errorf("%s：未提供必须是 nil（不是空切片）：needs=%#v assumptions=%#v",
				tc.name, got.Needs, got.Assumptions)
		}
	}
}

// 标题后面补一句说明不影响归属：模型常写「## 需要补全（缺两项）」。
func TestParseDeclared_HeadingSuffixDoesNotChangeOwnership(t *testing.T) {
	got := ParseDeclared("## 需要补全（缺两项）\n- 缺 A\n")
	if len(got.Needs) != 1 || got.Needs[0] != "缺 A" {
		t.Errorf("标题带说明时仍应归属 needs：%#v", got.Needs)
	}
}

// 去前缀只去真编号，不改写正文：小数与普通句子不能被当成列表项吃掉开头。
func TestParseDeclared_DoesNotEatNonListText(t *testing.T) {
	got := ParseDeclared("## 假设\n- 放宽到 2.5 倍超时仍然安全\n我按现状假定端口是 8080。\n")
	want := []string{"放宽到 2.5 倍超时仍然安全", "我按现状假定端口是 8080。"}
	if !reflect.DeepEqual(got.Assumptions, want) {
		t.Errorf("assumptions = %q，期望 %q", got.Assumptions, want)
	}
}

// ============================================================ 登记

// 在第几轮说就在第几轮登记：不靠收尾从上下文里捞——压缩落地后被下压掉的轮次捞不回来。
func TestAppendDeclared_AccumulatesAcrossTurns(t *testing.T) {
	s := NewSession(Config{})
	s.AppendDeclared("先说明一下。\n\n## 假设\n- 假定 A")
	s.AppendDeclared("## 需要补全\n- 缺 B")
	s.AppendDeclared("收尾。")

	got := s.Declared()
	if !reflect.DeepEqual(got.Needs, []string{"缺 B"}) {
		t.Errorf("needs = %q，期望 [缺 B]", got.Needs)
	}
	if !reflect.DeepEqual(got.Assumptions, []string{"假定 A"}) {
		t.Errorf("assumptions = %q，期望 [假定 A]", got.Assumptions)
	}
}

// 走一遍轮边界，确认登记确实由 OnTurn 完成（而不是只有 AppendDeclared 能用）。
func TestOnTurn_DeclaredIsRecordedAtTheTurnItWasSaid(t *testing.T) {
	s := NewSession(Config{})
	run := &harness.Run{}
	if _, err := s.OnTurn(context.Background(), run, &harness.Turn{No: 1, Text: "先做第一步。"}); err != nil {
		t.Fatalf("OnTurn 失败：%v", err)
	}
	if _, err := s.OnTurn(context.Background(), run, &harness.Turn{No: 2, Text: "## 需要补全\n- 缺 A"}); err != nil {
		t.Fatalf("OnTurn 失败：%v", err)
	}

	got := s.Declared().Needs
	if len(got) != 1 || got[0] != "缺 A" {
		t.Errorf("第 2 轮的自陈必须在第 2 轮就登记：%#v", got)
	}
}

// ============================================================ 收尾收敛

// newFinalizeSession 装配到「能跑收尾」的状态：收尾要取差异、清理与上报终态，所以需要
// git 与事件出口的桩；ops 为空时不会真的提交。
func newFinalizeSession(sink EventSink) *Session {
	return NewSession(Config{
		Bounty: Bounty{ID: "b1", Task: "t", Repo: gitRepoRef()},
		Git:    &stubBaselineGit{},
		Sink:   sink,
	})
}

func needsInputCount(sink *captureSink) int {
	n := 0
	for _, ev := range sink.events {
		if ev.Type == reasonNeedsInput {
			n++
		}
	}
	return n
}

func huntEndStatus(sink *captureSink) string {
	for i := len(sink.events) - 1; i >= 0; i-- {
		if sink.events[i].Type == "hunt_end" {
			s, _ := sink.events[i].Payload["status"].(string)
			return s
		}
	}
	return ""
}

func countEvent(sink *captureSink, typ string) int {
	n := 0
	for _, ev := range sink.events {
		if ev.Type == typ {
			n++
		}
	}
	return n
}

// 「写小节 = 停止信号」的落点：引擎按「无工具调用」给出 succeeded 时，若模型自陈了需要
// 补全的条件，终态收敛为 blocked——退出码仍是 0（对话正常走完），改动照常交付。
func TestFinalize_NeedsInputConvergesToBlocked(t *testing.T) {
	sink := &captureSink{}
	s := newFinalizeSession(sink)
	s.AppendDeclared("## 需要补全\n- 缺 A\n- 缺 B\n\n## 假设\n- 假定 C")

	run := &harness.Run{}
	run.SetTerminal(harness.Terminal{Status: harness.StatusSucceeded, Reason: "no_tool_call", Code: harness.ExitOK})
	if err := s.Finalize(context.Background(), run); err != nil {
		t.Fatalf("Finalize 失败：%v", err)
	}

	out := run.Outcome()
	if out.Status != harness.StatusBlocked || out.Reason != reasonNeedsInput {
		t.Errorf("终态 = %s/%s，期望 blocked/%s", out.Status, out.Reason, reasonNeedsInput)
	}
	if out.ExitCode != harness.ExitOK {
		t.Errorf("退出码 = %d，期望 0——blocked 是「对话正常走完」，不是失败", out.ExitCode)
	}
	if n := needsInputCount(sink); n != 2 {
		t.Errorf("needs_input 事件 = %d 条，期望逐条发 2 条：%+v", n, sink.events)
	}
	if n := countEvent(sink, "assumption"); n != 1 {
		t.Errorf("assumption 事件 = %d 条，期望 1 条：%+v", n, sink.events)
	}
	// 收敛必须排在 hunt_end 之前：顺序反了平台会在终态事件里读到 succeeded，与结果文件不一致。
	if got := huntEndStatus(sink); got != string(harness.StatusBlocked) {
		t.Errorf("hunt_end.status = %q，期望 %q——收敛必须排在上报之前", got, harness.StatusBlocked)
	}
}

// 机制性终止不被改写：预算耗尽是引擎（经策略）给的结论，一句自陈不能把它抹成 blocked。
func TestFinalize_BlockedDoesNotOverrideMechanicalTerminal(t *testing.T) {
	s := newFinalizeSession(&captureSink{})
	s.AppendDeclared("## 需要补全\n- 缺 A")

	run := &harness.Run{}
	run.SetTerminal(harness.Terminal{
		Status: harness.StatusFailed, Reason: "budget_exhausted:tokens", Code: harness.ExitAborted,
	})
	if err := s.Finalize(context.Background(), run); err != nil {
		t.Fatalf("Finalize 失败：%v", err)
	}

	out := run.Outcome()
	if out.Status != harness.StatusFailed || out.Reason != "budget_exhausted:tokens" {
		t.Errorf("机制性终止被改写了：%s/%s", out.Status, out.Reason)
	}
	if out.ExitCode != harness.ExitAborted {
		t.Errorf("退出码被改写成 %d，期望 %d", out.ExitCode, harness.ExitAborted)
	}
}

// 没自陈就还是 succeeded：不得把「没写小节」反推成失败——任务本身可能就是要产出一份小结。
func TestFinalize_NoDeclarationStaysSucceeded(t *testing.T) {
	s := newFinalizeSession(&captureSink{})
	s.AppendDeclared("## 假设\n- 假定 A")

	run := &harness.Run{}
	run.SetTerminal(harness.Terminal{Status: harness.StatusSucceeded, Reason: "no_tool_call", Code: harness.ExitOK})
	if err := s.Finalize(context.Background(), run); err != nil {
		t.Fatalf("Finalize 失败：%v", err)
	}

	out := run.Outcome()
	if out.Status != harness.StatusSucceeded || out.Reason != "no_tool_call" {
		t.Errorf("只有假设清单不该改终态：%s/%s", out.Status, out.Reason)
	}
}
