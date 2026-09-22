package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// 端到端：真实二进制路径（run → 组装 → 引擎 → 协议客户端 → 工具执行 → git 提交）
// 跑通一次完整 Hunt——基线 → 至少一轮 → 交付提交 → hunt_end。
//
// 只有"真 git + 真 SSE 假上游"能证明这条链路成立：之前每个包各自绿，主链路却
// 断在第一行（git 四动作全是桩）。夹具仓库是本地的裸仓库，假上游是本地 httptest，
// 全程不联网、不碰开发机的 ~/.xhunter 与 ~/.gitconfig。
func TestEndToEnd_LocalRunProducesDeliveryCommit(t *testing.T) {
	requireGitForE2E(t)
	fx := newRepoFixture(t)
	tmp := t.TempDir()
	// TMPDIR 决定 git 实现建临时工作树的位置：指到独立目录，才能断言"收尾清理过"。
	workParent := filepath.Join(tmp, "work")
	if err := os.MkdirAll(workParent, 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	t.Setenv("TMPDIR", workParent)
	t.Setenv("HOME", filepath.Join(tmp, "home"))

	const task = "给仓库加一个 hello.txt\n验收：文件出现在交付提交里"
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if atomic.AddInt32(&calls, 1) == 1 {
			// 第一轮：要一个 write 调用（参数是 JSON 字符串，与对话补全协议一致）。
			io.WriteString(w, sseWithToolCall("call_1", "write", `{"path":"hello.txt","content":"hi\n"}`))
		} else {
			// 第二轮：不再要工具 → 引擎判"模型认为做完了"。
			io.WriteString(w, sseWithText("完成"))
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer srv.Close()

	taskPath, cfgPath := writeRunInputs(t, tmp, task, srv.URL+"/v1")
	setRunEnv(t, taskPath, cfgPath, fx)

	resultPath := filepath.Join(tmp, "out", "result.json") // 父目录不存在：装配层要建
	patchPath := filepath.Join(tmp, "out", "delivery.patch")

	stdout, stderr := swapStdStreams(t)
	code := run([]string{"--bounty", taskPath, "--result", resultPath, "--patch", patchPath})
	stdoutText, stderrText := drainStdStreams(t, stdout, stderr)

	if code != exitOK {
		t.Fatalf("完整一次 Hunt 应成功退出（0），实际 %d\nstdout:\n%s\nstderr:\n%s", code, stdoutText, stderrText)
	}

	// 事件流：全是合法事件行，且关键节点都在。
	types := map[string]map[string]any{}
	for _, line := range nonEmptyLines(stdoutText) {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("stdout 出现非事件行：%q", line)
		}
		name, _ := ev["type"].(string)
		if name == "" {
			t.Fatalf("事件缺 type：%q", line)
		}
		types[name] = ev
	}
	for _, want := range []string{"tool_result", "deliverable", "hunt_end"} {
		if _, ok := types[want]; !ok {
			t.Errorf("事件流缺少 %s：\n%s", want, stdoutText)
		}
	}
	if got := types["hunt_end"]["status"]; got != "succeeded" {
		t.Errorf("hunt_end.status = %v，期望 succeeded", got)
	}
	if got := types["hunt_end"]["reason"]; got != "no_tool_call" {
		t.Errorf("hunt_end.reason = %v，期望 no_tool_call", got)
	}
	if got := types["tool_result"]["ok"]; got != true {
		t.Errorf("tool_result.ok = %v，期望 true", got)
	}
	if strings.Contains(stderrText, `"type":`) {
		t.Errorf("事件泄漏到 stderr：%q", stderrText)
	}

	// 交付物在远端的任务分支上。注意"交付 = 分支 tip"：本轮改动已在轮边界被检查点
	// 提交，收尾**不产生空提交**（FR-1.3c「无新写操作不提交」），因此提交数是
	// 基线 + 检查点 = 2，tip 是检查点提交。收尾提交只在有未提交改动时才有意义。
	tip := gitIn(t, fx.remote, "rev-parse", "refs/heads/"+fx.branch)
	if tip == fx.base {
		t.Fatal("远端任务分支没有推进——交付没有发生")
	}
	if n := gitIn(t, fx.remote, "rev-list", "--count", "refs/heads/"+fx.branch); n != "2" {
		t.Errorf("提交数 = %s，期望 2（基线 + 轮边界检查点，收尾不得产生空提交）", n)
	}
	if msg := gitIn(t, fx.remote, "log", "-1", "--format=%s", "refs/heads/"+fx.branch); !strings.Contains(msg, "检查点") {
		t.Errorf("tip 的提交信息 = %q，期望轮边界检查点", msg)
	}
	if got := gitIn(t, fx.remote, "show", tip+":hello.txt"); got != "hi" {
		t.Errorf("交付提交里的文件内容 = %q，期望 hi", got)
	}

	// 附带交付：deliverable 列出改动文件。
	files, _ := types["deliverable"]["files"].([]any)
	if len(files) != 1 || files[0] != "hello.txt" {
		t.Errorf("deliverable.files = %v，期望 [hello.txt]", types["deliverable"]["files"])
	}

	// 结果文件（FR-1.5、使用手册 §6）：交付记录必须能独立读出来。
	var res resultFile
	raw, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("结果文件未写出：%v", err)
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("结果文件不是合法 JSON：%v\n%s", err, raw)
	}
	if res.Status != "succeeded" || res.ExitCode != exitOK || res.BountyID == "" {
		t.Errorf("结果文件终态不对：%+v", res)
	}
	if res.Branch != fx.branch || res.BaseCommit != fx.base {
		t.Errorf("结果文件缺少任务事实：%+v", res)
	}
	if res.CommitSHA == "" || res.CommitSHA != tip {
		t.Errorf("commit_sha = %q，期望交付提交 %q", res.CommitSHA, tip)
	}
	if len(res.FilesChanged) != 1 || res.FilesChanged[0] != "hello.txt" {
		t.Errorf("files_changed = %v", res.FilesChanged)
	}
	if res.Usage.Turns != 2 || res.Usage.InputTokens != 32 || res.Usage.OutputTokens != 10 {
		t.Errorf("usage 不对（两轮共 32/10）：%+v", res.Usage)
	}
	if res.PatchPath != patchPath {
		t.Errorf("patch_path = %q，期望 %q", res.PatchPath, patchPath)
	}
	if res.Error != nil {
		t.Errorf("成功路径不该有 error：%+v", res.Error)
	}

	// 补丁必须能应用到干净基线上——"能应用"是 patch 交付的定义（FR-6.1、AC-1）。
	patch, err := os.ReadFile(patchPath)
	if err != nil {
		t.Fatalf("补丁文件未写出：%v", err)
	}
	if !strings.Contains(string(patch), "hello.txt") {
		t.Errorf("补丁里应含改动文件：%s", patch)
	}
	applyDir := filepath.Join(tmp, "apply")
	gitIn(t, "", "clone", "-q", fx.remote, applyDir)
	gitIn(t, applyDir, "checkout", "-q", fx.base)
	apply := exec.Command("git", "apply", "--check", patchPath)
	apply.Dir = applyDir
	if out, err := apply.CombinedOutput(); err != nil {
		t.Fatalf("补丁不能应用到基线：%v\n%s\n--- patch ---\n%s", err, out, patch)
	}

	// 收尾清理：临时工作树不留残骸。
	assertDirEmpty(t, workParent)
}

// SIGTERM 的进程级断言（AC-6）：上游挂住不响应时收到信号 → 退出 3、
// 事件流以 hunt_end{cancelled} 收尾、临时工作树被回收。
func TestEndToEnd_SigtermConvergesToCancelled(t *testing.T) {
	requireGitForE2E(t)
	fx := newRepoFixture(t)
	tmp := t.TempDir()
	workParent := filepath.Join(tmp, "work")
	if err := os.MkdirAll(workParent, 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	t.Setenv("TMPDIR", workParent)
	t.Setenv("HOME", filepath.Join(tmp, "home"))

	// 收到请求就不响应，直到客户端断开——模拟"流挂在半路"。
	received := make(chan struct{})
	var once int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if atomic.AddInt32(&once, 1) == 1 {
			close(received)
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	taskPath, cfgPath := writeRunInputs(t, tmp, "随便做点什么", srv.URL+"/v1")
	setRunEnv(t, taskPath, cfgPath, fx)

	stdout, stderr := swapStdStreams(t)
	done := make(chan int, 1)
	go func() { done <- run([]string{"--bounty", taskPath}) }()

	select {
	case <-received:
	case <-time.After(10 * time.Second):
		t.Fatal("上游始终没收到请求：端到端链路没走到模型调用")
	}
	// 信号只在运行段被接管：此刻 handler 已注册（signalContext）。
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Skipf("环境不支持发送信号：%v", err)
	}

	var code int
	select {
	case code = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("收到 SIGTERM 后没有在约定时间内收敛")
	}
	stdoutText, stderrText := drainStdStreams(t, stdout, stderr)

	if code != exitCancelled {
		t.Fatalf("取消应退出 3，实际 %d\nstdout:\n%s\nstderr:\n%s", code, stdoutText, stderrText)
	}
	if !strings.Contains(stdoutText, `"status":"cancelled"`) {
		t.Errorf("事件流应以 hunt_end{cancelled} 收尾：\n%s", stdoutText)
	}
	if !strings.Contains(stdoutText, `"type":"hunt_end"`) {
		t.Errorf("取消也必须上报终态（INV-3）：\n%s", stdoutText)
	}
	assertDirEmpty(t, workParent)
}

// 结果文件在**失败路径**上同样要写出（FR-1.5："结束时写出结果文件"）：平台靠它
// 记账与决定是否重派，所以失败时 error.kind 与 retryable 必须齐。
func TestEndToEnd_ResultFileWrittenOnFailure(t *testing.T) {
	requireGitForE2E(t)
	tmp := t.TempDir()
	t.Setenv("HOME", filepath.Join(tmp, "home"))
	t.Setenv("TMPDIR", filepath.Join(tmp, "work"))

	taskPath, cfgPath := writeRunInputs(t, tmp, "做点什么", "http://127.0.0.1:1/v1")
	t.Setenv("XHUNTER_REPO_URL", filepath.Join(tmp, "no-such-repo.git"))
	t.Setenv("XHUNTER_REPO_BASE_COMMIT", "0123456789abcdef0123456789abcdef01234567")
	t.Setenv("XHUNTER_REPO_BRANCH", "xhunter/fail")
	t.Setenv("XHUNTER_PROVIDER", "localgw")
	t.Setenv("XHUNTER_MODEL", "test-model")
	t.Setenv("XHUNTER_PROVIDER_CONFIG", cfgPath)

	resultPath := filepath.Join(tmp, "result.json")
	stdout, stderr := swapStdStreams(t)
	code := run([]string{"--bounty", taskPath, "--result", resultPath})
	_, stderrText := drainStdStreams(t, stdout, stderr)

	if code != exitEnv {
		t.Fatalf("远端不可达应退出 2，实际 %d\n%s", code, stderrText)
	}
	raw, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("失败路径也必须写出结果文件：%v", err)
	}
	var res resultFile
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("结果文件不是合法 JSON：%v", err)
	}
	if res.Status != "failed" || res.ExitCode != exitEnv {
		t.Errorf("终态不对：%+v", res)
	}
	if res.Error == nil {
		t.Fatalf("失败必须带 error 对象：%+v", res)
	}
	if res.Error.Kind == "" || !res.Error.Retryable {
		t.Errorf("环境问题应标可重试且给出 kind：%+v", res.Error)
	}
	if !strings.Contains(res.Error.Message, res.Error.Kind) {
		t.Errorf("message 应含完整原因：%+v", res.Error)
	}
	if res.Branch != "xhunter/fail" || res.BaseCommit == "" {
		t.Errorf("结果文件缺少任务事实：%+v", res)
	}
}

// 预算耗尽必须立即终止并上报耗尽维度（FR-9、AC-5）：环境变量 → Bounty → 策略 →
// 轮末守卫 → 终态，整条链一起验。上游每轮都要求工具调用（模型"不打算停"），
// 因此唯一的收敛点就是预算。
func TestEndToEnd_BudgetExhaustionStopsTheRun(t *testing.T) {
	requireGitForE2E(t)
	fx := newRepoFixture(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", filepath.Join(tmp, "work"))
	t.Setenv("HOME", filepath.Join(tmp, "home"))

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		atomic.AddInt32(&calls, 1)
		// 每轮都读 README.md：调用总能成功，因此不会触发失败止损，
		// 只有轮数预算能把它停下来。
		io.WriteString(w, sseWithToolCall("c", "read", `{"path":"README.md"}`))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer srv.Close()

	taskPath, cfgPath := writeRunInputs(t, tmp, "随便看看", srv.URL+"/v1")
	setRunEnv(t, taskPath, cfgPath, fx)
	t.Setenv(envMaxTurns, "2")

	stdout, stderr := swapStdStreams(t)
	code := run([]string{"--bounty", taskPath})
	stdoutText, stderrText := drainStdStreams(t, stdout, stderr)

	if code != exitFailed {
		t.Fatalf("预算耗尽应退出 1，实际 %d\nstdout:\n%s\nstderr:\n%s", code, stdoutText, stderrText)
	}
	if !strings.Contains(stdoutText, `"status":"failed"`) || !strings.Contains(stdoutText, "budget_exhausted:turns") {
		t.Errorf("终态应上报耗尽维度：\n%s", stdoutText)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("轮数预算为 2，上游应恰好被调用 2 次，实际 %d", got)
	}
}

// 通道断裂即环境错误（FR-10.4、AC-19）：消费者已不在通道上时，继续跑只是自说自话。
// 这里让 stdout 指向一个**读端已关**的管道：写入得到 EPIPE，进程必须以退出码 2
// 收敛并记下原因——而不是把"写不出去"降级成一条 warn。
//
// 场景取"本来会成功"的那条路：否则退出码 2 无法与任务自身的失败区分开。
func TestEndToEnd_BrokenEventChannelIsEnvError(t *testing.T) {
	requireGitForE2E(t)
	fx := newRepoFixture(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", filepath.Join(tmp, "work"))
	t.Setenv("HOME", filepath.Join(tmp, "home"))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, sseWithText("做完了"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer srv.Close()

	taskPath, cfgPath := writeRunInputs(t, tmp, "随便看看", srv.URL+"/v1")
	setRunEnv(t, taskPath, cfgPath, fx)

	// stderr 收进文件；stdout 接一个读端已关的管道。
	stderrFile, err := os.CreateTemp(t.TempDir(), "stderr-*")
	if err != nil {
		t.Fatalf("建临时 stderr 失败：%v", err)
	}
	oldErr := os.Stderr
	os.Stderr = stderrFile
	t.Cleanup(func() { os.Stderr = oldErr })

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("建管道失败：%v", err)
	}
	reader.Close() // 读端关掉：写入必然失败
	oldOut := os.Stdout
	os.Stdout = writer
	t.Cleanup(func() { os.Stdout = oldOut; writer.Close() })

	code := run([]string{"--bounty", taskPath})

	os.Stderr = oldErr
	if err := stderrFile.Sync(); err != nil {
		t.Fatalf("刷新 stderr 失败：%v", err)
	}
	errText, err := os.ReadFile(stderrFile.Name())
	if err != nil {
		t.Fatalf("读取 stderr 失败：%v", err)
	}
	_ = writer.Close()

	if code != exitEnv {
		t.Fatalf("事件写不出去必须以退出码 2 收敛，实际 %d\nstderr:\n%s", code, errText)
	}
	if !strings.Contains(string(errText), "事件通道写入失败") {
		t.Errorf("必须记下通道断裂的原因：\n%s", errText)
	}
}

// ============================================================ 夹具

type repoFixture struct {
	remote string
	base   string
	branch string
}

// newRepoFixture 造一个远端裸仓库：main 上一条基线提交，任务分支由被测程序创建。
func newRepoFixture(t *testing.T) repoFixture {
	t.Helper()
	src := t.TempDir()
	gitIn(t, src, "init", "-q", "-b", "main")
	gitIn(t, src, "config", "user.name", "fixture")
	gitIn(t, src, "config", "user.email", "fixture@example.com")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("写夹具文件失败：%v", err)
	}
	gitIn(t, src, "add", "-A")
	gitIn(t, src, "commit", "-qm", "init")

	remote := filepath.Join(t.TempDir(), "remote.git")
	gitIn(t, "", "init", "--bare", "-q", remote)
	gitIn(t, src, "remote", "add", "origin", remote)
	gitIn(t, src, "push", "-q", "origin", "HEAD:refs/heads/main")

	return repoFixture{remote: remote, base: gitIn(t, src, "rev-parse", "HEAD"), branch: "xhunter/e2e"}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("夹具 git %s 失败：%v\n%s", strings.Join(args, " "), err, errb.String())
	}
	return strings.TrimSpace(out.String())
}

func requireGitForE2E(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("环境没有 git：%v", err)
	}
}

func writeRunInputs(t *testing.T, dir, task, baseURL string) (taskPath, cfgPath string) {
	t.Helper()
	taskPath = filepath.Join(dir, "task.txt")
	if err := os.WriteFile(taskPath, []byte(task+"\n"), 0o644); err != nil {
		t.Fatalf("写任务文件失败：%v", err)
	}
	cfgPath = filepath.Join(dir, "provider.json")
	cfg := fmt.Sprintf(`{"provider":{"localgw":{"npm":"@ai-sdk/openai-compatible",
      "options":{"baseURL":%q},
      "models":{"test-model":{"limit":{"context":200000,"output":8192}}}}}}`, baseURL)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("写 provider 配置失败：%v", err)
	}
	return taskPath, cfgPath
}

func setRunEnv(t *testing.T, taskPath, cfgPath string, fx repoFixture) {
	t.Helper()
	t.Setenv("XHUNTER_REPO_URL", fx.remote)
	t.Setenv("XHUNTER_REPO_BASE_COMMIT", fx.base)
	t.Setenv("XHUNTER_REPO_BRANCH", fx.branch)
	t.Setenv("XHUNTER_PROVIDER", "localgw")
	t.Setenv("XHUNTER_MODEL", "test-model")
	t.Setenv("XHUNTER_PROVIDER_CONFIG", cfgPath)
}

// assertDirEmpty 断言目录里没有残骸（临时工作树被回收）。
func assertDirEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取 %s 失败：%v", dir, err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("收尾没有清理干净，残留：%v", names)
	}
}

// ============================================================ 假上游的 SSE 形状

func sseWithToolCall(id, name, args string) string {
	delta := fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":%q,"function":{"name":%q,"arguments":%q}}]},"finish_reason":null}]}`,
		id, name, args)
	return "data: " + delta + "\n\n" +
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		`data: {"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":7}}` + "\n\n" +
		"data: [DONE]\n\n"
}

func sseWithText(text string) string {
	delta := fmt.Sprintf(`{"choices":[{"delta":{"content":%q},"finish_reason":null}]}`, text)
	return "data: " + delta + "\n\n" +
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		`data: {"choices":[],"usage":{"prompt_tokens":20,"completion_tokens":3}}` + "\n\n" +
		"data: [DONE]\n\n"
}
