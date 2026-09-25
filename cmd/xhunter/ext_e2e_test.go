package main

import (
	"strings"
	"testing"
)

// 外挂后端的端到端：真 git ＋ 真上游假 SSE ＋ **真把符号后端换成外挂进程**，
// 证明两件串起来才成立的事——① 外挂后端起不来时，符号原语给结构化错误、任务照常收敛
// （FR-13.4）；② 这次用的是哪个后端如实落在生效配置快照里（FR-13.7）。
// 两段各自单测绿过，串起来是否仍成立是另一件事。

func TestEndToEnd_ExternalBackendUnavailableStillConverges(t *testing.T) {
	scripts := []string{
		// 先按符号寻址：外挂后端起不来，这一步只能拿到结构化错误。
		sseWithToolCall("call_1", "symbol_read", `{"symbol":"Alpha"}`),
		// 模型因此退回文本路径——这正是 FR-13.4 要求它走的那条路。
		sseWithToolCall("call_2", "write", `{"path":"out.txt","content":"done"}`),
	}
	code, result, stdoutText, stderrText := runSymbolicHuntEnv(t,
		map[string]string{"main.go": goSource}, scripts,
		map[string]string{envExtCommand: "/nonexistent/xhunter-ext"})

	// 后端不可用**不是**装配缺陷：它是可以带着跑的事实，不该让整趟失败。
	if code != exitOK {
		t.Fatalf("外挂后端不可用不该让任务失败，实际退出 %d\nstdout:\n%s\nstderr:\n%s",
			code, stdoutText, stderrText)
	}
	if result["status"] != "succeeded" && result["status"] != "blocked" {
		t.Errorf("应正常收敛，实际 %v（reason=%v）", result["status"], result["reason"])
	}
	if sha, _ := result["commit_sha"].(string); sha == "" {
		t.Errorf("退回文本路径后仍应有交付提交：%v", result)
	}

	// 快照如实报出"这次用的是外挂后端"：精度档位变了却报不出来，是诊断时最难查的一类事。
	huntStart := ""
	for _, line := range nonEmptyLines(stdoutText) {
		if strings.Contains(line, `"type":"hunt_start"`) {
			huntStart = line
			break
		}
	}
	if huntStart == "" {
		t.Fatalf("事件流里找不到 hunt_start：\n%s", stdoutText)
	}
	if !strings.Contains(huntStart, "ext:xhunter-ext") {
		t.Errorf("hunt_start 的生效快照应报出外挂后端：\n%s", huntStart)
	}
}
