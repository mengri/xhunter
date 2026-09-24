package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// 符号路径的端到端：模型用 `symbol_read` / `symbol_edit` 改一个 Go 函数，改动落进交付提交。
// 只有真 git + 真语法后端能证明这条链路成立——符号定位、区间替换、落盘与交付各段单独绿过，
// 串起来是否仍然成立是另一件事。

func runSymbolicHunt(t *testing.T, files map[string]string, scripts []string) (int, map[string]any, string, string) {
	t.Helper()
	requireGitForE2E(t)
	fx := newRepoFixtureWith(t, files)
	tmp := t.TempDir()
	workParent := filepath.Join(tmp, "work")
	if err := os.MkdirAll(workParent, 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	t.Setenv("TMPDIR", workParent)
	t.Setenv("HOME", filepath.Join(tmp, "home"))

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		n := int(atomic.AddInt32(&calls, 1)) - 1
		if n < len(scripts) {
			io.WriteString(w, scripts[n])
		} else {
			io.WriteString(w, sseWithText("完成"))
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer srv.Close()

	taskPath := writeRunInputs(t, tmp, "把 Alpha 的返回值改成 new\n验收：symbol_edit 的改动出现在交付提交里")
	setRunEnv(t, fx, srv.URL+"/v1")
	resultPath := filepath.Join(tmp, "out", "result.json")

	stdout, stderr := swapStdStreams(t)
	code := run([]string{"--bounty", taskPath, "--result", resultPath})
	stdoutText, stderrText := drainStdStreams(t, stdout, stderr)

	raw, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("结果文件不可读：%v\nstdout:\n%s\nstderr:\n%s", err, stdoutText, stderrText)
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("结果文件不是 JSON：%v", err)
	}
	return code, result, stdoutText, stderrText
}

const goSource = "package demo\n\nfunc Alpha() string {\n\treturn \"old\"\n}\n"

// 符号读 → 符号编辑：改动按**符号区间**落盘，而不是整份文件替换。
func TestEndToEnd_SymbolEditLandsInTheDelivery(t *testing.T) {
	scripts := []string{
		sseWithToolCall("call_1", "symbol_read", `{"symbol":"Alpha"}`),
		sseWithToolCall("call_2", "symbol_edit", `{"symbol":"Alpha","content":"func Alpha() string { return \"new\" }"}`),
	}
	code, result, stdoutText, stderrText := runSymbolicHunt(t,
		map[string]string{"main.go": goSource}, scripts)

	if code != exitOK {
		t.Fatalf("符号路径应正常收敛（0），实际 %d\nstdout:\n%s\nstderr:\n%s", code, stdoutText, stderrText)
	}
	if result["status"] != "succeeded" {
		t.Errorf("应成功交付，实际 %v（reason=%v）", result["status"], result["reason"])
	}
	changed, _ := result["files_changed"].([]any)
	if len(changed) == 0 {
		t.Fatalf("符号改动应进交付清单：%v", result["files_changed"])
	}
	found := false
	for _, f := range changed {
		if f == "main.go" {
			found = true
		}
	}
	if !found {
		t.Errorf("改动的应是 main.go：%v", changed)
	}
	if sha, _ := result["commit_sha"].(string); sha == "" {
		t.Error("交付提交应存在")
	}
}

// 符号读给出的是**那一段定义**，不是整份文件：模型据此改符号，而不是重写文件。
func TestEndToEnd_SymbolReadReturnsJustTheDefinition(t *testing.T) {
	scripts := []string{
		sseWithToolCall("call_1", "symbol_read", `{"symbol":"Alpha"}`),
	}
	code, _, stdoutText, stderrText := runSymbolicHunt(t,
		map[string]string{"main.go": goSource}, scripts)
	if code != exitOK {
		t.Fatalf("应正常收敛，实际 %d\nstdout:\n%s\nstderr:\n%s", code, stdoutText, stderrText)
	}
	// 结果事件里 tool_result 的 summary 即回灌给模型的那一份。
	if !containsAll(stdoutText, "func Alpha() string", "精度", "syntactic") {
		t.Errorf("符号读应给出定义那一段与精度标注：\n%s", stdoutText)
	}
}

// 符号后端对未注册语言给结构化错误，不静默改成文本替换。
func TestEndToEnd_SymbolEditOnUnregisteredLanguageIsStructuredError(t *testing.T) {
	scripts := []string{
		sseWithToolCall("call_1", "symbol_edit", `{"symbol":"alpha","path":"main.py","content":"X"}`),
	}
	code, _, stdoutText, stderrText := runSymbolicHunt(t,
		map[string]string{"main.py": "def alpha():\n    pass\n"}, scripts)
	if code != exitOK {
		t.Fatalf("结构化错误不该让整趟失败，实际 %d\nstdout:\n%s\nstderr:\n%s", code, stdoutText, stderrText)
	}
	if !containsAll(stdoutText, "language_unregistered") {
		t.Errorf("应给出 language_unregistered：\n%s", stdoutText)
	}
}

func containsAll(haystack string, needles ...string) bool {
	for _, n := range needles {
		found := false
		for i := 0; i+len(n) <= len(haystack); i++ {
			if haystack[i:i+len(n)] == n {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
