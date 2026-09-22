package symbolic

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"xhunter/hunt"
)

// symbolic 一期只声明不实现；但它的**寻址语义**本身就是必须钉住的东西：
// 三个符号原语只走符号寻址，没有内容寻址的降级形态——环境不支持时返回结构化错误，
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

// 三个原语共用同一个构造形状：工作区与扩展宿主都是构造参数，一期可以都为 nil
// （不实现即不碰它们）。
func primitives(t *testing.T) map[hunt.PrimitiveName]hunt.Primitive {
	t.Helper()
	return map[hunt.PrimitiveName]hunt.Primitive{
		SymbolRead:   SymbolReadTool(nil, nil),
		SymbolEdit:   SymbolEditTool(nil, nil),
		SymbolRename: SymbolRenameTool(nil, nil),
	}
}

// 未实现必须是**可解释的结果**而不是执行失败，且不可重试（原样重试还是同一结果）。
func TestSymbolics_ReportNotImplemented(t *testing.T) {
	calls := map[hunt.PrimitiveName]hunt.Call{
		SymbolRead:   {ID: "c1", Primitive: SymbolRead, Target: "a.go", Selector: hunt.Selector{Symbol: "pkg.Fn"}},
		SymbolEdit:   {ID: "c2", Primitive: SymbolEdit, Target: "a.go", Selector: hunt.Selector{Symbol: "pkg.Fn"}, Content: "NEW"},
		SymbolRename: {ID: "c3", Primitive: SymbolRename, Target: "a.go", Selector: hunt.Selector{Symbol: "pkg.Fn"}, NewName: "Fn2"},
	}

	for name, prim := range primitives(t) {
		res, edits, err := prim.Execute(context.Background(), calls[name], nil)
		if err != nil {
			t.Fatalf("%s：未实现不是执行失败：%v", name, err)
		}
		if res.Err == nil || res.Err.Kind != "not_implemented" {
			t.Fatalf("%s：应返回 not_implemented：%+v", name, res)
		}
		if res.Err.Retryable {
			t.Errorf("%s：尚未实现不该标为可重试", name)
		}
		if !strings.Contains(res.Err.Message, string(name)) {
			t.Errorf("%s：错误信息应指明是哪个原语：%q", name, res.Err.Message)
		}
		if len(edits) != 0 {
			t.Errorf("%s：不得产出任何编辑：%+v", name, edits)
		}
	}
}

// 只走符号寻址：参数面里**没有**内容寻址的槽位（literal），
// 这就是"没有降级形态"在模型可见面上的体现。
func TestSymbolics_SurfaceHasNoContentAddressingSlot(t *testing.T) {
	for name, prim := range primitives(t) {
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

	prims := primitives(t)
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

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
