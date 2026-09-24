package hunt

import (
	"context"
	"strings"
	"testing"

	"xhunter/ext"
	"xhunter/git"
	"xhunter/harness"
	"xhunter/workspace"
)

// 本文件钉住门禁与运行主链的接线：收尾补跑、终态裁决、检查点抑制、事件出口与提示词注入。
// 判据本身（Expect）与清单加载各有自己的用例文件，这里只管"接得对不对"。

// countingGit 记下提交次数：检查点抑制这类断言只能靠"到底提没提交"来证明。
type countingGit struct {
	commits int
	lastMsg string
}

func (g *countingGit) PrepareBaseline(context.Context, git.RepoRef) (string, error) {
	return "root", nil
}
func (g *countingGit) Commit(_ context.Context, _ git.RepoRef, msg string) (git.Commit, error) {
	g.commits++
	g.lastMsg = msg
	return git.Commit{SHA: "deadbeef", Created: true}, nil
}
func (g *countingGit) Diff(context.Context, git.RepoRef) ([]string, error) { return nil, nil }
func (g *countingGit) Patch(context.Context, git.RepoRef) (string, error)  { return "", nil }
func (g *countingGit) Clean(context.Context) error                         { return nil }
func (g *countingGit) ReadFileAtCommit(context.Context, git.RepoRef, string) ([]byte, bool, error) {
	return nil, false, nil
}

// stubGateRunner 按预设给结论或执行失败；它同时记下自己被要求跑了哪些门禁。
type stubGateRunner struct {
	passed bool
	err    error
	ran    []string
}

func (r *stubGateRunner) Run(_ context.Context, _ string, g Gate, _ string) (GateResult, error) {
	r.ran = append(r.ran, g.Name)
	if r.err != nil {
		return GateResult{}, r.err
	}
	return GateResult{Name: g.Name, Passed: r.passed, Summary: "stub"}, nil
}

func finalizeWithGates(t *testing.T, gates []Gate, runner GateRunner, results []GateResult) (*Session, harness.Outcome, *captureSink, *stubGateRunner) {
	t.Helper()
	sink := &captureSink{}
	s := NewSession(Config{
		Bounty: Bounty{ID: "b1", Task: "task"},
		Git:    &countingGit{},
		Policy: allowAll{},
		Sink:   sink,
		Tools:  func(workspace.Workspace, ext.ExtHost) []Primitive { return nil },
	})
	if runner == nil {
		runner = &stubGateRunner{}
	}
	s.cfg.GateRunner = runner
	s.gates = gates
	s.gateSource = GateSourceRepo
	s.root = t.TempDir()
	s.storage = &memStorage{files: map[string]string{}}
	for _, r := range results {
		s.RecordGateResult(r)
	}
	// 引擎在循环正常结束时给出 succeeded；这里直接调 Finalize（不跑引擎），
	// 所以先把这个前提摆上——否则读到的是"没有终态"，测的就不是门禁了。
	run := &harness.Run{}
	run.SetTerminal(harness.Terminal{Status: harness.StatusSucceeded, Reason: "completed", Code: harness.ExitOK})
	if err := s.Finalize(context.Background(), run); err != nil {
		t.Fatalf("Finalize 失败：%v", err)
	}
	return s, run.Outcome(), sink, runner.(*stubGateRunner)
}

// 必需门禁**未通过** → 交付失败；但对话正常走完（退出码 0），改动照常交付。
func TestFinalize_RequiredGateFailureFailsTheDelivery(t *testing.T) {
	gates := []Gate{{Name: "unit", Argv: []string{"false"}, Required: true, Expect: Expect{Kind: ExpectExitZero}}}
	_, out, _, _ := finalizeWithGates(t, gates, &stubGateRunner{passed: false}, nil)
	if out.Status != harness.StatusFailed {
		t.Fatalf("必需门禁未通过应判失败，实际 %q（%s）", out.Status, out.Reason)
	}
	if out.ExitCode != harness.ExitOK {
		t.Errorf("退出码应为 0（对话正常走完，失败由证据给出），实际 %d", out.ExitCode)
	}
	if !strings.Contains(out.Reason, "unit") {
		t.Errorf("失败原因应指名是哪条门禁：%q", out.Reason)
	}
	if strings.Contains(out.Reason, "从未运行") {
		t.Errorf("跑过但未通过不该报成从未运行：%q", out.Reason)
	}
}

// 必需门禁**从未运行** → 同样判失败：这次交付没有质量证据。
// 与"未通过"区分开（原因里说得清是哪一种），处置上却是同一件事：都不能算成功交付。
func TestFinalize_RequiredGateNeverRunFailsTheDelivery(t *testing.T) {
	gates := []Gate{{Name: "unit", Argv: []string{"true"}, Required: true, Expect: Expect{Kind: ExpectExitZero}}}
	_, out, sink, _ := finalizeWithGates(t, gates, &stubGateRunner{err: context.DeadlineExceeded}, nil)
	if out.Status != harness.StatusFailed {
		t.Fatalf("必需门禁从未运行应判失败，实际 %q（%s）", out.Status, out.Reason)
	}
	if out.ExitCode != harness.ExitOK {
		t.Errorf("退出码应为 0，实际 %d", out.ExitCode)
	}
	if !strings.Contains(out.Reason, "从未运行") {
		t.Errorf("应说明是「从未运行」：%q", out.Reason)
	}
	// 跑不起来必须可见：静默跳过会让平台以为这次没有门禁。
	degraded := sink.ofType("degraded")
	found := false
	for _, d := range degraded {
		if d["scope"] == "gate" {
			found = true
		}
	}
	if !found {
		t.Errorf("门禁跑不起来应发一条 degraded(scope: gate)：%+v", degraded)
	}
}

// 对照：必需门禁跑过且通过 → 成功交付。
func TestFinalize_RequiredGatePassedSucceeds(t *testing.T) {
	gates := []Gate{{Name: "unit", Argv: []string{"true"}, Required: true, Expect: Expect{Kind: ExpectExitZero}}}
	_, out, _, _ := finalizeWithGates(t, gates, &stubGateRunner{passed: true}, nil)
	if out.Status != harness.StatusSucceeded {
		t.Fatalf("必需门禁通过应判成功，实际 %q（%s）", out.Status, out.Reason)
	}
}

// 非必需门禁没跑过不算失败：没有"必需"就没有"必须有证据"这回事。
func TestFinalize_OptionalGateDoesNotRequireEvidence(t *testing.T) {
	gates := []Gate{{Name: "lint", Argv: []string{"true"}, Required: false, Expect: Expect{Kind: ExpectExitZero}}}
	_, out, _, runner := finalizeWithGates(t, gates, &stubGateRunner{passed: false}, nil)
	if out.Status != harness.StatusSucceeded {
		t.Errorf("非必需门禁没跑过不该判失败，实际 %q（%s）", out.Status, out.Reason)
	}
	if len(runner.ran) != 0 {
		t.Errorf("非必需门禁不该被收尾补跑：%+v", runner.ran)
	}
}

// 收尾补跑只补**没跑过**的必需门禁：跑过的（哪怕没通过）已经留下了结论，
// 再跑一遍只是多花墙钟，而且会把"模型看到的结论"覆盖成另一份。
func TestFinalize_BackfillRunsOnlyRequiredGatesThatNeverRan(t *testing.T) {
	gates := []Gate{
		{Name: "unit", Argv: []string{"true"}, Required: true, Expect: Expect{Kind: ExpectExitZero}},
		{Name: "cover", Argv: []string{"true"}, Required: true, Expect: Expect{Kind: ExpectExitZero}},
		{Name: "opt", Argv: []string{"true"}, Required: false, Expect: Expect{Kind: ExpectExitZero}},
	}
	// unit 已经跑过（未通过）→ 不补；cover 没跑 → 补；opt 非必需 → 不补。
	_, _, _, runner := finalizeWithGates(t, gates, &stubGateRunner{passed: false},
		[]GateResult{{Name: "unit", Passed: false, Summary: "模型跑过"}})
	if len(runner.ran) != 1 || runner.ran[0] != "cover" {
		t.Errorf("只该补跑 cover：%+v", runner.ran)
	}
}

// 门禁未通过 → 抑制后续自动检查点（FR-5.2c）：不合格的中间态不值得钉在分支上。
// 这里把结构判据强制为"通过"，以证明抑制来自门禁而不是判据。
func TestCheckpoint_GateFailureSuppressesAutoCheckpoint(t *testing.T) {
	gitBackend := &countingGit{}
	s := NewSession(Config{
		Bounty: Bounty{ID: "b1", Task: "task"},
		Git:    gitBackend,
		Policy: allowAll{},
		Tools:  func(workspace.Workspace, ext.ExtHost) []Primitive { return nil },
	})
	s.storage = &memStorage{files: map[string]string{}}
	s.root = t.TempDir()
	s.ops = []WriteOp{{File: "a.txt", Primitive: "write"}}
	s.gateFailed = true
	s.ext = &fakeExt{parses: []ext.ParseVerdict{ext.ParseOK}}

	s.checkpoint(context.Background(), &harness.Turn{No: 1})
	if gitBackend.commits != 0 {
		t.Fatalf("门禁未通过时不该产生自动检查点，实际提交了 %d 次（%q）", gitBackend.commits, gitBackend.lastMsg)
	}
}

// 同一结构判据下，门禁通过（或没有门禁）时自动检查点照常提交——
// 证明上一条的"不提交"确实来自门禁，而不是判据或别的什么。
func TestCheckpoint_WithoutGateFailureAutoCheckpointStillCommits(t *testing.T) {
	gitBackend := &countingGit{}
	s := NewSession(Config{
		Bounty: Bounty{ID: "b1", Task: "task"},
		Git:    gitBackend,
		Policy: allowAll{},
		Tools:  func(workspace.Workspace, ext.ExtHost) []Primitive { return nil },
	})
	s.storage = &memStorage{files: map[string]string{}}
	s.root = t.TempDir()
	s.ops = []WriteOp{{File: "a.txt", Primitive: "write"}}
	s.ext = &fakeExt{parses: []ext.ParseVerdict{ext.ParseOK}}

	s.checkpoint(context.Background(), &harness.Turn{No: 1})
	if gitBackend.commits != 1 {
		t.Fatalf("判据通过时应提交一次，实际 %d 次", gitBackend.commits)
	}
}

// 门禁结论的事件出口：MR 评审最需要的证据（跑没跑、过没过、是不是缓存结论）从这里出。
func TestGateResult_EmitsCheckResultEvent(t *testing.T) {
	sink := &captureSink{}
	s := NewSession(Config{Sink: sink, Bounty: Bounty{ID: "b1"}})
	s.gateSource = GateSourceRepo

	s.RecordGateResult(GateResult{CallID: "c1", Name: "unit", Passed: true, Cached: true,
		ExitCode: 0, DurationMS: 1234, Summary: "通过"})
	events := sink.ofType("check_result")
	if len(events) != 1 {
		t.Fatalf("一次结论应发一条事件，实际 %d 条", len(events))
	}
	ev := events[0]
	for _, key := range []string{"call_id", "gate", "passed", "cached", "exit_code", "duration_ms", "source", "summary"} {
		if _, ok := ev[key]; !ok {
			t.Errorf("事件缺字段 %q：%v", key, ev)
		}
	}
	if ev["gate"] != "unit" || ev["passed"] != true || ev["cached"] != true {
		t.Errorf("事件内容应与结论一致：%v", ev)
	}
	if ev["call_id"] != "c1" {
		t.Errorf("事件要能配回那一次调用：%v", ev)
	}
	if ev["source"] != GateSourceRepo {
		t.Errorf("事件应带清单来源档位：%v", ev)
	}
}

// 门禁名必须进上下文：门禁是具名条目，模型不知道名字就无从调用——
// "跑到一半才发现有门禁"等于没有门禁。
func TestGateFacts_NamesReachTheUserTurn(t *testing.T) {
	gates := []Gate{
		{Name: "unit", Required: true, Expect: Expect{Kind: ExpectExitZero}},
		{Name: "lint", Expect: Expect{Kind: ExpectEmptyOutput}},
	}
	got := firstPrompt("SYS", "USER", Bounty{Task: "t"}, gates)
	if len(got) != 2 {
		t.Fatalf("两段都要在：%+v", got)
	}
	user := got[1].Content
	if !strings.Contains(user, "unit") || !strings.Contains(user, "lint") {
		t.Errorf("user 段应列出本次可用的门禁名：%q", user)
	}
	if !strings.Contains(user, "必需") {
		t.Errorf("必需门禁应标注出来，模型才知道哪些不过关就不能交付：%q", user)
	}
	// 插件正文仍在其后：门禁块插在环境事实与插件正文之间，不挤掉任何一块。
	if strings.Index(user, "unit") > strings.Index(user, "USER") {
		t.Errorf("门禁块应排在插件正文之前：%q", user)
	}
	// 没有门禁时这个块整个不出现：留一个空标题等于告诉模型"有门禁但没有名字"。
	empty := firstPrompt("SYS", "USER", Bounty{Task: "t"}, nil)
	if strings.Contains(empty[1].Content, "质量门禁") {
		t.Errorf("没有门禁时不该出现门禁块：%q", empty[1].Content)
	}
}
