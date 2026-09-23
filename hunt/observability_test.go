package hunt

import (
	"context"
	"testing"
	"time"

	"xhunter/harness"
	"xhunter/llm"
	"xhunter/workspace"
)

// 本文件补的是 MS-2 里"最后一条事件"与"用量口径"两块**没被既有用例覆盖的路径**。
//
// 既有证据只在成功路径上断言过 hunt_end 是最后一条（含心跳）。这里把同一性质钉在**每一条
// 终止路径**上，并把心跳窗口放大到必现——停止点若从"终态块之前"挪到之后，这些用例会红。

// TestFinalize_HuntEndIsLastForEveryTerminal 逐条终止路径断言：hunt_end 恰好一条、且是最后
// 一条事件；收尾期间心跳确实在滴答（否则这个窗口没被真正验证）。
//
// 放大手段：心跳间隔调到 1ms，并让收尾的 Diff 睡 20ms——滴答必然发生在 Finalize 内部。
// 停止点若放在 hunt_end 之后，sleep 之后的计数会继续增长、hunt_end 也不再是最后一条。
func TestFinalize_HuntEndIsLastForEveryTerminal(t *testing.T) {
	cases := []struct {
		name     string
		terminal harness.Terminal
	}{
		{"成功", harness.Terminal{Status: harness.StatusSucceeded, Reason: "no_tool_call", Code: harness.ExitOK}},
		{"预算耗尽", harness.Terminal{Status: harness.StatusFailed, Reason: "budget_exhausted:turns", Code: harness.ExitAborted}},
		{"推理失败", harness.Terminal{Status: harness.StatusFailed, Reason: "infer_failed: boom", Code: harness.ExitEnv}},
		{"取消", harness.Terminal{Status: harness.StatusCancelled, Reason: "cancelled", Code: harness.ExitCancelled}},
		{"阻塞", harness.Terminal{Status: harness.StatusBlocked, Reason: "needs_input", Code: harness.ExitOK}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &orderedSink{}
			s := NewSession(Config{
				Bounty:    Bounty{ID: "b1", Task: "t", Repo: gitRepoRef()},
				Git:       &slowGit{delay: 20 * time.Millisecond}, // 收尾有耗时 → 心跳窗口必现
				Opener:    stubOpener{},
				Policy:    allowAll{},
				Sink:      sink,
				Heartbeat: time.Millisecond,
			})

			if err := s.Prepare(context.Background(), &harness.Run{}); err != nil {
				t.Fatalf("Prepare 失败：%v", err)
			}
			run := &harness.Run{}
			run.SetTerminal(tc.terminal)
			if err := s.Finalize(context.Background(), run); err != nil {
				t.Fatalf("Finalize 失败：%v", err)
			}
			// 收尾之后再等若干个间隔：停止点放错的话，这里会看到 hunt_end 之后还有心跳。
			time.Sleep(15 * time.Millisecond)

			got := sink.snapshot()
			ends, beats := 0, 0
			for i, e := range got {
				switch e {
				case "emit:hunt_end":
					ends++
					if i != len(got)-1 {
						t.Errorf("hunt_end 不是最后一条（第 %d/%d 条）：%v", i+1, len(got), got)
					}
				case "heartbeat":
					beats++
				}
			}
			if ends != 1 {
				t.Errorf("应恰好一条 hunt_end，实际 %d：%v", ends, got)
			}
			if beats == 0 {
				t.Errorf("收尾期间应有心跳滴答，否则这条用例没验证到「停止点在终态之前」：%v", got)
			}
		})
	}
}

// TestPrepareFailure_StillEndsWithHuntEnd 覆盖 Prepare 失败这条路径：飞不起来（插件失败）时
// 循环不开始、心跳从未启动，但收尾仍必须给出唯一的 hunt_end 作为最后一条事件——否则平台会
// 一直等一个永远不来的终态。用真实引擎跑，顺带钉住"Prepare 失败时零次推理"。
func TestPrepareFailure_StillEndsWithHuntEnd(t *testing.T) {
	sink := &captureSink{}
	s := NewSession(Config{
		Bounty: Bounty{ID: "b1", Task: "t", Repo: gitRepoRef()},
		Git:    &stubBaselineGit{},
		Opener: stubOpener{},
		Policy: allowAll{},
		Sink:   sink,
		SystemPlugins: func(workspace.Workspace) []PromptPlugin {
			return []PromptPlugin{failingPlugin{}}
		},
	})
	provider := &stubProvider{turns: []turnScript{{text: "不该跑到这里"}}}
	engine, err := harness.New(provider,
		[]harness.PrepareHandler{s.Prepare},
		[]harness.OnTurnHandler{s.OnTurn},
		[]harness.FinalHandler{s.Finalize},
	)
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}

	engine.Run(context.Background(), harness.Input{})

	if provider.infers != 0 {
		t.Errorf("Prepare 失败时循环不该开始：infer 次数 = %d", provider.infers)
	}
	ends := sink.ofType("hunt_end")
	if len(ends) != 1 {
		t.Fatalf("Prepare 失败也必须给出唯一 hunt_end，实际 %d 条：%v", len(ends), sink.events)
	}
	if last := sink.events[len(sink.events)-1]; last.Type != "hunt_end" {
		t.Errorf("hunt_end 必须是最后一条，实得 %q：%v", last.Type, sink.events)
	}
}

// TestOnTurn_UsageReportedOnlyOnTheTurnUpstreamSpeaks 覆盖「上游只在最后一轮才回报用量」：
// 中间轮没有 usage 事件、末轮有、hunt_end 的累计正确、reported=true。
//
// 与既有 TestOnTurn_EmitsUsageDeltaPerTurn（每轮都回报）互补——那种口径下"中间轮无事件"这条
// 分支根本没被走到。
func TestOnTurn_UsageReportedOnlyOnTheTurnUpstreamSpeaks(t *testing.T) {
	sink := &captureSink{}
	s := newFinalizeSession(sink)
	ctx := context.Background()

	run := &harness.Run{} // 前两轮沉默：run.Usage 恒为零
	for n := 1; n <= 2; n++ {
		if _, err := s.OnTurn(ctx, run, &harness.Turn{No: n}); err != nil {
			t.Fatalf("OnTurn 失败：%v", err)
		}
	}
	if us := sink.ofType("usage"); len(us) != 0 {
		t.Fatalf("上游沉默的轮次不得发 usage，实际 %d 条：%v", len(us), us)
	}

	// 第三轮（末轮）才回报：一条 usage，且是完整增量。
	run.Usage = llm.Usage{InputTokens: 100, OutputTokens: 50, CachedInputTokens: 20}
	if _, err := s.OnTurn(ctx, run, &harness.Turn{No: 3}); err != nil {
		t.Fatalf("OnTurn 失败：%v", err)
	}
	us := sink.ofType("usage")
	if len(us) != 1 {
		t.Fatalf("末轮应恰一条 usage，实际 %d 条：%v", len(us), us)
	}
	for k, v := range map[string]any{"input_tokens": 100, "output_tokens": 50, "cached_input_tokens": 20} {
		if us[0][k] != v {
			t.Errorf("末轮 usage.%s = %v，期望 %v", k, us[0][k], v)
		}
	}

	// 收尾：hunt_end 的累计必须等于末轮值，reported 为真，且不因"前两轮沉默"发 degraded。
	run.Usage.Turns = 3
	run.SetTerminal(harness.Terminal{Status: harness.StatusSucceeded, Reason: "no_tool_call", Code: harness.ExitOK})
	if err := s.Finalize(ctx, run); err != nil {
		t.Fatalf("Finalize 失败：%v", err)
	}
	ends := sink.ofType("hunt_end")
	if len(ends) != 1 {
		t.Fatalf("应恰一条 hunt_end：%v", sink.events)
	}
	got, ok := ends[0]["usage"].(UsageReport)
	if !ok {
		t.Fatalf("hunt_end.usage 应是 UsageReport：%T", ends[0]["usage"])
	}
	want := UsageReport{Reported: true, InputTokens: 100, OutputTokens: 50, CachedInputTokens: 20, Turns: 3}
	if got != want {
		t.Errorf("累计用量 = %+v，期望 %+v", got, want)
	}
	if got != s.Delivery().Usage {
		t.Errorf("hunt_end 与交付事实必须是同一份用量：%+v vs %+v", got, s.Delivery().Usage)
	}
	if n := countEvent(sink, "degraded"); n != 0 {
		t.Errorf("用量已回报时不得发 degraded，实际 %d 条：%v", n, sink.events)
	}
}

// TestCharge_SilentUpstreamFeedsNoBudget 覆盖 FR-9.7 的硬口径：上游全程沉默时**不拿 0 去
// 触发/消耗预算**。判据是 charge 只在增量非零时把用量交策略——沉默的每一轮都不该向预算记 0。
//
// 若哪天有人把 charge 改成"无条件把累计值交策略"，这条会红（recordingPolicy 会收到零增量）。
func TestCharge_SilentUpstreamFeedsNoBudget(t *testing.T) {
	pol := &recordingPolicy{}
	sink := &captureSink{}
	s := NewSession(Config{Policy: pol, Sink: sink})
	ctx := context.Background()
	run := &harness.Run{} // run.Usage 恒为零：上游全程沉默

	for n := 1; n <= 3; n++ {
		if _, err := s.OnTurn(ctx, run, &harness.Turn{No: n}); err != nil {
			t.Fatalf("OnTurn 失败：%v", err)
		}
	}
	if len(pol.charges) != 0 {
		t.Errorf("用量不可得时不得向预算记任何一笔（含 0）：%v", pol.charges)
	}
	if us := sink.ofType("usage"); len(us) != 0 {
		t.Errorf("全程沉默不得发 usage，实际 %d 条：%v", len(us), us)
	}
}
