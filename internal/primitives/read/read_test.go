package read

import (
	"xhunter/internal/workspace/osfs"
	"context"
	"strings"
	"testing"

	"xhunter/harness"
)

// facts 只实现本原语用到的东西：工作区与台账。
// 其余方法靠嵌入的接口占位——真被调用会 panic，这恰好证明 read 不碰它们
// （不提交、不读门禁、不碰检查点意图）。
type facts struct {
	harness.TaskFacts
	storage harness.Storage
	ledger  *harness.Ledger
}

func (f *facts) Workspace() harness.Workspace { return f.storage }
func (f *facts) Ledger() *harness.Ledger      { return f.ledger }

// fixture 把内容写进临时工作区，返回可直接喂给 Plan 的输入。
func fixture(t *testing.T, content string) harness.PlanInput {
	t.Helper()
	st := newStore(t)
	if _, err := st.WriteRange("sample.md", harness.ByteRange{Start: 0, End: 0}, content); err != nil {
		t.Fatalf("准备夹具失败：%v", err)
	}
	return harness.PlanInput{
		Call:  harness.Call{ID: "c1", Primitive: Name, Target: "sample.md"},
		Route: harness.Route{Path: harness.PathText, Reason: "内容寻址选择器"},
		Facts: &facts{storage: st, ledger: harness.NewLedger()},
	}
}

func plan(t *testing.T, in harness.PlanInput) harness.Result {
	t.Helper()
	p, err := impl{}.Plan(context.Background(), in)
	if err != nil {
		t.Fatalf("读取失败：%v", err)
	}
	return p.Result
}

// 全文读取必须完整返回正文（行号前缀不改变内容完整性），并做换行符归一：
// CRLF 的 \r 属于分隔符，不进入回显——否则模型在一个 CRLF 仓库里做行计数会悄悄错位。
func TestPlan_FullContentWithLineNumbers(t *testing.T) {
	content := strings.Repeat("第 n 行内容，故意长一点。\r\n", 500) // CRLF，约 12KB
	in := fixture(t, content)
	got := plan(t, in).Summary

	if strings.Contains(got, "\r") {
		t.Error("回显不得含 \\r（换行符归一）")
	}
	// 行号右对齐到 3 位（总行数 500 的宽度）：首行补两个空格，末行无填充。
	if !strings.Contains(got, "  1→第 n 行内容") {
		t.Errorf("首行应有行号前缀 1：%s", head(got))
	}
	if !strings.Contains(got, "\n500→第 n 行内容") {
		t.Error("末行应有行号前缀 500")
	}
	if strings.Contains(got, "已截断") {
		t.Errorf("未超预算的全文不该标截断：%s", head(got))
	}
}

// 超过单次预算时允许截断，但必须标注显示范围与**可直接照抄的续读起点**——
// "截断必须显式标注位置与总量"对全文读取同样生效。
func TestPlan_OversizeTruncatesWithExplicitAnnotation(t *testing.T) {
	in := fixture(t, strings.Repeat("0123456789ABCDEF\n", 20000)) // 约 340KB
	got := plan(t, in).Summary

	if !strings.Contains(got, "已截断") {
		t.Error("超预算截断必须显式标注")
	}
	if !strings.Contains(got, "显示第 1–3855 行") || !strings.Contains(got, "请从第 3856 行续读") {
		t.Errorf("标注必须给出已显示范围与续读起点：%s", head(got))
	}
	// 正文以完整的第 3855 行收尾：无填充、无半行、无空行号前缀。
	if !strings.HasSuffix(got, "\n3855→0123456789ABCDEF") {
		t.Errorf("正文末行应为行号 3855 的完整行，实际结尾：%q", tail(got))
	}
	// 台账登记的是全文件指纹：文件确实被完整读过一次，后续编辑不应被误拒。
	if _, ok := in.Facts.Ledger().Fingerprint("sample.md"); !ok {
		t.Error("读取后必须登记台账")
	}
}

// 符号路径：定位由扩展完成，这里只消费结果。
func TestPlan_SymbolPath(t *testing.T) {
	in := fixture(t, "x")
	in.Route = harness.Route{Path: harness.PathSymbol}
	got := plan(t, in)
	if !strings.Contains(got.Summary, "符号视图") {
		t.Errorf("符号路径应给出符号视图结果：%q", got.Summary)
	}
}

// 声明与实际收参数的能力必须一致：这里只钉住本原语自己的声明形状。
func TestTool_DeclShape(t *testing.T) {
	tool := Tool()
	if tool.Name != Name || tool.Decl.Name != string(Name) {
		t.Errorf("名字与声明必须一致：%+v", tool)
	}
	if tool.Address != harness.AddressedBySelector {
		t.Errorf("read 的寻址应由选择器决定，实际 %q", tool.Address)
	}
	if len(tool.Decl.Schema) == 0 {
		t.Error("声明必须带参数形状")
	}
}

func head(s string) string {
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}

func tail(s string) string {
	if len(s) > 60 {
		return "…" + s[len(s)-60:]
	}
	return s
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
