package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 通道分工是外部契约（FR-1.4、FR-11.4、INV-6）：事件流独占 stdout、人类日志走 stderr。
//
// 这条用例必须走**真实装配**（`run` → `huntCmd` → eventSink），而不是给 sink 注入
// bytes.Buffer——注入 buffer 的单测恰好绕开了"哪个 io.Writer 被接上"这件事，这正是
// 通道写反却长期全绿的原因。这里把进程级的 os.Stdout / os.Stderr 接到临时文件上，
// 断言的就是"最终谁写到了哪条通道"。
//
// 场景取"初始化期失败"（远端不可达）：它同样会发出 hunt_end，因此无需模型与 git 可用，
// 在 git 适配器落地前后都稳定成立。
func TestHuntCmd_EventsGoToStdoutAndLogsGoToStderr(t *testing.T) {
	tmp := t.TempDir()

	taskPath := filepath.Join(tmp, "task.txt")
	if err := os.WriteFile(taskPath, []byte("fix the bug\n"), 0o644); err != nil {
		t.Fatalf("写任务文件失败：%v", err)
	}

	t.Setenv("XHUNTER_BOUNTY_ID", "b-42")
	t.Setenv("XHUNTER_TRACE_ID", "trace-envelope")
	t.Setenv("XHUNTER_REPO_URL", filepath.Join(tmp, "no-such-repo.git"))
	t.Setenv("XHUNTER_REPO_BASE_COMMIT", "0123456789abcdef0123456789abcdef01234567")
	// 模型接入事实同样走环境变量；协议缺省即对话补全。
	t.Setenv("XHUNTER_MODEL", "test-model")
	t.Setenv("XHUNTER_BASE_URL", "http://127.0.0.1:1/v1")
	t.Setenv("XHUNTER_MODEL_CONTEXT_TOKENS", "200000")
	t.Setenv("XHUNTER_MODEL_OUTPUT_TOKENS", "8192")

	stdout, stderr := swapStdStreams(t)
	code := run([]string{"--bounty", taskPath})
	stdoutText, stderrText := drainStdStreams(t, stdout, stderr)

	checked := 0
	for _, line := range nonEmptyLines(stdoutText) {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Errorf("stdout 只允许出现事件行，这一行不是合法 JSON：%q", line)
			continue
		}
		// 信封四字段每行都要有（FR-11.3、IA-5.7）。
		for _, k := range []string{"type", "bounty_id", "trace_id", "ts"} {
			if v, ok := ev[k].(string); !ok || v == "" {
				t.Errorf("事件缺信封字段 %s：%q", k, line)
			}
		}
		if ev["trace_id"] != "trace-envelope" || ev["bounty_id"] != "b-42" {
			t.Errorf("信封取值应来自投递事实：%q", line)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("stdout 上没有任何事件行")
	}

	// 初始化期失败也是显式终态（INV-3），因此事件流里必须有 hunt_end。
	if !strings.Contains(stdoutText, `"type":"hunt_end"`) {
		t.Errorf("stdout 缺少 hunt_end 事件：%q", stdoutText)
	}
	if code != exitEnv {
		t.Errorf("远端不可达应属环境问题，退出码 = %d，期望 %d", code, exitEnv)
	}

	// 反向：人类日志绝不能出现在 stdout，事件也绝不能出现在 stderr。
	if strings.Contains(stdoutText, "已装配") {
		t.Errorf("人类日志泄漏到 stdout：%q", stdoutText)
	}
	if strings.Contains(stderrText, `"type":`) {
		t.Errorf("事件泄漏到 stderr：%q", stderrText)
	}
	if !strings.Contains(stderrText, "已装配") {
		t.Errorf("诊断信息应走 stderr：%q", stderrText)
	}
}

// swapStdStreams 把 os.Stdout / os.Stderr 换成临时文件，返回文件供稍后读取。
// 还原放在 cleanup 里：任何断言失败都要把进程的流恢复原状，否则测试输出会消失。
func swapStdStreams(t *testing.T) (stdout, stderr *os.File) {
	t.Helper()
	var err error
	stdout, err = os.CreateTemp(t.TempDir(), "stdout-*")
	if err != nil {
		t.Fatalf("创建临时 stdout 失败：%v", err)
	}
	stderr, err = os.CreateTemp(t.TempDir(), "stderr-*")
	if err != nil {
		t.Fatalf("创建临时 stderr 失败：%v", err)
	}

	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = stdout, stderr
	t.Cleanup(func() { os.Stdout, os.Stderr = origOut, origErr })
	return stdout, stderr
}

func drainStdStreams(t *testing.T, stdout, stderr *os.File) (string, string) {
	t.Helper()
	if err := stdout.Sync(); err != nil {
		t.Fatalf("刷新 stdout 失败：%v", err)
	}
	if err := stderr.Sync(); err != nil {
		t.Fatalf("刷新 stderr 失败：%v", err)
	}
	out, err := os.ReadFile(stdout.Name())
	if err != nil {
		t.Fatalf("读取 stdout 失败：%v", err)
	}
	errText, err := os.ReadFile(stderr.Name())
	if err != nil {
		t.Fatalf("读取 stderr 失败：%v", err)
	}
	return string(out), string(errText)
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}
