package skills

import (
	"context"
	"strings"
	"testing"
	"xhunter/internal/workspace/osfs"

	"xhunter/git"
	"xhunter/hunt"
	"xhunter/workspace"
)

// fixture 造一个带工作区与基线的输入。
func fixture(t *testing.T, files map[string]string, base string) (hunt.PromptInput, workspace.Workspace) {
	t.Helper()
	st := newStore(t)
	for path, content := range files {
		if _, err := st.WriteRange(path, workspace.ByteRange{Start: 0, End: 0}, content); err != nil {
			t.Fatalf("准备夹具失败：%v", err)
		}
	}
	return hunt.PromptInput{
		Bounty: hunt.Bounty{Repo: git.RepoRef{BaseCommit: base}},
	}, st
}

// build 用夹具的工作区构造插件并执行一次 Build。
func build(t *testing.T, files map[string]string, base string) (hunt.PromptPart, error) {
	t.Helper()
	in, ws := fixture(t, files, base)
	return New(ws).Build(context.Background(), in)
}

func skill(name, desc string) string {
	return "---\nname: " + name + "\ndescription: " + desc + "\n---\n\n# " + name + "\n正文\n"
}

// 清单只带 name + description，并**必须带入口路径**——模型据此用 read 取全文。
func TestBuild_ListsNameDescriptionAndPath(t *testing.T) {
	in, ws := fixture(t, map[string]string{
		dir + "release/SKILL.md": skill("release", "发布流程与版本号约定"),
		dir + "lint/SKILL.md":    skill("lint", "静态检查约定"),
	}, "deadbeefcafe1234")

	part, err := New(ws).Build(context.Background(), in)
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}
	if len(part.Notices) != 0 {
		t.Fatalf("合法输入不该有降级：%+v", part.Notices)
	}
	for _, want := range []string{
		"- release：发布流程与版本号约定（" + dir + "release/SKILL.md）",
		"- lint：静态检查约定（" + dir + "lint/SKILL.md）",
	} {
		if !strings.Contains(part.Body, want) {
			t.Errorf("清单缺少条目：%q\n实际：%s", want, part.Body)
		}
	}
	// 正文（SKILL.md 的 body）不得进清单：那是"激活"这一步的事。
	if strings.Contains(part.Body, "正文") {
		t.Error("清单不该包含 SKILL.md 正文")
	}
	if len(part.Sources) != 1 || !strings.Contains(part.Sources[0], "skills(2)") {
		t.Errorf("来源应记录条数与基线：%v", part.Sources)
	}
}

// 单条非法只跳过这一条：一份坏文件不该让整个任务起不来，但必须留痕。
func TestBuild_InvalidEntryIsSkippedWithNotice(t *testing.T) {
	in, ws := fixture(t, map[string]string{
		dir + "good/SKILL.md": skill("good", "这条没问题"),
		dir + "bad/SKILL.md":  "---\nname: bad\n---\n没有 description\n",
	}, "abc")

	part, err := New(ws).Build(context.Background(), in)
	if err != nil {
		t.Fatalf("单条非法不该让构造失败：%v", err)
	}
	if !strings.Contains(part.Body, "- good：") {
		t.Errorf("合法条目仍应进清单：%s", part.Body)
	}
	if strings.Contains(part.Body, "bad") {
		t.Error("非法条目不得进清单")
	}
	if len(part.Notices) != 1 {
		t.Fatalf("应有一条降级记录：%+v", part.Notices)
	}
	n := part.Notices[0]
	if n.Scope != scope || !strings.HasSuffix(n.Subject, "bad/SKILL.md") || !strings.Contains(n.Reason, "description") {
		t.Errorf("降级记录应指明对象与原因：%+v", n)
	}
}

// name 与目录名不一致时无法判断该信哪个——跳过比猜更安全。
func TestBuild_NameMustMatchDirectory(t *testing.T) {
	in, ws := fixture(t, map[string]string{
		dir + "release/SKILL.md": skill("publish", "名字与目录不符"),
	}, "abc")

	part, err := New(ws).Build(context.Background(), in)
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}
	if part.Body != "" {
		t.Errorf("不一致的条目应被跳过：%s", part.Body)
	}
	if len(part.Notices) != 1 || !strings.Contains(part.Notices[0].Reason, "目录名不一致") {
		t.Errorf("应报告不一致：%+v", part.Notices)
	}
}

// 只认约定目录：别处的同名文件不是技能。否则仓库里任何一个 SKILL.md
// 都会悄悄进入模型可见面。
func TestBuild_IgnoresSkillFilesOutsideDirectory(t *testing.T) {
	in, ws := fixture(t, map[string]string{
		"SKILL.md":               skill("root", "根目录的同名文件"),
		"vendor/other/SKILL.md":  skill("other", "第三方目录里的"),
		dir + "release/SKILL.md": skill("release", "这才是技能"),
	}, "abc")

	part, err := New(ws).Build(context.Background(), in)
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}
	if strings.Contains(part.Body, "根目录的同名文件") || strings.Contains(part.Body, "第三方目录里的") {
		t.Errorf("约定目录之外的同类文件不得进清单：%s", part.Body)
	}
	if !strings.Contains(part.Body, "这才是技能") {
		t.Errorf("约定目录内的条目应进清单：%s", part.Body)
	}
}

func TestBuild_NoSkillsYieldsEmptyPart(t *testing.T) {
	part, err := build(t, nil, "abc")
	if err != nil {
		t.Fatalf("没有技能不该报错：%v", err)
	}
	if part.Body != "" || len(part.Notices) != 0 {
		t.Errorf("空目录应产出空结果：%+v", part)
	}
}

// 清单上限：溢出截断并上报——清单本身也会把预算吃光。
func TestBuild_TruncatesBeyondLimit(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < maxEntries+3; i++ {
		name := "s" + string(rune('a'+i%26)) + pad(i)
		files[dir+name+"/SKILL.md"] = skill(name, "说明 "+name)
	}
	part, err := build(t, files, "abc")
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}
	if got := strings.Count(part.Body, "\n- "); got != maxEntries {
		t.Errorf("清单条目数 = %d，期望 %d", got, maxEntries)
	}
	if len(part.Notices) != 1 || !strings.Contains(part.Notices[0].Reason, "超过上限") {
		t.Errorf("溢出应留下降级记录：%+v", part.Notices)
	}
}

func pad(i int) string {
	const digits = "0123456789"
	return string([]byte{digits[i/10], digits[i%10]})
}

// newStore 造一个挂在临时目录上的本地工作区（实现来自 internal/workspace/osfs）。
func newStore(t *testing.T) workspace.Storage {
	t.Helper()
	st, err := osfs.Opener{}.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开工作区失败：%v", err)
	}
	return st
}
