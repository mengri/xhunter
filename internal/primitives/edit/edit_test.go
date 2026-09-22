package edit

import (
	"xhunter/internal/workspace/osfs"
	"context"
	"errors"
	"strings"
	"testing"

	"xhunter/harness"
)

// facts 只实现本原语用到的东西：工作区（读原文）与台账。
// 其余方法靠嵌入的接口占位，真被调用即 panic——证明 edit 只产出编辑计划、不自己落盘。
type facts struct {
	harness.TaskFacts
	ws     harness.Workspace
	ledger *harness.Ledger
}

func (f *facts) Workspace() harness.Workspace { return f.ws }
func (f *facts) Ledger() *harness.Ledger      { return f.ledger }

var errNoRead = errors.New("本次调用不该读文件")

// noReadWorkspace 的读一律失败：用来证明某些路径**根本不读文件**。
type noReadWorkspace struct{ harness.Storage }

func (w noReadWorkspace) Read(string, harness.LineRange) (harness.FileContent, error) {
	return harness.FileContent{}, errNoRead
}

func newFacts(st harness.Storage) *facts {
	return &facts{ws: st, ledger: harness.NewLedger()}
}

// textInput 造一个内容寻址的调用：目标文件已写好，字面量是 file 里的一段。
func textInput(t *testing.T, file, literal string) harness.PlanInput {
	t.Helper()
	st := newStore(t)
	if _, err := st.WriteRange("a.go", harness.ByteRange{Start: 0, End: 0}, file); err != nil {
		t.Fatal(err)
	}
	return harness.PlanInput{
		Call: harness.Call{
			ID: "c1", Primitive: Name, Target: "a.go",
			Selector: harness.Selector{Literal: literal},
			Content:  "NEW",
		},
		Route: harness.Route{Path: harness.PathText, Reason: "内容寻址选择器"},
		Facts: newFacts(st),
	}
}

// 唯一匹配时给出精确区间。用重复模板做夹具：精确命中的前提是"只有一处"。
func TestPlan_UniqueMatchProducesExactRange(t *testing.T) {
	file := "func a() {}\nfunc b() {}\nfunc c() {}\n"
	in := textInput(t, file, "func b() {}")

	got, err := impl{}.Plan(context.Background(), in)
	if err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	if len(got.Edits) != 1 {
		t.Fatalf("编辑数 = %d，期望 1", len(got.Edits))
	}
	e := got.Edits[0]
	if e.NewContent != "NEW" || e.File != "a.go" {
		t.Errorf("编辑 = %+v", e)
	}
	// 区间必须正好覆盖"func b() {}"，且原文与该区间一致——
	// 改错位置是这类编辑最坏的失败（模型察觉不到）。
	want := strings.Index(file, "func b() {}")
	if e.ByteRange != (harness.ByteRange{Start: want, End: want + len("func b() {}")}) {
		t.Errorf("区间 = %+v，期望 [%d,%d)", e.ByteRange, want, want+len("func b() {}"))
	}
	if file[e.ByteRange.Start:e.ByteRange.End] != "func b() {}" {
		t.Error("区间取值与目标字面量不一致")
	}
}

// 找不到就报找不到：不做模糊匹配、不做"最接近的一处"。
func TestPlan_NoMatchReportsNotFound(t *testing.T) {
	got, err := impl{}.Plan(context.Background(), textInput(t, "func a() {}\n", "func z() {}"))
	if err != nil {
		t.Fatalf("未匹配不是执行失败：%v", err)
	}
	if got.Result.Err == nil || got.Result.Err.Kind != "not_found" {
		t.Fatalf("应返回 not_found：%+v", got.Result)
	}
	if !got.Result.Err.Retryable {
		t.Error("补足上下文后可以重试")
	}
	if len(got.Edits) != 0 {
		t.Errorf("未匹配不得产出编辑：%+v", got.Edits)
	}
}

// 匹配到多处也报错，并给出数量：取第一处会静默改错位置，全部替换会静默放大影响面。
func TestPlan_AmbiguousReportsCount(t *testing.T) {
	got, err := impl{}.Plan(context.Background(), textInput(t, "x\nx\nx\n", "x"))
	if err != nil {
		t.Fatalf("多处匹配不是执行失败：%v", err)
	}
	if got.Result.Err == nil || got.Result.Err.Kind != "ambiguous" {
		t.Fatalf("应返回 ambiguous：%+v", got.Result)
	}
	if !strings.Contains(got.Result.Err.Message, "3") {
		t.Errorf("错误信息应报出匹配数量：%q", got.Result.Err.Message)
	}
	if len(got.Edits) != 0 {
		t.Errorf("歧义不得产出编辑：%+v", got.Edits)
	}
}

// 内容寻址没给字面量：调用形状不成立，属执行错误而不是"没找到"。
func TestPlan_MissingLiteralIsError(t *testing.T) {
	in := textInput(t, "x\n", "")
	_, err := impl{}.Plan(context.Background(), in)
	if err == nil {
		t.Fatal("缺字面量必须报错")
	}
	var te *harness.ToolError
	if !errors.As(err, &te) || te.Kind != "bad_selector" {
		t.Errorf("错误类型 = %v", err)
	}
}

// 符号路径：区间由扩展给出，**本原语不读文件**——用"读必失败"的工作区证明这一点。
func TestPlan_SymbolPathUsesPreparedRangeWithoutReading(t *testing.T) {
	st := newStore(t)
	in := harness.PlanInput{
		Call: harness.Call{
			ID: "c1", Primitive: Name, Target: "a.go",
			Selector: harness.Selector{Symbol: "pkg.Fn"},
			Content:  "NEW",
		},
		Route:    harness.Route{Path: harness.PathSymbol, Reason: "符号寻址 + 语言已注册且语法可解析"},
		Prepared: harness.Prepared{File: "a.go", ByteRange: harness.ByteRange{Start: 10, End: 20}},
		Facts:    &facts{ws: noReadWorkspace{Storage: st}, ledger: harness.NewLedger()},
	}

	got, err := impl{}.Plan(context.Background(), in)
	if err != nil {
		t.Fatalf("符号路径不该读文件，却失败了：%v", err)
	}
	if len(got.Edits) != 1 {
		t.Fatalf("编辑数 = %d，期望 1", len(got.Edits))
	}
	if got.Edits[0].ByteRange != (harness.ByteRange{Start: 10, End: 20}) || got.Edits[0].NewContent != "NEW" {
		t.Errorf("应原样搬运扩展给出的区间与内容：%+v", got.Edits[0])
	}
}

func TestTool_Shape(t *testing.T) {
	tool := Tool()
	if tool.Name != Name || tool.Decl.Name != string(Name) {
		t.Errorf("名字与声明必须一致：%+v", tool)
	}
	// 寻址由选择器表达式决定：写字面量走文本，写限定名走符号。
	if tool.Address != harness.AddressedBySelector {
		t.Errorf("寻址性质 = %q，期望由选择器决定", tool.Address)
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
