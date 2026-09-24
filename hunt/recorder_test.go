package hunt

import (
	"context"
	"errors"
	"strings"
	"testing"

	"xhunter/ext"
	"xhunter/harness"
	"xhunter/llm"
	"xhunter/workspace"
)

// MS-6：Session 把材料交给记录器（ops / usage），且记录器故障只降级、不阻断。

// fakeRecorder 记录 Session 交给记录器的东西，并可注入 Open / Snapshot 失败。
type fakeRecorder struct {
	openErr error
	snapErr error
	ops     []WriteOp
	usages  []llm.Usage
	turns   int
	opens   int
	trace   []string // 调用次序，供"快照排在记账之后"这条回归断言
}

func (r *fakeRecorder) Open(string) error {
	r.opens++
	r.trace = append(r.trace, "open")
	return r.openErr
}
func (r *fakeRecorder) RecordTurn(harness.Turn) { r.turns++; r.trace = append(r.trace, "record-turn") }
func (r *fakeRecorder) RecordOp(op WriteOp) {
	r.ops = append(r.ops, op)
	r.trace = append(r.trace, "record-op")
}
func (r *fakeRecorder) RecordUsage(u llm.Usage) {
	r.usages = append(r.usages, u)
	r.trace = append(r.trace, "record-usage")
}
func (r *fakeRecorder) Ops() []WriteOp                { return r.ops }
func (r *fakeRecorder) Snapshot() error               { r.trace = append(r.trace, "snapshot"); return r.snapErr }
func (r *fakeRecorder) Load(string) (Restored, error) { return Restored{}, nil }

// 材料位置绑定失败只降级：Prepare 不失败，只留一条 warn。
func TestRecorder_OpenFailureDegradesWithoutFailingPrepare(t *testing.T) {
	sink := &captureSink{}
	rec := &fakeRecorder{openErr: errors.New("目录不可写")}
	s := NewSession(Config{
		Bounty: Bounty{ID: "b1", Task: "t", Repo: gitRepoRef()},
		Git:    &stubBaselineGit{}, Opener: stubOpener{}, Policy: allowAll{}, Sink: sink, Session: rec,
	})
	if err := s.Prepare(context.Background(), &harness.Run{}); err != nil {
		t.Fatalf("材料绑定失败只降级，不该让 Prepare 失败：%v", err)
	}
	if rec.opens != 1 {
		t.Errorf("工作区就绪后应绑定材料一次：%d", rec.opens)
	}
	if !strings.Contains(strings.Join(sink.logs, "\n"), "会话材料") {
		t.Errorf("应留下降级 warn：%v", sink.logs)
	}
}

// 落盘失败不该让任务判失败（IA-6.6）：跑完一次、只留 warn、终态照常。
func TestSnapshot_FailureDoesNotBlockTheRun(t *testing.T) {
	sink := &captureSink{}
	rec := &fakeRecorder{snapErr: errors.New("磁盘满")}
	s := NewSession(Config{
		Bounty: Bounty{ID: "b1", Task: "t", Repo: gitRepoRef()},
		Git:    &stubBaselineGit{}, Opener: stubOpener{}, Policy: allowAll{}, Sink: sink, Session: rec,
		Tools: func(workspace.Workspace, ext.ExtHost) []Primitive { return []Primitive{&writingPrim{}} },
	})
	provider := &stubProvider{turns: []turnScript{
		{calls: []llm.ToolCall{call("c1", "writer", `{}`)}},
		{text: "完成"},
	}}
	engine, err := harness.New(provider,
		[]harness.PrepareHandler{s.Prepare},
		[]harness.OnTurnHandler{s.OnTurn},
		[]harness.FinalHandler{s.Finalize},
	)
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	out := engine.Run(context.Background(), harness.Input{})
	if out.Status != harness.StatusSucceeded {
		t.Errorf("材料落盘失败不该让任务判失败：%s/%s", out.Status, out.Reason)
	}
	if !strings.Contains(strings.Join(sink.logs, "\n"), "会话材料落盘失败") {
		t.Errorf("应留下降级 warn：%v", sink.logs)
	}
}

// 记录器拿到的 op 与 usage 与 Session 内部同源：op 数 == 实际写操作数；usage 数 == 事件流增量数。
func TestRecorder_RecordsOpsAndUsageFromTheSession(t *testing.T) {
	sink := &captureSink{}
	rec := &fakeRecorder{}
	s := NewSession(Config{
		Bounty: Bounty{ID: "b1", Task: "t", Repo: gitRepoRef()},
		Git:    &stubBaselineGit{}, Opener: stubOpener{}, Policy: allowAll{}, Sink: sink, Session: rec,
		Tools: func(workspace.Workspace, ext.ExtHost) []Primitive { return []Primitive{&writingPrim{}} },
	})
	provider := &stubProvider{turns: []turnScript{
		{calls: []llm.ToolCall{call("c1", "writer", `{}`)}, usage: llm.Usage{InputTokens: 10, OutputTokens: 4, CachedInputTokens: 2}},
		{text: "完成", usage: llm.Usage{InputTokens: 5, OutputTokens: 2, CachedInputTokens: 1}},
	}}
	engine, err := harness.New(provider,
		[]harness.PrepareHandler{s.Prepare},
		[]harness.OnTurnHandler{s.OnTurn},
		[]harness.FinalHandler{s.Finalize},
	)
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	engine.Run(context.Background(), harness.Input{})

	// op：记录器拿到的与 Session 攥着的是同一份。
	if len(rec.ops) != len(s.ops) || len(rec.ops) != 1 {
		t.Errorf("op 记录数 = %d，Session 写操作数 = %d，期望都是 1", len(rec.ops), len(s.ops))
	}
	// usage：记录器拿到的条数与事件流的 usage 事件数一致，且是**增量**。
	if got, want := len(rec.usages), countEvent(sink, "usage"); got != want {
		t.Errorf("usage 记录数 = %d，事件流 usage 数 = %d，期望同源", got, want)
	}
	if len(rec.usages) != 2 ||
		rec.usages[0] != (llm.Usage{InputTokens: 10, OutputTokens: 4, CachedInputTokens: 2}) ||
		rec.usages[1] != (llm.Usage{InputTokens: 5, OutputTokens: 2, CachedInputTokens: 1}) {
		t.Errorf("usage 记录应是每轮增量：%+v", rec.usages)
	}
}

// 回归：快照必须排在记账**之后**——否则本轮的 usage 要等下一次快照才落盘，末轮的就永远进不了
// 交付提交（写对了但没交上去）。断言调用次序，而不只是最终数量。
func TestRecorder_SnapshotAfterCharge(t *testing.T) {
	rec := &fakeRecorder{}
	s := NewSession(Config{
		Bounty: Bounty{ID: "b1", Task: "t", Repo: gitRepoRef()},
		Git:    &stubBaselineGit{}, Opener: stubOpener{}, Policy: allowAll{}, Sink: &captureSink{}, Session: rec,
	})
	run := &harness.Run{Usage: llm.Usage{InputTokens: 10, OutputTokens: 4, CachedInputTokens: 2}}
	if _, err := s.OnTurn(context.Background(), run, &harness.Turn{No: 1, Text: "x"}); err != nil {
		t.Fatalf("OnTurn 失败：%v", err)
	}
	if got, want := strings.Join(rec.trace, ","), "record-turn,record-usage,snapshot"; got != want {
		t.Errorf("调用次序 = %q，期望 %q（快照排在记账之后，且仍在 checkpoint 之前）", got, want)
	}
}
