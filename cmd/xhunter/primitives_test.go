package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"xhunter/harness"
	"xhunter/hunt"
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
	for _, prim := range defaultTools(nil, nil) {
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

// 工具面固定：本产品的原语恰好是这 9 个、顺序固定。顺序进入每一轮请求的前缀。
func TestDefaultTools_FaceIsFixed(t *testing.T) {
	want := []string{"read", "write", "edit", "find", "glob",
		"symbol_read", "symbol_edit", "symbol_rename", "check"}
	tools := defaultTools(nil, nil)
	if len(tools) != len(want) {
		t.Fatalf("原语数 = %d，期望 %d", len(tools), len(want))
	}
	for i, prim := range tools {
		if prim.Decl().Name != want[i] {
			t.Errorf("第 %d 个原语 = %q，期望 %q", i+1, prim.Decl().Name, want[i])
		}
	}
}

var _ = hunt.PrimCheckpoint // 控制原语声明存在，装配工具面时殿后
