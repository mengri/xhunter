package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xhunter/internal/git/cli"
)

// runCapturingStderr 跑一次入口并把三条通道收成字符串（stdout / stderr）。
func runCapturingStderr(t *testing.T, args []string) (code int, stdout, stderr string) {
	t.Helper()
	out, errFile := swapStdStreams(t)
	code = run(args)
	stdout, stderr = drainStdStreams(t, out, errFile)
	return code, stdout, stderr
}

// localRepoFixture 是本地驱动用的夹具：一个**用户的工作仓库**（origin 指向可推送的裸远端）
// 与它的基线 commit。用户仓库只被只读探测。
type localRepoFixture struct {
	repo   string // 用户的工作仓库（含 .git；origin 指向 remote）
	remote string // 可推送的裸远端
	base   string // 基线 commit（HEAD）
}

func newLocalRepoFixture(t *testing.T) localRepoFixture {
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

	return localRepoFixture{repo: src, remote: remote, base: gitIn(t, src, "rev-parse", "HEAD")}
}

// 接收段不活动超时：未设置 → 0（取 harness 默认 120s）；正 duration → 原样；非法
// （含 0、负数、非 duration）→ 启动期失败（退出 1）——不静默退回默认、也不把"关闭"暴露给部署侧。
func TestParseStreamIdleTimeout_DefaultOverrideAndRejects(t *testing.T) {
	t.Run("未设置取默认", func(t *testing.T) {
		d, err := parseStreamIdleTimeout(fakeEnv(nil))
		if err != nil || d != 0 {
			t.Errorf("未设置应返回 0（由 harness 取默认 120s）：%v %v", d, err)
		}
	})
	t.Run("显式覆盖", func(t *testing.T) {
		if d, err := parseStreamIdleTimeout(fakeEnv(map[string]string{envStreamIdleTimeout: "90s"})); err != nil || d != 90*time.Second {
			t.Errorf("90s 应解析为 90s：%v %v", d, err)
		}
		if d, err := parseStreamIdleTimeout(fakeEnv(map[string]string{envStreamIdleTimeout: "2m"})); err != nil || d != 2*time.Minute {
			t.Errorf("2m 应解析为 2m：%v %v", d, err)
		}
	})
	t.Run("非法即失败", func(t *testing.T) {
		for _, bad := range []string{"0", "0s", "-5s", "abc", "半小时"} {
			if _, err := parseStreamIdleTimeout(fakeEnv(map[string]string{envStreamIdleTimeout: bad})); err == nil {
				t.Errorf("%q 必须报错（0/负会被读成不限/关闭，两种都不是承诺语义）", bad)
			}
		}
	})
}

// 缺 --repo / --task：报出缺什么并打印用法行（"下一步怎么做"）。
func TestRunCmd_MissingRepoFlag(t *testing.T) {
	for name, args := range map[string][]string{
		"都缺":  {"run"},
		"缺任务": {"run", "--repo", t.TempDir()},
	} {
		t.Run(name, func(t *testing.T) {
			code, _, stderrText := runCapturingStderr(t, args)
			if code != exitEnv {
				t.Fatalf("缺参应退出 1，实际 %d", code)
			}
			if !strings.Contains(stderrText, "缺少 --repo/--task") {
				t.Errorf("应报出缺什么：%q", stderrText)
			}
			if !strings.Contains(stderrText, "用法：xhunter run") {
				t.Errorf("应打印用法行作为下一步指引：%q", stderrText)
			}
		})
	}
}

// --repo 指向不存在的路径：明确说"路径不存在"并给出下一步。
func TestRunCmd_RepoPathMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	code, _, stderrText := runCapturingStderr(t, []string{"run", "--repo", missing, "--task", "做点什么"})
	if code != exitEnv {
		t.Fatalf("路径不存在应退出 1，实际 %d", code)
	}
	if !strings.Contains(stderrText, "本地仓库路径不存在") {
		t.Errorf("应报出路径不存在：%q", stderrText)
	}
	if !strings.Contains(stderrText, "把 --repo 指向一个存在的目录") {
		t.Errorf("应给出下一步指引：%q", stderrText)
	}
}

// --repo 指向一个存在但不是 git 仓库的目录：与"路径不存在"分开报。
func TestRunCmd_NotAGitRepo(t *testing.T) {
	requireGitForE2E(t)
	dir := t.TempDir()
	code, _, stderrText := runCapturingStderr(t, []string{"run", "--repo", dir, "--task", "做点什么"})
	if code != exitEnv {
		t.Fatalf("非仓库应退出 1，实际 %d", code)
	}
	if !strings.Contains(stderrText, "不是 git 仓库") {
		t.Errorf("应报出不是仓库：%q", stderrText)
	}
	if !strings.Contains(stderrText, "--repo 指向仓库根") {
		t.Errorf("应给出下一步指引：%q", stderrText)
	}
}

// 本地仓库有提交但没有远端：无法确定可推送的仓库地址。
func TestRunCmd_NoRemote(t *testing.T) {
	requireGitForE2E(t)
	src := t.TempDir()
	gitIn(t, src, "init", "-q", "-b", "main")
	gitIn(t, src, "config", "user.name", "fixture")
	gitIn(t, src, "config", "user.email", "fixture@example.com")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("写文件失败：%v", err)
	}
	gitIn(t, src, "add", "-A")
	gitIn(t, src, "commit", "-qm", "init")

	code, _, stderrText := runCapturingStderr(t, []string{"run", "--repo", src, "--task", "做点什么"})
	if code != exitEnv {
		t.Fatalf("缺远端应退出 1，实际 %d", code)
	}
	if !strings.Contains(stderrText, "本地仓库没有远端") {
		t.Errorf("应报出没有远端：%q", stderrText)
	}
	if !strings.Contains(stderrText, "remote add origin") {
		t.Errorf("应给出下一步指引：%q", stderrText)
	}
}

// 仓库没有提交：无法取 HEAD 作为基线。夹具刻意给一个远端，隔离出"无提交"这一条（否则会先报缺远端）。
func TestRunCmd_EmptyRepo(t *testing.T) {
	requireGitForE2E(t)
	src := t.TempDir()
	gitIn(t, src, "init", "-q", "-b", "main")
	remote := filepath.Join(t.TempDir(), "remote.git")
	gitIn(t, "", "init", "--bare", "-q", remote)
	gitIn(t, src, "remote", "add", "origin", remote)

	code, _, stderrText := runCapturingStderr(t, []string{"run", "--repo", src, "--task", "做点什么"})
	if code != exitEnv {
		t.Fatalf("无提交应退出 1，实际 %d", code)
	}
	if !strings.Contains(stderrText, "仓库没有提交") {
		t.Errorf("应报出无提交：%q", stderrText)
	}
	if !strings.Contains(stderrText, "先产生一次提交") && !strings.Contains(stderrText, "里产生一次提交") {
		t.Errorf("应给出下一步指引：%q", stderrText)
	}
}

// 探测是只读的：探测前后工作区状态、HEAD 与全部引用三者都不变。
func TestRunCmd_ProbeIsReadOnly(t *testing.T) {
	requireGitForE2E(t)
	fx := newLocalRepoFixture(t)

	beforeStatus := gitIn(t, fx.repo, "status", "--porcelain")
	beforeHead := gitIn(t, fx.repo, "rev-parse", "HEAD")
	beforeRefs := gitIn(t, fx.repo, "for-each-ref")

	if _, err := cli.ProbeLocalRepo(context.Background(), fx.repo); err != nil {
		t.Fatalf("探测失败：%v", err)
	}

	if got := gitIn(t, fx.repo, "status", "--porcelain"); got != beforeStatus {
		t.Errorf("探测改动了工作区状态：%q -> %q", beforeStatus, got)
	}
	if got := gitIn(t, fx.repo, "rev-parse", "HEAD"); got != beforeHead {
		t.Errorf("探测改动了 HEAD：%q -> %q", beforeHead, got)
	}
	if got := gitIn(t, fx.repo, "for-each-ref"); got != beforeRefs {
		t.Errorf("探测改动了引用：%q -> %q", beforeRefs, got)
	}
}
