package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"xhunter/git"
	"xhunter/llm"
)

// 本包的用例都跑真实 git：契约要的是"人和工具跑的是同一条命令"，用一个假 git 去测
// 只会把命令拼装错误一起假掉。没有 git 的环境跳过（不假装通过）。
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("环境没有 git：%v", err)
	}
}

// runGit 是本包用例自己的 git 夹具执行器（与被测实现无关，故意不复用它）。
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("夹具 git %s 失败：%v\n%s", strings.Join(args, " "), err, errb.String())
	}
	return strings.TrimSpace(out.String())
}

// fixture 是一个远端裸仓库 + 一条基线 commit 的历史。
type fixture struct {
	remote string // 裸仓库路径（作为 RepoRef.Remote）
	base   string // 基线 commit
	work   string // 工作区父目录
}

// newFixture 造出：源仓库（一次提交）→ 推给裸远端 → 返回远端与基线 SHA。
func newFixture(t *testing.T) fixture {
	t.Helper()
	src := t.TempDir()
	runGit(t, src, "init", "-q", "-b", "main")
	runGit(t, src, "config", "user.name", "fixture")
	runGit(t, src, "config", "user.email", "fixture@example.com")
	writeFile(t, filepath.Join(src, "README.md"), "hello\n")
	runGit(t, src, "add", "-A")
	runGit(t, src, "commit", "-qm", "init")

	remote := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, "", "init", "--bare", "-q", remote)
	runGit(t, src, "remote", "add", "origin", remote)
	runGit(t, src, "push", "-q", "origin", "HEAD:refs/heads/main")

	return fixture{
		remote: remote,
		base:   runGit(t, src, "rev-parse", "HEAD"),
		work:   t.TempDir(),
	}
}

func (f fixture) repo(branch string) git.RepoRef {
	return git.RepoRef{Remote: f.remote, Branch: branch, BaseCommit: f.base}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写文件失败：%v", err)
	}
}

// 基线获取：在临时工作树里取到基线、以基线创建任务分支、并立刻推送（把基线钉在远端）。
func TestPrepareBaseline_FreshTaskCreatesAndPushesBranch(t *testing.T) {
	requireGit(t)
	f := newFixture(t)
	g := New(Config{WorkDir: f.work})
	repo := f.repo("xhunter/s1")

	root, err := g.PrepareBaseline(context.Background(), repo)
	if err != nil {
		t.Fatalf("获取基线失败：%v", err)
	}
	if !strings.HasPrefix(root, f.work) {
		t.Errorf("工作树应建在配置的父目录下：%s", root)
	}
	if got := runGit(t, root, "rev-parse", "HEAD"); got != f.base {
		t.Errorf("工作区 HEAD = %s，期望基线 %s", got, f.base)
	}
	if got := runGit(t, root, "rev-parse", "--abbrev-ref", "HEAD"); got != "xhunter/s1" {
		t.Errorf("当前分支 = %s，期望 xhunter/s1", got)
	}
	// 分支已推送到远端，且远端 tip 就是基线——"把基线钉在远端"。
	if got := runGit(t, f.remote, "rev-parse", "refs/heads/xhunter/s1"); got != f.base {
		t.Errorf("远端任务分支 = %s，期望基线 %s", got, f.base)
	}
	// 初始化结束时工作区必须干净。
	if out := runGit(t, root, "status", "--porcelain"); out != "" {
		t.Errorf("初始化后工作区应干净：%q", out)
	}
}

// 幂等（续跑）：任务分支已存在且 tip 是基线的后代 → 成功，且 checkout 到分支 tip。
func TestPrepareBaseline_ResumeChecksOutBranchTip(t *testing.T) {
	requireGit(t)
	f := newFixture(t)
	repo := f.repo("xhunter/s2")

	first := New(Config{WorkDir: f.work})
	root, err := first.PrepareBaseline(context.Background(), repo)
	if err != nil {
		t.Fatalf("首次获取基线失败：%v", err)
	}
	writeFile(t, filepath.Join(root, "a.txt"), "one\n")
	cm, err := first.Commit(context.Background(), repo, "turn 1 检查点")
	if err != nil {
		t.Fatalf("提交失败：%v", err)
	}

	// 换一个实例（模拟重派到新机器）：分支已存在、tip 在基线之后，必须按续跑处理。
	second := New(Config{WorkDir: f.work})
	root2, err := second.PrepareBaseline(context.Background(), repo)
	if err != nil {
		t.Fatalf("续跑失败（tip 为基线后代时应幂等成功）：%v", err)
	}
	if got := runGit(t, root2, "rev-parse", "HEAD"); got != cm.SHA {
		t.Errorf("续跑应 checkout 到分支 tip %s，实际 %s", cm.SHA, got)
	}
	if _, err := os.Stat(filepath.Join(root2, "a.txt")); err != nil {
		t.Errorf("续跑后上次检查点的内容应在工作区里：%v", err)
	}
}

// 分叉即环境错误：既有分支的 tip 不是基线的后代时，既不续跑也不覆盖。
func TestPrepareBaseline_DivergedBranchIsEnvError(t *testing.T) {
	requireGit(t)
	f := newFixture(t)
	// 造一条与基线无关的历史，推到同名任务分支上。
	other := t.TempDir()
	runGit(t, other, "init", "-q", "-b", "main")
	runGit(t, other, "config", "user.name", "other")
	runGit(t, other, "config", "user.email", "other@example.com")
	runGit(t, other, "checkout", "-q", "--orphan", "unrelated")
	writeFile(t, filepath.Join(other, "x.txt"), "x\n")
	runGit(t, other, "add", "-A")
	runGit(t, other, "commit", "-qm", "unrelated root")
	runGit(t, other, "remote", "add", "origin", f.remote)
	runGit(t, other, "push", "-q", "origin", "HEAD:refs/heads/xhunter/s3")

	g := New(Config{WorkDir: f.work})
	_, err := g.PrepareBaseline(context.Background(), f.repo("xhunter/s3"))
	if err == nil {
		t.Fatal("分叉的任务分支必须报错")
	}
	var fault *llm.Fault
	if !errors.As(err, &fault) || fault.Kind != "branch_diverged" {
		t.Fatalf("应是 branch_diverged 结构化错误：%v", err)
	}
	if !fault.Retryable {
		t.Error("环境问题应标记为可重试")
	}
}

// 提交：累积改动 → 提交 → fast-forward 推送；无改动时不产生空提交。
func TestCommit_PushesFastForwardAndSkipsEmptyCommit(t *testing.T) {
	requireGit(t)
	f := newFixture(t)
	repo := f.repo("xhunter/s4")
	g := New(Config{WorkDir: f.work})
	root, err := g.PrepareBaseline(context.Background(), repo)
	if err != nil {
		t.Fatalf("获取基线失败：%v", err)
	}

	writeFile(t, filepath.Join(root, "pkg/a.txt"), "one\n")
	writeFile(t, filepath.Join(root, "b.txt"), "two\n")
	cm, err := g.Commit(context.Background(), repo, "turn 1 检查点")
	if err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	if cm.Branch != repo.Branch {
		t.Errorf("提交结果的分支 = %s，期望 %s", cm.Branch, repo.Branch)
	}
	if got := runGit(t, f.remote, "rev-parse", "refs/heads/"+repo.Branch); got != cm.SHA {
		t.Errorf("远端 tip = %s，期望本次提交 %s", got, cm.SHA)
	}
	if got := runGit(t, f.remote, "log", "-1", "--format=%s", "refs/heads/"+repo.Branch); got != "turn 1 检查点" {
		t.Errorf("提交信息 = %q", got)
	}
	// 远端内容可读回：交付 = 分支 tip。
	if got := runGit(t, f.remote, "show", cm.SHA+":pkg/a.txt"); got != "one" {
		t.Errorf("远端该提交的内容 = %q，期望 one", got)
	}

	// 无改动：不产生空提交，HEAD 不动。
	headBefore := runGit(t, root, "rev-parse", "HEAD")
	again, err := g.Commit(context.Background(), repo, "turn 2 检查点")
	if err != nil {
		t.Fatalf("无改动时不该失败：%v", err)
	}
	if again.SHA != headBefore {
		t.Errorf("无改动不应产生新提交：%s → %s", headBefore, again.SHA)
	}
	if !cm.Created {
		t.Error("第一次提交应报告 Created=true")
	}
	if again.Created {
		t.Error("无改动的提交必须报告 Created=false：日志不能报「已创建检查点」")
	}
	if got := runGit(t, root, "rev-list", "--count", "HEAD"); got != "2" {
		t.Errorf("提交数 = %s，期望 2（基线 + 一次检查点）", got)
	}
}

// 只允许 fast-forward：远端 tip 领先于本地时推送必须被拒绝，且远端不被改写。
func TestCommit_RejectsNonFastForward(t *testing.T) {
	requireGit(t)
	f := newFixture(t)
	repo := f.repo("xhunter/s5")
	a := New(Config{WorkDir: f.work})
	rootA, err := a.PrepareBaseline(context.Background(), repo)
	if err != nil {
		t.Fatalf("A 获取基线失败：%v", err)
	}
	writeFile(t, filepath.Join(rootA, "a.txt"), "a\n")
	cmA, err := a.Commit(context.Background(), repo, "A 的提交")
	if err != nil {
		t.Fatalf("A 提交失败：%v", err)
	}

	// B 从 A 的提交续跑并再推一次：远端因此领先于 A 的本地。
	b := New(Config{WorkDir: f.work})
	rootB, err := b.PrepareBaseline(context.Background(), repo)
	if err != nil {
		t.Fatalf("B 续跑失败：%v", err)
	}
	writeFile(t, filepath.Join(rootB, "b.txt"), "b\n")
	cmB, err := b.Commit(context.Background(), repo, "B 的提交")
	if err != nil {
		t.Fatalf("B 提交失败：%v", err)
	}

	// A 基于旧 tip 再提交：推送必然是非快进。
	writeFile(t, filepath.Join(rootA, "c.txt"), "c\n")
	_, err = a.Commit(context.Background(), repo, "A 的第二次提交")
	if err == nil {
		t.Fatal("非快进推送必须被拒绝")
	}
	var fault *llm.Fault
	if !errors.As(err, &fault) || fault.Kind != "push_rejected" {
		t.Fatalf("应是 push_rejected：%v", err)
	}
	if got := runGit(t, f.remote, "rev-parse", "refs/heads/"+repo.Branch); got != cmB.SHA {
		t.Errorf("远端被改写：tip = %s，期望仍是 B 的提交 %s", got, cmB.SHA)
	}
	if cmA.SHA == cmB.SHA {
		t.Fatal("夹具不对：A 与 B 的提交应不同")
	}
}

// 差异：相对基线的改动文件清单。
func TestDiff_ListsFilesChangedSinceBaseline(t *testing.T) {
	requireGit(t)
	f := newFixture(t)
	repo := f.repo("xhunter/s6")
	g := New(Config{WorkDir: f.work})
	root, err := g.PrepareBaseline(context.Background(), repo)
	if err != nil {
		t.Fatalf("获取基线失败：%v", err)
	}
	writeFile(t, filepath.Join(root, "pkg/a.txt"), "one\n")
	writeFile(t, filepath.Join(root, "b.txt"), "two\n")
	if _, err := g.Commit(context.Background(), repo, "改动"); err != nil {
		t.Fatalf("提交失败：%v", err)
	}

	files, err := g.Diff(context.Background(), repo)
	if err != nil {
		t.Fatalf("产出差异失败：%v", err)
	}
	got := strings.Join(files, ",")
	if !strings.Contains(got, "pkg/a.txt") || !strings.Contains(got, "b.txt") {
		t.Errorf("差异清单 = %v，期望含两个新文件", files)
	}
}

// 补丁：相对基线的统一 diff，必须能原样应用到干净基线——否则"附带交付"是空话。
// 末尾换行属于内容：裁剪过的补丁会被 git apply 判为 corrupt。
func TestPatch_AppliesCleanlyToBaseline(t *testing.T) {
	requireGit(t)
	f := newFixture(t)
	repo := f.repo("xhunter/s8")
	g := New(Config{WorkDir: f.work})
	root, err := g.PrepareBaseline(context.Background(), repo)
	if err != nil {
		t.Fatalf("获取基线失败：%v", err)
	}

	// 无改动时补丁为空（不是错误）。
	empty, err := g.Patch(context.Background(), repo)
	if err != nil {
		t.Fatalf("无改动时产出补丁不该失败：%v", err)
	}
	if strings.TrimSpace(empty) != "" {
		t.Errorf("无改动应有空补丁：%q", empty)
	}

	writeFile(t, filepath.Join(root, "pkg/a.txt"), "new\n")
	writeFile(t, filepath.Join(root, "b.txt"), "two\n")
	if _, err := g.Commit(context.Background(), repo, "改动"); err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	patch, err := g.Patch(context.Background(), repo)
	if err != nil {
		t.Fatalf("产出补丁失败：%v", err)
	}
	if !strings.Contains(patch, "pkg/a.txt") || !strings.Contains(patch, "b.txt") {
		t.Fatalf("补丁缺改动文件：%s", patch)
	}

	// 干净基线上必须能应用。
	apply := filepath.Join(t.TempDir(), "apply")
	runGit(t, "", "clone", "-q", f.remote, apply)
	runGit(t, apply, "checkout", "-q", f.base)
	patchFile := filepath.Join(t.TempDir(), "x.patch")
	if err := os.WriteFile(patchFile, []byte(patch), 0o644); err != nil {
		t.Fatalf("写补丁失败：%v", err)
	}
	runGit(t, apply, "apply", patchFile)
	if b, err := os.ReadFile(filepath.Join(apply, "pkg", "a.txt")); err != nil || string(b) != "new\n" {
		t.Fatalf("补丁应用结果不对：%q err=%v", b, err)
	}
}

// 回收：临时工作树被删除；重复调用是空操作。
func TestClean_RemovesWorktreeAndIsIdempotent(t *testing.T) {
	requireGit(t)
	f := newFixture(t)
	g := New(Config{WorkDir: f.work})
	root, err := g.PrepareBaseline(context.Background(), f.repo("xhunter/s7"))
	if err != nil {
		t.Fatalf("获取基线失败：%v", err)
	}
	if err := g.Clean(context.Background()); err != nil {
		t.Fatalf("回收失败：%v", err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("工作树未被回收：%v", err)
	}
	if err := g.Clean(context.Background()); err != nil {
		t.Errorf("重复回收应是空操作：%v", err)
	}
	// 回收之后再提交必须显式失败，而不是在空工作区里"成功"。
	if _, err := g.Commit(context.Background(), f.repo("xhunter/s7"), "x"); err == nil {
		t.Error("工作树已回收，提交必须显式失败")
	}
}

// 部署事实缺失 / 远端不可达：启动期即环境错误，且不留临时工作树。
func TestPrepareBaseline_FailsAtStartupOnUnusableFacts(t *testing.T) {
	requireGit(t)
	work := t.TempDir()
	cases := []struct {
		name string
		repo git.RepoRef
	}{
		{"缺远端", git.RepoRef{Branch: "b", BaseCommit: "abc"}},
		{"缺基线", git.RepoRef{Remote: "r", Branch: "b"}},
		{"缺分支", git.RepoRef{Remote: "r", BaseCommit: "abc"}},
		{"远端不可达", git.RepoRef{Remote: filepath.Join(work, "nope.git"), Branch: "b", BaseCommit: "abc"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := New(Config{WorkDir: work})
			_, err := g.PrepareBaseline(context.Background(), tc.repo)
			if err == nil {
				t.Fatal("必须显式失败")
			}
			var fault *llm.Fault
			if !errors.As(err, &fault) {
				t.Fatalf("应是结构化错误：%v", err)
			}
			if !fault.Retryable {
				t.Error("环境问题应标记为可重试")
			}
		})
	}
	// 失败路径不留半成品工作树。
	entries, err := os.ReadDir(work)
	if err != nil {
		t.Fatalf("读取工作区父目录失败：%v", err)
	}
	if len(entries) != 0 {
		t.Errorf("失败路径不该留下临时工作树：%v", entries)
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

// 交付 diff / patch 排除**本次会话的材料目录**，但保留同目录下的其它路径（如 skills.draft）：
// 材料随检查点进分支是对的，但它不是交付内容（FR-6.1）；技能草稿要进补丁供人 review（FR-15.3、AC-25）。
func TestDiff_ExcludesMaterialDirButKeepsSkillsDraft(t *testing.T) {
	requireGit(t)
	f := newFixture(t)
	const session = "s-material"
	repo := f.repo("xhunter/s9")
	repo.MaterialDir = ".xhunter/" + session
	g := New(Config{WorkDir: f.work})
	root, err := g.PrepareBaseline(context.Background(), repo)
	if err != nil {
		t.Fatalf("获取基线失败：%v", err)
	}

	writeFile(t, filepath.Join(root, "biz.txt"), "biz\n")
	writeFile(t, filepath.Join(root, ".xhunter", session, "session.jsonl"), `{"type":"meta"}`+"\n")
	writeFile(t, filepath.Join(root, ".xhunter", "skills.draft", "sk.md"), "draft\n")
	if _, err := g.Commit(context.Background(), repo, "改动"); err != nil {
		t.Fatalf("提交失败：%v", err)
	}

	files, err := g.Diff(context.Background(), repo)
	if err != nil {
		t.Fatalf("产出差异失败：%v", err)
	}
	got := strings.Join(files, ",")
	if strings.Contains(got, ".xhunter/"+session) {
		t.Errorf("差异清单不该含材料目录：%v", files)
	}
	if !strings.Contains(got, "biz.txt") {
		t.Errorf("差异清单应含业务文件：%v", files)
	}
	if !strings.Contains(got, ".xhunter/skills.draft/sk.md") {
		t.Errorf("差异清单应保留技能草稿：%v", files)
	}

	patch, err := g.Patch(context.Background(), repo)
	if err != nil {
		t.Fatalf("产出补丁失败：%v", err)
	}
	if strings.Contains(patch, "session.jsonl") || strings.Contains(patch, ".xhunter/"+session) {
		t.Errorf("补丁不该含材料目录的内容差异：\n%s", patch)
	}
	if !strings.Contains(patch, "skills.draft/sk.md") || !strings.Contains(patch, "biz.txt") {
		t.Errorf("补丁应含业务文件与技能草稿：\n%s", patch)
	}
}

// MaterialDir 为空时不加排除 pathspec：现行为不被破坏（`.xhunter/**` 也照常出现）。
func TestDiff_NoMaterialDirKeepsEverything(t *testing.T) {
	requireGit(t)
	f := newFixture(t)
	repo := f.repo("xhunter/s10") // 无 MaterialDir
	g := New(Config{WorkDir: f.work})
	root, err := g.PrepareBaseline(context.Background(), repo)
	if err != nil {
		t.Fatalf("获取基线失败：%v", err)
	}
	writeFile(t, filepath.Join(root, ".xhunter", "s-x", "session.jsonl"), "x\n")
	if _, err := g.Commit(context.Background(), repo, "改动"); err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	files, err := g.Diff(context.Background(), repo)
	if err != nil {
		t.Fatalf("产出差异失败：%v", err)
	}
	if got := strings.Join(files, ","); !strings.Contains(got, ".xhunter/s-x/session.jsonl") {
		t.Errorf("无 MaterialDir 时不该排除任何路径：%v", files)
	}
}
