package agentsmd

import (
	"xhunter/internal/workspace/osfs"
	"context"
	"strings"
	"testing"

	"xhunter/harness"
)

// fixture 造一个带工作区与基线的输入：约定文件的读取必须走只读视图。
func fixture(t *testing.T, files map[string]string, base string) harness.PromptInput {
	t.Helper()
	st := newStore(t)
	for path, content := range files {
		if _, err := st.WriteRange(path, harness.ByteRange{Start: 0, End: 0}, content); err != nil {
			t.Fatalf("准备夹具失败：%v", err)
		}
	}
	return harness.PromptInput{
		Bounty:    harness.Bounty{Repo: harness.RepoRef{BaseCommit: base}},
		Workspace: st,
	}
}

func TestBuild_InjectsRootConventions(t *testing.T) {
	in := fixture(t, map[string]string{
		fileName: "# 约定\n\n- 提交信息用中文\n- 禁止引入第三方依赖\n",
	}, "deadbeefcafe1234")

	part, err := New().Build(context.Background(), in)
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}
	if !strings.Contains(part.Body, "提交信息用中文") || !strings.Contains(part.Body, "禁止引入第三方依赖") {
		t.Errorf("正文应含约定全文：%q", part.Body)
	}
	// 来源要能回答"这次用的是基线里哪一版约定"。
	if len(part.Sources) != 1 || part.Sources[0] != "base:deadbeefcafe:"+fileName {
		t.Errorf("来源 = %v，期望带上基线标识", part.Sources)
	}
	if len(part.Notices) != 0 {
		t.Errorf("正常路径不该有降级：%+v", part.Notices)
	}
}

// 没有约定文件是合法状态：不是错误，也不进正文——否则模型会看到一段空的"约定"。
func TestBuild_MissingFileIsNotAnError(t *testing.T) {
	part, err := New().Build(context.Background(), fixture(t, nil, "abc"))
	if err != nil {
		t.Fatalf("缺少约定文件不该报错：%v", err)
	}
	if part.Body != "" || len(part.Sources) != 0 || len(part.Notices) != 0 {
		t.Errorf("空仓库应产出空结果：%+v", part)
	}
}

func TestBuild_BlankContentIsTreatedAsAbsent(t *testing.T) {
	part, err := New().Build(context.Background(), fixture(t, map[string]string{
		fileName: "   \n\n\t\n",
	}, "abc"))
	if err != nil {
		t.Fatalf("空白内容不该报错：%v", err)
	}
	if part.Body != "" {
		t.Errorf("空白内容不该进正文：%q", part.Body)
	}
}

// 文件名精确匹配：大小写变体不算规范文件——跨平台大小写敏感度不同，
// 认下来会让"注入了什么"随文件系统而变。
func TestBuild_CaseVariantIsNotRecognized(t *testing.T) {
	part, err := New().Build(context.Background(), fixture(t, map[string]string{
		"agents.md": "# 小写变体\n",
	}, "abc"))
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}
	if part.Body != "" {
		t.Errorf("大小写变体不得被当作规范文件注入：%q", part.Body)
	}
}

// 超限截断必须留痕：静默截断会让模型以为约定只有这些。
func TestBuild_OversizeTruncatesWithNotice(t *testing.T) {
	big := strings.Repeat("这是一条很长的约定条目，用来把文件撑过上限。\n", 2000) // 约 80KB
	part, err := New().Build(context.Background(), fixture(t, map[string]string{fileName: big}, "abc"))
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}
	if len(part.Notices) != 1 {
		t.Fatalf("超限应产生一条降级记录：%+v", part.Notices)
	}
	n := part.Notices[0]
	if n.Scope != scope || n.Subject != fileName || !strings.Contains(n.Reason, "已截断") {
		t.Errorf("降级记录不完整：%+v", n)
	}
	if len(part.Body) > maxBytes+200 {
		t.Errorf("正文应在上限附近收刀，实际 %d 字节", len(part.Body))
	}
	if !strings.HasSuffix(part.Body, "\n") {
		t.Error("截断点应落在行边界上")
	}
}

// 基线缺省（本地工作区直读）时来源同样要可追溯。
func TestBuild_WithoutBaseCommitFallsBackToWorktree(t *testing.T) {
	part, err := New().Build(context.Background(), fixture(t, map[string]string{fileName: "x\n"}, ""))
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}
	if len(part.Sources) != 1 || !strings.Contains(part.Sources[0], "worktree") {
		t.Errorf("无基线时来源应标 worktree：%v", part.Sources)
	}
}

// newStore 造一个挂在临时目录上的本地工作区（实现来自 internal/workspace/osfs）。
func newStore(t *testing.T) harness.Storage {
	t.Helper()
	st, err := osfs.Opener{}.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开工作区失败：%v", err)
	}
	return st
}
