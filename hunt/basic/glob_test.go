package basic

import (
	"context"
	"strings"
	"testing"

	"xhunter/hunt"
	"xhunter/workspace"
)

// globFixture 造一棵小树：覆盖"按文件名匹配、递归、点开头目录可见、.git 不可见"四件事。
func globFixture(t *testing.T) (hunt.Call, workspace.Storage) {
	t.Helper()
	st := newStore(t)
	for _, p := range []string{"a.go", "b.md", "sub/c.go", "sub/deep/d.go"} {
		seed(t, st, p, "x")
	}
	// .git 是宿主内部（永不放行）；.cache 是点开头目录，点开头不代表不是项目内容，因此可见。
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
	// 模式按**文件名**匹配，不是路径：所以 b.md 不进来。
	if strings.Contains(res.Summary, "b.md") {
		t.Errorf("不匹配的扩展名不得出现：%s", res.Summary)
	}
	// .git 是宿主内部，永不进入枚举面。
	if strings.Contains(res.Summary, "e.go") {
		t.Errorf(".git 不该进入枚举面：%s", res.Summary)
	}
	// 点开头目录**可见**：点开头只是“默认不显示”的约定，不代表不是项目内容。
	if !strings.Contains(res.Summary, ".cache/f.go") {
		t.Errorf("点开头目录必须可见：%s", res.Summary)
	}
	if !strings.Contains(res.Summary, "4 个匹配") {
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
	if got := propNames(s.Properties); len(got) != 2 || got[0] != "include" || got[1] != "scope" {
		t.Errorf("参数面 = %v，期望 [include scope]", got)
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

// include 必须真的到达枚举层：绑定层收下 → Selector → List → 结果与「未枚举」附注。
// 只断言 schema 里有 include 不够——绑定层漏收或原语漏传，参数面都会虚假成立。
func TestGlob_IncludeReachesTheEnumeration(t *testing.T) {
	st := newStore(t)
	for _, p := range []string{"a.go", "node_modules/pkg/dep.go"} {
		seed(t, st, p, "x")
	}
	call := hunt.Call{ID: "c1", Primitive: Glob, Selector: hunt.Selector{Scope: "*.go"}}

	got, _, err := GlobTool(st).Execute(context.Background(), call, newFacts())
	if err != nil {
		t.Fatalf("枚举不该报错：%v", err)
	}
	if !strings.Contains(got.Summary, "未枚举：node_modules") {
		t.Errorf("排除必须上报：%s", got.Summary)
	}
	if strings.Contains(got.Summary, "node_modules/pkg/dep.go") {
		t.Errorf("默认名单命中时不得匹配：%s", got.Summary)
	}

	call.Selector.Include = []string{"node_modules"}
	got, _, err = GlobTool(st).Execute(context.Background(), call, newFacts())
	if err != nil {
		t.Fatalf("枚举不该报错：%v", err)
	}
	if !strings.Contains(got.Summary, "node_modules/pkg/dep.go") {
		t.Errorf("include 必须到达枚举：%s", got.Summary)
	}
	if strings.Contains(got.Summary, "未枚举") {
		t.Errorf("放行后不该再报未枚举：%s", got.Summary)
	}
}
