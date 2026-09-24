package hunt

import (
	"context"
	"slices"
	"strings"
	"testing"

	"xhunter/harness"
	"xhunter/llm"
)

// 本文件补的是 MS-3「止损完备（两段式）」在**执行体侧**的接线：观测点（executeCall 每次调用喂
// 结局）、守卫次序（连败 > 止损 > 预算）、以及"换策略"提示的注入。
//
// 阈值字面值与两段式三态的演进取 `internal/policy` 的用例；这里只钉接线与次序。

// recordingStopLossPolicy 记录 ObserveFailure 收到的实参序列，用于断言观测接线点。
type recordingStopLossPolicy struct {
	allowAll
	observed []string
}

func (p *recordingStopLossPolicy) ObserveFailure(kind string) (StopLoss, string) {
	p.observed = append(p.observed, kind)
	return StopContinue, ""
}

// scriptedStopLossPolicy 是可脚本化的策略替身：ObserveFailure 对非空 kind 返回配置的处置，
// Exhausted / DeniedCount 也按配置返回——用于把「止损门」与「预算门」同时点亮。
type scriptedStopLossPolicy struct {
	observe    StopLoss
	observeMsg string
	denied     int
	deniedOver bool
	exhausted  bool
}

func (p *scriptedStopLossPolicy) Decide(context.Context, Call) (Decision, error) {
	return Decision{Verdict: VerdictAllow, Reason: "测试放行"}, nil
}
func (p *scriptedStopLossPolicy) Charge(llm.Usage) {}
func (p *scriptedStopLossPolicy) Exhausted(TurnNo) (bool, string) {
	if p.exhausted {
		return true, "turns"
	}
	return false, ""
}
func (p *scriptedStopLossPolicy) ObserveFailure(kind string) (StopLoss, string) {
	if kind == "" {
		return StopContinue, ""
	}
	return p.observe, p.observeMsg
}
func (p *scriptedStopLossPolicy) DeniedCount() (int, bool) { return p.denied, p.deniedOver }

// failingPrim 恒返回一次带 kind 的失败结果；okPrim 恒成功。二者共用 stubPrim。
func faultPrim(kind string) Primitive {
	return stubPrim{name: "fault_prim", res: Result{Err: &llm.Fault{Kind: kind, Message: "失败"}}}
}
func okPrim() Primitive {
	return stubPrim{name: "ok_prim", res: Result{Summary: "干完了"}}
}

// 观测接线点：失败喂 kind、成功喂空串——每条出口（这里取失败与成功两条）都要喂一次。
func TestExecuteCall_ObservesOutcomePerCall(t *testing.T) {
	st := &memStorage{files: map[string]string{"a.txt": "hello"}}
	pol := &recordingStopLossPolicy{}
	s := newTestSession(t, st, pol, &captureSink{}, faultPrim("not_found"), okPrim())

	turn := &harness.Turn{No: 1}
	s.executeCall(context.Background(), turn, call("c1", "fault_prim", `{}`))
	s.executeCall(context.Background(), turn, call("c2", "ok_prim", `{}`))

	want := []string{"not_found", ""}
	if !slices.Equal(pol.observed, want) {
		t.Errorf("ObserveFailure 实参序列 = %q，期望 %q（失败喂 kind、成功喂空串）", pol.observed, want)
	}
}

// 守卫次序：同类失败处置为 terminate 且预算**同时**耗尽 → 上报 stop_loss_same_kind（退出 2），
// 而不是被预算盖掉——止损说得出撞的是哪堵墙。
func TestOnTurn_SameKindStopLossOutranksBudget(t *testing.T) {
	pol := &scriptedStopLossPolicy{
		observe:    StopTerminate,
		observeMsg: "连续 3 次同类失败（not_found）",
		exhausted:  true, // 预算本轮也正好耗尽
	}
	s := newTestSession(t, &memStorage{files: map[string]string{}}, pol, &captureSink{}, faultPrim("not_found"))

	run := &harness.Run{}
	turn := &harness.Turn{No: 3, Calls: []llm.ToolCall{call("c1", "fault_prim", `{}`)}}
	cont, err := s.OnTurn(context.Background(), run, turn)
	if err != nil {
		t.Fatalf("OnTurn 失败：%v", err)
	}
	if cont {
		t.Error("同类失败达 terminate 应停止")
	}
	out := run.Outcome()
	if !strings.HasPrefix(out.Reason, "stop_loss_same_kind") {
		t.Errorf("终态原因 = %q，期望以 stop_loss_same_kind 开头（止损应先于预算被读到）", out.Reason)
	}
	if out.ExitCode != harness.ExitAborted {
		t.Errorf("退出码 = %d，期望 %d", out.ExitCode, harness.ExitAborted)
	}
}

// 守卫次序：连续拒绝达阈值且预算**同时**耗尽 → 上报 stop_loss_denied（退出 2）。
func TestOnTurn_DeniedStreakOutranksBudget(t *testing.T) {
	pol := &scriptedStopLossPolicy{denied: 3, deniedOver: true, exhausted: true}
	s := newTestSession(t, &memStorage{files: map[string]string{}}, pol, &captureSink{})

	run := &harness.Run{}
	cont, err := s.OnTurn(context.Background(), run, &harness.Turn{No: 3})
	if err != nil {
		t.Fatalf("OnTurn 失败：%v", err)
	}
	if cont {
		t.Error("连续拒绝达阈值应停止")
	}
	out := run.Outcome()
	if !strings.HasPrefix(out.Reason, "stop_loss_denied") {
		t.Errorf("终态原因 = %q，期望以 stop_loss_denied 开头", out.Reason)
	}
	if out.ExitCode != harness.ExitAborted {
		t.Errorf("退出码 = %d，期望 %d", out.ExitCode, harness.ExitAborted)
	}
}

// 守卫次序的三门竞争：提交连败、止损、预算**同时**成立 → 上报提交连败（退出 1）。连败是环境
// 问题（修好可重跑、比同为退出 2 的止损/预算更根因），必须最先被读到。
func TestOnTurn_CommitStreakOutranksStopLoss(t *testing.T) {
	pol := &scriptedStopLossPolicy{
		observe: StopTerminate, observeMsg: "连续 3 次同类失败（not_found）",
		denied: 3, deniedOver: true, exhausted: true,
	}
	s := newTestSession(t, &memStorage{files: map[string]string{}}, pol, &captureSink{}, faultPrim("not_found"))
	s.commitFailStreak = checkpointFailStreakLimit // 三条门同时成立

	run := &harness.Run{}
	turn := &harness.Turn{No: 3, Calls: []llm.ToolCall{call("c1", "fault_prim", `{}`)}}
	cont, err := s.OnTurn(context.Background(), run, turn)
	if err != nil {
		t.Fatalf("OnTurn 失败：%v", err)
	}
	if cont {
		t.Error("三门同时成立应停止")
	}
	out := run.Outcome()
	if !strings.HasPrefix(out.Reason, checkpointFailedStreak) {
		t.Errorf("终态原因 = %q，期望以 %q 开头（连败 > 止损 > 预算）", out.Reason, checkpointFailedStreak)
	}
	if out.ExitCode != harness.ExitEnv {
		t.Errorf("退出码 = %d，期望 %d（环境问题、可重跑）", out.ExitCode, harness.ExitEnv)
	}
}

// 「换策略」提示只影响下一轮：达 switch 阈值但那轮不终止时，下一轮消息末尾多一条 role=user
// 且含「换一种做法」的消息。
func TestOnTurn_SwitchHintAppendedToNextTurnMessages(t *testing.T) {
	pol := &scriptedStopLossPolicy{
		observe:    StopSwitch,
		observeMsg: "上一步连续 2 次因同一类原因失败（not_found）。换一种做法：不要重复刚才的动作，先根据失败信息调整参数或改用别的工具。",
	}
	s := newTestSession(t, &memStorage{files: map[string]string{}}, pol, &captureSink{}, faultPrim("not_found"))

	run := &harness.Run{}
	turn := &harness.Turn{No: 2, Calls: []llm.ToolCall{call("c1", "fault_prim", `{}`)}}
	cont, err := s.OnTurn(context.Background(), run, turn)
	if err != nil {
		t.Fatalf("OnTurn 失败：%v", err)
	}
	if !cont {
		t.Fatal("达 switch 阈值不应终止（两段式的第一段：换策略而非停下）")
	}
	if len(run.Messages) == 0 {
		t.Fatal("应把换策略提示附在下一轮的消息里")
	}
	last := run.Messages[len(run.Messages)-1]
	if last.Role != llm.RoleUser || !strings.Contains(last.Content, "换一种做法") {
		t.Errorf("末尾应是含「换一种做法」的 user 消息：%+v", last)
	}
}

// 一轮内「只升不降」：同轮先出现的 terminate 不得被其后的成功调用降级。
// observeOutcome 逐次把结局喂给策略；后一次成功（喂空串 → continue）秩更低，不该覆盖本轮已定的 terminate。
func TestOnTurn_HeaviestStopLossSurvivesLaterSuccess(t *testing.T) {
	pol := &scriptedStopLossPolicy{
		observe:    StopTerminate,
		observeMsg: "连续 3 次同类失败（not_found）",
	}
	s := newTestSession(t, &memStorage{files: map[string]string{}},
		pol, &captureSink{}, faultPrim("not_found"), okPrim())

	run := &harness.Run{}
	turn := &harness.Turn{No: 3, Calls: []llm.ToolCall{
		call("c1", "fault_prim", `{}`), // 先失败 → terminate
		call("c2", "ok_prim", `{}`),    // 后成功 → continue（秩更低）
	}}
	cont, err := s.OnTurn(context.Background(), run, turn)
	if err != nil {
		t.Fatalf("OnTurn 失败：%v", err)
	}
	if cont {
		t.Error("本轮已定的 terminate 不该被后一次成功降级")
	}
	out := run.Outcome()
	if !strings.HasPrefix(out.Reason, "stop_loss_same_kind") {
		t.Errorf("终态原因 = %q，期望以 stop_loss_same_kind 开头", out.Reason)
	}
	if out.ExitCode != harness.ExitAborted {
		t.Errorf("退出码 = %d，期望 %d", out.ExitCode, harness.ExitAborted)
	}
}

// 处置是本轮的：上一轮的 switch 不该泄漏到本轮、让本轮再次注入换策略提示。
// OnTurn 每轮开工复位 s.stopLoss/s.stopMsg；去掉复位就会让「上一轮 switch ＋ 本轮全成功」也再注入一次。
func TestOnTurn_StopLossDoesNotLeakAcrossTurns(t *testing.T) {
	const hint = "上一步连续 2 次因同一类原因失败（not_found）。换一种做法：不要重复刚才的动作，先根据失败信息调整参数或改用别的工具。"
	pol := &scriptedStopLossPolicy{observe: StopSwitch, observeMsg: hint}
	s := newTestSession(t, &memStorage{files: map[string]string{}},
		pol, &captureSink{}, faultPrim("not_found"), okPrim())

	// 第 1 轮：一次同类失败 → switch，提示进下一轮消息。
	run1 := &harness.Run{}
	cont1, err := s.OnTurn(context.Background(), run1,
		&harness.Turn{No: 1, Calls: []llm.ToolCall{call("c1", "fault_prim", `{}`)}})
	if err != nil {
		t.Fatalf("第 1 轮 OnTurn 失败：%v", err)
	}
	if !cont1 {
		t.Fatal("达 switch 阈值不应终止")
	}
	if len(run1.Messages) == 0 {
		t.Fatal("第 1 轮应把换策略提示附在下一轮消息里")
	}
	if last := run1.Messages[len(run1.Messages)-1]; last.Role != llm.RoleUser || !strings.Contains(last.Content, "不要重复刚才的动作") {
		t.Fatalf("第 1 轮末尾应是含提示的 user 消息：%+v", last)
	}

	// 第 2 轮：换一个新的 run，本轮全是成功调用 → 不该再注入任何换策略提示。
	run2 := &harness.Run{}
	cont2, err := s.OnTurn(context.Background(), run2,
		&harness.Turn{No: 2, Calls: []llm.ToolCall{call("c2", "ok_prim", `{}`)}})
	if err != nil {
		t.Fatalf("第 2 轮 OnTurn 失败：%v", err)
	}
	if !cont2 {
		t.Fatal("本轮无失败不应终止")
	}
	for _, m := range run2.Messages {
		if strings.Contains(m.Content, "不要重复刚才的动作") {
			t.Errorf("上一轮的 switch 不得泄漏到本轮：第 2 轮消息含换策略提示 %+v", m)
		}
	}
}

// 止损门内部次序：同轮「同类失败达 terminate」与「连续拒绝达阈值」皆成立时，先判同类失败。
func TestOnTurn_SameKindOutranksDeniedStreak(t *testing.T) {
	pol := &scriptedStopLossPolicy{
		observe:    StopTerminate,
		observeMsg: "连续 3 次同类失败（not_found）",
		denied:     3,
		deniedOver: true, // 两轴同轮皆达 termination
	}
	s := newTestSession(t, &memStorage{files: map[string]string{}},
		pol, &captureSink{}, faultPrim("not_found"))
	if s.commitFailStreak != 0 {
		t.Fatalf("前置：commitFailStreak 应为 0（否则更靠前的提交连败门会先拦下），实得 %d", s.commitFailStreak)
	}

	run := &harness.Run{}
	turn := &harness.Turn{No: 3, Calls: []llm.ToolCall{call("c1", "fault_prim", `{}`)}}
	cont, err := s.OnTurn(context.Background(), run, turn)
	if err != nil {
		t.Fatalf("OnTurn 失败：%v", err)
	}
	if cont {
		t.Error("两轴皆达 termination 应停止")
	}
	out := run.Outcome()
	if !strings.HasPrefix(out.Reason, "stop_loss_same_kind") {
		t.Errorf("终态原因 = %q，期望以 stop_loss_same_kind 开头（同类失败先于连续拒绝）", out.Reason)
	}
	if out.ExitCode != harness.ExitAborted {
		t.Errorf("退出码 = %d，期望 %d", out.ExitCode, harness.ExitAborted)
	}
}
