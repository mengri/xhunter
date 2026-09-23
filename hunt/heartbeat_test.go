package hunt

import (
	"context"
	"sync"
	"testing"
	"time"

	"xhunter/git"
	"xhunter/harness"
	"xhunter/llm"
	"xhunter/workspace"
)

// heartbeatSink 记录心跳调用（与阶段），用于断言心跳的频率与阶段。
type heartbeatSink struct {
	mu    sync.Mutex
	beats []Phase
}

func (s *heartbeatSink) Emit(ExternalEvent) error   { return nil }
func (s *heartbeatSink) Log(string, string, ...any) { return }

func (s *heartbeatSink) Heartbeat(p Phase) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.beats = append(s.beats, p)
	return nil
}

func (s *heartbeatSink) Failed() error { return nil }

func (s *heartbeatSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.beats)
}

func (s *heartbeatSink) phases() []Phase {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Phase(nil), s.beats...)
}

// orderedSink 按发生顺序记录 Emit 与 Heartbeat，用于断言"hunt_end 之后没有任何事件"。
type orderedSink struct {
	mu     sync.Mutex
	events []string
}

func (s *orderedSink) Emit(ev ExternalEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, "emit:"+ev.Type)
	return nil
}

func (s *orderedSink) Log(string, string, ...any) {}

func (s *orderedSink) Heartbeat(Phase) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, "heartbeat")
	return nil
}

func (s *orderedSink) Failed() error { return nil }

func (s *orderedSink) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

// 短间隔下应持续收到心跳；阶段取当前阶段。
func TestHeartbeat_EmitsAtInterval(t *testing.T) {
	sink := &heartbeatSink{}
	stop := startHeartbeat(context.Background(), sink, 5*time.Millisecond, func() Phase { return PhaseInfer })

	deadline := time.Now().Add(2 * time.Second)
	for sink.count() < 2 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	stop()

	if n := sink.count(); n < 2 {
		t.Fatalf("按 5ms 间隔应在时间窗内发出 ≥2 条心跳，实际 %d", n)
	}
	for _, p := range sink.phases() {
		if p != PhaseInfer {
			t.Errorf("心跳阶段应取当前阶段，实得 %q", p)
		}
	}
}

// 随 ctx 取消立即停：取消后计数不得再增长。
func TestHeartbeat_StopsOnContextCancel(t *testing.T) {
	sink := &heartbeatSink{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := startHeartbeat(ctx, sink, 5*time.Millisecond, func() Phase { return PhaseInfer })
	defer stop()

	deadline := time.Now().Add(2 * time.Second)
	for sink.count() < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()

	time.Sleep(20 * time.Millisecond)
	after := sink.count()
	time.Sleep(20 * time.Millisecond)
	if sink.count() != after {
		t.Errorf("取消后不得再发心跳：%d → %d", after, sink.count())
	}
}

// stop 幂等：连调两次不 panic、之后不再发。
func TestHeartbeat_StopIsIdempotent(t *testing.T) {
	sink := &heartbeatSink{}
	stop := startHeartbeat(context.Background(), sink, 5*time.Millisecond, func() Phase { return PhaseInfer })

	deadline := time.Now().Add(2 * time.Second)
	for sink.count() < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	stop()
	stop() // 第二次不得 panic
	n := sink.count()
	time.Sleep(20 * time.Millisecond)
	if sink.count() != n {
		t.Errorf("stop 后不得再发心跳：%d → %d", n, sink.count())
	}
}

// sink 为 nil 时是空操作：stop 可安全调用。
func TestHeartbeat_NilSinkIsNoOp(t *testing.T) {
	stop := startHeartbeat(context.Background(), nil, 1*time.Millisecond, func() Phase { return PhaseInfer })
	stop()
	stop()
}

// 阶段在各注入点如实切换：bootstrap（取基线）→ assemble（构造提示词）→ tools（执行工具）
// → finalize（收尾）；Prepare 之后与 OnTurn 返回后是默认态 infer。
func TestSession_PhaseTracksStages(t *testing.T) {
	sink := &captureSink{}
	var seen []Phase
	var sess *Session
	phase := func() Phase { return sess.Phase() }

	cfg := Config{
		Bounty: Bounty{ID: "b1", Task: "t", Repo: gitRepoRef()},
		Git:    &phaseGit{phase: phase, seen: &seen},
		Opener: stubOpener{},
		Policy: allowAll{},
		Sink:   sink,
		Tools: func(workspace.Workspace) []Primitive {
			return []Primitive{phasePrim{stubPrim: stubPrim{name: "probe"}, phase: phase, seen: &seen}}
		},
		SystemPlugins: func(workspace.Workspace) []PromptPlugin {
			return []PromptPlugin{phasePlugin{phase: phase, seen: &seen}}
		},
	}
	sess = NewSession(cfg)

	if err := sess.Prepare(context.Background(), &harness.Run{}); err != nil {
		t.Fatalf("Prepare 失败：%v", err)
	}
	if got := sess.Phase(); got != PhaseInfer {
		t.Errorf("Prepare 之后应是默认态 infer：%q", got)
	}

	run := &harness.Run{}
	if _, err := sess.OnTurn(context.Background(), run, &harness.Turn{
		No: 1, Calls: []llm.ToolCall{call("c1", "probe", `{}`)},
	}); err != nil {
		t.Fatalf("OnTurn 失败：%v", err)
	}
	if got := sess.Phase(); got != PhaseInfer {
		t.Errorf("OnTurn 返回之后应回到 infer：%q", got)
	}

	run.SetTerminal(harness.Terminal{Status: harness.StatusSucceeded, Reason: "no_tool_call", Code: harness.ExitOK})
	if err := sess.Finalize(context.Background(), run); err != nil {
		t.Fatalf("Finalize 失败：%v", err)
	}

	want := []Phase{PhaseBootstrap, PhaseAssemble, PhaseTools, PhaseFinalize}
	if len(seen) != len(want) {
		t.Fatalf("注入点阶段 = %v，期望 %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("第 %d 个注入点阶段 = %q，期望 %q", i+1, seen[i], want[i])
		}
	}
}

// 本次核心回归：Finalize 在终态事件块之前把心跳停死，因此 hunt_end 之后没有任何事件
// （含心跳）。停止点若放到终态之后，这条会红。
func TestFinalize_StopsHeartbeatBeforeHuntEnd(t *testing.T) {
	sink := &orderedSink{}
	s := NewSession(Config{
		Bounty:    Bounty{ID: "b1", Task: "t", Repo: gitRepoRef()},
		Git:       &slowGit{delay: 8 * time.Millisecond}, // Diff 里睡一下，给心跳留出滴答窗口
		Opener:    stubOpener{},
		Policy:    allowAll{},
		Sink:      sink,
		Heartbeat: time.Millisecond,
	})

	if err := s.Prepare(context.Background(), &harness.Run{}); err != nil {
		t.Fatalf("Prepare 失败：%v", err)
	}
	run := &harness.Run{}
	run.SetTerminal(harness.Terminal{Status: harness.StatusSucceeded, Reason: "no_tool_call", Code: harness.ExitOK})
	if err := s.Finalize(context.Background(), run); err != nil {
		t.Fatalf("Finalize 失败：%v", err)
	}
	// 收尾之后再等几个间隔：停止点若放错，这里会看到 hunt_end 之后还有心跳。
	time.Sleep(15 * time.Millisecond)

	got := sink.snapshot()
	end := -1
	beats := 0
	for i, e := range got {
		if e == "heartbeat" {
			beats++
		}
		if e == "emit:hunt_end" {
			end = i
		}
	}
	if end < 0 {
		t.Fatalf("应有 hunt_end：%v", got)
	}
	if end != len(got)-1 {
		t.Errorf("hunt_end 必须是最后一条（含心跳）：%v", got)
	}
	if beats == 0 {
		t.Errorf("收尾期间应有心跳滴答，才能证明停止点在终态之前：%v", got)
	}
}

// ============================================================ 桩

// phaseGit 在取基线与收尾取差异时，记录当时的会话阶段。
type phaseGit struct {
	stubBaselineGit
	phase func() Phase
	seen  *[]Phase
}

func (g *phaseGit) PrepareBaseline(context.Context, git.RepoRef) (string, error) {
	*g.seen = append(*g.seen, g.phase())
	return "root", nil
}

func (g *phaseGit) Diff(context.Context, string) ([]string, error) {
	*g.seen = append(*g.seen, g.phase())
	return nil, nil
}

// slowGit 在 Diff 上睡一下，给"收尾期间心跳滴答"留出时间窗。
type slowGit struct {
	stubBaselineGit
	delay time.Duration
}

func (g *slowGit) Diff(context.Context, string) ([]string, error) {
	time.Sleep(g.delay)
	return nil, nil
}

// phasePlugin 在构造提示词时记录当时的会话阶段。
type phasePlugin struct {
	phase func() Phase
	seen  *[]Phase
}

func (p phasePlugin) Name() string { return "phase-probe" }

func (p phasePlugin) Build(context.Context, PromptInput) (PromptPart, error) {
	*p.seen = append(*p.seen, p.phase())
	return PromptPart{Body: "probe"}, nil
}

// phasePrim 在执行工具时记录当时的会话阶段。
type phasePrim struct {
	stubPrim
	phase func() Phase
	seen  *[]Phase
}

func (p phasePrim) Execute(context.Context, Call, Facts) (Result, []workspace.FileEdit, error) {
	*p.seen = append(*p.seen, p.phase())
	return Result{Summary: "probe"}, nil, nil
}
