package hunt

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"xhunter/harness"
	"xhunter/llm"
	"xhunter/workspace"
)

// 本文件补的是"主生命周期零用例"这块：executeCall 的每条出口都必须留下
// **一条带 call_id 的 tool_result**，策略拒绝还要额外留 policy_denied。

// ============================================================ 桩

// memStorage 是内存版工作区：只为这几条用例服务，不引入 internal 侧实现。
type memStorage struct{ files map[string]string }

func (m *memStorage) Read(rel string, _ workspace.LineRange) (workspace.FileContent, error) {
	content, ok := m.files[rel]
	if !ok {
		return workspace.FileContent{}, &llm.Fault{Kind: "not_found", Message: "文件不存在：" + rel, Retryable: true}
	}
	return workspace.FileContent{Path: rel, Raw: content, Fingerprint: content, TotalLines: strings.Count(content, "\n") + 1}, nil
}

func (m *memStorage) Stat(rel string) (workspace.FileInfo, error) {
	_, ok := m.files[rel]
	return workspace.FileInfo{Path: rel, Exists: ok}, nil
}

func (m *memStorage) List(string) ([]string, error) { return nil, nil }

func (m *memStorage) WriteRange(rel string, br workspace.ByteRange, content string) (string, error) {
	cur := m.files[rel]
	if br.Start < 0 || br.End > len(cur) || br.Start > br.End {
		return "", &llm.Fault{Kind: "bad_range", Message: "区间非法"}
	}
	next := cur[:br.Start] + content + cur[br.End:]
	m.files[rel] = next
	return next, nil
}

type stubPrim struct {
	name  PrimitiveName
	res   Result
	edits []workspace.FileEdit
	err   error
}

func (p stubPrim) Decl() llm.ToolDecl { return llm.ToolDecl{Name: string(p.name), Description: "stub"} }

func (p stubPrim) Execute(context.Context, Call, Facts) (Result, []workspace.FileEdit, error) {
	return p.res, p.edits, p.err
}

// Writes 由桩的编辑计划推出：有编辑即写盘，让桩的写盘性质与它在 executeCall 里的行为一致。
func (p stubPrim) Writes() bool { return len(p.edits) > 0 }

type captureSink struct {
	events []ExternalEvent
	logs   []string
}

func (c *captureSink) Emit(ev ExternalEvent) error { c.events = append(c.events, ev); return nil }
func (c *captureSink) Log(_, msg string, kv ...any) {
	c.logs = append(c.logs, msg+fmt.Sprint(kv...))
}
func (c *captureSink) Heartbeat(Phase) error { return nil }
func (c *captureSink) Failed() error         { return nil }

func (c *captureSink) ofType(kind string) []map[string]any {
	var out []map[string]any
	for _, ev := range c.events {
		if ev.Type == kind {
			out = append(out, ev.Payload)
		}
	}
	return out
}

// allowAll 是"全部放行"的策略桩：本文件的用例关心的是事件与落盘，不是边界判定。
type allowAll struct{}

func (allowAll) Decide(context.Context, Call) (Decision, error) {
	return Decision{Verdict: VerdictAllow, Reason: "测试放行"}, nil
}
func (allowAll) Charge(llm.Usage)                {}
func (allowAll) Exhausted(TurnNo) (bool, string) { return false, "" }
func (allowAll) ObserveFailure(string) (StopLoss, string) {
	return StopContinue, ""
}
func (allowAll) DeniedCount() (int, bool) { return 0, false }

type denyAll struct{ reason string }

func (d denyAll) Decide(context.Context, Call) (Decision, error) {
	return Decision{Verdict: VerdictDeny, Reason: d.reason}, nil
}
func (denyAll) Charge(llm.Usage)                {}
func (denyAll) Exhausted(TurnNo) (bool, string) { return false, "" }
func (denyAll) ObserveFailure(string) (StopLoss, string) {
	return StopContinue, ""
}
func (denyAll) DeniedCount() (int, bool) { return 0, false }

// newTestSession 直接装配到"可执行"状态：绕过 Prepare（它要 git/opener），
// 只钉住 executeCall 这一层的行为。
func newTestSession(t *testing.T, st workspace.Storage, policy Policy, sink EventSink, prims ...Primitive) *Session {
	t.Helper()
	s := NewSession(Config{
		Policy: policy,
		Sink:   sink,
		Tools:  func(workspace.Workspace) []Primitive { return prims },
	})
	s.storage = st
	if err := s.buildTools(st); err != nil {
		t.Fatalf("构造工具面失败：%v", err)
	}
	return s
}

func call(id, name, args string) llm.ToolCall {
	return llm.ToolCall{ID: id, Name: name, Arguments: []byte(args)}
}

// ============================================================ 用例

// 每条出口都恰好一条 tool_result，且都带 call_id / tool / ok。
func TestExecuteCall_EveryOutcomeEmitsOneToolResult(t *testing.T) {
	st := &memStorage{files: map[string]string{"a.txt": "hello"}}
	sink := &captureSink{}
	prims := []Primitive{
		stubPrim{name: "ok_prim", res: Result{Summary: "干完了"}},
		stubPrim{name: "fault_prim", res: Result{Err: &llm.Fault{Kind: "not_found", Message: "没找到", Retryable: true}}},
		stubPrim{name: "boom_prim", err: errors.New("工具炸了")},
		stubPrim{name: "write_prim", res: Result{Summary: "已新建 new.txt"}, edits: []workspace.FileEdit{{File: "new.txt", NewContent: "x"}}},
		stubPrim{name: "stale_prim", edits: []workspace.FileEdit{{File: "a.txt", NewContent: "y"}}},
	}
	session := newTestSession(t, st, allowAll{}, sink, prims...)

	cases := []struct {
		name     string
		call     llm.ToolCall
		wantErr  bool
		wantKind string
	}{
		{"成功", call("c1", "ok_prim", `{}`), false, ""},
		{"原语报错", call("c2", "fault_prim", `{}`), true, "not_found"},
		{"执行失败", call("c3", "boom_prim", `{}`), true, "execute_failed"},
		{"落盘", call("c4", "write_prim", `{}`), false, ""},
		{"未读即写", call("c5", "stale_prim", `{}`), true, "commit_failed"},
		{"名字不认识", call("c6", "nope", `{}`), true, "unknown_tool"},
		{"绑定失败", call("c7", "ok_prim", `{"未知字段":1}`), true, "invalid_arguments"},
		{"检查点", call("c8", string(PrimCheckpoint), `{"summary":"这里值得停一下"}`), false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(sink.events)
			res := session.executeCall(context.Background(), &harness.Turn{No: 1}, tc.call)

			if res.IsError != tc.wantErr {
				t.Errorf("IsError = %v，期望 %v（回灌文本：%q）", res.IsError, tc.wantErr, res.Output)
			}
			if res.CallID != tc.call.ID {
				t.Errorf("回灌结果的 CallID = %q，期望 %q", res.CallID, tc.call.ID)
			}
			if tc.wantErr && res.Output == "" {
				t.Error("失败必须给出原因：模型要靠它换做法")
			}

			events := sink.events[before:]
			var results []map[string]any
			for _, ev := range events {
				if ev.Type == "tool_result" {
					results = append(results, ev.Payload)
				}
			}
			if len(results) != 1 {
				t.Fatalf("每次调用必须恰好一条 tool_result，实际 %d 条：%v", len(results), events)
			}
			p := results[0]
			if p["call_id"] != tc.call.ID {
				t.Errorf("tool_result.call_id = %v，期望 %q（配对唯一依据）", p["call_id"], tc.call.ID)
			}
			if p["tool"] != tc.call.Name {
				t.Errorf("tool_result.tool = %v，期望 %q", p["tool"], tc.call.Name)
			}
			if p["ok"] != !tc.wantErr {
				t.Errorf("tool_result.ok = %v，期望 %v", p["ok"], !tc.wantErr)
			}
			if _, ok := p["duration_ms"]; !ok {
				t.Error("tool_result 应带 duration_ms")
			}
			if tc.wantKind != "" && p["error"] != tc.wantKind {
				t.Errorf("tool_result.error = %v，期望 %q", p["error"], tc.wantKind)
			}
			if tc.wantKind == "" {
				if _, hasErr := p["error"]; hasErr {
					t.Errorf("成功路径不该带 error：%v", p)
				}
			}
		})
	}
}

// 策略拒绝要留两条痕迹：回灌给模型的原因，以及外部可见的 policy_denied。
func TestExecuteCall_PolicyDenialIsReported(t *testing.T) {
	sink := &captureSink{}
	session := newTestSession(t, &memStorage{files: map[string]string{}}, denyAll{reason: "禁止写入引擎控制目录"},
		sink, stubPrim{name: "write_prim"})

	res := session.executeCall(context.Background(), &harness.Turn{No: 1},
		call("c1", "write_prim", `{"path":".xhunter/x"}`))

	if !res.IsError || !strings.Contains(res.Output, "禁止写入引擎控制目录") {
		t.Errorf("拒绝原因必须回灌给模型：%+v", res)
	}
	denied := sink.ofType("policy_denied")
	if len(denied) != 1 {
		t.Fatalf("应恰好一条 policy_denied，实际 %d：%v", len(denied), sink.events)
	}
	if denied[0]["call_id"] != "c1" {
		t.Errorf("policy_denied.call_id = %v，期望 c1（要能回溯到具体调用）", denied[0]["call_id"])
	}
	if denied[0]["action"] != "write_prim" {
		t.Errorf("policy_denied.action = %v", denied[0]["action"])
	}
	if denied[0]["reason"] != "禁止写入引擎控制目录" {
		t.Errorf("policy_denied.reason = %v", denied[0]["reason"])
	}
	// 被拒的调用不得留下任何写操作。
	if len(session.ops) != 0 {
		t.Errorf("被拒调用不得落盘：%+v", session.ops)
	}
	results := sink.ofType("tool_result")
	if len(results) != 1 || results[0]["error"] != "policy_denied" {
		t.Errorf("被拒调用也要有 tool_result：%v", results)
	}
}

// 成功路径的 summary 与落盘记录都要齐：tool_result 是外部唯一的进度凭据。
func TestExecuteCall_SuccessRecordsOpsAndSummary(t *testing.T) {
	st := &memStorage{files: map[string]string{}}
	sink := &captureSink{}
	session := newTestSession(t, st, allowAll{}, sink,
		stubPrim{name: "write_prim", res: Result{Summary: "已新建 new.txt"},
			edits: []workspace.FileEdit{{File: "new.txt", NewContent: "hi"}}})

	res := session.executeCall(context.Background(), &harness.Turn{No: 3},
		call("c1", "write_prim", `{"path":"new.txt","content":"hi"}`))
	if res.IsError {
		t.Fatalf("不该失败：%+v", res)
	}
	if got := st.files["new.txt"]; got != "hi" {
		t.Errorf("落盘内容 = %q", got)
	}
	if len(session.ops) != 1 || session.ops[0].Turn != 3 || session.ops[0].Primitive != "write_prim" {
		t.Errorf("写操作记录不完整：%+v", session.ops)
	}
	results := sink.ofType("tool_result")
	if len(results) != 1 || results[0]["summary"] != "已新建 new.txt" || results[0]["ok"] != true {
		t.Errorf("tool_result 载荷不对：%v", results)
	}
}

// 未装配策略 = 默认拒绝（INV-4）：忘装配不该让执行体变成无边界的写入者。
func TestExecuteCall_MissingPolicyFailsClosed(t *testing.T) {
	st := &memStorage{files: map[string]string{}}
	sink := &captureSink{}
	session := newTestSession(t, st, nil, sink,
		stubPrim{name: "write_prim", res: Result{Summary: "本不该执行"},
			edits: []workspace.FileEdit{{File: "new.txt", NewContent: "x"}}})

	res := session.executeCall(context.Background(), &harness.Turn{No: 1}, call("c1", "write_prim", `{}`))
	if !res.IsError {
		t.Fatalf("缺策略必须拒绝：%+v", res)
	}
	if len(st.files) != 0 || len(session.ops) != 0 {
		t.Errorf("被拒调用不得产生任何落盘：files=%v ops=%v", st.files, session.ops)
	}
	results := sink.ofType("tool_result")
	if len(results) != 1 || results[0]["error"] != "policy_missing" {
		t.Errorf("应以 policy_missing 上报：%v", results)
	}
}
