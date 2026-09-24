package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// MS-7 端到端：会话恢复（resume）——同一 session_id 的第二次投递从**最后一个检查点**继续，
// 不重放写操作、不重做已完成轮次；本次条件排在回灌历史之后。

// TestEndToEnd_ResumeContinuesFromLastCheckpoint
//
// 第一趟：第 1 轮 = write ＋ checkpoint（模型显式请求 → 轮边界提交检查点）；第 2 轮上游 500
// → 以非 0 收敛。第二趟用**同一 session_id** 重投（上游只回文本）。
// 断言：① 第二趟首轮请求体里含第一趟的历史（用第一趟独有字符串）；② 第二趟无新写操作
// （session_delta.ops_count == 0）；③ 分支 tip 未被推进；④ session_delta == {2,2,0}（字面量）。
func TestEndToEnd_ResumeContinuesFromLastCheckpoint(t *testing.T) {
	requireGitForE2E(t)
	fx := newRepoFixture(t)
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "work"), 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	t.Setenv("TMPDIR", filepath.Join(tmp, "work"))
	t.Setenv("HOME", filepath.Join(tmp, "home"))

	const sid = "s-resume"
	// 第一趟的历史里带一串**独有**标记：它只可能出现在回灌历史里（不在第二趟的提示词里）。
	const histMark = "RESUME-MARK-8877"

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			// 一轮里两次调用：write ＋ checkpoint（checkpoint 的结果文本带独有标记）。
			io.WriteString(w, sseTwoCalls(
				"c1", "write", `{"path":"hello.txt","content":"hi\n"}`,
				"c2", "checkpoint", `{"summary":"`+histMark+`"}`))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	firstTask := writeRunInputs(t, tmp, "给仓库加一个 hello.txt")
	setRunEnv(t, fx, srv.URL+"/v1")
	t.Setenv(envSessionID, sid)

	firstResult := filepath.Join(tmp, "out1", "result.json")
	stdout, stderr := swapStdStreams(t)
	code := run([]string{"--bounty", firstTask, "--result", firstResult})
	_, errText := drainStdStreams(t, stdout, stderr)
	if code == exitOK {
		t.Fatalf("第一趟应在第 2 轮上游 500 时非 0 收敛，实得 %d\nstderr:\n%s", code, errText)
	}

	tipAfterFirst := gitIn(t, fx.remote, "rev-parse", "refs/heads/"+fx.branch)
	if tipAfterFirst == fx.base {
		t.Fatal("第一趟应产生检查点提交（第 1 轮模型显式请求 checkpoint）")
	}

	// ---- 第二趟：同一 session_id，上游只回文本 ----
	var mu sync.Mutex
	var bodies []string
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, sseWithText("完成了"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer srv2.Close()

	secondTask := writeRunInputs(t, tmp, "继续把剩下的做完")
	setProviderEnv(t, srv2.URL+"/v1") // 同一仓库事实；只换模型接入点

	secondResult := filepath.Join(tmp, "out2", "result.json")
	stdout2, stderr2 := swapStdStreams(t)
	code2 := run([]string{"--bounty", secondTask, "--result", secondResult})
	out2, errText2 := drainStdStreams(t, stdout2, stderr2)
	if code2 != exitOK {
		t.Fatalf("第二趟应成功退出（上游只回文本 → no_tool_call），实得 %d\nstdout:\n%s\nstderr:\n%s",
			code2, out2, errText2)
	}

	// ① 首轮请求体里含第一趟的历史（独有标记）。
	mu.Lock()
	defer mu.Unlock()
	// 恢复趟应**恰好一次**推理：本次只有一轮；恢复段若多发一次推理（违反"零模型调用"）会变红。
	if len(bodies) != 1 {
		t.Fatalf("恢复趟应恰好一次推理（恢复段零模型调用），实得 %d 次", len(bodies))
	}
	if !strings.Contains(bodies[0], histMark) {
		t.Errorf("第二趟首轮请求体里应含第一趟的历史（%s）：\n%s", histMark, bodies[0])
	}

	// ③ 分支 tip 未被推进：本次零写操作（无交付提交）。
	if tipAfterSecond := gitIn(t, fx.remote, "rev-parse", "refs/heads/"+fx.branch); tipAfterSecond != tipAfterFirst {
		t.Errorf("第二趟不该推进分支 tip：first=%s second=%s", tipAfterFirst, tipAfterSecond)
	}

	raw, err := os.ReadFile(secondResult)
	if err != nil {
		t.Fatalf("第二趟结果文件未写出：%v", err)
	}
	var res resultFile
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("结果文件不是合法 JSON：%v\n%s", err, raw)
	}

	// ②④ session_delta 字面量：恢复 1 轮 + 本次 1 轮 → {turns_from:2, turns_to:2, ops_count:0}。
	if res.SessionDelta == nil {
		t.Fatalf("恢复时结果文件必须带 session_delta：%s", raw)
	}
	if res.SessionDelta.TurnsFrom != 2 || res.SessionDelta.TurnsTo != 2 || res.SessionDelta.OpsCount != 0 {
		t.Errorf("session_delta = %+v，期望 {turns_from:2, turns_to:2, ops_count:0}", *res.SessionDelta)
	}

	// 结果文件的 files_changed 相对**原始基线**（Diff 的基准是 repo.BaseCommit，恢复不改写它）：
	// 上一趟已交付的 hello.txt 仍在清单里——那不是本次新增。本次「零写操作」由 ops_count==0
	// 与「tip 未推进」共同钉住。
	if len(res.FilesChanged) != 1 || res.FilesChanged[0] != "hello.txt" {
		t.Errorf("files_changed = %v，期望 [hello.txt]（相对原始基线；非本次新增）", res.FilesChanged)
	}
}

// TestEndToEnd_ResumeCorruptedMaterialIsEnvError
//
// 任务分支上预置一份坏材料（schema_version 不认识）→ 投递同一 session_id → 退出 1、
// 结果文件 error.kind == "prepare_failed"、retryable == true（不自动迁移、不猜）。
func TestEndToEnd_ResumeCorruptedMaterialIsEnvError(t *testing.T) {
	requireGitForE2E(t)
	fx := newRepoFixture(t)
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "work"), 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	t.Setenv("TMPDIR", filepath.Join(tmp, "work"))
	t.Setenv("HOME", filepath.Join(tmp, "home"))

	const sid = "s-bad"
	branch := branchFor(sid)
	seedMaterialOnBranch(t, fx, branch, sid, `{"type":"meta","schema_version":999}`+"\n")

	// 仓库事实：分支名与 session_id 对齐（材料目录 .xhunter/<sid>/）。
	t.Setenv("XHUNTER_REPO_URL", fx.remote)
	t.Setenv("XHUNTER_REPO_BASE_COMMIT", fx.base)
	t.Setenv("XHUNTER_REPO_BRANCH", branch)
	t.Setenv(envSessionID, sid)
	setProviderEnv(t, "http://127.0.0.1:1/v1") // Prepare 失败在推理之前，模型接入点不会被调用

	taskPath := writeRunInputs(t, tmp, "随便做点什么")
	resultPath := filepath.Join(tmp, "out", "result.json")

	stdout, stderr := swapStdStreams(t)
	code := run([]string{"--bounty", taskPath, "--result", resultPath})
	out, errText := drainStdStreams(t, stdout, stderr)
	if code != exitEnv {
		t.Fatalf("材料损坏应以退出码 1 收敛，实得 %d\nstdout:\n%s\nstderr:\n%s", code, out, errText)
	}

	raw, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("失败路径也必须写出结果文件：%v", err)
	}
	var res resultFile
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("结果文件不是合法 JSON：%v\n%s", err, raw)
	}
	if res.Error == nil {
		t.Fatalf("材料损坏必须带 error 对象：%s", raw)
	}
	if res.Error.Kind != "prepare_failed" {
		t.Errorf("error.kind = %q，期望 prepare_failed", res.Error.Kind)
	}
	if !res.Error.Retryable {
		t.Errorf("环境问题应标可重试（retryable=true）：%+v", res.Error)
	}
}

// TestEndToEnd_FreshSessionWithoutMaterialIsNotAnError
//
// 给了 session_id 但分支上没有材料 → 正常跑完、退出 0，且结果文件**不含** session_delta 键
// （用 map 断言键不存在，而不是断言为 null）。
func TestEndToEnd_FreshSessionWithoutMaterialIsNotAnError(t *testing.T) {
	requireGitForE2E(t)
	fx := newRepoFixture(t)
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "work"), 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	t.Setenv("TMPDIR", filepath.Join(tmp, "work"))
	t.Setenv("HOME", filepath.Join(tmp, "home"))

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if atomic.AddInt32(&calls, 1) == 1 {
			io.WriteString(w, sseWithToolCall("c1", "write", `{"path":"hello.txt","content":"hi\n"}`))
		} else {
			io.WriteString(w, sseWithText("完成"))
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer srv.Close()

	taskPath := writeRunInputs(t, tmp, "给仓库加一个 hello.txt")
	setRunEnv(t, fx, srv.URL+"/v1")
	t.Setenv(envSessionID, "s-fresh") // 给了会话标识，但分支上没有材料

	resultPath := filepath.Join(tmp, "out", "result.json")
	stdout, stderr := swapStdStreams(t)
	code := run([]string{"--bounty", taskPath, "--result", resultPath})
	out, errText := drainStdStreams(t, stdout, stderr)
	if code != exitOK {
		t.Fatalf("没有材料不是错误：应正常退出 0，实得 %d\nstdout:\n%s\nstderr:\n%s", code, out, errText)
	}

	raw, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("结果文件未写出：%v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("结果文件不是合法 JSON：%v\n%s", err, raw)
	}
	if _, ok := keys["session_delta"]; ok {
		t.Errorf("未恢复时结果文件不该出现 session_delta 键（也不该是 null）：%s", raw)
	}
}

// TestEndToEnd_ResumeAfterClarificationAppliesNewConditions
//
// 第一趟模型声明「## 需要补全」→ blocked（退出 0，改动照常交付）。第二趟用同一 session_id ＋
// 含补充条件的新任务正文重投，断言请求体里**新条件（定位语）出现在回灌历史之后**（下标比较）。
func TestEndToEnd_ResumeAfterClarificationAppliesNewConditions(t *testing.T) {
	requireGitForE2E(t)
	fx := newRepoFixture(t)
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "work"), 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	t.Setenv("TMPDIR", filepath.Join(tmp, "work"))
	t.Setenv("HOME", filepath.Join(tmp, "home"))

	const sid = "s-clarify"
	// 第一趟最后一轮正文里的独有标记：它只出现在回灌历史里，不在第二趟的提示词里。
	const histMark = "CLARIFY-HIST-5566"

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		switch atomic.AddInt32(&calls, 1) {
		case 1:
			// 第 1 轮：write ＋ checkpoint（提交一个检查点，材料随之进分支）。
			io.WriteString(w, sseTwoCalls(
				"c1", "write", `{"path":"hello.txt","content":"hi\n"}`,
				"c2", "checkpoint", `{"summary":"首次自洽"}`))
		default:
			// 第 2 轮：只回正文，且声明「## 需要补全」→ blocked。
			io.WriteString(w, sseWithText("## 需要补全\n- 缺一个外部事实 "+histMark))
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer srv.Close()

	firstTask := writeRunInputs(t, tmp, "给仓库加一个 hello.txt")
	setRunEnv(t, fx, srv.URL+"/v1")
	t.Setenv(envSessionID, sid)

	stdout, stderr := swapStdStreams(t)
	code := run([]string{"--bounty", firstTask})
	_, errText := drainStdStreams(t, stdout, stderr)
	if code != exitOK {
		t.Fatalf("声明需要补全应正常退出（blocked / 退出 0），实得 %d\nstderr:\n%s", code, errText)
	}

	// ---- 第二趟：同一 session_id，带补充条件 ----
	var mu sync.Mutex
	var bodies []string
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, sseWithText("好的，按新条件继续"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer srv2.Close()

	const newCond = "补充条件就是 EXTRA-CONDITION-9911"
	secondTask := writeRunInputs(t, tmp, "接着做，"+newCond)
	setProviderEnv(t, srv2.URL+"/v1")

	stdout2, stderr2 := swapStdStreams(t)
	code2 := run([]string{"--bounty", secondTask})
	out2, errText2 := drainStdStreams(t, stdout2, stderr2)
	if code2 != exitOK {
		t.Fatalf("第二趟应成功退出，实得 %d\nstdout:\n%s\nstderr:\n%s", code2, out2, errText2)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) == 0 {
		t.Fatal("第二趟没有发起任何模型调用")
	}
	body := bodies[0]
	iHist := strings.Index(body, histMark)
	iCond := strings.Index(body, "本次投递的补充条件：")
	if iHist < 0 {
		t.Fatalf("第二趟请求体里应含第一趟的历史标记 %s：\n%s", histMark, body)
	}
	if iCond < 0 {
		t.Fatalf("第二趟请求体里应含补充条件定位语：\n%s", body)
	}
	if iCond < iHist {
		t.Errorf("新条件必须排在回灌历史之后：hist=%d cond=%d\n%s", iHist, iCond, body)
	}
	if !strings.Contains(body, newCond) {
		t.Errorf("第二趟请求体里应含新条件正文（%s）：\n%s", newCond, body)
	}
}

// seedMaterialOnBranch 在远端任务分支上预置一份材料文件：先在基线处造提交，再推送为任务分支。
// 用于模拟"分支上已存在一份坏材料"（恢复读它即报错）。分支 tip 必须是基线的后代，否则
// PrepareBaseline 会判定分叉。
func seedMaterialOnBranch(t *testing.T, fx repoFixture, branch, sid, content string) {
	t.Helper()
	work := t.TempDir()
	gitIn(t, "", "clone", "-q", fx.remote, work)
	gitIn(t, work, "config", "user.name", "fixture")
	gitIn(t, work, "config", "user.email", "fixture@example.com")
	gitIn(t, work, "checkout", "-q", "-b", branch, fx.base)
	dir := filepath.Join(work, materialDirFor(sid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建材料目录失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, materialFile), []byte(content), 0o644); err != nil {
		t.Fatalf("写材料失败：%v", err)
	}
	gitIn(t, work, "add", "-f", "-A")
	gitIn(t, work, "commit", "-qm", "seed material")
	gitIn(t, work, "push", "-q", "origin", "HEAD:refs/heads/"+branch)
}
