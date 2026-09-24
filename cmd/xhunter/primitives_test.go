package main

import (
	"context"

	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
	"xhunter/ext"

	"xhunter/git"
	"xhunter/harness"
	"xhunter/hunt"
	"xhunter/hunt/symbolic"
	"xhunter/llm"
	"xhunter/workspace"
)

// 装配层的职责在这里落地：清单是业务边界，它的自洽与它对模型的承诺都由本文件守着。

// 装配层要能真的把「动东西」的协作者造出来，并且它们满足内核的契约。
func TestBackends_SatisfyContracts(t *testing.T) {
	var ws workspace.WorkspaceOpener = defaultWorkspaces()
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

// 空根、不可访问、非目录，都是初始化阶段的装配错误，不该等到第一次读写才发现。
func TestBackends_RejectBadRoot(t *testing.T) {
	if _, err := defaultWorkspaces().Open("  "); err == nil {
		t.Fatal("空根必须被拒绝")
	}
	if _, err := defaultWorkspaces().Open(t.TempDir() + "/does-not-exist"); err == nil {
		t.Fatal("不存在的根必须被拒绝")
	}
}

// 装配处的契约对齐：三组 handler 的签名必须正好是循环要的，业务侧的协作者必须是
// 业务接口的实现。签名漂了在这里就编译不过，不必等到跑起来才发现。
func TestWiring_SatisfiesContracts(t *testing.T) {
	session := hunt.NewSession(hunt.Config{})
	var (
		_ harness.PrepareHandler = session.Prepare
		_ harness.OnTurnHandler  = session.OnTurn
		_ harness.FinalHandler   = session.Finalize

		_ hunt.ContextBuilder  = (*contextBuilder)(nil)
		_ hunt.SessionRecorder = (*sessionRecorder)(nil)
		_ hunt.EventSink       = (*eventSink)(nil)
	)
}

// 事件流是外部唯一通道：一行一条 JSON、type 在顶层、载荷摊平——顺序不能反，层级不能多。
func TestEventSink_EmitsFlatJSONLine(t *testing.T) {
	var events, logs bytes.Buffer
	sink := &eventSink{events: &events, logs: &logs}
	if err := sink.Emit(hunt.ExternalEvent{Type: "tool_result", Payload: map[string]any{
		"tool": "read", "ok": true,
	}}); err != nil {
		t.Fatalf("写事件失败：%v", err)
	}

	line := events.String()
	if !strings.HasSuffix(line, "\n") || strings.Count(line, "\n") != 1 {
		t.Fatalf("一条事件必须自成一行：%q", line)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &got); err != nil {
		t.Fatalf("事件不是合法 JSON：%v", err)
	}
	if got["type"] != "tool_result" {
		t.Errorf("type = %v，期望 tool_result", got["type"])
	}
	if got["tool"] != "read" {
		t.Errorf("载荷必须摊平到顶层：%v", got)
	}
	if _, nested := got["Payload"]; nested {
		t.Errorf("事件不该再套一层 Payload：%v", got)
	}
}

// 上下文的形状：提示词在前、历史按轮追加；空轮不留痕，一轮的调用与结果各成一条消息。
func TestContextBuilder_AssemblesPromptThenHistory(t *testing.T) {
	c := &contextBuilder{}
	c.SetPrompt([]llm.Message{{Role: llm.RoleSystem, Content: "S"}})
	c.Append(harness.Turn{No: 1, Text: "先看看", Calls: []llm.ToolCall{{ID: "c1", Name: "read"}}})
	c.Append(harness.Turn{No: 2})
	c.Append(harness.Turn{No: 3, Calls: []llm.ToolCall{{ID: "c2", Name: "write"}},
		Results: []llm.ToolResult{{CallID: "c2", Output: "ok"}}})

	msgs := c.Assemble()
	if len(msgs) != 4 {
		t.Fatalf("消息数 = %d，期望 4（提示词 1 + 轮1 1 + 轮3 2）：%+v", len(msgs), msgs)
	}
	if msgs[0].Role != llm.RoleSystem {
		t.Errorf("提示词必须在最前：%v", msgs[0].Role)
	}
	if msgs[1].Role != llm.RoleAssistant || len(msgs[1].Calls) != 1 {
		t.Errorf("轮的正文与调用应合成一条 assistant 消息：%+v", msgs[1])
	}
	if msgs[3].Role != llm.RoleTool || msgs[3].Results[0].CallID != "c2" {
		t.Errorf("调用结果应合成一条 tool 消息：%+v", msgs[3])
	}
}

// 每个原语的声明都必须是「已知形状」：名字、说明、合法 JSON Schema 缺一不可。
func TestDefaultTools_ShapeIsDeclared(t *testing.T) {
	for _, prim := range defaultTools(nil, nil, nil) {
		d := prim.Decl()
		if d.Name == "" {
			t.Error("原语名字不得为空")
		}
		if d.Description == "" {
			t.Errorf("%q 缺少说明：模型只能靠它决定什么时候用", d.Name)
		}
		if !json.Valid(d.Schema) {
			t.Errorf("%q 的参数形状不是合法 JSON：%s", d.Name, d.Schema)
		}
	}
}

// 工具面固定：本产品的原语**恰好是这 9 个**、顺序固定。顺序进入每一轮请求的前缀。
//
// 三个符号原语（symbol_read / symbol_edit / symbol_rename）随 MS-8 接回清单：符号后端已接入，
// 模型看得到它们。工具面一旦变化，这里必须是有意识的决定。
func TestDefaultTools_FaceIsFixed(t *testing.T) {
	want := []string{
		"read", "write", "edit", "find", "glob",
		"symbol_read", "symbol_edit", "symbol_rename",
		"check",
	}
	tools := defaultTools(nil, nil, nil)
	if len(tools) != len(want) {
		t.Fatalf("原语数 = %d，期望 %d", len(tools), len(want))
	}
	for i, prim := range tools {
		if prim.Decl().Name != want[i] {
			t.Errorf("第 %d 个原语 = %q，期望 %q", i+1, prim.Decl().Name, want[i])
		}
	}
}

// 「工具面恒定」的对照（IA-7.1）：**环境能力不决定注册**。
//
// 同一份装配在「符号后端可用 / 不可用」两情形下，模型的工具面必须**完全相同**——
// 名字集合与 schema 逐项相同。能力不可用时符号原语返回结构化错误，而不是从工具面上消失：
// 工具名随环境增删，会让"同一份提示词在不同机器上指向不同能力"，结论因此不可复现。
func TestDefaultTools_FaceIsIdenticalWhetherTheBackendIsAvailable(t *testing.T) {
	ws, err := defaultWorkspaces().Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开工作区失败：%v", err)
	}
	available := defaultTools(ws, defaultExt(ws), nil)
	absent := defaultTools(ws, ext.Unimplemented{}, nil)

	if len(available) != len(absent) {
		t.Fatalf("工具面数量随能力变化：可用 %d / 不可用 %d", len(available), len(absent))
	}
	for i := range available {
		a, b := available[i].Decl(), absent[i].Decl()
		if a.Name != b.Name {
			t.Fatalf("第 %d 个原语名字随能力变化：%q vs %q", i+1, a.Name, b.Name)
		}
		if string(a.Schema) != string(b.Schema) {
			t.Errorf("%s 的参数形状随能力变化：%s vs %s", a.Name, a.Schema, b.Schema)
		}
		if a.Description != b.Description {
			t.Errorf("%s 的说明随能力变化", a.Name)
		}
	}
	// 反证：这一条要能真的区分两种装配——后端可用时符号能力确实在线。
	caps := defaultExt(ws).Capabilities(context.Background())
	if !caps.Available {
		t.Fatal("默认后端应可用：否则上面两情形其实是同一种，对照不成立")
	}
	if (ext.Unimplemented{}).Capabilities(context.Background()).Available {
		t.Fatal("空后端应报不可用：否则上面两情形其实是同一种，对照不成立")
	}
}

// 符号原语在工具面上：模型看得到它们，才能在需要时按符号寻址
// （拿不到就只会退化成整块文本替换）。
func TestDefaultTools_SymbolPrimitivesAreRegistered(t *testing.T) {
	face := make(map[string]bool)
	for _, prim := range defaultTools(nil, nil, nil) {
		face[prim.Decl().Name] = true
	}
	for _, name := range []string{
		string(symbolic.SymbolRead),
		string(symbolic.SymbolEdit),
		string(symbolic.SymbolRename),
	} {
		if !face[name] {
			t.Errorf("工具面缺符号原语 %q", name)
		}
	}
}

// 「是否写盘」由原语自述（不再由策略侧镜像）：这份自述错了等于权限边界错了——
// 写原语被当成只读，就会绕开路径裁决。所以逐项钉住，并挡住"新增原语忘了声明性质"。
func TestDefaultTools_WritesIsDeclared(t *testing.T) {
	want := map[string]bool{
		"read": false, "write": true, "edit": true, "find": false, "glob": false,
		"symbol_read": false, "symbol_edit": true, "symbol_rename": true,
		"check": false,
	}
	for _, prim := range defaultTools(nil, nil, nil) {
		name := prim.Decl().Name
		expected, known := want[name]
		if !known {
			t.Errorf("工具面多了一个原语 %q：它的写盘性质要先登记在这里", name)
			continue
		}
		if got := prim.Writes(); got != expected {
			t.Errorf("%s.Writes() = %v，期望 %v", name, got, expected)
		}
		delete(want, name)
	}
	for name := range want {
		t.Errorf("工具面少了原语 %q", name)
	}
}

var _ = hunt.PrimCheckpoint // 控制原语声明存在，装配工具面时殿后

// 信封四字段由 sink 统一盖章：调用点只交业务载荷，"每个事件都带信封"是结构事实，
// 不靠每个发出点各自记得（FR-11.3、INV-5）；业务载荷覆盖不了它们。
func TestEventSink_StampsEnvelopeOnEveryEvent(t *testing.T) {
	var events, logs bytes.Buffer
	sink := &eventSink{
		events: &events, logs: &logs,
		bountyID: "b-1", traceID: "t-9",
		now: func() time.Time { return time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC) },
	}
	// 载荷里塞同名键：信封必须赢。
	if err := sink.Emit(hunt.ExternalEvent{Type: "tool_result", Payload: map[string]any{
		"tool": "read", "bounty_id": "伪造", "ts": "伪造",
	}}); err != nil {
		t.Fatalf("写事件失败：%v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(events.String())), &got); err != nil {
		t.Fatalf("事件不是合法 JSON：%v", err)
	}
	if got["type"] != "tool_result" || got["bounty_id"] != "b-1" || got["trace_id"] != "t-9" {
		t.Errorf("信封字段不对：%v", got)
	}
	if got["ts"] != "2026-09-22T12:00:00Z" {
		t.Errorf("ts 应为 RFC3339 的 UTC 时间：%v", got["ts"])
	}
	if got["tool"] != "read" {
		t.Errorf("业务载荷不该丢：%v", got)
	}
}

// 写入失败要留下痕迹：通道断裂由装配层据此收敛为环境错误（FR-10.4）。
func TestEventSink_RemembersFirstWriteFailure(t *testing.T) {
	sink := &eventSink{events: failingWriter{}, logs: &bytes.Buffer{}}
	if err := sink.Emit(hunt.ExternalEvent{Type: "tool_result"}); err == nil {
		t.Fatal("写入失败必须上抛")
	}
	if sink.Failed() == nil {
		t.Fatal("必须记住这次失败")
	}
	if sink.Failed().Error() != "管道断了" {
		t.Errorf("记住的应是第一次的原因：%v", sink.Failed())
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("管道断了") }

// git 能力**不作为原语暴露**——用类型级证据，而不是靠人记得：
//  1. 工具面名字集 ∩ git 词汇 = 空集；
//  2. 原语工厂的入参只有 workspace.Workspace：原语在构造期**拿不到** git.GitWorktree；
//  3. hunt.Primitive 的方法集里没有任何接受/返回 git.GitWorktree 的入口。
func TestDefaultTools_HasNoGitPrimitive(t *testing.T) {
	ws, err := defaultWorkspaces().Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开工作区失败：%v", err)
	}
	prims := defaultTools(ws, nil, nil)

	gitWords := []string{"git", "commit", "push", "checkout", "branch", "diff", "patch", "merge", "rebase", "clone", "remote", "fetch"}
	for _, p := range prims {
		name := strings.ToLower(p.Decl().Name)
		for _, w := range gitWords {
			if strings.Contains(name, w) {
				t.Errorf("工具面出现 git 相关名字 %q（含 %q）：git 能力不作为原语暴露", name, w)
			}
		}
	}

	// 原语工厂的入参只有 workspace.Workspace：构造期拿不到 git.GitWorktree。
	fac := reflect.TypeOf(hunt.ToolFactory(nil))
	if fac.Kind() != reflect.Func || fac.NumIn() != 1 {
		t.Fatalf("ToolFactory 形状变了：%v", fac)
	}
	if in := fac.In(0); in != reflect.TypeOf((*workspace.Workspace)(nil)).Elem() {
		t.Errorf("ToolFactory 入参 = %v，期望 workspace.Workspace（原语构造期拿不到 git）", in)
	}

	// hunt.Primitive 方法集里没有任何接受/返回 git.GitWorktree 的入口。
	gitType := reflect.TypeOf((*git.GitWorktree)(nil)).Elem()
	primType := reflect.TypeOf((*hunt.Primitive)(nil)).Elem()
	for i := 0; i < primType.NumMethod(); i++ {
		m := primType.Method(i)
		for j := 0; j < m.Type.NumIn(); j++ {
			if m.Type.In(j) == gitType {
				t.Errorf("Primitive.%s 接受 git.GitWorktree——git 能力会因此可达", m.Name)
			}
		}
		for j := 0; j < m.Type.NumOut(); j++ {
			if m.Type.Out(j) == gitType {
				t.Errorf("Primitive.%s 返回 git.GitWorktree——git 能力会因此可达", m.Name)
			}
		}
	}
}
