package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"xhunter/git"
)

// newGatesFixture 造一个**基线里就带 gates.yml** 的夹具。
//
// 它与 newFixture 的差别只在初始提交的内容，不复用是为了让"读基线那一份"这件事只留一个变量：
// 门禁的判据必须在运行开始前定死，所以读的是基线 commit，不是工作区。
func newGatesFixture(t *testing.T, content string) fixture {
	t.Helper()
	src := t.TempDir()
	runGit(t, src, "init", "-q", "-b", "main")
	runGit(t, src, "config", "user.name", "fixture")
	runGit(t, src, "config", "user.email", "fixture@example.com")
	writeFile(t, filepath.Join(src, "README.md"), "hello\n")
	writeFile(t, filepath.Join(src, "gates.yml"), content)
	runGit(t, src, "add", "-A")
	runGit(t, src, "commit", "-qm", "init")

	remote := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, "", "init", "--bare", "-q", remote)
	runGit(t, src, "remote", "add", "origin", remote)
	runGit(t, src, "push", "-q", "origin", "HEAD:refs/heads/main")

	return fixture{remote: remote, base: runGit(t, src, "rev-parse", "HEAD"), work: t.TempDir()}
}

// 门禁清单从基线读：工作区里的同名文件改了也不该改变这一趟的判据——否则模型改两行判据
// 就能让自己通过，这一趟的结论也不可复现。
func TestReadFileAtCommit_ReadsTheBaselineNotTheWorktree(t *testing.T) {
	requireGit(t)
	const baseline = "gates:\n  - name: fmt\n    argv: [gofmt, -l, .]\n"
	f := newGatesFixture(t, baseline)
	g := New(Config{})
	ctx := context.Background()
	root, err := g.PrepareBaseline(ctx, f.repo("xhunter/task"))
	if err != nil {
		t.Fatalf("准备基线失败：%v", err)
	}
	defer func() { _ = g.Clean(ctx) }()

	got, exists, err := g.ReadFileAtCommit(ctx, f.repo("xhunter/task"), "gates.yml")
	if err != nil {
		t.Fatalf("读取基线文件失败：%v", err)
	}
	if !exists {
		t.Fatal("基线里应该有 gates.yml，实际报告不存在")
	}
	if string(got) != baseline {
		t.Errorf("读到 %q，期望基线那一份 %q", got, baseline)
	}

	if err := os.WriteFile(filepath.Join(root, "gates.yml"), []byte("gates: []\n"), 0o644); err != nil {
		t.Fatalf("写工作区文件失败：%v", err)
	}
	again, _, err := g.ReadFileAtCommit(ctx, f.repo("xhunter/task"), "gates.yml")
	if err != nil {
		t.Fatalf("第二次读取失败：%v", err)
	}
	if string(again) != baseline {
		t.Errorf("工作区改动影响了基线读取：读到 %q，期望仍是 %q", again, baseline)
	}
}

// 仓库没有声明门禁是**正常事实**，不是错误：把它报成错误会让"没有门禁"和"读不出来"混为一谈。
func TestReadFileAtCommit_MissingFileIsNotAnError(t *testing.T) {
	requireGit(t)
	f := newFixture(t)
	g := New(Config{})
	ctx := context.Background()
	if _, err := g.PrepareBaseline(ctx, f.repo("xhunter/task")); err != nil {
		t.Fatalf("准备基线失败：%v", err)
	}
	defer func() { _ = g.Clean(ctx) }()

	content, exists, err := g.ReadFileAtCommit(ctx, f.repo("xhunter/task"), "gates.yml")
	if err != nil {
		t.Fatalf("基线没有该文件不该报错：%v", err)
	}
	if exists || content != nil {
		t.Errorf("期望 exists=false 且内容为 nil，实际 %v / %q", exists, content)
	}

	// 没有基线 commit 就是读不了：那是环境问题（可重派），不是"没有声明"。
	if _, _, err := g.ReadFileAtCommit(ctx, git.RepoRef{Remote: f.remote, Branch: "xhunter/task"}, "gates.yml"); err == nil {
		t.Error("缺少基线 commit 时应当报错")
	}
}
