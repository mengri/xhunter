package hunt

import (
	"context"
	"strings"
	"testing"

	"xhunter/harness"
	"xhunter/llm"
	"xhunter/workspace"
)

// 本文件钉住会话恢复（resume）在 Prepare 里的三条语义：
//  1. 恢复是**纯读**：读材料不执行任何原语、不提交；
//  2. 回灌的轮次按原有顺序进上下文，本次投递的新条件作为**新的 user 消息**追加在历史之后；
//  3. 材料不存在（SchemaVersion == 0）不算恢复——不回灌、不追加新条件。
//
// 另加一条：恢复时把上次用量**补喂一次**策略（token 预算续算），且只喂输入/输出。

// resumeRecorder 是一个可注入 Load 结果的会话记录器替身：只关心"读了什么、Open 了几次、
// 用的是哪个 root"。
type resumeRecorder struct {
	restored Restored
	loadErr  error
	root     string
	loads    int
	opens    int
}

func (r *resumeRecorder) Open(root string) error  { r.opens++; return nil }
func (r *resumeRecorder) RecordTurn(harness.Turn) {}
func (r *resumeRecorder) RecordOp(WriteOp)        {}
func (r *resumeRecorder) RecordUsage(llm.Usage)   {}
func (r *resumeRecorder) Ops() []WriteOp          { return nil }
func (r *resumeRecorder) Snapshot() error         { return nil }
func (r *resumeRecorder) Load(root string) (Restored, error) {
	r.loads++
	r.root = root
	return r.restored, r.loadErr
}

// resumeContext 是一个组装替身：Assemble 把 prompt 与已回灌的轮次拼成消息，供断言顺序。
type resumeContext struct {
	prompt   []llm.Message
	appended []harness.Turn
}

func (c *resumeContext) SetPrompt(msgs []llm.Message) { c.prompt = msgs }
func (c *resumeContext) Append(rec harness.Turn)      { c.appended = append(c.appended, rec) }
func (c *resumeContext) Assemble() []llm.Message {
	out := append([]llm.Message(nil), c.prompt...)
	for _, t := range c.appended {
		out = append(out, llm.Message{Role: llm.RoleAssistant, Content: t.Text})
	}
	return out
}

// resumeProbe 记录自己被执行的次数：恢复段若重放写操作，这里就会非零。
type resumeProbe struct{ execs int }

func (p *resumeProbe) Decl() llm.ToolDecl { return llm.ToolDecl{Name: "probe"} }
func (p *resumeProbe) Writes() bool       { return true }
func (p *resumeProbe) Execute(context.Context, Call, Facts) (Result, []workspace.FileEdit, error) {
	p.execs++
	return Result{}, nil, nil
}

// chargingPolicy 记录策略收到的每一次 Charge。
type chargingPolicy struct{ charges []llm.Usage }

func (p *chargingPolicy) Decide(context.Context, Call) (Decision, error) {
	return Decision{Verdict: VerdictAllow}, nil
}
func (p *chargingPolicy) Charge(u llm.Usage)              { p.charges = append(p.charges, u) }
func (p *chargingPolicy) Exhausted(TurnNo) (bool, string) { return false, "" }
func (p *chargingPolicy) ObserveFailure(string) (StopLoss, string) {
	return StopContinue, ""
}
func (p *chargingPolicy) DeniedCount() (int, bool) { return 0, false }

// 恢复段零工具执行、零提交；材料读取发生在 Open 之前（纯读先于绑定）。
func TestPrepare_ResumeSeedsContextWithoutExecutingTools(t *testing.T) {
	rec := &resumeRecorder{restored: Restored{
		SchemaVersion: 1,
		Turns:         []harness.Turn{{No: 1, Text: "上次的答复"}},
		Ops:           []WriteOp{{File: "a.txt", Primitive: "write"}},
	}}
	prim := &resumeProbe{}
	g := &recordingCommitGit{created: true}
	s := NewSession(Config{
		Bounty:  Bounty{ID: "b", Task: "t", Repo: gitRepoRef(), Session: &SessionRef{ID: "s"}},
		Git:     g,
		Opener:  stubOpener{},
		Policy:  allowAll{},
		Session: rec,
		Context: &resumeContext{},
		Tools:   func(workspace.Workspace) []Primitive { return []Primitive{prim} },
	})

	if err := s.Prepare(context.Background(), &harness.Run{}); err != nil {
		t.Fatalf("Prepare 失败：%v", err)
	}
	if prim.execs != 0 {
		t.Errorf("恢复段不得执行任何原语，实执行 %d 次", prim.execs)
	}
	if g.calls != 0 {
		t.Errorf("恢复段不得提交，实提交 %d 次", g.calls)
	}
	if rec.loads != 1 || rec.opens != 1 {
		t.Errorf("应各调一次 Load / Open，实得 Load=%d Open=%d", rec.loads, rec.opens)
	}
	// Load 必须拿到工作区根（读材料是纯读，先于 Open）：根由 PrepareBaseline 给出。
	if rec.root != "root" {
		t.Errorf("Load 应收到工作区根 root，实得 %q", rec.root)
	}
}

// 恢复时：回灌轮次按原有顺序，新条件（固定定位语）追加在回灌历史之后。
func TestPrepare_ResumeAppendsNewConditionsAfterRestoredHistory(t *testing.T) {
	const task = "把遗漏的边界补上"
	rec := &resumeRecorder{restored: Restored{
		SchemaVersion: 1,
		Turns: []harness.Turn{
			{No: 1, Text: "HIST-FIRST"},
			{No: 2, Text: "HIST-SECOND"},
		},
	}}
	ctx := &resumeContext{}
	s := NewSession(Config{
		Bounty:  Bounty{ID: "b", Task: task, Repo: gitRepoRef(), Session: &SessionRef{ID: "s"}},
		Git:     &stubBaselineGit{},
		Opener:  stubOpener{},
		Policy:  allowAll{},
		Session: rec,
		Context: ctx,
	})
	run := &harness.Run{}
	if err := s.Prepare(context.Background(), run); err != nil {
		t.Fatalf("Prepare 失败：%v", err)
	}

	// 回灌的轮次按原有顺序进上下文。
	if len(ctx.appended) != 2 || ctx.appended[0].Text != "HIST-FIRST" || ctx.appended[1].Text != "HIST-SECOND" {
		t.Errorf("回灌的轮次应按原有顺序：%+v", ctx.appended)
	}

	// 期望值写字面量（不引用 resumeConditionsLead 常量本身）。
	const lead = "本次投递的补充条件："
	last := run.Messages[len(run.Messages)-1]
	if !strings.HasPrefix(last.Content, lead) || !strings.Contains(last.Content, task) {
		t.Fatalf("末条消息应是新条件（定位语 + 本次任务正文），实得：%q", last.Content)
	}
	if last.Role != llm.RoleUser {
		t.Errorf("新条件应是 user 消息：%q", last.Role)
	}

	// 顺序：新条件必须排在回灌历史之后（用下标比较，不用 Contains）。
	var joined string
	for _, m := range run.Messages {
		joined += m.Content + "\n"
	}
	iHist, iCond := strings.Index(joined, "HIST-SECOND"), strings.Index(joined, lead)
	if iHist < 0 || iCond < 0 || iHist > iCond {
		t.Errorf("新条件必须排在回灌历史之后：hist=%d cond=%d\n%s", iHist, iCond, joined)
	}
}

// 材料不存在（SchemaVersion == 0）不算恢复：不回灌历史、不追加新条件。
func TestPrepare_FreshSessionWithoutMaterialIsNotResumed(t *testing.T) {
	rec := &resumeRecorder{restored: Restored{}} // 零值：材料不存在
	ctx := &resumeContext{}
	s := NewSession(Config{
		Bounty:  Bounty{ID: "b", Task: "t", Repo: gitRepoRef(), Session: &SessionRef{ID: "s"}},
		Git:     &stubBaselineGit{},
		Opener:  stubOpener{},
		Policy:  allowAll{},
		Session: rec,
		Context: ctx,
	})
	run := &harness.Run{}
	if err := s.Prepare(context.Background(), run); err != nil {
		t.Fatalf("Prepare 失败：%v", err)
	}
	if len(ctx.appended) != 0 {
		t.Errorf("未恢复不该回灌历史：%+v", ctx.appended)
	}
	for _, m := range run.Messages {
		if strings.Contains(m.Content, "本次投递的补充条件：") {
			t.Errorf("未恢复不该追加新条件：%q", m.Content)
		}
	}
}

// 恢复时把上次用量补喂一次策略（token 预算续算），且只喂输入/输出、不动本次运行的水位。
func TestPrepare_ResumeSeedsPolicyBudgetWithRestoredUsage(t *testing.T) {
	pol := &chargingPolicy{}
	rec := &resumeRecorder{restored: Restored{
		SchemaVersion: 1,
		Usage:         llm.Usage{InputTokens: 100, OutputTokens: 20, CachedInputTokens: 7},
	}}
	s := NewSession(Config{
		Bounty:  Bounty{ID: "b", Task: "t", Repo: gitRepoRef(), Session: &SessionRef{ID: "s"}},
		Git:     &stubBaselineGit{},
		Opener:  stubOpener{},
		Policy:  pol,
		Session: rec,
		Context: &resumeContext{},
	})
	if err := s.Prepare(context.Background(), &harness.Run{}); err != nil {
		t.Fatalf("Prepare 失败：%v", err)
	}
	// 恰好一次补喂：策略只吃输入/输出（不喂缓存明细），期望值写字面量。
	if len(pol.charges) != 1 {
		t.Fatalf("恢复应恰好补喂一次用量，实得 %d 次：%+v", len(pol.charges), pol.charges)
	}
	if pol.charges[0] != (llm.Usage{InputTokens: 100, OutputTokens: 20}) {
		t.Errorf("预算续算只喂输入/输出：%+v", pol.charges[0])
	}
	// 本次运行的水位不受影响（否则首轮增量会变负）。
	if s.charged != (llm.Usage{}) {
		t.Errorf("恢复不得改动本次运行的水位：%+v", s.charged)
	}
}

// 轮数口径：`turns_from` 按**记录条数**算，不取 turn `no` 的最大值——恢复后本趟轮号从 1 重新
// 起计，材料里会出现两段都从 1 开始的 `turn` 记录。夹具刻意让"记录条数(3)"与"no 最大值(2)"
// 不等，以区分两种口径（若按 no 最大值算，turns_from 会变成 3）。
func TestPrepare_ResumeCountsTurnsByRecordsNotByMaxTurnNo(t *testing.T) {
	rec := &resumeRecorder{restored: Restored{
		SchemaVersion: 1,
		Turns: []harness.Turn{
			{No: 1, Text: "a"},
			{No: 1, Text: "b"},
			{No: 2, Text: "c"},
		},
	}}
	s := NewSession(Config{
		Bounty:  Bounty{ID: "b", Task: "t", Repo: gitRepoRef(), Session: &SessionRef{ID: "s"}},
		Git:     &stubBaselineGit{},
		Opener:  stubOpener{},
		Policy:  allowAll{},
		Session: rec,
		Context: &resumeContext{},
		Sink:    &captureSink{},
	})
	run := &harness.Run{}
	if err := s.Prepare(context.Background(), run); err != nil {
		t.Fatalf("Prepare 失败：%v", err)
	}
	// 本次运行 2 轮；收尾定型会话增量。
	run.Usage.Turns = 2
	run.SetTerminal(harness.Terminal{Status: harness.StatusSucceeded, Reason: "no_tool_call", Code: harness.ExitOK})
	if err := s.Finalize(context.Background(), run); err != nil {
		t.Fatalf("Finalize 失败：%v", err)
	}
	d := s.Delivery().SessionDelta
	if d == nil {
		t.Fatal("恢复时应有 SessionDelta")
	}
	// 期望值写字面量：记录条数 3 → turns_from = 4。
	if d.TurnsFrom != 4 {
		t.Errorf("TurnsFrom = %d，期望 4（按记录条数 3 算，不是 no 最大值 2）", d.TurnsFrom)
	}
	if d.TurnsTo != 5 {
		t.Errorf("TurnsTo = %d，期望 5（恢复 3 轮 + 本次 2 轮）", d.TurnsTo)
	}
	if d.OpsCount != 0 {
		t.Errorf("OpsCount = %d，期望 0", d.OpsCount)
	}
}

// 材料只有 meta 行（零记录）也算"存在且合法"——即"`schema_version != 0` 即已恢复"这条判据
// 的退化情形：`Prepare` 应置 `resumed`，增量从 turns_from == 1 起算。
func TestPrepare_MetaOnlyMaterialIsTreatedAsResumed(t *testing.T) {
	rec := &resumeRecorder{restored: Restored{SchemaVersion: 1}} // 材料存在、零记录
	s := NewSession(Config{
		Bounty:  Bounty{ID: "b", Task: "t", Repo: gitRepoRef(), Session: &SessionRef{ID: "s"}},
		Git:     &stubBaselineGit{},
		Opener:  stubOpener{},
		Policy:  allowAll{},
		Session: rec,
		Context: &resumeContext{},
		Sink:    &captureSink{},
	})
	run := &harness.Run{}
	if err := s.Prepare(context.Background(), run); err != nil {
		t.Fatalf("Prepare 失败：%v", err)
	}
	if s.resumed == nil {
		t.Fatal("材料存在（SchemaVersion != 0）即算已恢复")
	}
	run.Usage.Turns = 1
	run.SetTerminal(harness.Terminal{Status: harness.StatusSucceeded, Reason: "no_tool_call", Code: harness.ExitOK})
	if err := s.Finalize(context.Background(), run); err != nil {
		t.Fatalf("Finalize 失败：%v", err)
	}
	d := s.Delivery().SessionDelta
	if d == nil {
		t.Fatal("恢复时应有 SessionDelta")
	}
	if d.TurnsFrom != 1 {
		t.Errorf("零记录材料的增量应从 turns_from == 1 起算：%+v", d)
	}
}
