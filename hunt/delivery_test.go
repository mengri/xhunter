package hunt

import (
	"context"
	"testing"

	"xhunter/harness"
)

// TestFinalize_TextOnlyDeliverySucceeds 钉住「无产出即失败」的闸门**不存在**：任务不一定改
// 代码——任务内容本身可能就是"产出一份小结"，那份最终答复就是交付物。全程零写操作时，
// 终态仍是 succeeded（退出 0），交付事实的 summary 就是那份小结。
//
// 这条把状态文档 §2.2 里 IA-6.7 引用的用例名落到实处：此前文档写了这个名字、代码里没有，
// 一致性审计因此报出它悬空。
func TestFinalize_TextOnlyDeliverySucceeds(t *testing.T) {
	sink := &captureSink{}
	s := newFinalizeSession(sink)

	run := &harness.Run{}
	const summary = "任务完成：这是最终小结，没有任何文件改动。"
	// 一轮：只给正文、不发起工具调用 → 引擎判 no_tool_call（模型认为做完了）。
	if _, err := s.OnTurn(context.Background(), run, &harness.Turn{No: 1, Text: summary}); err != nil {
		t.Fatalf("OnTurn 失败：%v", err)
	}
	if len(s.ops) != 0 {
		t.Fatalf("本例应全程零写操作：%+v", s.ops)
	}
	run.SetTerminal(harness.Terminal{Status: harness.StatusSucceeded, Reason: "no_tool_call", Code: harness.ExitOK})

	if err := s.Finalize(context.Background(), run); err != nil {
		t.Fatalf("Finalize 失败：%v", err)
	}

	out := run.Outcome()
	if out.Status != harness.StatusSucceeded || out.ExitCode != harness.ExitOK {
		t.Errorf("零写操作不得被判失败：终态 = %s/%d，期望 succeeded/0", out.Status, out.ExitCode)
	}
	if got := s.Delivery().Summary; got != summary {
		t.Errorf("交付事实的 summary = %q，期望那份小结", got)
	}
	if last := sink.events[len(sink.events)-1]; last.Type != "hunt_end" {
		t.Errorf("仍必须以 hunt_end 收尾：%v", sink.events)
	}
}
