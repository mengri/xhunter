package osfs

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"xhunter/workspace"
)

// 工作区是唯一写入原语与只读视图的落点，它的两条行为值得单独钉住：
// 枚举面（什么该被看见）与路径边界（什么表达不出来）。

// 枚举跳过隐藏目录是为了躲噪音（版本控制、编辑器、构建产物），
// 但**本系统自己的控制目录例外**——它是配置，不是噪音；跳过它就等于
// 技能清单这类东西根本看不见。
func TestList_SkipsNoiseButKeepsControlDir(t *testing.T) {
	root := t.TempDir()
	st := open(t, root)
	for _, p := range []string{
		"a.go",
		"sub/b.go",
		".git/c.go",
		".cache/d.go",
		controlDir + "/gates.yml",
		controlDir + "/skills/release/SKILL.md",
	} {
		if _, err := st.WriteRange(p, workspace.ByteRange{Start: 0, End: 0}, "x"); err != nil {
			t.Fatalf("准备夹具失败：%v", err)
		}
	}

	got, err := st.List("*.go")
	if err != nil {
		t.Fatalf("枚举失败：%v", err)
	}
	if want := []string{"a.go", "sub/b.go"}; !reflect.DeepEqual(got, want) {
		t.Errorf("*.go 枚举 = %v，期望 %v（隐藏目录是噪音）", got, want)
	}

	all, err := st.List("SKILL.md")
	if err != nil {
		t.Fatalf("枚举失败：%v", err)
	}
	if want := []string{controlDir + "/skills/release/SKILL.md"}; !reflect.DeepEqual(all, want) {
		t.Errorf("控制目录必须可见：%v", all)
	}
}

// 顺序稳定是刻意的：同样的工作区状态每次应当得到同样的结果，
// 否则同一份上下文在不同次运行里会不一样（缓存与可复现性一起失效）。
func TestList_OrderIsStable(t *testing.T) {
	st := open(t, t.TempDir())
	for _, p := range []string{"z.go", "a.go", "m.go"} {
		if _, err := st.WriteRange(p, workspace.ByteRange{Start: 0, End: 0}, "x"); err != nil {
			t.Fatal(err)
		}
	}
	first, err := st.List("*.go")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a.go", "m.go", "z.go"}; !reflect.DeepEqual(first, want) {
		t.Errorf("枚举 = %v，期望按字典序 %v", first, want)
	}
	second, _ := st.List("*.go")
	if !reflect.DeepEqual(first, second) {
		t.Errorf("两次枚举结果不一致：%v → %v", first, second)
	}
}

// 路径边界：绝对路径与 ".." 逃逸在**表达层面**就被拒——不是被检查拦下，
// 而是这类位置根本不在接口的词汇表里。符号链接逃逸是第三条，需要真实软链，
// 由下面的用例覆盖。
func TestResolve_RejectsPathsOutsideWorkspace(t *testing.T) {
	st, _ := Opener{}.Open(t.TempDir())
	for _, rel := range []string{"", "   ", "/etc/passwd", "../outside", "a/../../outside"} {
		if _, err := st.Read(rel, workspace.LineRange{}); err == nil {
			t.Errorf("%q 必须被拒绝", rel)
		}
		if _, err := st.Stat(rel); err == nil {
			t.Errorf("%q 的 Stat 也必须被拒绝", rel)
		}
	}
}

// 符号链接逃逸：路径本身在工作区内，但指向外面——前两道拦不住它。
func TestResolve_RejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := writeOutside(outside); err != nil {
		t.Skipf("环境不支持创建符号链接：%v", err)
	}
	link := filepath.Join(root, "escape")
	if err := symlink(outside, link); err != nil {
		t.Skipf("环境不支持创建符号链接：%v", err)
	}

	st := open(t, root)
	if _, err := st.Read("escape/secret.txt", workspace.LineRange{}); err == nil {
		t.Error("指向工作区之外的符号链接必须被拒绝")
	}
}

// 写盘路径上的符号链接逃逸：中间级是软链、**叶子还不存在**时也必须拦住。
//
// 这是最容易漏的一档：叶子不存在，EvalSymlinks 对整条路径会失败，若据此跳过边界检查，
// 写新文件就会顺着软链落到工作区之外——而"写新文件"正是这条边界最该生效的场合。
func TestResolve_RejectsSymlinkEscapeForNewFile(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "escape")
	if err := symlink(outside, link); err != nil {
		t.Skipf("环境不支持创建符号链接：%v", err)
	}

	st := open(t, root)
	// 读不存在的叶子
	if _, err := st.Read("escape/new.txt", workspace.LineRange{}); err == nil {
		t.Error("经符号链接读工作区外的不存在文件必须被拒绝")
	}
	// 写不存在的叶子：唯一写盘入口同样要走这道检查
	if _, err := st.WriteRange("escape/new.txt", workspace.ByteRange{}, "pwned"); err == nil {
		t.Error("经符号链接写工作区外的新文件必须被拒绝")
	}
	if _, err := os.Stat(filepath.Join(outside, "new.txt")); err == nil {
		t.Fatal("工作区之外出现了写入结果——边界被绕过")
	}
	// 更深一层同理：祖先链上任何一级越界都要拦住。
	if _, err := st.WriteRange("escape/deep/new.txt", workspace.ByteRange{}, "pwned"); err == nil {
		t.Error("深层路径同样必须被拒绝")
	}
}

// 反向：指向**工作区内部**的符号链接不是逃逸，不能误伤。
func TestResolve_AllowsSymlinkInsideWorkspace(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	if err := symlink(filepath.Join("real"), filepath.Join(root, "link")); err != nil {
		t.Skipf("环境不支持创建符号链接：%v", err)
	}

	st := open(t, root)
	if _, err := st.WriteRange("link/new.txt", workspace.ByteRange{}, "ok"); err != nil {
		t.Fatalf("指向工作区内部的软链应放行：%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "real", "new.txt")); err != nil {
		t.Errorf("写入应落在软链目标（工作区内）：%v", err)
	}
}

// 符号链接相关的测试辅助。

func writeOutside(dir string) error {
	return os.WriteFile(dir+"/secret.txt", []byte("secret"), 0o644)
}

func symlink(target, link string) error { return os.Symlink(target, link) }

// open 打开一个以 root 为根的工作区。
func open(t *testing.T, root string) workspace.Storage {
	t.Helper()
	st, err := Opener{}.Open(root)
	if err != nil {
		t.Fatalf("打开工作区失败：%v", err)
	}
	return st
}

// 模式语义要覆盖模型惯用的两种写法：带目录的路径模式与 `**/` 通配。
// 此前只按 basename 匹配，`internal/*.go` 与 `**/*.go` 都静默返回空——
// "调用写错了"看起来像"仓库里没有这类文件"。
func TestList_PatternSemantics(t *testing.T) {
	root := t.TempDir()
	st := open(t, root)
	for _, p := range []string{"top.go", "internal/a.go", "internal/deep/b.go", "docs/readme.md"} {
		if _, err := st.WriteRange(p, workspace.ByteRange{Start: 0, End: 0}, "x"); err != nil {
			t.Fatalf("准备夹具失败：%v", err)
		}
	}
	cases := []struct {
		pattern string
		want    []string
	}{
		{"*.go", []string{"internal/a.go", "internal/deep/b.go", "top.go"}}, // 文件名模式：任意深度
		{"**/*.go", []string{"internal/a.go", "internal/deep/b.go", "top.go"}},
		{"internal/*.go", []string{"internal/a.go"}}, // 路径模式：`*` 不跨目录
		{"internal/**/*.go", []string{"internal/a.go", "internal/deep/b.go"}},
		{"docs/*.md", []string{"docs/readme.md"}},
		{"*.md", []string{"docs/readme.md"}},
	}
	for _, tc := range cases {
		got, err := st.List(tc.pattern)
		if err != nil {
			t.Fatalf("List(%q) 失败：%v", tc.pattern, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("List(%q) = %v，期望 %v", tc.pattern, got, tc.want)
		}
	}

	// 空模式是显式错误：它一个都匹配不到，返回空列表会把"调用写错"伪装成"没有这类文件"。
	if _, err := st.List("   "); err == nil {
		t.Error("空模式必须报错")
	}
}

// 按行范围读取要交出真实起点行号：模型据此写出的"第 N 行"必须指向同一行。
func TestRead_ReportsFirstLineOfRange(t *testing.T) {
	st := open(t, t.TempDir())
	if _, err := st.WriteRange("a.txt", workspace.ByteRange{Start: 0, End: 0}, "L1\nL2\nL3\nL4\n"); err != nil {
		t.Fatalf("准备夹具失败：%v", err)
	}
	fc, err := st.Read("a.txt", workspace.LineRange{From: 3, To: 4})
	if err != nil {
		t.Fatalf("读取失败：%v", err)
	}
	if fc.Raw != "L3\nL4" || !fc.Truncated {
		t.Fatalf("区间读取结果不对：%q truncated=%v", fc.Raw, fc.Truncated)
	}
	if fc.FirstLine != 3 {
		t.Errorf("FirstLine = %d，期望 3", fc.FirstLine)
	}
	if fc.TotalLines != 4 {
		t.Errorf("TotalLines = %d，期望 4（总量不受区间影响）", fc.TotalLines)
	}

	// 全文读取：起点是 1。
	all, _ := st.Read("a.txt", workspace.LineRange{})
	if all.FirstLine != 1 {
		t.Errorf("全文读取的 FirstLine = %d，期望 1", all.FirstLine)
	}

	// 区间起点越界：交出空内容，但起点仍如实报告（不假装是第 1 行）。
	beyond, _ := st.Read("a.txt", workspace.LineRange{From: 99, To: 100})
	if beyond.Raw != "" || beyond.FirstLine != 99 {
		t.Errorf("越界区间应给空内容与真实起点：%q first=%d", beyond.Raw, beyond.FirstLine)
	}
}
