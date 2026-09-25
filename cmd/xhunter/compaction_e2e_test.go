package main

import (
	"strings"
	"testing"
)

// 压缩的端到端：真 git ＋ 真上游假 SSE ＋ **真窗口**（部署事实调小），
// 证明"长任务逼近窗口时会压、压完照常收敛"这条链在系统里真的连得上——
// 水位、分层、事件三段各自单测绿过，串起来是否仍成立是另一件事。

// 逼近窗口 → 压缩发生（发 `context_compacted`）→ 任务照常收敛、交付物不缺。
//
// 脚本至少三轮：**当前轮永不压**，第一轮之后历史里还没有可压的老轮，压缩最早只能发生在
// 第三轮请求组装时——只给两轮会走进"压不动"那条路，那是另一条事实（另有单测钉住）。
func TestEndToEnd_OverWindowCompactsAndStillConverges(t *testing.T) {
	big := strings.Repeat("0123456789abcdefghij", 400) // 8k 字符 ≈ 2k token
	scripts := []string{
		sseWithToolCall("call_1", "read", `{"path":"big.txt"}`),
		sseWithToolCall("call_2", "read", `{"path":"big.txt"}`),
		sseWithToolCall("call_3", "write", `{"path":"out.txt","content":"done"}`),
	}
	code, result, stdoutText, stderrText := runSymbolicHuntEnv(t,
		map[string]string{"big.txt": big}, scripts,
		map[string]string{
			"XHUNTER_MODEL_CONTEXT_TOKENS": "6000",
			"XHUNTER_MODEL_OUTPUT_TOKENS":  "500",
		})
	if code != exitOK {
		t.Fatalf("压缩不该让整趟失败，实际 %d\nstdout:\n%s\nstderr:\n%s", code, stdoutText, stderrText)
	}
	if result["status"] != "succeeded" && result["status"] != "blocked" {
		t.Errorf("应正常收敛，实际 %v（reason=%v）", result["status"], result["reason"])
	}

	levels := map[string]bool{}
	for _, line := range nonEmptyLines(stdoutText) {
		if !strings.Contains(line, `"type":"context_compacted"`) {
			continue
		}
		for _, want := range []string{"L0", "L1", "L2", "L3"} {
			if strings.Contains(line, `"level":"`+want+`"`) {
				levels[want] = true
			}
		}
	}
	if len(levels) == 0 {
		t.Fatalf("超窗场景应至少发一条 context_compacted：\n%s", stdoutText)
	}

	// 交付物不依赖上下文：压缩过之后改动清单与提交照旧。
	if sha, _ := result["commit_sha"].(string); sha == "" {
		t.Errorf("压缩之后仍应有交付提交：%v", result)
	}
	changed, _ := result["files_changed"].([]any)
	found := false
	for _, f := range changed {
		if f == "out.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("改动清单应含 out.txt：%v", changed)
	}
}

// 压完仍不低于硬上限 → **退出 2**（usage§7"上下文达硬上限 → 按预算耗尽处理"，不重派）。
//
// 这一档必须真的会停：只把它记成 `watermark: hard` 却继续发，换来的是上游的报错
// ——而"撞了硬上限却还在跑"比"停在这里"难诊断得多。
func TestEndToEnd_HardWatermarkStopsTheRun(t *testing.T) {
	small := strings.Repeat("0123456789abcdefghij", 100) // 2k 字符 ≈ 500 token
	big := strings.Repeat("0123456789abcdefghij", 1000)  // 20k 字符 ≈ 5k token
	scripts := []string{
		sseWithToolCall("call_1", "read", `{"path":"small.txt"}`),
		sseWithToolCall("call_2", "read", `{"path":"big.txt"}`),
	}
	code, result, stdoutText, stderrText := runSymbolicHuntEnv(t,
		map[string]string{"small.txt": small, "big.txt": big}, scripts,
		map[string]string{
			"XHUNTER_MODEL_CONTEXT_TOKENS": "6000",
			"XHUNTER_MODEL_OUTPUT_TOKENS":  "500",
		})
	if code != exitAborted {
		t.Fatalf("压不下来应按预算耗尽处理（退出 %d），实际 %d\nstdout:\n%s\nstderr:\n%s",
			exitAborted, code, stdoutText, stderrText)
	}
	if result["status"] != "failed" {
		t.Errorf("硬上限压不下来应判失败，实际 %v", result["status"])
	}
	reason, _ := result["reason"].(string)
	if reason != "budget_exhausted:context" {
		t.Errorf("原因应说清是上下文装不下：%v", reason)
	}
	// 压了几层是**已发生的事实**，事件照发——终止不改写发生过什么。
	compacted := 0
	for _, line := range nonEmptyLines(stdoutText) {
		if strings.Contains(line, `"type":"context_compacted"`) {
			compacted++
		}
	}
	if compacted == 0 {
		t.Errorf("终止之前该压的还是会压：\n%s", stdoutText)
	}
}
