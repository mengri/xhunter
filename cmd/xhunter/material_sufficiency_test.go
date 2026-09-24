package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"xhunter/ext"
	"xhunter/git"
	"xhunter/harness"
	"xhunter/hunt"
	"xhunter/internal/workspace/osfs"
	"xhunter/llm"
	"xhunter/workspace"
)

// 本节独立核对「材料是不是真的够用来续跑」——既有用例主要证明"文件写出来了"，这里把落盘的
// JSONL 逐条读出来，对着"自含续跑信息"的清单逐项核验（对话历史 / 工具名 / 完整参数 /
// 结果摘要 / turn 序号 / 用量 / 能力指纹位），并检查参数非法时仍如实保留退化形态。

// TestEndToEnd_MaterialIsSelfSufficientForResume 跑一次真实多轮运行（真 git 夹具 ＋ 本地假上游），
// 从**已交付的分支**里把材料读回来逐项核对。用 `git show <branch>:<path>` 读远端提交里的内容——
// 这正是"崩溃后按 tip ＋ 材料恢复"会读到的那一份。
func TestEndToEnd_MaterialIsSelfSufficientForResume(t *testing.T) {
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
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if atomic.AddInt32(&calls, 1) == 1 {
			// 第 1 轮：一个合法 write ＋ 一个参数非法 JSON 的调用（材料要如实保留退化形态）。
			io.WriteString(w, sseTwoCalls(
				"c1", "write", `{"path":"hello.txt","content":"hi\n"}`,
				"c2", "write", `{这不是合法 JSON`))
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

	stdout, stderr := swapStdStreams(t)
	code := run([]string{"--bounty", taskPath})
	out, errText := drainStdStreams(t, stdout, stderr)
	if code != exitOK {
		t.Fatalf("成功运行应退出 0，实际 %d\nstdout:\n%s\nstderr:\n%s", code, out, errText)
	}

	sid := fx.base[:12] // 未给 bounty_id/session_id → 会话标识缺省取基线前 12 位
	path := ".xhunter/" + sid + "/session.jsonl"
	raw := gitIn(t, fx.remote, "show", "refs/heads/"+fx.branch+":"+path)
	t.Logf("=== 材料 %s（交付分支上）===\n%s\n=== 材料结束 ===", path, raw)

	// 逐行解析。
	var lines []map[string]any
	for _, l := range nonEmptyLines(raw) {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("材料行不是合法 JSON：%q（%v）", l, err)
		}
		lines = append(lines, m)
	}
	if len(lines) == 0 {
		t.Fatal("材料为空")
	}

	// meta：版本 ＋ 任务事实 ＋ 能力指纹位。
	meta := lines[0]
	if meta["type"] != "meta" {
		t.Errorf("首行应是 meta：%v", meta["type"])
	}
	if meta["schema_version"] != float64(1) {
		t.Errorf("schema_version = %v，期望 1", meta["schema_version"])
	}
	if meta["session_id"] != sid {
		t.Errorf("meta.session_id = %v，期望 %s", meta["session_id"], sid)
	}
	if meta["base_commit"] != fx.base {
		t.Errorf("meta.base_commit = %v，期望基线", meta["base_commit"])
	}
	if ext, ok := meta["ext"].([]any); !ok || len(ext) != 0 {
		t.Errorf("meta.ext 应是空数组（能力指纹位，本期为空）：%v", meta["ext"])
	}

	var turns, ops, usages []map[string]any
	for _, m := range lines[1:] {
		switch m["type"] {
		case "turn":
			turns = append(turns, m)
		case "op":
			ops = append(ops, m)
		case "usage":
			usages = append(usages, m)
		}
	}

	// turn：序号 ＋ 对话历史 ＋ 完整参数（含退化形态）＋ 结果摘要。
	if len(turns) != 2 {
		t.Fatalf("应有两个 turn 记录，实际 %d：%v", len(turns), turns)
	}
	t1 := turns[0]
	if t1["no"] != float64(1) {
		t.Errorf("turn 序号 = %v，期望 1", t1["no"])
	}
	if msgs, ok := t1["messages"].([]any); !ok || len(msgs) == 0 {
		t.Errorf("turn.messages（对话历史）缺失或为空：%v", t1["messages"])
	}
	callList, ok := t1["calls"].([]any)
	if !ok || len(callList) != 2 {
		t.Fatalf("第 1 轮应有两次工具调用记录，实际 %v", t1["calls"])
	}
	sawDegradedArgs := false
	for _, c := range callList {
		cm, _ := c.(map[string]any)
		if cm["id"] == nil || cm["id"] == "" || cm["name"] == nil || cm["name"] == "" {
			t.Errorf("工具调用记录缺 id/name：%v", cm)
		}
		if s, isStr := cm["arguments"].(string); isStr && strings.Contains(s, "合法") {
			sawDegradedArgs = true
		}
	}
	if !sawDegradedArgs {
		t.Errorf("参数非法的调用应以字符串退化形态保留在材料里（否则 flush 会被一次非法参数带下水）：%v", callList)
	}
	resList, ok := t1["results"].([]any)
	if !ok || len(resList) == 0 {
		t.Errorf("turn.results（结果摘要）缺失：%v", t1["results"])
	}
	for _, res := range resList {
		rm, _ := res.(map[string]any)
		if rm["call_id"] == nil || rm["call_id"] == "" {
			t.Errorf("结果摘要应带回 call_id：%v", rm)
		}
		if _, has := rm["output"]; !has {
			t.Errorf("结果摘要应含 output：%v", rm)
		}
	}
	if t2 := turns[1]; t2["no"] != float64(2) || t2["text"] != "完成" {
		t.Errorf("第 2 轮记录不对：no=%v text=%v", t2["no"], t2["text"])
	}

	// op：写操作序列（恢复的唯一刚需）——工具名 ＋ 文件 ＋ 区间 ＋ 前后内容 ＋ turn。
	if len(ops) != 1 {
		t.Fatalf("应有 1 条写操作记录，实际 %d：%v", len(ops), ops)
	}
	for _, k := range []string{"tool", "file", "range", "before", "after", "turn"} {
		if _, has := ops[0][k]; !has {
			t.Errorf("写操作记录缺 %q：%v", k, ops[0])
		}
	}
	if ops[0]["tool"] != "write" || ops[0]["file"] != "hello.txt" {
		t.Errorf("写操作记录内容不对：%v", ops[0])
	}

	// usage：**每轮各一条**增量（三项）。只断言"至少一条"会让"末轮用量没进提交"这个缺口变绿——
	// 材料是恢复的唯一状态源，少一条用量 = 恢复后按材料记账会少算末轮（这正是本次修复的根因）。
	if len(usages) != len(turns) {
		t.Fatalf("材料应每轮各一条 usage（实际 %d 条，轮数 %d）：%v", len(usages), len(turns), lines)
	}
	for _, u := range usages {
		for _, k := range []string{"input_tokens", "output_tokens", "cached_input_tokens"} {
			if _, has := u[k]; !has {
				t.Errorf("用量记录缺 %q：%v", k, u)
			}
		}
	}
}

// TestRecorder_FlushesAtTurnBoundary 证明"周期性落盘"：一轮 OnTurn 之后（收尾之前）材料就已在盘上，
// 不是攒到收尾才写。
func TestRecorder_FlushesAtTurnBoundary(t *testing.T) {
	root := t.TempDir()
	bounty := hunt.Bounty{
		ID: "b1", Task: "t",
		Repo: git.RepoRef{Remote: "r", Branch: "xhunter/b1", BaseCommit: "0123456789abcdef"},
	}
	rec := &sessionRecorder{bounty: bounty}
	sess := hunt.NewSession(hunt.Config{
		Bounty:  bounty,
		Git:     &stubGit{root: root},
		Opener:  osfs.Opener{},
		Policy:  defaultPolicy(hunt.Budget{}),
		Tools:   func(ws workspace.Workspace, ex ext.ExtHost) []hunt.Primitive { return defaultTools(ws, nil, nil) },
		Context: &contextBuilder{},
		Session: rec,
	})

	run := &harness.Run{}
	if err := sess.Prepare(context.Background(), run); err != nil {
		t.Fatalf("Prepare 失败：%v", err)
	}
	if _, err := sess.OnTurn(context.Background(), run, &harness.Turn{
		No:    1,
		Calls: []llm.ToolCall{{ID: "c1", Name: "write", Arguments: []byte(`{"path":"a.txt","content":"x"}`)}},
	}); err != nil {
		t.Fatalf("OnTurn 失败：%v", err)
	}

	// 收尾之前读：材料应已在盘上（快照发生在轮边界）。
	p := filepath.Join(root, materialDirFor("b1"), materialFile)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("轮边界后材料应已落盘（周期落盘），却读不到：%v", err)
	}
	got := string(raw)
	if !strings.Contains(got, `"type":"turn"`) || !strings.Contains(got, `"no":1`) {
		t.Errorf("轮边界后材料应含第 1 轮记录：\n%s", got)
	}
	if !strings.Contains(got, `"type":"op"`) {
		t.Errorf("轮边界后材料应含写操作记录：\n%s", got)
	}
}

// TestSave_ConcurrentSessionsDoNotInterfere 把"按任务隔离"从顺序拉到并行：多个 session 同时
// （各自 recorder、各自目录）往同一仓库根落盘，互不干扰、各自文件完整。
func TestSave_ConcurrentSessionsDoNotInterfere(t *testing.T) {
	root := t.TempDir()
	const sessions, rounds = 4, 30

	var wg sync.WaitGroup
	for i := 0; i < sessions; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sid := fmt.Sprintf("s-%d", i)
			rec := &sessionRecorder{bounty: hunt.Bounty{
				ID: hunt.BountyID(sid), Session: &hunt.SessionRef{ID: sid},
			}}
			if err := rec.Open(root); err != nil {
				t.Errorf("session %s Open 失败：%v", sid, err)
				return
			}
			for r := 1; r <= rounds; r++ {
				rec.RecordTurn(harness.Turn{No: r, Text: sid})
				if err := rec.Snapshot(); err != nil {
					t.Errorf("session %s Snapshot 失败：%v", sid, err)
					return
				}
			}
		}(i)
	}
	wg.Wait()

	for i := 0; i < sessions; i++ {
		sid := fmt.Sprintf("s-%d", i)
		raw, err := os.ReadFile(filepath.Join(root, materialDirFor(sid), materialFile))
		if err != nil {
			t.Errorf("session %s 的材料不完整/缺失：%v", sid, err)
			continue
		}
		got := string(raw)
		if strings.Count(got, `"type":"turn"`) != rounds {
			t.Errorf("session %s 的轮记录数 = %d，期望 %d（被别的 session 干扰了？）",
				sid, strings.Count(got, `"type":"turn"`), rounds)
		}
		if strings.Contains(got, `"text":"s-`) && !strings.Contains(got, `"text":"`+sid+`"`) {
			t.Errorf("session %s 的材料里混进了别的 session 的记录", sid)
		}
	}
}

// sseTwoCalls 造一轮里的两次工具调用（共用一次 usage），用于同时喂进"合法参数"与"非法参数"。
func sseTwoCalls(id1, name1, args1, id2, name2, args2 string) string {
	call := func(index int, id, name, args string) string {
		return fmt.Sprintf(
			`{"choices":[{"delta":{"tool_calls":[{"index":%d,"id":%q,"function":{"name":%q,"arguments":%q}}]},"finish_reason":null}]}`,
			index, id, name, args)
	}
	return "data: " + call(0, id1, name1, args1) + "\n\n" +
		"data: " + call(1, id2, name2, args2) + "\n\n" +
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		`data: {"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":8}}}` + "\n\n" +
		"data: [DONE]\n\n"
}
