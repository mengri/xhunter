package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"xhunter/hunt"
)

// 门禁的端到端：清单从**基线 commit** 的 `gates.yml` 读（FR-5.2g），跑在真 git 夹具 + 假上游上。
// 这两条证明的是产品最硬的那一句：**验收没过，交付照样交出去，但状态必须是失败**。

// newRepoFixtureWith 造一个带额外文件的夹具仓库：门禁清单必须在**基线提交**里，
// 否则测的就不是"从基线读"这条规则。
func newRepoFixtureWith(t *testing.T, files map[string]string) repoFixture {
	t.Helper()
	src := t.TempDir()
	gitIn(t, src, "init", "-q", "-b", "main")
	gitIn(t, src, "config", "user.name", "fixture")
	gitIn(t, src, "config", "user.email", "fixture@example.com")
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(src, name), []byte(content), 0o644); err != nil {
			t.Fatalf("写夹具文件失败：%v", err)
		}
	}
	write("README.md", "hello\n")
	for name, content := range files {
		write(name, content)
	}
	gitIn(t, src, "add", "-A")
	gitIn(t, src, "commit", "-qm", "init")

	remote := filepath.Join(t.TempDir(), "remote.git")
	gitIn(t, "", "init", "--bare", "-q", remote)
	gitIn(t, src, "remote", "add", "origin", remote)
	gitIn(t, src, "push", "-q", "origin", "HEAD:refs/heads/main")

	return repoFixture{remote: remote, base: gitIn(t, src, "rev-parse", "HEAD"), branch: "xhunter/e2e-gates"}
}

// runGatedHunt 跑一次带门禁的 Hunt：fixture 带清单、模型按 script 行动、返回退出码与产物。
func runGatedHunt(t *testing.T, manifest string, scripts []string) (int, string, map[string]any, []map[string]any) {
	t.Helper()
	requireGitForE2E(t)
	fx := newRepoFixtureWith(t, map[string]string{"gates.yml": manifest})
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

	taskPath := writeRunInputs(t, tmp, "给仓库加一个 hello.txt\n验收：文件出现在交付提交里")
	setRunEnv(t, fx, srv.URL+"/v1")
	resultPath := filepath.Join(tmp, "out", "result.json")

	stdout, stderr := swapStdStreams(t)
	code := run([]string{"--bounty", taskPath, "--result", resultPath})
	stdoutText, stderrText := drainStdStreams(t, stdout, stderr)
	if code != exitOK && code != exitEnv && code != exitAborted {
		t.Fatalf("退出码异常 %d\nstdout:\n%s\nstderr:\n%s", code, stdoutText, stderrText)
	}

	raw, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("结果文件不可读：%v\nstdout:\n%s\nstderr:\n%s", err, stdoutText, stderrText)
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("结果文件不是 JSON：%v", err)
	}
	return code, stdoutText, result, eventsOf(t, stdoutText)
}

// 必需门禁**跑不起来**（超时）→ 没有质量证据 → 交付失败，但改动照常交付、退出码仍是 0。
func TestEndToEnd_RequiredGateNeverRunFailsWithDelivery(t *testing.T) {
	manifest := "gates:\n" +
		"  - name: unit\n" +
		"    argv: [sleep, 30]\n" +
		"    timeout: 200ms\n" +
		"    required: true\n" +
		"    expect: {kind: exit_zero}\n"
	scripts := []string{
		sseWithToolCall("call_1", "write", `{"path":"hello.txt","content":"hi\n"}`),
	}
	code, stdoutText, result, events := runGatedHunt(t, manifest, scripts)

	if code != exitOK {
		t.Errorf("对话正常走完，退出码应为 0，实际 %d", code)
	}
	if result["status"] != string(huntStatusFailed) {
		t.Errorf("必需门禁从未运行应判失败，实际 %v（reason=%v）", result["status"], result["reason"])
	}
	if result["exit_code"] != float64(0) {
		t.Errorf("退出码应为 0，实际 %v", result["exit_code"])
	}
	// 结果文件必须列出未运行的门禁（passed: null）——不列等于告诉平台"这次没门禁"。
	gate := gateNamed(t, result, "unit")
	if gate == nil {
		t.Fatalf("结果文件缺 unit：%v", result["gates"])
	}
	if _, has := gate["passed"]; !has || gate["passed"] != nil {
		t.Errorf("未运行的门禁应是 passed: null，实际 %v", gate["passed"])
	}
	if gate["source"] != hunt.GateSourceRepo {
		t.Errorf("应标注清单来自基线：%v", gate["source"])
	}
	// 改动照常交付：验收没过不等于代码不能交。
	files, _ := result["files_changed"].([]any)
	if len(files) == 0 {
		t.Errorf("改动应照常交付：%v", result["files_changed"])
	}
	if sha, _ := result["commit_sha"].(string); sha == "" {
		t.Errorf("交付提交应存在：%v", result["commit_sha"])
	}
	// 跑不起来必须可见：静默跳过会让平台以为这次没有门禁。
	if !hasDegradedWithScope(events, "gate") {
		t.Errorf("门禁跑不起来应发一条 degraded(scope: gate)：\n%s", stdoutText)
	}
}

// 必需门禁**未通过** → 失败并留下证据，改动照常交付。
func TestEndToEnd_GateFailureFailsButStillDelivers(t *testing.T) {
	manifest := "gates:\n" +
		"  - name: unit\n" +
		"    argv: [false]\n" +
		"    required: true\n" +
		"    expect: {kind: exit_zero}\n"
	scripts := []string{
		sseWithToolCall("call_1", "write", `{"path":"hello.txt","content":"hi\n"}`),
		sseWithToolCall("call_2", "check", `{"name":"unit"}`),
	}
	code, _, result, events := runGatedHunt(t, manifest, scripts)

	if code != exitOK {
		t.Errorf("退出码应为 0，实际 %d", code)
	}
	if result["status"] != string(huntStatusFailed) {
		t.Errorf("必需门禁未通过应判失败，实际 %v（reason=%v）", result["status"], result["reason"])
	}
	// 结论进事件流：MR 评审要看"跑没跑、过没过"。
	checks := eventsOfType(events, "check_result")
	if len(checks) == 0 {
		t.Fatal("没有 check_result 事件")
	}
	last := checks[len(checks)-1]
	if last["gate"] != "unit" || last["passed"] != false {
		t.Errorf("check_result 应记为未通过：%v", last)
	}
	gate := gateNamed(t, result, "unit")
	if gate == nil {
		t.Fatalf("结果文件缺 unit：%v", result["gates"])
	}
	if gate["passed"] != false {
		t.Errorf("未通过的门禁应是 passed: false，实际 %v", gate["passed"])
	}
	files, _ := result["files_changed"].([]any)
	if len(files) == 0 {
		t.Errorf("改动应照常交付：%v", result["files_changed"])
	}
}

// 对照：必需门禁通过 → 成功交付，且清单来源如实标出。
func TestEndToEnd_RequiredGatePassedSucceeds(t *testing.T) {
	manifest := "gates:\n" +
		"  - name: unit\n" +
		"    argv: [true]\n" +
		"    required: true\n" +
		"    expect: {kind: exit_zero}\n"
	scripts := []string{
		sseWithToolCall("call_1", "write", `{"path":"hello.txt","content":"hi\n"}`),
	}
	code, _, result, _ := runGatedHunt(t, manifest, scripts)

	if code != exitOK {
		t.Errorf("退出码应为 0，实际 %d", code)
	}
	if result["status"] != string(huntStatusSucceeded) {
		t.Errorf("必需门禁通过应判成功，实际 %v（reason=%v）", result["status"], result["reason"])
	}
	gate := gateNamed(t, result, "unit")
	if gate == nil || gate["passed"] != true {
		t.Errorf("通过的门禁应为 passed: true：%v", gate)
	}
}

// ============================================================ 断言辅助

// huntStatusFailed / huntStatusSucceeded 用字面量而不是引用 harness 常量：
// 引用被测常量本身做期望值，会把"常量被改掉"这件事变绿。
const (
	huntStatusFailed    = "failed"
	huntStatusSucceeded = "succeeded"
)

func eventsOf(t *testing.T, stdout string) []map[string]any {
	t.Helper()
	out := make([]map[string]any, 0)
	for _, line := range nonEmptyLines(stdout) {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("stdout 出现非事件行：%q", line)
		}
		out = append(out, ev)
	}
	return out
}

func eventsOfType(events []map[string]any, kind string) []map[string]any {
	var out []map[string]any
	for _, ev := range events {
		if ev["type"] == kind {
			out = append(out, ev)
		}
	}
	return out
}

func hasDegradedWithScope(events []map[string]any, scope string) bool {
	for _, ev := range eventsOfType(events, "degraded") {
		if ev["scope"] == scope {
			return true
		}
	}
	return false
}

func gateNamed(t *testing.T, result map[string]any, name string) map[string]any {
	t.Helper()
	raw, _ := result["gates"].([]any)
	for _, item := range raw {
		g, _ := item.(map[string]any)
		if g["name"] == name {
			return g
		}
	}
	return nil
}

// 门禁命令在 PATH 上要存在，否则夹具仓库根本起不来（元门禁会拒掉清单）。
func TestGateE2E_CommandsExistOnThisHost(t *testing.T) {
	for _, cmd := range []string{"true", "false", "sleep"} {
		if _, err := exec.LookPath(cmd); err != nil {
			t.Skipf("环境缺命令 %q：%v", cmd, err)
		}
	}
}

var _ = strings.TrimSpace
