package hunt

import (
	"context"
	"testing"
	"time"

	"xhunter/harness"
	"xhunter/llm"
)

// 终态可观测：`hunt_end` 带累计用量、`usage.reported` 如实、失败时发结构化 `error`。
// 这三件事与结果文件读的是**同一份**事实（同源），因此这里断言"事件与交付事实一致"。

// indexOf 返回某类型事件在事件流里的下标（-1 表示没有）。
func indexOf(sink *captureSink, typ string) int {
	for i, ev := range sink.events {
		if ev.Type == typ {
			return i
		}
	}
	return -1
}

// hunt_end 带整次 Hunt 的累计用量，形状与结果文件同一份（含 reported / turns / elapsed_ms）。
func TestHuntEnd_CarriesCumulativeUsage(t *testing.T) {
	sink := &captureSink{}
	s := newFinalizeSession(sink)
	// 一轮里上游回报过用量：Reported 的判据在 charge，这里据此把它置真。
	s.charge(llm.Usage{InputTokens: 30, OutputTokens: 12, CachedInputTokens: 8})

	run := &harness.Run{Usage: llm.Usage{
		InputTokens: 40, OutputTokens: 15, CachedInputTokens: 10,
		Turns: 2, Elapsed: 1500 * time.Millisecond,
	}}
	run.SetTerminal(harness.Terminal{Status: harness.StatusSucceeded, Reason: "no_tool_call", Code: harness.ExitOK})
	if err := s.Finalize(context.Background(), run); err != nil {
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
	// 与结果文件同源：两者读的都是 Delivery.Usage。
	if got != s.Delivery().Usage {
		t.Errorf("hunt_end 与交付事实必须是同一份用量：%+v vs %+v", got, s.Delivery().Usage)
	}
	want := UsageReport{Reported: true, InputTokens: 40, OutputTokens: 15, CachedInputTokens: 10, Turns: 2, ElapsedMS: 1500}
	if got != want {
		t.Errorf("累计用量 = %+v，期望 %+v", got, want)
	}
}

// hunt_end 必须是最后一条：平台据此认定"这次跑完了"，后面再有事件就是乱序。
func TestHuntEnd_IsTheLastEvent(t *testing.T) {
	sink := &captureSink{}
	s := newFinalizeSession(sink)
	s.AppendDeclared("## 需要补全\n- 缺 A") // 收尾会先发 needs_input，再发终态

	run := &harness.Run{}
	run.SetTerminal(harness.Terminal{Status: harness.StatusSucceeded, Reason: "no_tool_call", Code: harness.ExitOK})
	if err := s.Finalize(context.Background(), run); err != nil {
		t.Fatalf("Finalize 失败：%v", err)
	}
	if len(sink.events) == 0 {
		t.Fatal("应有事件发出")
	}
	if last := sink.events[len(sink.events)-1]; last.Type != "hunt_end" {
		t.Errorf("hunt_end 必须是最后一条，实得 %q：%v", last.Type, sink.events)
	}
}

// 失败才发 error：kind 是原因首段（供程序分支），retryable 与退出码同源，context 给人定位。
func TestFinalize_EmitsErrorOnFailureMatchingExitCode(t *testing.T) {
	cases := []struct {
		name      string
		reason    string
		code      harness.ExitCode
		wantKind  string
		wantRetry bool
	}{
		{"环境问题", "prepare_failed: 远端不可达", harness.ExitEnv, "prepare_failed", true},
		{"被引擎中止", "budget_exhausted:turns", harness.ExitAborted, "budget_exhausted", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &captureSink{}
			s := newFinalizeSession(sink)
			run := &harness.Run{Usage: llm.Usage{Turns: 3}}
			run.SetTerminal(harness.Terminal{Status: harness.StatusFailed, Reason: tc.reason, Code: tc.code})
			if err := s.Finalize(context.Background(), run); err != nil {
				t.Fatalf("Finalize 失败：%v", err)
			}

			errs := sink.ofType("error")
			if len(errs) != 1 {
				t.Fatalf("failed 必须恰一条 error：%v", sink.events)
			}
			if errs[0]["kind"] != tc.wantKind {
				t.Errorf("kind = %v，期望 %v（原因首段）", errs[0]["kind"], tc.wantKind)
			}
			if errs[0]["retryable"] != tc.wantRetry {
				t.Errorf("retryable = %v，期望 %v（与退出码同源）", errs[0]["retryable"], tc.wantRetry)
			}
			if errs[0]["context"] != "turn 3" {
				t.Errorf("context 应给出定位线索（轮次）：%v", errs[0]["context"])
			}
			if last := sink.events[len(sink.events)-1]; last.Type != "hunt_end" {
				t.Errorf("error 之后必须还有 hunt_end 收尾：%v", sink.events)
			}
		})
	}
}

// 取消不是错误；blocked 是模型的正常判断（它"缺什么"已由 needs_input 逐条报出）——都不发 error。
func TestFinalize_NoErrorEventOnBlockedOrCancelled(t *testing.T) {
	cases := []struct {
		name     string
		terminal harness.Terminal
	}{
		{"blocked", harness.Terminal{Status: harness.StatusBlocked, Reason: "needs_input", Code: harness.ExitOK}},
		{"cancelled", harness.Terminal{Status: harness.StatusCancelled, Reason: "cancelled", Code: harness.ExitCancelled}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &captureSink{}
			s := newFinalizeSession(sink)
			run := &harness.Run{}
			run.SetTerminal(tc.terminal)
			if err := s.Finalize(context.Background(), run); err != nil {
				t.Fatalf("Finalize 失败：%v", err)
			}
			if n := countEvent(sink, "error"); n != 0 {
				t.Errorf("%s 不发 error，实际 %d 条：%v", tc.name, n, sink.events)
			}
		})
	}
}

// 用量不可得时如实标注：恰一条 degraded（scope: usage），排在 hunt_end 之前；有回报则不发。
func TestFinalize_DegradedOnceWhenUsageUnavailable(t *testing.T) {
	t.Run("上游沉默发一条", func(t *testing.T) {
		sink := &captureSink{}
		s := newFinalizeSession(sink)
		run := &harness.Run{} // usageReported 保持 false
		run.SetTerminal(harness.Terminal{Status: harness.StatusSucceeded, Reason: "no_tool_call", Code: harness.ExitOK})
		if err := s.Finalize(context.Background(), run); err != nil {
			t.Fatalf("Finalize 失败：%v", err)
		}

		deg := sink.ofType("degraded")
		if len(deg) != 1 {
			t.Fatalf("用量不可得时恰一条 degraded：%v", sink.events)
		}
		if deg[0]["scope"] != "usage" {
			t.Errorf("degraded.scope = %v，期望 usage", deg[0]["scope"])
		}
		if deg[0]["subject"] == "" || deg[0]["reason"] == "" {
			t.Errorf("degraded 应带 subject 与 reason：%v", deg[0])
		}
		if s.Delivery().Usage.Reported {
			t.Error("用量不可得时 usage.reported 必须为 false")
		}

		end, degIdx := indexOf(sink, "hunt_end"), indexOf(sink, "degraded")
		if end != len(sink.events)-1 {
			t.Errorf("hunt_end 必须是最后一条：%v", sink.events)
		}
		if degIdx < 0 || degIdx > end {
			t.Errorf("degraded 必须在 hunt_end 之前：%v", sink.events)
		}
	})

	t.Run("有回报不发", func(t *testing.T) {
		sink := &captureSink{}
		s := newFinalizeSession(sink)
		s.charge(llm.Usage{InputTokens: 5}) // 任一非零增量即把 reported 置真
		run := &harness.Run{Usage: llm.Usage{InputTokens: 5, Turns: 1}}
		run.SetTerminal(harness.Terminal{Status: harness.StatusSucceeded, Reason: "no_tool_call", Code: harness.ExitOK})
		if err := s.Finalize(context.Background(), run); err != nil {
			t.Fatalf("Finalize 失败：%v", err)
		}
		if n := countEvent(sink, "degraded"); n != 0 {
			t.Errorf("用量有回报时不得发 degraded，实际 %d 条：%v", n, sink.events)
		}
		if !s.Delivery().Usage.Reported {
			t.Error("有回报时 usage.reported 应为 true")
		}
	})
}
