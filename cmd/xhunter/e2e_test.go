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
// 全程不联网、不碰开发机的 ~/.gitconfig。
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

	taskPath := writeRunInputs(t, tmp, task)
	setRunEnv(t, fx, srv.URL+"/v1")

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

	// 交付物在远端的任务分支上。"交付 = 分支 tip"。本例的改动**不落在结构完整点上**
	// （符号扩展未接入 → 判据不可判定 → 不产生自动检查点），因此轮边界不提交，
	// tip 是收尾的**交付提交**（FR-1.3c：无新写操作不提交，收尾必提交）。
	tip := gitIn(t, fx.remote, "rev-parse", "refs/heads/"+fx.branch)
	if tip == fx.base {
		t.Fatal("远端任务分支没有推进——交付没有发生")
	}
	if n := gitIn(t, fx.remote, "rev-list", "--count", "refs/heads/"+fx.branch); n != "2" {
		t.Errorf("提交数 = %s，期望 2（基线 + 收尾交付提交）", n)
	}
	if msg := gitIn(t, fx.remote, "log", "-1", "--format=%s", "refs/heads/"+fx.branch); !strings.Contains(msg, "任务改动") {
		t.Errorf("tip 的提交信息 = %q，期望收尾交付提交", msg)
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
	// 用量口径贯穿全链：上游明细 → 协议翻译 → 循环累加 → 结果文件。
	if res.Usage.Turns != 2 || res.Usage.InputTokens != 32 || res.Usage.OutputTokens != 10 {
		t.Errorf("usage 不对（两轮共 32/10）：%+v", res.Usage)
	}
	if res.Usage.CachedInputTokens != 24 {
		t.Errorf("cached_input_tokens = %d，期望 24（两轮 8+16）：缓存读要一路带到结果文件",
			res.Usage.CachedInputTokens)
	}
	if !res.Usage.Reported {
		t.Error("上游回报过用量，usage.reported 应为 true")
	}
	if res.PatchPath != patchPath {
		t.Errorf("patch_path = %q，期望 %q", res.PatchPath, patchPath)
	}
	if res.Error != nil {
		t.Errorf("成功路径不该有 error：%+v", res.Error)
	}

	// 轮级事件补齐：assistant_text 必须在事件流里，且结果文件 summary 等于最后一轮答复
	// （"任务就是要产出一份小结"时，那份答复就是交付物）。
	if _, ok := types["assistant_text"]; !ok {
		t.Errorf("事件流缺少 assistant_text：\n%s", stdoutText)
	}
	if got := types["assistant_text"]["text"]; got != "完成" {
		t.Errorf("assistant_text.text = %v，期望最后一轮答复 完成", got)
	}
	if res.Summary != "完成" {
		t.Errorf("summary = %q，期望最后一轮答复 完成", res.Summary)
	}
	if _, ok := types["tool_call"]; !ok {
		t.Errorf("事件流缺少 tool_call：\n%s", stdoutText)
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

	taskPath := writeRunInputs(t, tmp, "随便做点什么")
	setRunEnv(t, fx, srv.URL+"/v1")

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

	taskPath := writeRunInputs(t, tmp, "做点什么")
	setProviderEnv(t, "http://127.0.0.1:1/v1")
	t.Setenv("XHUNTER_REPO_URL", filepath.Join(tmp, "no-such-repo.git"))
	t.Setenv("XHUNTER_REPO_BASE_COMMIT", "0123456789abcdef0123456789abcdef01234567")
	t.Setenv("XHUNTER_REPO_BRANCH", "xhunter/fail")

	resultPath := filepath.Join(tmp, "result.json")
	stdout, stderr := swapStdStreams(t)
	code := run([]string{"--bounty", taskPath, "--result", resultPath})
	stdoutText, stderrText := drainStdStreams(t, stdout, stderr)

	if code != exitEnv {
		t.Fatalf("远端不可达应退出 1，实际 %d\n%s", code, stderrText)
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

	// 终态事件带累计用量（与结果文件同源）；失败另发一条结构化 error，与结果文件 error 同源。
	end := eventPayload(t, stdoutText, "hunt_end")
	usage, _ := end["usage"].(map[string]any)
	if usage == nil {
		t.Errorf("hunt_end 必须带累计用量：%v", end)
	} else if usage["reported"] != false {
		t.Errorf("上游不可达时 usage.reported 应为 false：%v", usage)
	}
	errEv := eventPayload(t, stdoutText, "error")
	if errEv["kind"] != res.Error.Kind || errEv["retryable"] != res.Error.Retryable {
		t.Errorf("error 事件必须与结果文件同源：event=%v file=%+v", errEv, res.Error)
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

	taskPath := writeRunInputs(t, tmp, "随便看看")
	setRunEnv(t, fx, srv.URL+"/v1")
	t.Setenv(envBudgetTurns, "2")

	stdout, stderr := swapStdStreams(t)
	code := run([]string{"--bounty", taskPath})
	stdoutText, stderrText := drainStdStreams(t, stdout, stderr)

	if code != exitAborted {
		t.Fatalf("预算耗尽应退出 2，实际 %d\nstdout:\n%s\nstderr:\n%s", code, stdoutText, stderrText)
	}
	if !strings.Contains(stdoutText, `"status":"failed"`) || !strings.Contains(stdoutText, "budget_exhausted:turns") {
		t.Errorf("终态应上报耗尽维度：\n%s", stdoutText)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("轮数预算为 2，上游应恰好被调用 2 次，实际 %d", got)
	}

	// 终态带累计用量；预算耗尽是被引擎中止 → error.retryable 为 false。
	if _, ok := eventPayload(t, stdoutText, "hunt_end")["usage"].(map[string]any); !ok {
		t.Errorf("hunt_end 必须带累计用量：\n%s", stdoutText)
	}
	errEv := eventPayload(t, stdoutText, "error")
	if errEv["kind"] != "budget_exhausted" || errEv["retryable"] != false {
		t.Errorf("预算耗尽应报 budget_exhausted 且不可重试：%v", errEv)
	}
}

// 通道断裂即环境错误（FR-10.4、AC-19）：消费者已不在通道上时，继续跑只是自说自话。
// 这里让 stdout 指向一个**读端已关**的管道：写入得到 EPIPE，进程必须以退出码 1
// 收敛并记下原因——而不是把"写不出去"降级成一条 warn。
//
// 场景取"本来会成功"的那条路：否则退出码 1 无法与任务自身的失败区分开。
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

	taskPath := writeRunInputs(t, tmp, "随便看看")
	setRunEnv(t, fx, srv.URL+"/v1")

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

	resultPath := filepath.Join(tmp, "result.json")
	code := run([]string{"--bounty", taskPath, "--result", resultPath})

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
		t.Fatalf("事件写不出去必须以退出码 1 收敛，实际 %d\nstderr:\n%s", code, errText)
	}
	if !strings.Contains(string(errText), "事件通道写入失败") {
		t.Errorf("必须记下通道断裂的原因：\n%s", errText)
	}

	// 通道断裂由轮边界复查收敛（不止进程末尾收口）：终态应为 event_channel_failed / 环境错误。
	raw, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("结果文件未写出：%v", err)
	}
	var res resultFile
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("结果文件不是合法 JSON：%v", err)
	}
	if res.Status != "failed" || res.ExitCode != exitEnv {
		t.Errorf("通道断裂终态不对：%+v", res)
	}
	if !strings.HasPrefix(res.Reason, "event_channel_failed") {
		t.Errorf("reason = %q，期望以 event_channel_failed 开头", res.Reason)
	}
}

// 心跳端到端（FR-10.1、IA-5.4）：间隔调到很小，一次成功运行的事件流里至少一条 heartbeat，
// 且最后一条是 hunt_end——心跳绝不能在终态之后继续滴答。
func TestEndToEnd_HeartbeatEmittedAndHuntEndLast(t *testing.T) {
	requireGitForE2E(t)
	fx := newRepoFixture(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", filepath.Join(tmp, "work"))
	t.Setenv("HOME", filepath.Join(tmp, "home"))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		time.Sleep(40 * time.Millisecond) // 模拟模型延迟：给"等模型时的任务级心跳"留出滴答窗口
		io.WriteString(w, sseWithText("做完了"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer srv.Close()

	taskPath := writeRunInputs(t, tmp, "产出一份小结")
	setRunEnv(t, fx, srv.URL+"/v1")
	t.Setenv("XHUNTER_HEARTBEAT_INTERVAL", "5ms")

	stdout, stderr := swapStdStreams(t)
	code := run([]string{"--bounty", taskPath})
	stdoutText, stderrText := drainStdStreams(t, stdout, stderr)

	if code != exitOK {
		t.Fatalf("完整一次 Hunt 应成功退出（0），实际 %d\nstdout:\n%s\nstderr:\n%s", code, stdoutText, stderrText)
	}

	if n := strings.Count(stdoutText, `"type":"heartbeat"`); n < 1 {
		t.Errorf("按 5ms 间隔应至少一条 heartbeat：\n%s", stdoutText)
	}
	// 心跳的载荷形状（IA-5.4）：phase ＋ elapsed_ms。
	hb := eventPayload(t, stdoutText, "heartbeat")
	if hb["phase"] == "" || hb["elapsed_ms"] == nil {
		t.Errorf("heartbeat 应带 phase 与 elapsed_ms：%v", hb)
	}
	// 最后一条必须是 hunt_end（心跳不得滴答到终态之后）。
	lines := nonEmptyLines(stdoutText)
	if last := lines[len(lines)-1]; !strings.Contains(last, `"type":"hunt_end"`) {
		t.Errorf("hunt_end 必须是最后一条，实得：%s", last)
	}
}

// successRun 是一次成功运行的整段输出与结果文件路径。
type successRun struct {
	stdout string
	stderr string
	code   int
	result string
}

// runSuccessOnce 跑一次成功运行（真 git 夹具 + 本地假上游：第 1 轮写文件、第 2 轮收尾），
// 返回整段 stdout/stderr、退出码与结果文件路径。
func runSuccessOnce(t *testing.T) successRun {
	t.Helper()
	requireGitForE2E(t)
	fx := newRepoFixture(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", filepath.Join(tmp, "work"))
	t.Setenv("HOME", filepath.Join(tmp, "home"))

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if atomic.AddInt32(&calls, 1) == 1 {
			io.WriteString(w, sseWithToolCall("call_1", "write", `{"path":"hello.txt","content":"hi\n"}`))
		} else {
			io.WriteString(w, sseWithText("完成"))
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(srv.Close)

	taskPath := writeRunInputs(t, tmp, "给仓库加一个 hello.txt")
	setRunEnv(t, fx, srv.URL+"/v1")

	resultPath := filepath.Join(tmp, "out", "result.json")
	stdout, stderr := swapStdStreams(t)
	code := run([]string{"--bounty", taskPath, "--result", resultPath})
	stdoutText, stderrText := drainStdStreams(t, stdout, stderr)
	return successRun{stdout: stdoutText, stderr: stderrText, code: code, result: resultPath}
}

// IA-5.2 / AC-9：stdout 逐行都是合法事件行（含信封四字段、ts 为 RFC3339），无任何杂质；
// 同时 stderr 有人类日志——证明两条通道都活着，不是"什么都没输出"通过的。
func TestEventSink_StdoutIsPureNDJSON(t *testing.T) {
	r := runSuccessOnce(t)
	if r.code != exitOK {
		t.Fatalf("成功运行应退出 0，实际 %d\nstdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
	}

	lines := nonEmptyLines(r.stdout)
	if len(lines) == 0 {
		t.Fatal("stdout 上没有任何事件行")
	}
	for i, line := range lines {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("第 %d 行不是合法 JSON（stdout 只允许事件行）：%q（%v）", i+1, line, err)
		}
		if typ, ok := ev["type"].(string); !ok || typ == "" {
			t.Errorf("第 %d 行缺 type：%q", i+1, line)
		}
		for _, k := range []string{"bounty_id", "trace_id", "ts"} {
			if v, ok := ev[k].(string); !ok || v == "" {
				t.Errorf("第 %d 行缺信封字段 %s：%q", i+1, k, line)
			}
		}
		ts, _ := ev["ts"].(string)
		if _, err := time.Parse(time.RFC3339, ts); err != nil {
			t.Errorf("第 %d 行 ts 不是 RFC3339：%q", i+1, ts)
		}
	}

	if strings.TrimSpace(r.stderr) == "" {
		t.Error("stderr 应有人类日志——否则无法证明两条通道都活着")
	}
}

// IA-12.6（加强版）：成功运行的事件序列完整、首尾正确、配对正确。
func TestEndToEnd_EventSequenceIsComplete(t *testing.T) {
	r := runSuccessOnce(t)
	if r.code != exitOK {
		t.Fatalf("成功运行应退出 0，实际 %d\nstdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
	}

	type event struct {
		typ     string
		payload map[string]any
		idx     int
	}
	var evs []event
	for i, line := range nonEmptyLines(r.stdout) {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("stdout 出现非事件行：%q", line)
		}
		typ, _ := m["type"].(string)
		evs = append(evs, event{typ: typ, payload: m, idx: i})
	}
	if len(evs) == 0 {
		t.Fatal("stdout 上没有事件")
	}

	present := map[string]bool{}
	for _, e := range evs {
		present[e.typ] = true
	}
	for _, want := range []string{"hunt_start", "tool_call", "tool_result", "usage", "assistant_text", "deliverable", "hunt_end"} {
		if !present[want] {
			t.Errorf("事件流缺少 %s：\n%s", want, r.stdout)
		}
	}
	if evs[0].typ != "hunt_start" {
		t.Errorf("hunt_start 必须是第一条，实得 %q", evs[0].typ)
	}
	if last := evs[len(evs)-1].typ; last != "hunt_end" {
		t.Errorf("hunt_end 必须是最后一条，实得 %q", last)
	}

	callIdx, resultIdx := map[string]int{}, map[string]int{}
	for _, e := range evs {
		id, _ := e.payload["call_id"].(string)
		switch e.typ {
		case "tool_call":
			if _, ok := callIdx[id]; !ok {
				callIdx[id] = e.idx
			}
		case "tool_result":
			resultIdx[id] = e.idx
		}
	}
	for id, ci := range callIdx {
		ri, ok := resultIdx[id]
		if !ok {
			t.Errorf("call_id %q 的 tool_call 没有对应 tool_result", id)
			continue
		}
		if ci > ri {
			t.Errorf("call_id %q 的 tool_call 必须排在 tool_result 之前（%d > %d）", id, ci, ri)
		}
	}

	deliverableIdx, huntEndIdx := -1, -1
	for _, e := range evs {
		switch e.typ {
		case "deliverable":
			deliverableIdx = e.idx
		case "hunt_end":
			huntEndIdx = e.idx
		}
	}
	if deliverableIdx < 0 || huntEndIdx < 0 || deliverableIdx > huntEndIdx {
		t.Errorf("deliverable 必须在 hunt_end 之前：deliverable=%d hunt_end=%d", deliverableIdx, huntEndIdx)
	}
}

// 生效快照的空集合口径：`ext`（以及 `filters`）是**空数组**而不是 `null`——`[]` = 没有扩展（已知
// 事实），`null` = 未提供（"不知道有没有"）。事件流与结果文件读的是同一份，两处都要是 `[]`。
func TestEndToEnd_EffectiveConfigEmptyCollectionsAreArrays(t *testing.T) {
	r := runSuccessOnce(t)
	if r.code != exitOK {
		t.Fatalf("成功运行应退出 0，实际 %d\nstdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
	}

	// 事件流：hunt_start.effective_config 的集合字段必须是数组（ext/filters 为空数组而非 null）。
	var startLine string
	for _, line := range nonEmptyLines(r.stdout) {
		if strings.Contains(line, `"type":"hunt_start"`) {
			startLine = line
		}
	}
	if startLine == "" {
		t.Fatalf("事件流缺少 hunt_start：\n%s", r.stdout)
	}
	var startEv struct {
		EC map[string]json.RawMessage `json:"effective_config"`
	}
	if err := json.Unmarshal([]byte(startLine), &startEv); err != nil {
		t.Fatalf("hunt_start 不是合法 JSON：%v", err)
	}
	if got := string(startEv.EC["ext"]); got != "[]" {
		t.Errorf("hunt_start.effective_config.ext = %s，期望 []（空数组 = 没有扩展，不是 null）", got)
	}
	if got := string(startEv.EC["filters"]); got != "[]" {
		t.Errorf("hunt_start.effective_config.filters = %s，期望 []", got)
	}
	for _, k := range []string{"system_plugins", "user_plugins"} {
		if v := string(startEv.EC[k]); !strings.HasPrefix(v, "[") {
			t.Errorf("hunt_start.effective_config.%s = %s，期望数组（空也要 []，不是 null）", k, v)
		}
	}

	// 结果文件：读同一份快照，ext 同样必须是 []。
	raw, err := os.ReadFile(r.result)
	if err != nil {
		t.Fatalf("读结果文件失败：%v", err)
	}
	var res struct {
		EC map[string]json.RawMessage `json:"effective_config"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("结果文件不是合法 JSON：%v", err)
	}
	if got := string(res.EC["ext"]); got != "[]" {
		t.Errorf("结果文件 effective_config.ext = %s，期望 []", got)
	}
	if got := string(res.EC["filters"]); got != "[]" {
		t.Errorf("结果文件 effective_config.filters = %s，期望 []", got)
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

// writeRunInputs 只写任务正文：部署事实（仓库与模型接入）全部走环境变量，
// 因此这里不再产出任何"配置文件"。
func writeRunInputs(t *testing.T, dir, task string) string {
	t.Helper()
	taskPath := filepath.Join(dir, "task.txt")
	if err := os.WriteFile(taskPath, []byte(task+"\n"), 0o644); err != nil {
		t.Fatalf("写任务文件失败：%v", err)
	}
	return taskPath
}

// setProviderEnv 投递模型接入事实。刻意不设 XHUNTER_PROTOCOL：缺省协议也要能用。
func setProviderEnv(t *testing.T, baseURL string) {
	t.Helper()
	t.Setenv("XHUNTER_MODEL", "test-model")
	t.Setenv("XHUNTER_BASE_URL", baseURL)
	t.Setenv("XHUNTER_MODEL_CONTEXT_TOKENS", "200000")
	t.Setenv("XHUNTER_MODEL_OUTPUT_TOKENS", "8192")
}

func setRunEnv(t *testing.T, fx repoFixture, baseURL string) {
	t.Helper()
	t.Setenv("XHUNTER_REPO_URL", fx.remote)
	t.Setenv("XHUNTER_REPO_BASE_COMMIT", fx.base)
	t.Setenv("XHUNTER_REPO_BRANCH", fx.branch)
	setProviderEnv(t, baseURL)
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

// eventPayload 返回 stdout 事件流里某类型事件的载荷（行已摊平，直接是 map）。缺该类型即失败。
func eventPayload(t *testing.T, stdoutText, typ string) map[string]any {
	t.Helper()
	for _, line := range nonEmptyLines(stdoutText) {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("stdout 出现非事件行：%q", line)
		}
		if ev["type"] == typ {
			return ev
		}
	}
	t.Fatalf("事件流缺少 %s：\n%s", typ, stdoutText)
	return nil
}

// ============================================================ 假上游的 SSE 形状

func sseWithToolCall(id, name, args string) string {
	delta := fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":%q,"function":{"name":%q,"arguments":%q}}]},"finish_reason":null}]}`,
		id, name, args)
	return "data: " + delta + "\n\n" +
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		`data: {"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":8}}}` + "\n\n" +
		"data: [DONE]\n\n"
}

func sseWithText(text string) string {
	delta := fmt.Sprintf(`{"choices":[{"delta":{"content":%q},"finish_reason":null}]}`, text)
	return "data: " + delta + "\n\n" +
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		`data: {"choices":[],"usage":{"prompt_tokens":20,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":16}}}` + "\n\n" +
		"data: [DONE]\n\n"
}
