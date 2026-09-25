package basic

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"xhunter/hunt"
	"xhunter/llm"
	"xhunter/workspace"
)

// runFind 跑一次内容检索。find 是只读原语：任何情况下都不得产出编辑。
func runFind(t *testing.T, ws workspace.Workspace, call hunt.Call) hunt.Result {
	t.Helper()
	call.ID, call.Primitive = "c1", Find
	res, edits, err := FindTool(ws).Execute(context.Background(), call, newFacts())
	if err != nil {
		t.Fatalf("执行失败：%v", err)
	}
	if len(edits) != 0 {
		t.Fatalf("只读原语不得产出编辑：%+v", edits)
	}
	return res
}

// 检索结果必须带**文件与真实行号**，并把范围与命中数说清——模型据此决定要不要继续读。
func TestFind_MatchesWithLineNumbers(t *testing.T) {
	st := newStore(t)
	seed(t, st, "a.go", "package a\n\nvar TODO = 1\n")
	seed(t, st, "sub/b.go", "package b\n// TODO: 处理\n")

	res := runFind(t, st, hunt.Call{Selector: hunt.Selector{Literal: "TODO"}})
	if res.Err != nil {
		t.Fatalf("不该报错：%+v", res.Err)
	}
	for _, want := range []string{
		"找到 2 处匹配（2 个文件；范围：整个工作区）",
		"a.go:3: var TODO = 1",
		"sub/b.go:2: // TODO: 处理",
	} {
		if !strings.Contains(res.Summary, want) {
			t.Errorf("结果缺少 %q：\n%s", want, res.Summary)
		}
	}
}

// 「没找到」是**结论**而不是错误：模型据此判断"哪里都没有"，而不是"工具坏了"。
func TestFind_NoMatchIsSuccessWithExplicitText(t *testing.T) {
	st := newStore(t)
	seed(t, st, "a.go", "package a\n")

	res := runFind(t, st, hunt.Call{Selector: hunt.Selector{Literal: "不存在的片段"}})
	if res.Err != nil {
		t.Fatalf("未找到不该是错误：%+v", res.Err)
	}
	if !strings.Contains(res.Summary, "未找到匹配") || !strings.Contains(res.Summary, "扫描 1 个文件") {
		t.Errorf("未找到时必须说清范围与扫描量：%s", res.Summary)
	}
}

// scope 收窄目录：范围之外的同名内容不得出现在结果里。
func TestFind_ScopeLimitsSearch(t *testing.T) {
	st := newStore(t)
	seed(t, st, "inside/a.go", "// MARK\n")
	seed(t, st, "outside/b.go", "// MARK\n")

	res := runFind(t, st, hunt.Call{Selector: hunt.Selector{Literal: "MARK", Scope: "inside"}})
	if !strings.Contains(res.Summary, "范围：目录 inside") {
		t.Errorf("应说明被收窄的范围：%s", res.Summary)
	}
	if !strings.Contains(res.Summary, "inside/a.go:1") {
		t.Errorf("范围内应命中：%s", res.Summary)
	}
	if strings.Contains(res.Summary, "outside/b.go") {
		t.Errorf("范围外不得命中：%s", res.Summary)
	}
}

// path 限定单文件。
func TestFind_PathLimitsToSingleFile(t *testing.T) {
	st := newStore(t)
	seed(t, st, "a.go", "// HIT\n")
	seed(t, st, "b.go", "// HIT\n")

	res := runFind(t, st, hunt.Call{Target: "b.go", Selector: hunt.Selector{Literal: "HIT"}})
	if !strings.Contains(res.Summary, "范围：文件 b.go") {
		t.Errorf("应说明范围是单个文件：%s", res.Summary)
	}
	if strings.Contains(res.Summary, "a.go:") {
		t.Errorf("path 限定时不得检索其他文件：%s", res.Summary)
	}
}

// 命中数超上限：只显示前 N 处，但**总量照报**，并明确标注被截断——
// 否则模型会把"显示不完"读成"只有这些"。
func TestFind_TruncatesHitsButReportsTotal(t *testing.T) {
	st := newStore(t)
	var b strings.Builder
	total := findMaxHits + 7
	for i := 0; i < total; i++ {
		fmt.Fprintf(&b, "// HIT %d\n", i)
	}
	seed(t, st, "many.go", b.String())

	res := runFind(t, st, hunt.Call{Selector: hunt.Selector{Literal: "HIT"}})
	want := fmt.Sprintf("找到 %d 处匹配（1 个文件；范围：整个工作区），只显示前 %d 处", total, findMaxHits)
	if !strings.Contains(res.Summary, want) {
		t.Errorf("截断口径不对：\nwant %s\ngot  %s", want, head(res.Summary))
	}
	if got := strings.Count(res.Summary, "many.go:"); got != findMaxHits {
		t.Errorf("显示条数 = %d，期望 %d", got, findMaxHits)
	}
}

// 缺字面量：显式报错（与 edit 同一形状），不猜、不返回空结果。
func TestFind_RequiresLiteral(t *testing.T) {
	_, _, err := FindTool(newStore(t)).Execute(context.Background(),
		hunt.Call{ID: "c1", Primitive: Find}, newFacts())
	if err == nil {
		t.Fatal("缺字面量必须报错")
	}
	var fault *llm.Fault
	if !errors.As(err, &fault) || fault.Kind != "bad_selector" {
		t.Errorf("错误类型 = %v，期望 bad_selector", err)
	}
}

// 声明形状：参数是"检索片段 + 可选范围"，必填落在 anyOf 的分支里。
func TestFind_DeclShape(t *testing.T) {
	d := FindTool(nil).Decl()
	if d.Name != string(Find) {
		t.Errorf("声明名 = %q，期望 %q", d.Name, Find)
	}
	s := decodeSchema(t, d)
	if got := propNames(s.Properties); len(got) != 4 || got[0] != "include" || got[1] != "literal" || got[2] != "path" || got[3] != "scope" {
		t.Errorf("参数面 = %v，期望 [include literal path scope]", got)
	}
	schema := string(d.Schema)
	if !strings.Contains(schema, `"anyOf"`) {
		t.Errorf("检索片段应表达为 anyOf 分支：%s", schema)
	}
	if !strings.Contains(schema, `"literal"`) {
		t.Errorf("schema 必须声明检索片段：%s", schema)
	}
}

// 与 glob 同一条枚举面：find 也必须把 include 传下去，并把「未枚举」写进结论。
func TestFind_IncludeReachesTheEnumeration(t *testing.T) {
	st := newStore(t)
	seed(t, st, "a.go", "x")
	seed(t, st, "node_modules/pkg/dep.go", "x")

	got := runFind(t, st, hunt.Call{Selector: hunt.Selector{Literal: "x"}})
	if !strings.Contains(got.Summary, "未枚举：node_modules") {
		t.Errorf("排除必须上报：%s", got.Summary)
	}
	if strings.Contains(got.Summary, "node_modules/pkg/dep.go") {
		t.Errorf("默认名单命中时不得检索：%s", got.Summary)
	}

	got = runFind(t, st, hunt.Call{Selector: hunt.Selector{Literal: "x", Include: []string{"node_modules"}}})
	if !strings.Contains(got.Summary, "node_modules/pkg/dep.go") {
		t.Errorf("include 必须到达枚举：%s", got.Summary)
	}
}
