package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// MS-4 的"退出 1 但交付记录不丢"：检查点提交连败达上限、本轮结束即收敛之后，Finalize 仍须
// 照常跑完——交付提交尽力、结果文件仍写出、补丁仍产出、hunt_end 仍是最后一条。
//
// 夹具取舍：真实 git 无法只让"检查点提交"失败而放行"交付提交"。这里用一个全局
// `core.hooksPath` 指向的 `commit-msg` 钩子，把提交信息里含「检查点」的阶段性提交拒掉
// （模拟远端/环境持续拒绝阶段性提交），放行「任务改动」交付提交。除此之外全程是真实装配
// （真 git 夹具 ＋ 真命令行 git 实现 ＋ 真结果文件/补丁写出），不是打桩绕开链路。
func TestEndToEnd_CheckpointStreakStillDeliversRecord(t *testing.T) {
	requireGitForE2E(t)
	fx := newRepoFixture(t) // 裸远端 ＋ 基线 main（此时 HOME 仍是真实值 → 夹具不受钩子影响）
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "work"), 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}

	// 全局钩子：commit-msg 拒绝含「检查点」的提交信息，其余放行。
	hookDir := filepath.Join(tmp, "hooks")
	if err := os.MkdirAll(hookDir, 0o755); err != nil {
		t.Fatalf("建钩子目录失败：%v", err)
	}
	hook := "#!/bin/sh\n" +
		"if grep -q 检查点 \"$1\"; then\n" +
		"  echo 'checkpoint commit rejected (ms4 fixture)' >&2\n" +
		"  exit 1\n" +
		"fi\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(hookDir, "commit-msg"), []byte(hook), 0o755); err != nil {
		t.Fatalf("写钩子失败：%v", err)
	}
	home := filepath.Join(tmp, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("建 HOME 失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"),
		[]byte("[core]\n\thooksPath = "+hookDir+"\n"), 0o644); err != nil {
		t.Fatalf("写全局 gitconfig 失败：%v", err)
	}

	t.Setenv("TMPDIR", filepath.Join(tmp, "work"))
	t.Setenv("HOME", home)

	// 假上游：每轮发一个 write（新文件）＋ 一个 checkpoint（让阶段性提交被尝试 → 被钩子拒）。
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, sseWriteWithCheckpoint(atomic.AddInt32(&calls, 1)))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer srv.Close()

	taskPath := writeRunInputs(t, tmp, "写点文件")
	setRunEnv(t, fx, srv.URL+"/v1")
	resultPath := filepath.Join(tmp, "out", "result.json")
	patchPath := filepath.Join(tmp, "out", "delivery.patch")

	stdout, stderr := swapStdStreams(t)
	code := run([]string{"--bounty", taskPath, "--result", resultPath, "--patch", patchPath})
	out, errText := drainStdStreams(t, stdout, stderr)

	// 连败达上限 → 环境错误（退出 1）。不跑完剩余轮次：恰好 3 轮推理（第 4 轮没有发生）。
	if code != exitEnv {
		t.Fatalf("连败收敛应退出 %d，实际 %d\nstdout:\n%s\nstderr:\n%s", exitEnv, code, out, errText)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("连败达上限应本轮结束即收敛（第 4 轮不该发生）：上游调用 %d 次，期望 3", got)
	}
	// 收尾没被破坏：hunt_end 仍是最后一条事件，且终态原因带连败前缀。
	assertLastEventIsHuntEnd(t, out)
	if !strings.Contains(out, "checkpoint_failed_streak") {
		t.Errorf("事件流应含 checkpoint_failed_streak：\n%s", out)
	}

	// 交付记录不丢：结果文件仍写出，终态 failed / 退出 1 / error.kind 与事件同源且可重试。
	res := readResultFile(t, resultPath)
	if res.Status != "failed" || res.ExitCode != exitEnv {
		t.Errorf("结果文件终态不对：status=%s exit=%d", res.Status, res.ExitCode)
	}
	if res.Error == nil || res.Error.Kind != "checkpoint_failed_streak" || !res.Error.Retryable {
		t.Errorf("失败必须给出 kind=checkpoint_failed_streak 且可重试：%+v", res.Error)
	}

	// 补丁仍产出（改动不丢）：三轮写的文件都在里面。
	patch, err := os.ReadFile(patchPath)
	if err != nil {
		t.Fatalf("补丁未产出：%v", err)
	}
	for _, f := range []string{"f1.txt", "f2.txt", "f3.txt"} {
		if !strings.Contains(string(patch), f) {
			t.Errorf("补丁应含 %s：\n%s", f, patch)
		}
	}

	// 交付提交尽力且落地：远端任务分支 tip == 结果文件 commit_sha，且是交付口径。
	tip := gitIn(t, fx.remote, "rev-parse", "refs/heads/"+fx.branch)
	if res.CommitSHA == "" || res.CommitSHA != tip {
		t.Errorf("交付提交未落地远端：commit_sha=%q tip=%q", res.CommitSHA, tip)
	}
	if msg := gitIn(t, fx.remote, "log", "-1", "--format=%s", "refs/heads/"+fx.branch); !strings.Contains(msg, "任务改动") {
		t.Errorf("远端 tip 应是交付提交，实得 %q", msg)
	}
}

// sseWriteWithCheckpoint 造一轮：write 一个新文件 ＋ 请求一次 checkpoint（同一个 id 前缀按轮区分）。
func sseWriteWithCheckpoint(n int32) string {
	writeArgs := fmt.Sprintf(`{"path":"f%d.txt","content":"x\n"}`, n)
	write := fmt.Sprintf(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"w%d","function":{"name":"write","arguments":%q}}]},"finish_reason":null}]}`,
		n, writeArgs)
	cp := fmt.Sprintf(
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c%d","function":{"name":"checkpoint","arguments":%q}}]},"finish_reason":null}]}`,
		n, `{"summary":"`+strconv.Itoa(int(n))+` 轮改动自洽"}`)
	return "data: " + write + "\n\n" +
		"data: " + cp + "\n\n" +
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":4}}}` + "\n\n" +
		"data: [DONE]\n\n"
}
