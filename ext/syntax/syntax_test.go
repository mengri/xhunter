package syntax

import (
	"context"
	"strings"
	"testing"

	"xhunter/ext"
	"xhunter/workspace"
)

// 本文件钉住语法级后端能做什么、**不能**做什么：它是所有语言的兜底，因此"不能"那部分
// 必须如实（CanResolve=false、规模带 Unknown），而不是猜一个好看的答案。

type memWorkspace struct{ files map[string]string }

func (m *memWorkspace) Read(rel string, _ workspace.LineRange) (workspace.FileContent, error) {
	content, ok := m.files[rel]
	if !ok {
		return workspace.FileContent{}, workspace.ErrNotExist
	}
	return workspace.FileContent{Path: rel, Raw: content, Fingerprint: content}, nil
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
	return out, nil
}

const sample = `package demo

func Alpha() string {
	return "alpha"
}

func Beta() string {
	return Alpha()
}
`

func host(files map[string]string) *Host { return New(&memWorkspace{files: files}) }

func TestHost_CapabilitiesAreSyntacticAndCannotResolve(t *testing.T) {
	caps := host(nil).Capabilities(context.Background())
	if !caps.Available {
		t.Fatal("内置后端应当可用：它是首发也是兜底，不需要任何外部进程")
	}
	if caps.CanResolve {
		t.Error("语法级后端不得声称能解析引用：跨包引用不在它的视野内")
	}
	if caps.Precision != ext.PrecisionSyntactic {
		t.Errorf("精度 = %q，期望 %q", caps.Precision, ext.PrecisionSyntactic)
	}
	if !caps.Registered("go") {
		t.Error("首发语言应包含 go")
	}
	if caps.Registered("python") {
		t.Error("未注册的语言不得报成已注册")
	}
}

// 指纹必须能静态取到：装配层在拿到工作区之前就要把它写进生效配置快照。
func TestHost_FingerprintIsAvailableWithoutAWorkspace(t *testing.T) {
	tokens := FingerprintTokens()
	if len(tokens) == 0 {
		t.Fatal("指纹不得为空：空会被读成「没有符号能力」")
	}
	if joined := strings.Join(tokens, " "); !strings.Contains(joined, "lang:go") {
		t.Errorf("指纹应含语言清单：%v", tokens)
	}
	// 实例方法与静态函数同源：两处各算一遍迟早会漂。
	if strings.Join(host(nil).Fingerprint(), "|") != strings.Join(tokens, "|") {
		t.Error("实例指纹与静态指纹应同源")
	}
}

func TestHost_LocateFindsTheDeclarationRange(t *testing.T) {
	h := host(map[string]string{"a.go": sample})
	prep, err := h.Locate(context.Background(), ext.LocateRequest{Symbol: "Alpha", File: "a.go"})
	if err != nil {
		t.Fatalf("定位失败：%v", err)
	}
	if prep.File != "a.go" {
		t.Errorf("定位到 %q，期望 a.go", prep.File)
	}
	got := sample[prep.ByteRange.Start:prep.ByteRange.End]
	if !strings.HasPrefix(got, "func Alpha()") || !strings.HasSuffix(got, "}") {
		t.Errorf("区间应正好覆盖声明：%q", got)
	}
	if prep.Precision != ext.PrecisionSyntactic {
		t.Errorf("精度应随结论一起给出：%q", prep.Precision)
	}
}

// 限定名前面的部分用于筛选：`demo.Alpha` 只匹配包 demo（与接收者类型）里的那个 Alpha。
func TestHost_QualifierNarrowsTheSameNamedSymbol(t *testing.T) {
	files := map[string]string{
		"demo/a.go":  "package demo\n\nfunc Alpha() string { return \"d\" }\n",
		"other/a.go": "package other\n\nfunc Alpha() string { return \"o\" }\n",
	}
	h := host(files)
	prep, err := h.Locate(context.Background(), ext.LocateRequest{Symbol: "demo.Alpha"})
	if err != nil {
		t.Fatalf("定位失败：%v", err)
	}
	if prep.File != "demo/a.go" {
		t.Errorf("限定名应筛到 demo 包，实际 %q", prep.File)
	}
	// 不带限定名时找到任意一个同名符号即可（顺序由枚举给出，不承诺具体是哪个）。
	if _, err := h.Locate(context.Background(), ext.LocateRequest{Symbol: "Alpha"}); err != nil {
		t.Errorf("不带限定名也应能定位：%v", err)
	}
}

// 语法坏掉的文件跳过而不是报错：一个文件写坏了不该让整个符号能力不可用——
// 那是"这个文件的事实"，不是运行错误。
func TestHost_UnparsableFileIsSkippedNotAnError(t *testing.T) {
	files := map[string]string{
		"broken.go": "package demo\n\nfunc ( {",
		"ok.go":     sample,
	}
	h := host(files)
	prep, err := h.Locate(context.Background(), ext.LocateRequest{Symbol: "Alpha"})
	if err != nil {
		t.Fatalf("坏文件不该让定位失败：%v", err)
	}
	if prep.File != "ok.go" {
		t.Errorf("应跳过坏文件继续找：%q", prep.File)
	}
}

// 找不到是**结论**：这个符号不在这。
func TestHost_MissingSymbolIsAnError(t *testing.T) {
	if _, err := host(map[string]string{"a.go": sample}).Locate(context.Background(),
		ext.LocateRequest{Symbol: "NoSuchSymbol", File: "a.go"}); err == nil {
		t.Fatal("找不到的符号应报错")
	}
}

// All=true 一次给齐全部出现点（声明 + 引用）：重命名要一次改完，
// 不能"改完声明再去找引用"——两次定位之间会出现半完成状态。
func TestHost_AllListsDeclarationAndReferences(t *testing.T) {
	h := host(map[string]string{"a.go": sample})
	prep, err := h.Locate(context.Background(), ext.LocateRequest{Symbol: "Alpha", File: "a.go", All: true})
	if err != nil {
		t.Fatalf("定位失败：%v", err)
	}
	if len(prep.Sites) != 2 {
		t.Fatalf("应给出声明与调用点共 2 处，实际 %d：%+v", len(prep.Sites), prep.Sites)
	}
	for _, s := range prep.Sites {
		if got := sample[s.ByteRange.Start:s.ByteRange.End]; got != "Alpha" {
			t.Errorf("出现点应正好覆盖一个标识符：%q", got)
		}
	}
	// 规模与编辑计划同源：都来自这一次扫描。
	if prep.Impact.Occurrences != len(prep.Sites) || prep.Impact.FilesChanged != 1 {
		t.Errorf("规模应来自同一次扫描：%+v vs %d 处", prep.Impact, len(prep.Sites))
	}
	if !prep.Impact.Unknown {
		t.Error("语法级后端必须标注引用未穷尽：跨包引用不在视野内")
	}
}

// 同名字段与选择器不得被当成符号引用：把它们一起改掉就是把无关的结构改坏。
func TestHost_SitesSkipFieldNamesAndSelectors(t *testing.T) {
	src := `package demo

type Box struct {
	Alpha string
}

func Alpha() string {
	b := Box{Alpha: "x"}
	_ = b.Alpha
	return Alpha()
}
`
	h := host(map[string]string{"a.go": src})
	prep, err := h.Locate(context.Background(), ext.LocateRequest{Symbol: "Alpha", File: "a.go", All: true})
	if err != nil {
		t.Fatalf("定位失败：%v", err)
	}
	// 声明 1 处 + 调用 1 处；字段定义、字面量键、选择器都跳过。
	if len(prep.Sites) != 2 {
		t.Fatalf("应只有声明与调用 2 处，实际 %d：%+v", len(prep.Sites), prep.Sites)
	}
	lines := map[int]bool{}
	for _, s := range prep.Sites {
		line := strings.Count(src[:s.ByteRange.Start], "\n") + 1
		lines[line] = true
	}
	if lines[4] || lines[8] || lines[9] {
		t.Errorf("字段名 / 字面量键 / 选择器不该被列为出现点：行 %v", lines)
	}
}

// Close 无资源可收，且可重复调用——换成外挂后端时调用方的收尾代码不用改。
func TestHost_CloseIsIdempotent(t *testing.T) {
	h := host(nil)
	if err := h.Close(); err != nil {
		t.Errorf("Close 不该报错：%v", err)
	}
	if err := h.Close(); err != nil {
		t.Errorf("Close 应幂等：%v", err)
	}
}
