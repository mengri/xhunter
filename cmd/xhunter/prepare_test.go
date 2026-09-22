package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xhunter/git"
	"xhunter/harness"
	"xhunter/hunt"
	"xhunter/internal/workspace/osfs"
)

// stubGit 只做 PrepareBaseline：它把工作区根交出来，其余动作在本用例里不被调用。
type stubGit struct{ root string }

func (g *stubGit) PrepareBaseline(context.Context, git.RepoRef) (string, error) {
	return g.root, nil
}

func (g *stubGit) Commit(context.Context, git.RepoRef, string) (git.Commit, error) {
	return git.Commit{SHA: "deadbeef"}, nil
}

func (g *stubGit) Diff(context.Context, string) ([]string, error) { return nil, nil }
func (g *stubGit) Patch(context.Context, string) (string, error)  { return "", nil }
func (g *stubGit) Clean(context.Context) error                    { return nil }

// 首轮提示词必须让模型看到"要做什么"：任务正文只存在于 Bounty 里，若装配层不把它
// 放进 user 段，模型拿到的是"项目约定 + 技能清单"——任务会在第一轮退化成无工具调用。
//
// 本用例走真实装配（defaultSystemPlugins / defaultUserPlugins + hunt.Session.Prepare），
// 断言的是最终发给模型的消息，而不是某个插件自己的输出。
func TestPrepare_FirstPromptCarriesTaskAndConventions(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"),
		[]byte("# 仓库约定\n不要改生成的文件"), 0o644); err != nil {
		t.Fatalf("写约定文件失败：%v", err)
	}

	const marker = "TASK_MARKER_7c1f 给 read 原语补行范围校验"
	ctxb := &contextBuilder{}
	session := hunt.NewSession(hunt.Config{
		Bounty: hunt.Bounty{
			ID:   "b1",
			Task: "  " + marker + "\n验收：scripts/check.py 全绿",
			Repo: git.RepoRef{Remote: "r", Branch: "b", BaseCommit: "0123456789abcdef"},
		},
		Opener:        osfs.Opener{},
		Git:           &stubGit{root: root},
		Context:       ctxb,
		Session:       &sessionRecorder{},
		SystemPlugins: defaultSystemPlugins,
		UserPlugins:   defaultUserPlugins,
	})

	run := &harness.Run{}
	if err := session.Prepare(context.Background(), run); err != nil {
		t.Fatalf("Prepare 失败：%v", err)
	}

	if len(run.Messages) != 2 {
		t.Fatalf("首轮消息数 = %d，期望 2（system 约定 + user 任务）", len(run.Messages))
	}
	if run.Messages[0].Role != "system" {
		t.Errorf("第一段应为 system：%q", run.Messages[0].Role)
	}
	if run.Messages[1].Role != "user" {
		t.Errorf("第二段应为 user：%q", run.Messages[1].Role)
	}
	if !strings.Contains(run.Messages[0].Content, "仓库约定") {
		t.Errorf("system 段缺少项目约定：%q", run.Messages[0].Content)
	}
	if !strings.Contains(run.Messages[1].Content, marker) {
		t.Errorf("user 段缺少任务正文：%q", run.Messages[1].Content)
	}

	// 任务原文只应出现一次：重复注入等于同一条指令在提示词里出现两遍。
	all := run.Messages[0].Content + "\n" + run.Messages[1].Content
	if n := strings.Count(all, marker); n != 1 {
		t.Errorf("任务正文出现 %d 次，期望 1 次", n)
	}

	// 工具面在同一阶段定格：工作区就绪后才构造原语，声明与执行因此同源。
	if len(run.Tools) == 0 {
		t.Error("Prepare 必须定格工具面")
	}
}
