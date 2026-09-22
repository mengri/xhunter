package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"xhunter/harness"
	"xhunter/llm"
)

// 装配层的职责在这里落地：清单是业务边界，它的自洽与它对模型的承诺都由本文件守着。
// 框架侧只保证机制（分发、裁决、落盘），不认识的业务约束全部在这一层被审查。

// 装配层要能真的把"动东西"的协作者造出来，并且它们满足内核的契约。
// 编译期由各包的 `var _` 断言保证，这里再显式取一次，避免它们被误认为死代码。
func TestBackends_SatisfyContracts(t *testing.T) {
	var ws harness.WorkspaceOpener = defaultWorkspaces()
	st, err := ws.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开工作区失败：%v", err)
	}
	if st == nil {
		t.Fatal("opener 必须交出可用的工作区")
	}

	if git := defaultGit(); git == nil {
		t.Fatal("git 值得装配")
	}
}

// 空根是装配错误：没有根就没有工作区可言，不该等到第一次读文件才发现。
func TestBackends_RejectEmptyRoot(t *testing.T) {
	if _, err := defaultWorkspaces().Open("  "); err == nil {
		t.Fatal("空根必须被拒绝")
	}
}

// 内核的装配校验：缺协作者时一次说全，且不把"可以缺席"的（Ext）算作缺失。
func TestDeps_ValidateReportsAllMissing(t *testing.T) {
	err := harness.Deps{}.Validate()
	if err == nil {
		t.Fatal("空 Deps 必须被判为装配不完整")
	}
	for _, want := range []string{"Provider", "Context", "Tools", "Policy", "Sink", "Session", "Git", "Workspaces"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("缺失清单应包含 %s：%v", want, err)
		}
	}
	// Ext 与提示词插件是可选协作者：缺它们不该让装配失败（符号路径不可用与空清单
	// 都是合法的运行状态）。
	if err := (harness.Deps{
		Provider:   &stubProviderForDeps{},
		Context:    stubContextForDeps{},
		Tools:      stubToolsForDeps{},
		Policy:     stubPolicyForDeps{},
		Sink:       stubSinkForDeps{},
		Session:    stubSessionForDeps{},
		Git:        defaultGit(),
		Workspaces: defaultWorkspaces(),
	}).Validate(); err != nil {
		t.Errorf("可选协作者缺席不该导致装配失败：%v", err)
	}
}

// 下面几个桩只满足接口签名——Validate 只看"有没有"，不调用它们。
type stubProviderForDeps struct{}

func (stubProviderForDeps) Capabilities() harness.Caps { return harness.Caps{} }
func (stubProviderForDeps) Infer(context.Context, llm.Request) (harness.Session, error) {
	return nil, errors.New("桩")
}

type stubContextForDeps struct{}

func (stubContextForDeps) Assemble(context.Context, *harness.Hunt, harness.TurnNo) ([]harness.Message, error) {
	return nil, nil
}
func (stubContextForDeps) Append(harness.TurnRecord)                 {}
func (stubContextForDeps) Restore(context.Context, []harness.Message) error { return nil }
func (stubContextForDeps) TokenCount() int                          { return 0 }

type stubToolsForDeps struct{}

func (stubToolsForDeps) Execute(context.Context, harness.ExecInput) harness.Result { return harness.Result{} }
func (stubToolsForDeps) Decls() []harness.ToolDecl                                 { return nil }

type stubPolicyForDeps struct{}

func (stubPolicyForDeps) Decide(context.Context, harness.Call, harness.Route) (harness.Decision, error) {
	return harness.Decision{Verdict: harness.VerdictAllow}, nil
}
func (stubPolicyForDeps) Charge(harness.Usage)                    {}
func (stubPolicyForDeps) Exhausted(harness.TurnNo) (bool, string) { return false, "" }

type stubSinkForDeps struct{}

func (stubSinkForDeps) Emit(harness.ExternalEvent) error      { return nil }
func (stubSinkForDeps) Log(string, string, ...any)           {}
func (stubSinkForDeps) Heartbeat(harness.Phase) error         { return nil }

type stubSessionForDeps struct{}

func (stubSessionForDeps) RecordTurn(harness.TurnRecord) {}
func (stubSessionForDeps) RecordOp(harness.WriteOp)      {}
func (stubSessionForDeps) Ops() []harness.WriteOp        { return nil }
func (stubSessionForDeps) Snapshot() error               { return nil }

// 清单必须结构自洽：名字不空、实现齐备、声明与名字一致、不重复。
// 这条校验由框架提供（机制），但触发它的时机是装配——装配失败就该启动期失败。
func TestDefaultTools_StructurallySound(t *testing.T) {
	if _, err := harness.NewRuntime(nil, nil, defaultTools()); err != nil {
		t.Fatalf("装配失败：%v", err)
	}
}

// 每个原语的声明与寻址性质都必须是"已知形状"，不能是随手写下的字符串：
// 寻址性质决定分发路径，写错了会让调用静默走错内部路径（而不是报错）。
func TestDefaultTools_ShapeIsDeclared(t *testing.T) {
	known := map[harness.Addressing]bool{
		harness.AddressedBySelector: true,
		harness.AddressedAsText:     true,
		harness.AddressedAsSymbol:   true,
	}
	for _, tool := range defaultTools() {
		if tool.Name == "" {
			t.Error("原语名字不得为空")
		}
		if tool.Decl.Name != string(tool.Name) {
			t.Errorf("%q 的声明名字是 %q，两者必须一致", tool.Name, tool.Decl.Name)
		}
		if tool.Decl.Description == "" {
			t.Errorf("%q 缺少说明：模型只能靠它决定什么时候用", tool.Name)
		}
		if !json.Valid(tool.Decl.Schema) {
			t.Errorf("%q 的参数形状不是合法 JSON：%s", tool.Name, tool.Decl.Schema)
		}
		if !known[tool.Address] {
			t.Errorf("%q 的寻址性质 %q 不在已知取值内", tool.Name, tool.Address)
		}
		if tool.Impl == nil {
			t.Errorf("%q 没有实现", tool.Name)
		}
	}
}

// 工具面的业务约束：本产品的文件原语恰好是这 7 个、顺序固定。
// 顺序进入每一轮请求的前缀，因此它不只是"好看"，而是缓存与契约的一部分。
func TestDefaultTools_FaceIsFixed(t *testing.T) {
	want := []string{"read", "write", "edit", "find", "rename", "glob", "check"}
	tools := defaultTools()
	if len(tools) != len(want) {
		t.Fatalf("文件原语数 = %d，期望 %d", len(tools), len(want))
	}
	for i, tool := range tools {
		if string(tool.Name) != want[i] {
			t.Errorf("第 %d 个原语 = %q，期望 %q", i+1, tool.Name, want[i])
		}
	}
	// 声明顺序与清单一致，且控制原语殿后。
	decls := harness.VisibleToolDecls(tools)
	if len(decls) != len(want)+1 {
		t.Fatalf("声明数 = %d，期望 %d（7 文件原语 + 控制原语）", len(decls), len(want)+1)
	}
	for i, d := range decls[:len(want)] {
		if d.Name != want[i] {
			t.Errorf("声明顺序与清单不一致：第 %d 个是 %q", i+1, d.Name)
		}
	}
	if last := decls[len(decls)-1].Name; last != "checkpoint" {
		t.Errorf("控制原语应殿后，实际末位是 %q", last)
	}
}

// 参数形状与绑定层必须说同一句话：schema 声明的每个属性名，绑定层都要收得下。
//
// 这条是"描述 ↔ 实现"的一致性，必须由测试守——两者一旦分叉，模型照说明书填的参数
// 会被拒，而错在它看不到的地方（它只会收到一个参数错误，无从知道是说明书错了）。
func TestDefaultTools_SchemaPropertiesAreBindable(t *testing.T) {
	for _, d := range harness.VisibleToolDecls(defaultTools()) {
		props, required := schemaShape(t, d)
		for p := range props {
			payload, _ := json.Marshal(map[string]json.RawMessage{p: sampleValue(props, p)})
			if _, fault := harness.BindToolCall(llm.ToolCall{ID: "x", Name: d.Name, Arguments: payload}); fault != nil {
				t.Errorf("%s 的 schema 声明了绑定层不认的参数 %q：%+v", d.Name, p, fault)
			}
		}
		for _, r := range required {
			if _, ok := props[r]; !ok {
				t.Errorf("%s 的 required 提到未声明的属性 %q", d.Name, r)
			}
		}
	}
}

// 关键参数确实声明了：避免某个必要入参在说明书上静默消失。
func TestDefaultTools_DeclaresEssentialParameters(t *testing.T) {
	essential := map[string][]string{
		"read": {"path"}, "write": {"path", "content"}, "edit": {"path", "content"},
		"find": {"literal", "symbol"}, "rename": {"symbol", "new_name"},
		"glob": {"scope"}, "check": {"name"}, "checkpoint": {"summary"},
	}
	for _, d := range harness.VisibleToolDecls(defaultTools()) {
		props, _ := schemaShape(t, d)
		for _, want := range essential[d.Name] {
			if _, ok := props[want]; !ok {
				t.Errorf("%s 的 schema 缺少必要参数 %q", d.Name, want)
			}
		}
	}
}

// schema 只允许 properties 里列出的字段：绑定层拒绝未知字段，
// 说明书若允许额外字段，模型多传一个就吃到参数错误。
func TestDefaultTools_ForbidsAdditionalProperties(t *testing.T) {
	for _, d := range harness.VisibleToolDecls(defaultTools()) {
		var parsed struct {
			AdditionalProperties *bool `json:"additionalProperties"`
		}
		if err := json.Unmarshal(d.Schema, &parsed); err != nil {
			t.Fatalf("%s 的 schema 不是合法 JSON：%v", d.Name, err)
		}
		if parsed.AdditionalProperties == nil || *parsed.AdditionalProperties {
			t.Errorf("%s 的 schema 未禁止额外字段（与绑定层拒绝未知字段的行为不一致）", d.Name)
		}
	}
}

// schemaShape 取出属性名集合与全部 required（含 anyOf 分支里的）。
func schemaShape(t *testing.T, d harness.ToolDecl) (map[string]json.RawMessage, []string) {
	t.Helper()
	var parsed struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
		AnyOf      []struct {
			Required []string `json:"required"`
		} `json:"anyOf"`
	}
	if err := json.Unmarshal(d.Schema, &parsed); err != nil {
		t.Fatalf("%s 的 schema 不是合法 JSON：%v", d.Name, err)
	}
	if len(parsed.Properties) == 0 {
		t.Errorf("%s 的 schema 没有 properties", d.Name)
	}
	required := parsed.Required
	for _, b := range parsed.AnyOf {
		required = append(required, b.Required...)
	}
	return parsed.Properties, required
}

// sampleValue 按属性声明造一个最小合法值——绑定只做类型搬运，值的具体内容无关紧要。
func sampleValue(props map[string]json.RawMessage, name string) json.RawMessage {
	var spec struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(props[name], &spec)
	switch spec.Type {
	case "integer", "number":
		return json.RawMessage(`1`)
	case "boolean":
		return json.RawMessage(`true`)
	case "object", "array":
		return json.RawMessage(`{}`)
	default:
		return json.RawMessage(`"x"`)
	}
}
