package main

import (
	"testing"
)

// 结构检查的端到端：真 git ＋ 真语法后端，证明「自动检查点只在结构完整点上落地」这条
// 判据在装配好的系统里真的连得上——判据三段（语法判据、封闭性判据、收敛）单测各绿过，
// 串起来是否仍成立是另一件事。

// 语法完整、且改动封闭在符号内 → 轮边界真的留下检查点。
func TestEndToEnd_CheckpointLandsOnCompleteSyntax(t *testing.T) {
	scripts := []string{
		sseWithToolCall("call_1", "symbol_read", `{"symbol":"Alpha"}`),
		sseWithToolCall("call_2", "symbol_edit", `{"symbol":"Alpha","content":"func Alpha() string { return \"new\" }"}`),
	}
	code, _, stdoutText, stderrText := runSymbolicHunt(t,
		map[string]string{"main.go": goSource}, scripts)
	if code != exitOK {
		t.Fatalf("应正常收敛（0），实际 %d\nstdout:\n%s\nstderr:\n%s", code, stdoutText, stderrText)
	}
	if !containsAll(stderrText, "已创建检查点") {
		t.Errorf("改动封闭在符号内且语法完整 → 应留下检查点：\n%s", stderrText)
	}
}

// 语法不完整 → 不留检查点，措辞是"未落在结构完整点"（判过、没通过），
// 而不是"不可判定"（那种是判不了）。改动本身照常走交付提交，不丢。
func TestEndToEnd_IncompleteSyntaxSuppressesCheckpoint(t *testing.T) {
	scripts := []string{
		sseWithToolCall("call_1", "symbol_read", `{"symbol":"Alpha"}`),
		sseWithToolCall("call_2", "symbol_edit", `{"symbol":"Alpha","content":"func Alpha() string { return "}`),
	}
	code, result, stdoutText, stderrText := runSymbolicHunt(t,
		map[string]string{"main.go": goSource}, scripts)
	if code != exitOK {
		t.Fatalf("判据不通过不该让整趟失败，实际 %d\nstdout:\n%s\nstderr:\n%s", code, stdoutText, stderrText)
	}
	if !containsAll(stderrText, "未落在结构完整点") {
		t.Errorf("应如实记录跳过原因：\n%s", stderrText)
	}
	if containsAll(stderrText, "已创建检查点") {
		t.Errorf("语法不完整时不得留下检查点：\n%s", stderrText)
	}
	if sha, _ := result["commit_sha"].(string); sha == "" {
		t.Error("检查点被抑制，改动仍应随交付提交交上去")
	}
}
