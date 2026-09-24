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

// 本地驱动端到端：本地驱动从真实仓库探测出 Bounty 清单，并用同一份装配执行一次 Hunt，
// 交付提交落在**远端任务分支**上——而用户的工作仓库毫发无损（工作区是 clone 出来的）。
func TestEndToEnd_LocalRunDrivesBountyFromRepo(t *testing.T) {
	requireGitForE2E(t)
	fx := newLocalRepoFixture(t)
	tmp := t.TempDir()
	workParent := filepath.Join(tmp, "work")
	if err := os.MkdirAll(workParent, 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	t.Setenv("TMPDIR", workParent)
	t.Setenv("HOME", filepath.Join(tmp, "home"))

	// 假上游：第一轮写文件、第二轮收尾 → 收尾交付提交。
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
	defer srv.Close()
	setProviderEnv(t, srv.URL+"/v1")

	beforeStatus := gitIn(t, fx.repo, "status", "--porcelain")

	outPath := filepath.Join(tmp, "bounty.json")
	code, _, stderrText := runCapturingStderr(t, []string{
		"run", "--repo", fx.repo, "--task", "加一个 hello.txt", "--out", outPath,
	})
	if code != exitOK {
		t.Fatalf("本地驱动应成功退出（0），实际 %d\nstderr:\n%s", code, stderrText)
	}

	id := fx.base[:12]
	tip := gitIn(t, fx.remote, "rev-parse", "refs/heads/xhunter/"+id)
	if tip == fx.base {
		t.Fatal("远端任务分支没有推进——交付没有发生")
	}
	if got := gitIn(t, fx.remote, "show", tip+":hello.txt"); got != "hi" {
		t.Errorf("交付提交里的文件内容 = %q，期望 hi", got)
	}

	// Bounty 清单与执行同源。
	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("Bounty 清单未写出：%v", err)
	}
	var m bountyManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("Bounty 清单不是合法 JSON：%v\n%s", err, raw)
	}
	if m.Task != "加一个 hello.txt" {
		t.Errorf("task = %q", m.Task)
	}
	if m.Remote != fx.remote || m.BaseCommit != fx.base || m.Branch != "xhunter/"+id {
		t.Errorf("仓库事实不对：%+v", m)
	}
	if m.BountyID != id || m.SessionID != id {
		t.Errorf("两个标识应取基线前 12 位：%+v", m)
	}
	if m.GateCandidate {
		t.Error("夹具仓库根没有 gates.yml，gate_candidate 应为 false")
	}

	// 用户的工作仓库未被改动——这是"克隆到临时工作区"的直接证据。
	if got := gitIn(t, fx.repo, "status", "--porcelain"); got != beforeStatus {
		t.Errorf("用户仓库被改动了：%q -> %q", beforeStatus, got)
	}
	assertDirEmpty(t, workParent)
}

// FR-1.10 边界①：本地驱动必须 clone 到临时工作区，绝不把用户仓库目录当工作区。
// 这里断言"用户仓库 HEAD/分支/状态三不变，而交付落在远端分支上"。
func TestRunCmd_ClonesIntoIsolatedWorkspace(t *testing.T) {
	requireGitForE2E(t)
	fx := newLocalRepoFixture(t)
	tmp := t.TempDir()
	workParent := filepath.Join(tmp, "work")
	if err := os.MkdirAll(workParent, 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	t.Setenv("TMPDIR", workParent)
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
	defer srv.Close()
	setProviderEnv(t, srv.URL+"/v1")

	beforeStatus := gitIn(t, fx.repo, "status", "--porcelain")
	beforeHead := gitIn(t, fx.repo, "rev-parse", "HEAD")
	beforeBranch := gitIn(t, fx.repo, "rev-parse", "--abbrev-ref", "HEAD")

	code, _, stderrText := runCapturingStderr(t, []string{"run", "--repo", fx.repo, "--task", "加一个 hello.txt"})
	if code != exitOK {
		t.Fatalf("本地驱动应成功退出（0），实际 %d\nstderr:\n%s", code, stderrText)
	}

	if got := gitIn(t, fx.repo, "status", "--porcelain"); got != beforeStatus {
		t.Errorf("用户仓库状态被改动：%q -> %q", beforeStatus, got)
	}
	if got := gitIn(t, fx.repo, "rev-parse", "HEAD"); got != beforeHead {
		t.Errorf("用户仓库 HEAD 被改动：%q -> %q", beforeHead, got)
	}
	if got := gitIn(t, fx.repo, "rev-parse", "--abbrev-ref", "HEAD"); got != beforeBranch {
		t.Errorf("用户仓库分支被改动：%q -> %q", beforeBranch, got)
	}

	// 交付确实发生了，只是在远端任务分支上（证明工作区是从远端 clone 出来的）。
	id := fx.base[:12]
	if tip := gitIn(t, fx.remote, "rev-parse", "refs/heads/xhunter/"+id); tip == fx.base {
		t.Fatal("远端任务分支没有推进——工作区没有 clone 出来")
	}
	assertDirEmpty(t, workParent)
}

// 收发段挂起（上游接了请求就不说话、不关流）必须被看门狗停住：环境错误（退出 1），
// 不是"模型正常完成对话"。用短超时值，别让用例真等 120s。
func TestEndToEnd_IdleStreamConvergesToEnvError(t *testing.T) {
	requireGitForE2E(t)
	fx := newRepoFixture(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", filepath.Join(tmp, "work"))
	t.Setenv("HOME", filepath.Join(tmp, "home"))

	// 上游接住请求后既不写事件也不关流——接收段一直静默，只有看门狗能把它停住。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	taskPath := writeRunInputs(t, tmp, "随便做点什么")
	setRunEnv(t, fx, srv.URL+"/v1")
	t.Setenv(envStreamIdleTimeout, "200ms")

	resultPath := filepath.Join(tmp, "result.json")
	stdout, stderr := swapStdStreams(t)
	code := run([]string{"--bounty", taskPath, "--result", resultPath})
	stdoutText, stderrText := drainStdStreams(t, stdout, stderr)

	if code != exitEnv {
		t.Fatalf("接收段挂起应退出 1，实际 %d\nstdout:\n%s\nstderr:\n%s", code, stdoutText, stderrText)
	}

	end := eventPayload(t, stdoutText, "hunt_end")
	if end["status"] != "failed" || end["reason"] != "stream_idle_timeout" {
		t.Errorf("终态应为 failed/stream_idle_timeout：%v", end)
	}
	errEv := eventPayload(t, stdoutText, "error")
	if errEv["kind"] != "stream_idle_timeout" || errEv["retryable"] != true {
		t.Errorf("error 事件应为 stream_idle_timeout 且可重试：%v", errEv)
	}

	// 结果文件与事件同源（环境问题 → 退出码 1、可重试、reason 一致）。
	raw, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("结果文件未写出：%v", err)
	}
	var res resultFile
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("结果文件不是合法 JSON：%v", err)
	}
	if res.Status != "failed" || res.ExitCode != exitEnv || res.Reason != "stream_idle_timeout" {
		t.Errorf("结果文件终态不对：%+v", res)
	}
	if res.Error == nil || res.Error.Kind != "stream_idle_timeout" || !res.Error.Retryable {
		t.Errorf("结果文件 error 应为可重试的 stream_idle_timeout：%+v", res.Error)
	}
}

// 收尾清理的兜底断言：临时工作树不留残骸（run 路径同样成立）。
func TestRunCmd_TempWorkspaceIsReclaimed(t *testing.T) {
	requireGitForE2E(t)
	fx := newLocalRepoFixture(t)
	tmp := t.TempDir()
	workParent := filepath.Join(tmp, "work")
	if err := os.MkdirAll(workParent, 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	t.Setenv("TMPDIR", workParent)
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
	setProviderEnv(t, srv.URL+"/v1")

	code, _, stderrText := runCapturingStderr(t, []string{"run", "--repo", fx.repo, "--task", "随便看看"})
	if code != exitOK {
		t.Fatalf("本地驱动应成功退出（0），实际 %d\nstderr:\n%s", code, stderrText)
	}
	assertDirEmpty(t, workParent)
}

// XHUNTER_STREAM_IDLE_TIMEOUT 的生效值必须**如实**进 config_snapshot：把 env 设成非默认的 5s，
// 断言快照里报的是 5000（毫秒），而不是重抄的默认 120000。这条把「环境变量 → 装配 → 快照」整条链钉上。
func TestConfigSnapshot_StreamIdleTimeoutFollowsEnv(t *testing.T) {
	t.Setenv(envStreamIdleTimeout, "5s")
	r := runSuccessOnce(t)
	if r.code != exitOK {
		t.Fatalf("成功运行应退出 0，实际 %d\nstderr:\n%s", r.code, r.stderr)
	}
	p := eventPayload(t, r.stdout, "config_snapshot")
	got, ok := p["stream_idle_timeout_ms"].(float64)
	if !ok || got != 5000 {
		t.Errorf("config_snapshot.stream_idle_timeout_ms = %v（%T），期望字面量 5000",
			p["stream_idle_timeout_ms"], p["stream_idle_timeout_ms"])
	}
}
