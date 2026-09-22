package glob

import (
	"xhunter/internal/workspace/osfs"
	"context"
	"strings"
	"testing"

	"xhunter/harness"
)

// facts 只实现本原语用到的东西：工作区（枚举文件）。
type facts struct {
	harness.TaskFacts
	ws harness.Workspace
}

func (f *facts) Workspace() harness.Workspace { return f.ws }

// fixture 造一棵小树：够覆盖"按文件名匹配、递归、跳过隐藏目录"三件事。
func fixture(t *testing.T) (harness.PlanInput, *facts) {
	t.Helper()
	st := newStore(t)
	for _, p := range []string{"a.go", "b.md", "sub/c.go", "sub/deep/d.go"} {
		if _, err := st.WriteRange(p, harness.ByteRange{Start: 0, End: 0}, "x"); err != nil {
			t.Fatal(err)
		}
	}
	// 隐藏目录里的同名文件：工作区的枚举面会把 .git 这类噪音挡在外面。
	for _, p := range []string{".git/e.go", ".cache/f.go"} {
		if _, err := st.WriteRange(p, harness.ByteRange{Start: 0, End: 0}, "x"); err != nil {
			t.Fatal(err)
		}
	}
	f := &facts{ws: st}
	return harness.PlanInput{
		Call:  harness.Call{ID: "c1", Primitive: Name, Selector: harness.Selector{Scope: "*.go"}},
		Route: harness.Route{Path: harness.PathText, Reason: "该原语只有文本路径"},
		Facts: f,
	}, f
}

func TestPlan_MatchesByFileNameRecursively(t *testing.T) {
	in, _ := fixture(t)

	got, err := impl{}.Plan(context.Background(), in)
	if err != nil {
		t.Fatalf("枚举不该报错：%v", err)
	}
	for _, want := range []string{"a.go", "sub/c.go", "sub/deep/d.go"} {
		if !strings.Contains(got.Result.Summary, want) {
			t.Errorf("应匹配到 %s：%s", want, got.Result.Summary)
		}
	}
	// 模式按**文件名**匹配，不是路径：所以 b.md 不进来，.git 里的也不进来。
	if strings.Contains(got.Result.Summary, "b.md") {
		t.Errorf("不匹配的扩展名不得出现：%s", got.Result.Summary)
	}
	if strings.Contains(got.Result.Summary, "e.go") || strings.Contains(got.Result.Summary, "f.go") {
		t.Errorf("隐藏目录是噪音，不该进入枚举面：%s", got.Result.Summary)
	}
	if !strings.Contains(got.Result.Summary, "3 个匹配") {
		t.Errorf("应报出匹配数量：%s", got.Result.Summary)
	}
}

// Scope 缺省时退回 Target：两个槽位都表达"要匹配什么"，避免模型必须记住用哪个。
func TestPlan_FallsBackToTarget(t *testing.T) {
	in, _ := fixture(t)
	in.Call.Selector.Scope = ""
	in.Call.Target = "*.md"

	got, err := impl{}.Plan(context.Background(), in)
	if err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	if !strings.Contains(got.Result.Summary, "b.md") {
		t.Errorf("缺省应使用 Target 作为模式：%s", got.Result.Summary)
	}
}

func TestPlan_NoMatchIsNotAnError(t *testing.T) {
	in, _ := fixture(t)
	in.Call.Selector.Scope = "*.rs"

	got, err := impl{}.Plan(context.Background(), in)
	if err != nil {
		t.Fatalf("没有匹配不是错误：%v", err)
	}
	if !strings.HasPrefix(got.Result.Summary, "0 个匹配") {
		t.Errorf("应明确报出 0 个匹配：%s", got.Result.Summary)
	}
}

// 非法模式由工作区拒绝：原语不自己判断路径边界。
func TestPlan_InvalidPatternPropagates(t *testing.T) {
	in, _ := fixture(t)
	in.Call.Selector.Scope = "../*.go"

	if _, err := (impl{}).Plan(context.Background(), in); err == nil {
		t.Fatal("越界模式必须被拒绝")
	}
}

func TestTool_Shape(t *testing.T) {
	tool := Tool()
	if tool.Name != Name || tool.Decl.Name != string(Name) {
		t.Errorf("名字与声明必须一致：%+v", tool)
	}
	if tool.Address != harness.AddressedAsText {
		t.Errorf("枚举没有符号语义，寻址性质 = %q", tool.Address)
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
