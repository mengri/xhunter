package basic

import (
	"context"
	"strings"
	"testing"

	"xhunter/hunt"
	"xhunter/workspace"
)

// globFixture 造一棵小树：够覆盖"按文件名匹配、递归、跳过隐藏目录"三件事。
func globFixture(t *testing.T) (hunt.Call, workspace.Storage) {
	t.Helper()
	st := newStore(t)
	for _, p := range []string{"a.go", "b.md", "sub/c.go", "sub/deep/d.go"} {
		seed(t, st, p, "x")
	}
	// 隐藏目录里的同名文件：工作区的枚举面会把 .git 这类噪音挡在外面。
	for _, p := range []string{".git/e.go", ".cache/f.go"} {
		seed(t, st, p, "x")
	}
	return hunt.Call{ID: "c1", Primitive: Glob, Selector: hunt.Selector{Scope: "*.go"}}, st
}

func TestGlob_MatchesByFileNameRecursively(t *testing.T) {
	call, st := globFixture(t)

	res, edits, err := GlobTool(st).Execute(context.Background(), call, newFacts())
	if err != nil {
		t.Fatalf("枚举不该报错：%v", err)
	}
	if len(edits) != 0 {
		t.Errorf("枚举不得产出编辑：%+v", edits)
	}
	for _, want := range []string{"a.go", "sub/c.go", "sub/deep/d.go"} {
		if !strings.Contains(res.Summary, want) {
			t.Errorf("应匹配到 %s：%s", want, res.Summary)
		}
	}
	// 模式按**文件名**匹配，不是路径：所以 b.md 不进来，.git 里的也不进来。
	if strings.Contains(res.Summary, "b.md") {
		t.Errorf("不匹配的扩展名不得出现：%s", res.Summary)
	}
	if strings.Contains(res.Summary, "e.go") || strings.Contains(res.Summary, "f.go") {
		t.Errorf("隐藏目录是噪音，不该进入枚举面：%s", res.Summary)
	}
	if !strings.Contains(res.Summary, "3 个匹配") {
		t.Errorf("应报出匹配数量：%s", res.Summary)
	}
}

// Scope 缺省时退回 Target：两个槽位都表达"要匹配什么"，避免模型必须记住用哪个。
func TestGlob_FallsBackToTarget(t *testing.T) {
	call, st := globFixture(t)
	call.Selector.Scope = ""
	call.Target = "*.md"

	res, _, err := GlobTool(st).Execute(context.Background(), call, newFacts())
	if err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	if !strings.Contains(res.Summary, "b.md") {
		t.Errorf("缺省应使用 Target 作为模式：%s", res.Summary)
	}
}

// 没有匹配不是错误：模型需要能区分"此地无此物"与"这次调用坏了"。
func TestGlob_NoMatchIsNotAnError(t *testing.T) {
	call, st := globFixture(t)
	call.Selector.Scope = "*.rs"

	res, _, err := GlobTool(st).Execute(context.Background(), call, newFacts())
	if err != nil {
		t.Fatalf("没有匹配不是错误：%v", err)
	}
	if !strings.HasPrefix(res.Summary, "0 个匹配") {
		t.Errorf("应明确报出 0 个匹配：%s", res.Summary)
	}
}

// 非法模式由工作区拒绝：原语不自己判断路径边界。
func TestGlob_InvalidPatternPropagates(t *testing.T) {
	call, st := globFixture(t)
	call.Selector.Scope = "../*.go"

	if _, _, err := GlobTool(st).Execute(context.Background(), call, newFacts()); err == nil {
		t.Fatal("越界模式必须被拒绝")
	}
}

// 声明形状：唯一的参数是文件名模式。
func TestGlob_DeclShape(t *testing.T) {
	d := GlobTool(nil).Decl()
	if d.Name != string(Glob) {
		t.Errorf("声明名 = %q，期望 %q", d.Name, Glob)
	}
	s := decodeSchema(t, d)
	if got := propNames(s.Properties); len(got) != 1 || got[0] != "scope" {
		t.Errorf("参数面 = %v，期望 [scope]", got)
	}
	if len(s.Required) != 1 || s.Required[0] != "scope" {
		t.Errorf("required = %v，期望只有 scope", s.Required)
	}
}

// 目录模式与 `**/` 必须能用：这是模型最惯用的两种写法（见 List 的模式语义）。
func TestGlob_DirectoryAndDeepPatterns(t *testing.T) {
	st := newStore(t)
	for _, f := range []string{"top.go", "internal/a.go", "internal/deep/b.go"} {
		seed(t, st, f, "package x")
	}
	cases := map[string]string{
		"internal/*.go":    "internal/a.go",
		"internal/**/*.go": "internal/deep/b.go",
		"**/*.go":          "top.go",
	}
	for pattern, want := range cases {
		res, _, err := GlobTool(st).Execute(context.Background(),
			hunt.Call{ID: "c1", Primitive: Glob, Selector: hunt.Selector{Scope: pattern}}, newFacts())
		if err != nil {
			t.Fatalf("模式 %q 失败：%v", pattern, err)
		}
		if !strings.Contains(res.Summary, want) {
			t.Errorf("模式 %q 应命中 %q：%s", pattern, want, res.Summary)
		}
	}
}
