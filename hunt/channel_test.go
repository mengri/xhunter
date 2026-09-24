package hunt

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"xhunter/ext"
	"xhunter/harness"
	"xhunter/llm"
	"xhunter/workspace"
)

// 事件通道健康复查：出口断线要在**轮边界**被发现并收敛为环境错误（退出 1），而不是等
// 进程最末尾才收口——那意味着"跑完剩余轮次再报错"，消费者已不在通道上，剩余每一轮的推理与
// 工具执行都是在烧预算地自说自话。

// brokenSink 模拟出口断裂：Emit 超过 failAfter 条即失败，Failed() 据此返回原因。
// failAfter = -1 表示一开始就断（连 hunt_start 都写不出去）。
type brokenSink struct {
	mu        sync.Mutex
	err       error
	failAfter int
	emits     int
}

func newBrokenSink(failAfter int) *brokenSink {
	return &brokenSink{failAfter: failAfter, err: errors.New("write |1: broken pipe")}
}

func (s *brokenSink) Emit(ExternalEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emits++
	if s.emits > s.failAfter {
		return s.err
	}
	return nil
}

func (s *brokenSink) Log(string, string, ...any) {}

func (s *brokenSink) Heartbeat(Phase) error { return s.Emit(ExternalEvent{Type: "heartbeat"}) }

func (s *brokenSink) Failed() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.emits > s.failAfter {
		return s.err
	}
	return nil
}

// countingPrim 记录 Execute 被真正调用的次数。
type countingPrim struct {
	stubPrim
	calls int
}

func (p *countingPrim) Execute(context.Context, Call, Facts) (Result, []workspace.FileEdit, error) {
	p.calls++
	return Result{Summary: "ok"}, nil, nil
}

// 轮前复查（L2）：通道已断时，本轮一个工具都不该执行。
func TestOnTurn_StopsBeforeWorkWhenChannelAlreadyFailed(t *testing.T) {
	sink := newBrokenSink(-1) // 开工前就断
	prim := &countingPrim{stubPrim: stubPrim{name: "probe"}}
	s := newTestSession(t, &memStorage{files: map[string]string{}}, allowAll{}, sink, prim)

	run := &harness.Run{}
	cont, err := s.OnTurn(context.Background(), run, &harness.Turn{
		No: 1, Calls: []llm.ToolCall{call("c1", "probe", `{}`)},
	})
	if err != nil {
		t.Fatalf("OnTurn 不该上抛（收敛为终态）：%v", err)
	}
	if cont {
		t.Error("通道已断时不该继续")
	}
	if prim.calls != 0 {
		t.Errorf("通道已断时一个工具都不该执行，实际 %d 次", prim.calls)
	}
	out := run.Outcome()
	if out.Status != harness.StatusFailed || out.ExitCode != harness.ExitEnv {
		t.Errorf("终态 = %s/%d，期望 failed/%d", out.Status, out.ExitCode, harness.ExitEnv)
	}
	if !strings.HasPrefix(out.Reason, "event_channel_failed") {
		t.Errorf("原因应以 event_channel_failed 开头：%q", out.Reason)
	}
}

// 轮末复查（L6）：本轮发出时把通道写断了——本轮工具已执行完（那是已发生的事实），但不再进入
// 下一轮。用真实 harness.Engine ＋ 假上游断言"只跑了这一轮"。
func TestOnTurn_StopsAfterTurnWhenChannelFailsDuringTurn(t *testing.T) {
	sink := newBrokenSink(2) // hunt_start + config_snapshot（启动两条）成功；本轮的两条发出即断
	prim := &countingPrim{stubPrim: stubPrim{name: "probe"}}
	s := NewSession(Config{
		Bounty: Bounty{ID: "b1", Task: "t", Repo: gitRepoRef()},
		Git:    &stubBaselineGit{},
		Opener: stubOpener{},
		Policy: allowAll{},
		Sink:   sink,
		Tools:  func(workspace.Workspace, ext.ExtHost) []Primitive { return []Primitive{prim} },
	})

	provider := &stubProvider{turns: []turnScript{
		{calls: []llm.ToolCall{call("c1", "probe", `{}`)}}, // 第 1 轮：要一次工具
		{text: "不该跑到这里"},                                   // 第 2 轮：跑到了就说明没停下来
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
	if out.Status != harness.StatusFailed || out.ExitCode != harness.ExitEnv {
		t.Errorf("终态 = %s/%d，期望 failed/%d", out.Status, out.ExitCode, harness.ExitEnv)
	}
	if !strings.HasPrefix(out.Reason, "event_channel_failed") {
		t.Errorf("原因应以 event_channel_failed 开头：%q", out.Reason)
	}
	if prim.calls != 1 {
		t.Errorf("本轮工具应执行一次（已发生的事实），实际 %d", prim.calls)
	}
	if provider.infers != 1 {
		t.Errorf("通道断后不得进入下一轮：infer 次数 = %d，期望 1", provider.infers)
	}
}

// 取消优先：ctx 已取消且通道已断时，终态仍是 cancelled / 退出码 3——用户主动取消不能被通道
// 问题改写。
func TestOnTurn_ChannelFailureDoesNotOverrideCancellation(t *testing.T) {
	sink := newBrokenSink(-1)
	s := newTestSession(t, &memStorage{files: map[string]string{}}, allowAll{}, sink)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	run := &harness.Run{}
	cont, err := s.OnTurn(ctx, run, &harness.Turn{No: 1})
	if err != nil {
		t.Fatalf("OnTurn 不该上抛：%v", err)
	}
	if cont {
		t.Error("取消后不该继续")
	}
	out := run.Outcome()
	if out.Status != harness.StatusCancelled || out.ExitCode != harness.ExitCancelled {
		t.Errorf("取消优先：终态 = %s/%d，期望 cancelled/%d", out.Status, out.ExitCode, harness.ExitCancelled)
	}
}

// ============================================================ 假上游

// turnScript 是假上游一轮要产出的东西：正文、工具调用与用量。
type turnScript struct {
	text  string
	calls []llm.ToolCall
	usage llm.Usage
}

// stubProvider 按脚本逐轮交出事件序列，记录被调用了几轮。
type stubProvider struct {
	turns  []turnScript
	infers int
}

func (p *stubProvider) Capabilities() llm.Caps { return llm.Caps{} }

func (p *stubProvider) Infer(_ context.Context, _ llm.Request) (llm.Session, error) {
	p.infers++
	var s turnScript
	if p.infers-1 < len(p.turns) {
		s = p.turns[p.infers-1]
	}
	ch := make(chan llm.Event, len(s.calls)+2)
	for _, c := range s.calls {
		ch <- llm.Event{Kind: llm.EvToolUse, Call: c}
	}
	if s.text != "" {
		ch <- llm.Event{Kind: llm.EvText, Text: s.text}
	}
	if s.usage != (llm.Usage{}) {
		ch <- llm.Event{Kind: llm.EvUsage, Usage: s.usage}
	}
	close(ch)
	return stubSession{ch: ch}, nil
}

type stubSession struct{ ch <-chan llm.Event }

func (s stubSession) Events() <-chan llm.Event { return s.ch }
func (s stubSession) Cancel() error            { return nil }
