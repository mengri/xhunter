package symbolic

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"xhunter/ext"
	"xhunter/ext/syntax"
	"xhunter/hunt"
	"xhunter/llm"
	"xhunter/workspace"
)

// 符号原语的定位语义必须钉住：三个原语**只走符号寻址**，环境不支持时返回结构化错误，
// 不悄悄改走文本替换（那会产出一个改了声明、没改调用点的半完成重命名）。

type shape struct {
	Properties           map[string]any `json:"properties"`
	Required             []string       `json:"required"`
	AdditionalProperties *bool          `json:"additionalProperties"`
}

func decode(t *testing.T, d hunt.Primitive) shape {
	t.Helper()
	decl := d.Decl()
	if decl.Name == "" || decl.Description == "" {
		t.Fatalf("声明必须自带名字与说明：%+v", decl)
	}
	if !json.Valid(decl.Schema) {
		t.Fatalf("%s 的参数形状不是合法 JSON：%s", decl.Name, decl.Schema)
	}
	var s shape
	if err := json.Unmarshal(decl.Schema, &s); err != nil {
		t.Fatalf("解不开 %s 的参数形状：%v", decl.Name, err)
	}
	return s
}

// ============================================================ 桩

// memWorkspace 是内存工作区：符号后端要读源码，测试用它给出确定的源码内容。
type memWorkspace struct{ files map[string]string }

func (m *memWorkspace) Read(rel string, _ workspace.LineRange) (workspace.FileContent, error) {
	content, ok := m.files[rel]
	if !ok {
		return workspace.FileContent{}, &llm.Fault{Kind: "not_found", Message: "文件不存在：" + rel, Retryable: true}
	}
	return workspace.FileContent{Path: rel, Raw: content, Fingerprint: content, TotalLines: strings.Count(content, "\n") + 1}, nil
}

func (m *memWorkspace) Stat(rel string) (workspace.FileInfo, error) {
	_, ok := m.files[rel]
	return workspace.FileInfo{Path: rel, Exists: ok}, nil
}
func (m *memWorkspace) List(string) ([]string, error) {
	out := make([]string, 0, len(m.files))
	for name := range m.files {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// stubFacts 是原语能看到的事实的最小替身（符号原语只用到台账）。
type stubFacts struct{ ledger *hunt.Ledger }

func (f *stubFacts) Gates() []hunt.Gate               { return nil }
func (f *stubFacts) Ledger() *hunt.Ledger             { return f.ledger }
func (f *stubFacts) RequestCheckpoint(string)         {}
func (f *stubFacts) CheckpointRequested() bool        { return false }
func (f *stubFacts) WorkRoot() string                 { return "root" }
func (f *stubFacts) ChangeFingerprint() string        { return "clean" }
func (f *stubFacts) RecordGateResult(hunt.GateResult) {}

const srcAlpha = `package demo

func Alpha() string {
	return "alpha"
}

func Beta() string {
	return Alpha()
}
`

func newHost(t *testing.T, files map[string]string) ext.ExtHost {
	t.Helper()
	return syntax.New(&memWorkspace{files: files})
}

func primitives(t *testing.T, host ext.ExtHost) map[hunt.PrimitiveName]hunt.Primitive {
	t.Helper()
	ws := &memWorkspace{files: map[string]string{}}
	return map[hunt.PrimitiveName]hunt.Primitive{
		SymbolRead:   SymbolReadTool(ws, host),
		SymbolEdit:   SymbolEditTool(ws, host),
		SymbolRename: SymbolRenameTool(ws, host),
	}
}

// ============================================================ 未装配后端

// 没有符号后端时给出**结构化错误**而不是执行失败：模型据此改用文本寻址，
// 而不是以为自己还在改符号。
func TestSymbolics_UnavailableBackendIsStructuredError(t *testing.T) {
	for name, prim := range primitives(t, ext.Unimplemented{}) {
		call := hunt.Call{ID: "c1", Primitive: name, Target: "a.go", Selector: hunt.Selector{Symbol: "Alpha"}}
		res, edits, err := prim.Execute(context.Background(), call, nil)
		if err != nil {
			t.Fatalf("%s：能力不可用不是执行失败：%v", name, err)
		}
		if res.Err == nil || res.Err.Kind != "ext_unavailable" {
			t.Fatalf("%s：应返回 ext_unavailable：%+v", name, res.Err)
		}
		if res.Err.Retryable {
			t.Errorf("%s：装配缺件不该标为可重试（重跑同一装配还是没有）", name)
		}
		if len(edits) != 0 {
			t.Errorf("%s：不得产出任何编辑：%+v", name, edits)
		}
	}
}

// ============================================================ 定位与读写

// 符号读：定位到声明区间并读出**那一段**，不是整份文件。
func TestSymbolRead_LocatesAndReadsRange(t *testing.T) {
	host := newHost(t, map[string]string{"a.go": srcAlpha})
	prim := SymbolReadTool(&memWorkspace{files: map[string]string{"a.go": srcAlpha}}, host)
	facts := &stubFacts{ledger: &hunt.Ledger{}}

	res, edits, err := prim.Execute(context.Background(),
		hunt.Call{ID: "c1", Primitive: SymbolRead, Target: "a.go", Selector: hunt.Selector{Symbol: "Alpha"}}, facts)
	if err != nil {
		t.Fatalf("执行失败：%v", err)
	}
	if res.Err != nil {
		t.Fatalf("不应有错误：%+v", res.Err)
	}
	if len(edits) != 0 {
		t.Errorf("读不产出编辑：%+v", edits)
	}
	if !strings.Contains(res.Summary, "func Alpha() string") {
		t.Errorf("应读出符号定义的那一段：%q", res.Summary)
	}
	if strings.Contains(res.Summary, "func Beta()") {
		t.Errorf("不该把别的东西一起读出来：%q", res.Summary)
	}
	// 读过即登记台账：「改前必读」在符号路径上同样成立。
	if _, ok := facts.ledger.Fingerprint("a.go"); !ok {
		t.Error("符号读应登记台账")
	}
}

// 定位不到是**结论**（这个符号不在这），不可重试——重跑同一个请求不会改变结果。
func TestSymbolRead_NotFoundIsAVerdictNotAnEnvFault(t *testing.T) {
	ws := &memWorkspace{files: map[string]string{"a.go": srcAlpha}}
	prim := SymbolReadTool(ws, newHost(t, map[string]string{"a.go": srcAlpha}))

	res, _, err := prim.Execute(context.Background(),
		hunt.Call{ID: "c1", Primitive: SymbolRead, Target: "a.go", Selector: hunt.Selector{Symbol: "NoSuchSymbol"}}, nil)
	if err != nil {
		t.Fatalf("定位不到不该上抛为执行失败：%v", err)
	}
	if res.Err == nil || res.Err.Kind != "symbol_not_found" {
		t.Fatalf("应返回 symbol_not_found：%+v", res.Err)
	}
	if res.Err.Retryable {
		t.Error("定位不到不该标为可重试")
	}
}

// 符号编辑：只替换定位到的那一段——区间之外一个字节都不能动。
func TestSymbolEdit_ReplacesOnlyTargetRange(t *testing.T) {
	files := map[string]string{"a.go": srcAlpha}
	ws := &memWorkspace{files: files}
	prim := SymbolEditTool(ws, newHost(t, files))

	res, edits, err := prim.Execute(context.Background(),
		hunt.Call{ID: "c1", Primitive: SymbolEdit, Target: "a.go",
			Selector: hunt.Selector{Symbol: "Alpha"}, Content: "func Alpha() string { return \"new\" }"}, nil)
	if err != nil {
		t.Fatalf("执行失败：%v", err)
	}
	if res.Err != nil {
		t.Fatalf("不应有错误：%+v", res.Err)
	}
	if len(edits) != 1 {
		t.Fatalf("应产出 1 处编辑：%+v", edits)
	}
	edit := edits[0]
	if edit.File != "a.go" {
		t.Errorf("编辑应落在定位到的文件上：%+v", edit)
	}
	if got := srcAlpha[edit.ByteRange.Start:edit.ByteRange.End]; !strings.HasPrefix(got, "func Alpha() string {") ||
		!strings.HasSuffix(got, "}") {
		t.Errorf("区间应正好覆盖 Alpha 的声明：%q", got)
	}
	if !strings.Contains(res.Summary, "a.go") {
		t.Errorf("结论应说明改在哪：%q", res.Summary)
	}
}

// 语言未注册 → 结构化错误。**绝不降级为文本替换**：那会改到同名字符串上。
func TestSymbolic_ReportsStructuredErrorWhenLanguageUnregistered(t *testing.T) {
	files := map[string]string{"main.py": "def alpha():\n    pass\n"}
	ws := &memWorkspace{files: files}
	host := newHost(t, files)
	for name, prim := range map[hunt.PrimitiveName]hunt.Primitive{
		SymbolRead:   SymbolReadTool(ws, host),
		SymbolEdit:   SymbolEditTool(ws, host),
		SymbolRename: SymbolRenameTool(ws, host),
	} {
		call := hunt.Call{ID: "c1", Primitive: name, Target: "main.py",
			Selector: hunt.Selector{Symbol: "alpha"}, Content: "X", NewName: "beta"}
		res, edits, err := prim.Execute(context.Background(), call, nil)
		if err != nil {
			t.Fatalf("%s：不该上抛为执行失败：%v", name, err)
		}
		if res.Err == nil || res.Err.Kind != "language_unregistered" {
			t.Fatalf("%s：应返回 language_unregistered：%+v", name, res.Err)
		}
		if len(edits) != 0 {
			t.Errorf("%s：未注册语言不得产出任何编辑：%+v", name, edits)
		}
	}
}

// 精度如实上报：模型据此知道这条结论有多可信。
func TestSymbolic_ReportsSyntacticPrecision(t *testing.T) {
	files := map[string]string{"a.go": srcAlpha}
	ws := &memWorkspace{files: files}
	prim := SymbolReadTool(ws, newHost(t, files))

	res, _, err := prim.Execute(context.Background(),
		hunt.Call{ID: "c1", Primitive: SymbolRead, Target: "a.go", Selector: hunt.Selector{Symbol: "Alpha"}}, nil)
	if err != nil || res.Err != nil {
		t.Fatalf("执行失败：%v / %+v", err, res.Err)
	}
	if !strings.Contains(res.Summary, string(ext.PrecisionSyntactic)) {
		t.Errorf("应上报语法级精度：%q", res.Summary)
	}
	if !strings.Contains(res.Summary, "未穷尽") {
		t.Errorf("语法级必须如实说明引用未穷尽：%q", res.Summary)
	}
}

// ============================================================ 重命名（MS-10）

// 重命名一次改完**声明 + 全部引用点**：半完成的重命名（改了声明没改调用点）
// 比不做更坏——代码立刻变成不能编译的状态。
func TestSymbolRename_UpdatesDeclarationAndAllReferences(t *testing.T) {
	files := map[string]string{"a.go": srcAlpha}
	ws := &memWorkspace{files: files}
	prim := SymbolRenameTool(ws, newHost(t, files))

	res, edits, err := prim.Execute(context.Background(),
		hunt.Call{ID: "c1", Primitive: SymbolRename, Target: "a.go",
			Selector: hunt.Selector{Symbol: "Alpha"}, NewName: "Gamma"}, nil)
	if err != nil {
		t.Fatalf("执行失败：%v", err)
	}
	if res.Err != nil {
		t.Fatalf("不应有错误：%+v", res.Err)
	}
	// 声明 1 处 + Beta 里调用 1 处。
	if len(edits) != 2 {
		t.Fatalf("应改 2 处（声明与调用点），实际 %d：%+v", len(edits), edits)
	}
	for _, e := range edits {
		if e.NewContent != "Gamma" {
			t.Errorf("每处都应改成新名字：%+v", e)
		}
		if got := srcAlpha[e.ByteRange.Start:e.ByteRange.End]; got != "Alpha" {
			t.Errorf("区间应正好覆盖一个标识符，实际 %q", got)
		}
	}
}

// 后端列不出引用点时返回结构化错误，**不降级为文本替换**。
func TestSymbolRename_CanResolveFalseIsStructuredErrorNotTextReplace(t *testing.T) {
	files := map[string]string{"a.go": srcAlpha}
	ws := &memWorkspace{files: files}
	// 空后端：能力可用（保证走到重命名），但列不出出现点。
	prim := SymbolRenameTool(ws, &noSitesHost{caps: ext.ExtCaps{Available: true, Languages: []string{"go"}, Precision: ext.PrecisionSyntactic}})

	res, edits, err := prim.Execute(context.Background(),
		hunt.Call{ID: "c1", Primitive: SymbolRename, Target: "a.go",
			Selector: hunt.Selector{Symbol: "Alpha"}, NewName: "Gamma"}, nil)
	if err != nil {
		t.Fatalf("不该上抛为执行失败：%v", err)
	}
	if res.Err == nil || res.Err.Kind != "cannot_resolve" {
		t.Fatalf("列不出引用点应返回 cannot_resolve：%+v", res.Err)
	}
	if len(edits) != 0 {
		t.Errorf("列不出引用点时不该产出任何编辑：%+v", edits)
	}
}

// 规模上报：文件数 / 处数来自**同一次定位**，与落盘的编辑计划同源。
func TestSymbolRename_ReportsFootprint(t *testing.T) {
	files := map[string]string{"a.go": srcAlpha, "b.go": "package demo\n\nfunc Use() string { return Alpha() }\n"}
	ws := &memWorkspace{files: files}
	prim := SymbolRenameTool(ws, newHost(t, files))

	res, edits, err := prim.Execute(context.Background(),
		hunt.Call{ID: "c1", Primitive: SymbolRename, Selector: hunt.Selector{Symbol: "Alpha"}, NewName: "Gamma"}, nil)
	if err != nil || res.Err != nil {
		t.Fatalf("执行失败：%v / %+v", err, res.Err)
	}
	// 上报的处数必须等于落盘的编辑数：两处各算一遍迟早会漂。
	if !strings.Contains(res.Summary, "2 个文件") || !strings.Contains(res.Summary, "3 处") {
		t.Errorf("规模应如实上报：%q", res.Summary)
	}
	if len(edits) != 3 {
		t.Errorf("落盘编辑数应与上报处数一致：%d vs %+v", len(edits), edits)
	}
}

// 未知就是未知：语法级后端看不到跨包引用，必须标出来而不是猜一个"都改完了"。
func TestSymbolRename_UnknownFootprintIsReportedNotGuessed(t *testing.T) {
	files := map[string]string{"a.go": srcAlpha}
	ws := &memWorkspace{files: files}
	prim := SymbolRenameTool(ws, newHost(t, files))

	res, _, err := prim.Execute(context.Background(),
		hunt.Call{ID: "c1", Primitive: SymbolRename, Target: "a.go",
			Selector: hunt.Selector{Symbol: "Alpha"}, NewName: "Gamma"}, nil)
	if err != nil || res.Err != nil {
		t.Fatalf("执行失败：%v / %+v", err, res.Err)
	}
	if !strings.Contains(res.Summary, "未穷尽") {
		t.Errorf("语法级后端必须标注引用未穷尽：%q", res.Summary)
	}
}

// noSitesHost 能力可用但列不出出现点：用来证明"半套重命名"会被拒绝。
type noSitesHost struct{ caps ext.ExtCaps }

func (h *noSitesHost) Capabilities(context.Context) ext.ExtCaps { return h.caps }
func (h *noSitesHost) Locate(context.Context, ext.LocateRequest) (ext.Prepared, error) {
	return ext.Prepared{File: "a.go", Precision: h.caps.Precision}, nil
}
func (h *noSitesHost) Fingerprint() []string                          { return []string{"stub/1"} }
func (h *noSitesHost) Parse(context.Context, string) ext.ParseVerdict { return ext.ParseUnknown }
func (h *noSitesHost) Enclose(context.Context, ext.EncloseRequest) (ext.Prepared, bool, error) {
	return ext.Prepared{}, false, ext.ErrUnavailable
}
func (h *noSitesHost) Close() error { return nil }

// ============================================================ 参数面

// 只走符号寻址：参数面里**没有**内容寻址的槽位（literal），
// 这就是"没有降级形态"在模型可见面上的体现。
func TestSymbolics_SurfaceHasNoContentAddressingSlot(t *testing.T) {
	for name, prim := range primitives(t, nil) {
		s := decode(t, prim)
		for prop := range s.Properties {
			if prop == "literal" {
				t.Errorf("%s：符号原语不得接受内容寻址槽位：%v", name, s.Properties)
			}
		}
		if _, ok := s.Properties["symbol"]; !ok {
			t.Errorf("%s：符号定位槽位必须存在：%v", name, s.Properties)
		}
		if s.AdditionalProperties == nil || *s.AdditionalProperties {
			t.Errorf("%s：必须禁止额外字段", name)
		}
	}
}

// 逐个原语的声明形状：名字与必填集合。
func TestSymbolics_DeclShapes(t *testing.T) {
	cases := []struct {
		name     hunt.PrimitiveName
		required []string
		props    []string
	}{
		{SymbolRead, []string{"symbol"}, []string{"path", "symbol"}},
		{SymbolEdit, []string{"symbol", "content"}, []string{"content", "path", "symbol"}},
		{SymbolRename, []string{"symbol", "new_name"}, []string{"new_name", "path", "symbol"}},
	}

	prims := primitives(t, nil)
	for _, c := range cases {
		prim, ok := prims[c.name]
		if !ok {
			t.Fatalf("缺少原语 %s", c.name)
		}
		if got := prim.Decl().Name; got != string(c.name) {
			t.Errorf("声明名 = %q，期望 %q", got, c.name)
		}
		s := decode(t, prim)
		if got := keys(s.Properties); strings.Join(got, ",") != strings.Join(c.props, ",") {
			t.Errorf("%s：参数面 = %v，期望 %v", c.name, got, c.props)
		}
		if strings.Join(s.Required, ",") != strings.Join(c.required, ",") {
			t.Errorf("%s：required = %v，期望 %v", c.name, s.Required, c.required)
		}
	}
}

// 写盘性质由原语自述：符号读不写、编辑与重命名写。
func TestSymbolics_WritesIsDeclaredByThePrimitive(t *testing.T) {
	prims := primitives(t, nil)
	if prims[SymbolRead].Writes() {
		t.Error("符号读不该声明写盘")
	}
	if !prims[SymbolEdit].Writes() || !prims[SymbolRename].Writes() {
		t.Error("符号编辑与重命名必须声明写盘")
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
