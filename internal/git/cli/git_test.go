package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"xhunter/git"
	"xhunter/llm"
)

// git 实现目前只到"入口就位"。这个包要守的是**失败形态**：
// 未实现必须是显式失败，不能静默返回零值——零值会让上层以为"基线已经就绪"，
// 然后在一个并不存在的工作区里继续跑。

func TestGit_UnimplementedFailsLoudly(t *testing.T) {
	g := New(Config{})
	repo := git.RepoRef{Remote: "git@example.com:x/y.git", Branch: "task/b-1", BaseCommit: "abc"}

	cases := []struct {
		name string
		call func() error
	}{
		{"PrepareBaseline", func() error { _, err := g.PrepareBaseline(context.Background(), repo); return err }},
		{"Commit", func() error { _, err := g.Commit(context.Background(), repo, "turn 1 检查点"); return err }},
		{"Diff", func() error { _, err := g.Diff(context.Background(), repo.BaseCommit); return err }},
		{"Clean", func() error { return g.Clean(context.Background()) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("未实现必须显式失败，不得返回零值")
			}
			var te *llm.Fault
			if !errors.As(err, &te) {
				t.Fatalf("应是结构化错误：%v", err)
			}
			if te.Kind != "not_implemented" {
				t.Errorf("错误类型 = %q", te.Kind)
			}
			if te.Retryable {
				t.Error("未实现不是可重试的：原样重试还是同一个结果")
			}
			if !strings.Contains(te.Message, tc.name) {
				t.Errorf("错误信息应指明是哪个动作：%q", te.Message)
			}
		})
	}
}

// 构造只做参数归一：真正的动作以任务为单位发生（那时才知道哪个仓库、哪条分支）。
func TestNew_NormalizesConfig(t *testing.T) {
	if g := New(Config{}); g.workDir == "" || g.remote != "origin" {
		t.Errorf("缺省应落到临时目录 + origin：%+v", g)
	}
	if g := New(Config{WorkDir: "/tmp/wt", Remote: "upstream"}); g.workDir != "/tmp/wt" || g.remote != "upstream" {
		t.Errorf("显式配置应原样采用：%+v", g)
	}
}
