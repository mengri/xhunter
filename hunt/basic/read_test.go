package basic

import (
	"context"
	"strings"
	"testing"

	"xhunter/hunt"
	"xhunter/workspace"
)

// 全文读取必须完整返回正文并逐行带行号前缀；CRLF 的 \r 属于分隔符，不进入回显——
// 否则模型在一个 CRLF 仓库里做行计数会悄悄错位。
func TestRead_FullContentIsNumberedAndCRLFNormalized(t *testing.T) {
	st := newStore(t)
	seed(t, st, "sample.md", strings.Repeat("第 n 行内容，故意长一点。\r\n", 500))
	f := newFacts()

	res, edits, err := ReadTool(st).Execute(context.Background(),
		hunt.Call{ID: "c1", Primitive: Read, Target: "sample.md"}, f)
	if err != nil {
		t.Fatalf("读取失败：%v", err)
	}
	if len(edits) != 0 {
		t.Errorf("读原语不得产出编辑：%+v", edits)
	}
	if res.Err != nil {
		t.Fatalf("正常读取不该带结构化错误：%+v", res.Err)
	}
	if strings.Contains(res.Summary, "\r") {
		t.Error("回显不得含 \\r（换行符归一）")
	}
	// 行号右对齐到 3 位（总行数 500 的宽度）：首行补两个空格，末行无填充。
	if !strings.Contains(res.Summary, "  1→第 n 行内容") {
		t.Errorf("首行应有行号前缀 1：%s", head(res.Summary))
	}
	if !strings.Contains(res.Summary, "\n500→第 n 行内容") {
		t.Error("末行应有行号前缀 500")
	}
	if strings.Contains(res.Summary, "已截断") {
		t.Errorf("未超预算的全文不该标截断：%s", head(res.Summary))
	}
	// 台账登记的是全文件指纹：文件确实被完整读过一次，后续编辑不该被误拒。
	if _, ok := f.Ledger().Fingerprint("sample.md"); !ok {
		t.Error("读取后必须登记台账")
	}
}

// 超过单次预算时允许截断，但必须标注已显示范围与**可直接照抄的续读起点**。
func TestRead_OversizeTruncatesWithContinuationHint(t *testing.T) {
	st := newStore(t)
	seed(t, st, "big.txt", strings.Repeat("0123456789ABCDEF\n", 20000)) // 约 340KB

	res, _, err := ReadTool(st).Execute(context.Background(),
		hunt.Call{ID: "c1", Primitive: Read, Target: "big.txt"}, newFacts())
	if err != nil {
		t.Fatalf("读取失败：%v", err)
	}
	if !strings.Contains(res.Summary, "已截断") {
		t.Error("超预算截断必须显式标注")
	}
	if !strings.Contains(res.Summary, "显示第 1–3855 行") || !strings.Contains(res.Summary, "请从第 3856 行续读") {
		t.Errorf("标注必须给出已显示范围与续读起点：%s", head(res.Summary))
	}
	// 正文以完整的第 3855 行收尾：无填充、无半行、无空行号前缀。
	if !strings.HasSuffix(res.Summary, "\n3855→0123456789ABCDEF") {
		t.Errorf("正文末行应为行号 3855 的完整行：%q", tail(res.Summary))
	}
}

// 按行范围读取：只回显该区间。
func TestRead_LineRangeIsHonoured(t *testing.T) {
	st := newStore(t)
	seed(t, st, "a.txt", "L1\nL2\nL3\nL4\n")

	rng := workspace.LineRange{From: 2, To: 3}
	res, _, err := ReadTool(st).Execute(context.Background(), hunt.Call{
		ID: "c1", Primitive: Read, Target: "a.txt",
		Selector: hunt.Selector{Range: &rng},
	}, newFacts())
	if err != nil {
		t.Fatalf("读取失败：%v", err)
	}
	if !strings.Contains(res.Summary, "L2") || strings.Contains(res.Summary, "L1") {
		t.Errorf("只应回显区间内的行：%s", head(res.Summary))
	}
}

// 读不存在的文件由工作区报错——原语不自己判断路径与存在性。
func TestRead_MissingFileIsError(t *testing.T) {
	if _, _, err := ReadTool(newStore(t)).Execute(context.Background(),
		hunt.Call{ID: "c1", Primitive: Read, Target: "nope.txt"}, newFacts()); err == nil {
		t.Fatal("读不存在的文件必须报错")
	}
}

// 声明形状：参数面精确等于 {path, range}，只有 path 必填。
func TestRead_DeclShape(t *testing.T) {
	d := ReadTool(nil).Decl()
	if d.Name != string(Read) {
		t.Errorf("声明名 = %q，期望 %q", d.Name, Read)
	}
	s := decodeSchema(t, d)
	if got := propNames(s.Properties); len(got) != 2 || got[0] != "path" || got[1] != "range" {
		t.Errorf("参数面 = %v，期望 [path range]", got)
	}
	if len(s.Required) != 1 || s.Required[0] != "path" {
		t.Errorf("required = %v，期望只有 path", s.Required)
	}
	if s.AdditionalProperties == nil || *s.AdditionalProperties {
		t.Errorf("必须禁止额外字段：%s", d.Schema)
	}
}
