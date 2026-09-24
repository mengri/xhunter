package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"xhunter/git"
	"xhunter/harness"
	"xhunter/hunt"
	"xhunter/internal/workspace/osfs"
	"xhunter/workspace"
)

// 本文件是 MS-2 的跨通道与逐路径独立验证：既有的成功路径断言之外，把"事件流与结果文件读
// 同一份"钉在真实运行的产物上，把 hunt_end 的"最后一条"钉在每一条终止路径上，并独立核对
// 生效配置快照的原语顺序确实等于实际发给供应商的工具面。

// assertLastEventIsHuntEnd 断言 stdout 的最后一条非空行是 hunt_end。
func assertLastEventIsHuntEnd(t *testing.T, stdoutText string) {
	t.Helper()
	lines := nonEmptyLines(stdoutText)
	if len(lines) == 0 {
		t.Fatal("stdout 上没有任何事件行")
	}
	if last := lines[len(lines)-1]; !strings.Contains(last, `"type":"hunt_end"`) {
		t.Errorf("hunt_end 必须是最后一条事件，实得：%s", last)
	}
}

// TestEndToEnd_EventAndResultShareEffectiveConfigAndUsage 独立证明跨通道一致性：同一次成功
// 运行里，结果文件的 effective_config 与事件流 hunt_start 的 effective_config **逐字段相同**，
// 结果文件的 usage 与 hunt_end 的 usage **逐字段相同**（不是"都有这个键"）。
//
// 这两条是 MS-2 的核心承诺（"事件流与结果文件读同一份"）——任何一处各算一遍，这条就会红。
func TestEndToEnd_EventAndResultShareEffectiveConfigAndUsage(t *testing.T) {
	r := runSuccessOnce(t)
	if r.code != exitOK {
		t.Fatalf("成功运行应退出 0，实际 %d\nstdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
	}

	// 结果文件按原始 JSON 解成 map，与事件行同构比较。
	rawRes, err := os.ReadFile(r.result)
	if err != nil {
		t.Fatalf("读结果文件失败：%v", err)
	}
	var res map[string]any
	if err := json.Unmarshal(rawRes, &res); err != nil {
		t.Fatalf("结果文件不是合法 JSON：%v", err)
	}

	start := eventPayload(t, r.stdout, "hunt_start")
	evEC, ok1 := start["effective_config"]
	resEC, ok2 := res["effective_config"]
	if !ok1 || !ok2 {
		t.Fatalf("两侧都必须有 effective_config：事件流=%v 结果文件=%v", ok1, ok2)
	}
	if !reflect.DeepEqual(evEC, resEC) {
		t.Errorf("effective_config 跨通道不一致：\n事件流 %#v\n结果文件 %#v", evEC, resEC)
	}
	// 非空断言：避免"两边同为 null"蒙混过关。
	ecMap, _ := evEC.(map[string]any)
	if ecMap == nil || ecMap["primitives"] == nil {
		t.Fatalf("effective_config 应含原语清单：%#v", evEC)
	}
	prim, _ := ecMap["primitives"].([]any)
	wantPrim := []any{"read", "write", "edit", "find", "glob", "check", "checkpoint"}
	if !reflect.DeepEqual(prim, wantPrim) {
		t.Errorf("事件流里的 primitives = %v，期望 %v", prim, wantPrim)
	}

	end := eventPayload(t, r.stdout, "hunt_end")
	evU, ok3 := end["usage"]
	resU, ok4 := res["usage"]
	if !ok3 || !ok4 {
		t.Fatalf("两侧都必须有 usage：事件流=%v 结果文件=%v", ok3, ok4)
	}
	if !reflect.DeepEqual(evU, resU) {
		t.Errorf("usage 跨通道不一致：\n事件流 %#v\n结果文件 %#v", evU, resU)
	}
	uMap, _ := evU.(map[string]any)
	for _, k := range []string{"reported", "input_tokens", "output_tokens", "cached_input_tokens", "turns", "elapsed_ms"} {
		if _, ok := uMap[k]; !ok {
			t.Errorf("usage 缺字段 %s：%#v", k, uMap)
		}
	}
	if uMap["reported"] != true {
		t.Errorf("上游回报过用量，reported 应为 true：%#v", uMap)
	}
	if uMap["cached_input_tokens"] != float64(24) {
		t.Errorf("cached_input_tokens = %v，期望 24（缓存读要一路带到结果文件）", uMap["cached_input_tokens"])
	}
}

// TestEndToEnd_HuntEndIsLastForEveryTerminalPath 逐条终止路径独立验证：hunt_end 收了没有、
// 是不是最后一条。成功路径已由 TestEndToEnd_EventSequenceIsComplete 覆盖，这里补上
// **预算耗尽 / 准备失败 / 取消** 三条非成功路径——它们此前的断言只到"hunt_end 存在"。
func TestEndToEnd_HuntEndIsLastForEveryTerminalPath(t *testing.T) {
	t.Run("预算耗尽", func(t *testing.T) {
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
			atomic.AddInt32(&calls, 1)
			// 每轮都读 README.md：调用总能成功，只有轮数预算能把它停下来。
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
		out, errText := drainStdStreams(t, stdout, stderr)

		if code != exitAborted {
			t.Fatalf("预算耗尽应退出 %d，实际 %d\nstderr:\n%s", exitAborted, code, errText)
		}
		assertLastEventIsHuntEnd(t, out)
	})

	t.Run("准备失败", func(t *testing.T) {
		requireGitForE2E(t)
		tmp := t.TempDir()
		if err := os.MkdirAll(filepath.Join(tmp, "work"), 0o755); err != nil {
			t.Fatalf("建目录失败：%v", err)
		}
		t.Setenv("HOME", filepath.Join(tmp, "home"))
		t.Setenv("TMPDIR", filepath.Join(tmp, "work"))

		taskPath := writeRunInputs(t, tmp, "做点什么")
		setProviderEnv(t, "http://127.0.0.1:1/v1")
		// 远端不可达：失败发生在 Prepare（取基线），循环根本不开始。
		t.Setenv("XHUNTER_REPO_URL", filepath.Join(tmp, "no-such-repo.git"))
		t.Setenv("XHUNTER_REPO_BASE_COMMIT", "0123456789abcdef0123456789abcdef01234567")
		t.Setenv("XHUNTER_REPO_BRANCH", "xhunter/fail")

		stdout, stderr := swapStdStreams(t)
		code := run([]string{"--bounty", taskPath})
		out, errText := drainStdStreams(t, stdout, stderr)

		if code != exitEnv {
			t.Fatalf("准备失败应退出 %d，实际 %d\nstderr:\n%s", exitEnv, code, errText)
		}
		assertLastEventIsHuntEnd(t, out)
	})

	t.Run("取消", func(t *testing.T) {
		requireGitForE2E(t)
		fx := newRepoFixture(t)
		tmp := t.TempDir()
		if err := os.MkdirAll(filepath.Join(tmp, "work"), 0o755); err != nil {
			t.Fatalf("建目录失败：%v", err)
		}
		t.Setenv("TMPDIR", filepath.Join(tmp, "work"))
		t.Setenv("HOME", filepath.Join(tmp, "home"))

		// 上游收到请求就不响应，直到客户端断开——把进程卡在运行段，好让信号有落点。
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
		if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
			t.Skipf("环境不支持发送信号：%v", err)
		}

		var code int
		select {
		case code = <-done:
		case <-time.After(15 * time.Second):
			t.Fatal("收到 SIGTERM 后没有在约定时间内收敛")
		}
		out, errText := drainStdStreams(t, stdout, stderr)

		if code != exitCancelled {
			t.Fatalf("取消应退出 %d，实际 %d\nstderr:\n%s", exitCancelled, code, errText)
		}
		assertLastEventIsHuntEnd(t, out)
	})
}

// TestEndToEnd_BrokenChannelStopsToolsBeforeTheyRun 覆盖通道断裂的"不再烧预算"实质：断链之后
// 不得继续执行工具。上游每轮都请求写文件——若断链后工具仍被执行，远端任务分支就会推进。
// 断言：退出码为环境错误、结果文件仍写出且 reason 前缀 event_channel_failed、任务分支停在基线。
func TestEndToEnd_BrokenChannelStopsToolsBeforeTheyRun(t *testing.T) {
	requireGitForE2E(t)
	fx := newRepoFixture(t)
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "work"), 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	t.Setenv("TMPDIR", filepath.Join(tmp, "work"))
	t.Setenv("HOME", filepath.Join(tmp, "home"))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, sseWithToolCall("c1", "write", `{"path":"hello.txt","content":"x"}`))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer srv.Close()

	taskPath := writeRunInputs(t, tmp, "写个文件")
	setRunEnv(t, fx, srv.URL+"/v1")

	// stderr 收进文件；stdout 接一个读端已关的管道：第一条事件（hunt_start）就写不出去。
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
	_ = writer.Close()

	if code != exitEnv {
		t.Fatalf("事件写不出去必须以退出码 %d 收敛，实际 %d", exitEnv, code)
	}

	// 交付记录不能因为通道断了就丢：结果文件必须仍写出，且 reason 前缀 event_channel_failed。
	res := readResultFile(t, resultPath)
	if !strings.HasPrefix(res.Reason, "event_channel_failed") {
		t.Errorf("reason = %q，期望以 event_channel_failed 开头", res.Reason)
	}

	// 断链之后不得继续执行工具：远端任务分支必须停在基线（没有交付提交）。
	tip := gitIn(t, fx.remote, "rev-parse", "refs/heads/"+fx.branch)
	if tip != fx.base {
		t.Errorf("通道断裂后仍推进了任务分支（工具被继续执行）：tip=%s base=%s", tip, fx.base)
	}
}

// TestDefaultAssembly_EffectiveConfigPrimitivesMatchToolFace 独立核对"快照不是第二张会漂的表"：
// 用**真实装配**（defaultTools）跑一次 Prepare，断言生效配置快照里的原语顺序**等于**实际发给
// 供应商的工具面顺序（run.Tools），并等于写死的期望顺序（含殿后 checkpoint）。
//
// 既有 TestEffectiveConfig_ListsPrimitivesInToolFaceOrder 用的是 5 个桩原语，绕开了装配层
// 的真实清单——本用例把它接回真实装配。
func TestDefaultAssembly_EffectiveConfigPrimitivesMatchToolFace(t *testing.T) {
	root := t.TempDir()
	sess := hunt.NewSession(hunt.Config{
		Bounty: hunt.Bounty{
			ID: "b1", Task: "改点什么",
			Repo: git.RepoRef{Remote: "r", Branch: "b", BaseCommit: "0123456789abcdef"},
		},
		Tools: func(ws workspace.Workspace) []hunt.Primitive {
			return defaultTools(ws, nil) // 与 huntCmd 同一份真实清单
		},
		Policy:        defaultPolicy(hunt.Budget{}),
		Opener:        osfs.Opener{},
		Git:           &stubGit{root: root},
		Context:       &contextBuilder{},
		Session:       &sessionRecorder{},
		SystemPlugins: defaultSystemPlugins,
		UserPlugins:   defaultUserPlugins,
		Assembly:      assemblyFacts(harness.DefaultConfig()),
	})

	run := &harness.Run{}
	if err := sess.Prepare(context.Background(), run); err != nil {
		t.Fatalf("Prepare 失败：%v", err)
	}

	var face []string
	for _, d := range run.Tools {
		face = append(face, d.Name)
	}
	ec := sess.EffectiveConfig().Primitives

	if !slices.Equal(face, ec) {
		t.Errorf("effective_config.primitives 与实际工具面顺序不一致：\n工具面 %v\n快照   %v", face, ec)
	}
	want := []string{"read", "write", "edit", "find", "glob", "check", "checkpoint"}
	if !slices.Equal(want, ec) {
		t.Errorf("primitives = %v，期望 %v", ec, want)
	}
}
