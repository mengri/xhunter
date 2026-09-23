package hunt

import (
	"context"
	"encoding/json"
	"testing"

	"xhunter/harness"
	"xhunter/llm"
)

// 轮级事件：`tool_call`（执行前）/ `assistant_text`（收流合并后）/ `usage`（每轮增量）。
// 本文件用假 sink、内存工作区，断言的是"事件流里能看到模型一步步做了什么、说了什么、花了多少"。

// marshallingSink 像真出口那样把载荷序列化：用来证明"参数不合法也不会让事件通道报错"。
// 真出口的 Emit 一旦序列化失败就记下写错误，随后会被通道健康检查当成环境问题——一次普通的
// 参数错误不该被升级成进程终止，所以这条必须守住。
type marshallingSink struct {
	events []ExternalEvent
	errs   []error
}

func (m *marshallingSink) Emit(ev ExternalEvent) error {
	if _, err := json.Marshal(ev.Payload); err != nil {
		m.errs = append(m.errs, err)
		return err
	}
	m.events = append(m.events, ev)
	return nil
}
func (m *marshallingSink) Failed() error              { return nil }
func (m *marshallingSink) Log(string, string, ...any) {}
func (m *marshallingSink) Heartbeat(Phase) error      { return nil }

// tool_call 与 tool_result 必须成对、同一个 call_id，且 tool_call 在前；tool 是模型原始名字。
func TestOnTurn_EmitsToolCallBeforeResult(t *testing.T) {
	st := &memStorage{files: map[string]string{"a.txt": "hello"}}
	sink := &captureSink{}
	s := newTestSession(t, st, allowAll{}, sink, stubPrim{name: "ok_prim", res: Result{Summary: "干完了"}})

	turn := &harness.Turn{No: 1, Calls: []llm.ToolCall{call("c1", "ok_prim", `{}`)}}
	if _, err := s.OnTurn(context.Background(), &harness.Run{}, turn); err != nil {
		t.Fatalf("OnTurn 失败：%v", err)
	}

	callIdx, resultIdx := -1, -1
	for i, ev := range sink.events {
		switch ev.Type {
		case "tool_call":
			callIdx = i
		case "tool_result":
			resultIdx = i
		}
	}
	if callIdx < 0 || resultIdx < 0 {
		t.Fatalf("应各有 tool_call 与 tool_result：%v", sink.events)
	}
	if callIdx > resultIdx {
		t.Error("tool_call 必须排在对应 tool_result 之前")
	}
	callEv, resEv := sink.events[callIdx].Payload, sink.events[resultIdx].Payload
	if callEv["call_id"] != "c1" || resEv["call_id"] != "c1" {
		t.Errorf("tool_call / tool_result 必须用同一个 call_id 配对：%v vs %v", callEv["call_id"], resEv["call_id"])
	}
	if callEv["tool"] != "ok_prim" {
		t.Errorf("tool 应是模型原始名字：%v", callEv["tool"])
	}
}

// 参数不合法时 tool_call 照发、通道不报错；随后的 tool_result 仍是结构化失败。
func TestOnTurn_ToolCallArgsSurviveMalformedJSON(t *testing.T) {
	st := &memStorage{files: map[string]string{}}
	sink := &marshallingSink{}
	s := newTestSession(t, st, allowAll{}, sink, stubPrim{name: "ok_prim"})

	const bad = "{这不是合法 JSON"
	turn := &harness.Turn{No: 1, Calls: []llm.ToolCall{call("c1", "ok_prim", bad)}}
	if _, err := s.OnTurn(context.Background(), &harness.Run{}, turn); err != nil {
		t.Fatalf("OnTurn 失败：%v", err)
	}
	if len(sink.errs) != 0 {
		t.Fatalf("非法参数不得让事件通道报错：%v", sink.errs)
	}

	var tc, tr map[string]any
	for _, ev := range sink.events {
		switch ev.Type {
		case "tool_call":
			tc = ev.Payload
		case "tool_result":
			tr = ev.Payload
		}
	}
	if tc == nil {
		t.Fatal("tool_call 必须照发——参数不合法是后续要报的结构化错误，不是不发事件的理由")
	}
	if args, ok := tc["args"].(string); !ok || args != bad {
		t.Errorf("非法参数应退化成字符串原样带上：%#v", tc["args"])
	}
	if tr == nil || tr["ok"] != false {
		t.Errorf("随后的 tool_result 应是结构化失败：%v", tr)
	}
}

// 用量事件是**增量**：第二轮上报的是第二轮的净增，而不是累计值。
func TestOnTurn_EmitsUsageDeltaPerTurn(t *testing.T) {
	sink := &captureSink{}
	s := NewSession(Config{Policy: allowAll{}, Sink: sink})
	ctx := context.Background()

	run := &harness.Run{Usage: llm.Usage{InputTokens: 100, OutputTokens: 50, CachedInputTokens: 20}}
	if _, err := s.OnTurn(ctx, run, &harness.Turn{No: 1}); err != nil {
		t.Fatalf("OnTurn 失败：%v", err)
	}
	run.Usage = llm.Usage{InputTokens: 150, OutputTokens: 80, CachedInputTokens: 35}
	if _, err := s.OnTurn(ctx, run, &harness.Turn{No: 2}); err != nil {
		t.Fatalf("OnTurn 失败：%v", err)
	}

	usages := sink.ofType("usage")
	if len(usages) != 2 {
		t.Fatalf("每轮各一条 usage，实际 %d：%v", len(usages), usages)
	}
	want := map[string]any{"input_tokens": 50, "output_tokens": 30, "cached_input_tokens": 15}
	for k, v := range want {
		if usages[1][k] != v {
			t.Errorf("第二轮 usage.%s = %v，期望 %v（增量而非累计）", k, usages[1][k], v)
		}
	}
	// 增量在水位那一处算出：第二轮水位过后累计值原样保存。
	if run.Usage.InputTokens != 150 {
		t.Errorf("水位不应改写累计值：%+v", run.Usage)
	}
}

// 上游一直不回报用量：不发 usage——一条全零会被读成"这轮不花钱"，那是假数字。
func TestOnTurn_NoUsageEventWhenUpstreamSilent(t *testing.T) {
	sink := &captureSink{}
	s := NewSession(Config{Policy: allowAll{}, Sink: sink})
	ctx := context.Background()
	run := &harness.Run{} // run.Usage 恒为零：上游沉默

	for n := 1; n <= 3; n++ {
		if _, err := s.OnTurn(ctx, run, &harness.Turn{No: n}); err != nil {
			t.Fatalf("OnTurn 失败：%v", err)
		}
	}
	if usages := sink.ofType("usage"); len(usages) != 0 {
		t.Errorf("上游沉默时不得发 usage，实际 %d 条：%v", len(usages), usages)
	}
}

// assistant_text 必须是收流合并后的正文，且与进历史、自陈解析用的是同一份；空正文不发。
func TestOnTurn_EmitsAssistantTextMatchingHistory(t *testing.T) {
	ctx := context.Background()

	t.Run("有正文", func(t *testing.T) {
		ctxLog := &recordingContext{}
		sink := &captureSink{}
		s := NewSession(Config{Context: ctxLog, Sink: sink, Policy: allowAll{}})

		const text = "结论如下。\n\n## 需要补全\n- 缺一个前提"
		if _, err := s.OnTurn(ctx, &harness.Run{}, &harness.Turn{No: 1, Text: text}); err != nil {
			t.Fatalf("OnTurn 失败：%v", err)
		}

		texts := sink.ofType("assistant_text")
		if len(texts) != 1 || texts[0]["text"] != text {
			t.Fatalf("assistant_text 必须是收流后的正文：%v", texts)
		}
		if len(ctxLog.appended) != 1 || ctxLog.appended[0].Text != text {
			t.Errorf("进历史的必须是同一份正文：%+v", ctxLog.appended)
		}
		if needs := s.Declared().Needs; len(needs) != 1 || needs[0] != "缺一个前提" {
			t.Errorf("自陈解析用的也必须是同一份正文：%v", needs)
		}
		if s.Delivery().Summary != text {
			t.Errorf("最终答复（交付事实）应是这份正文：%q", s.Delivery().Summary)
		}
	})

	t.Run("空正文不发", func(t *testing.T) {
		sink := &captureSink{}
		s := NewSession(Config{Sink: sink, Policy: allowAll{}})
		if _, err := s.OnTurn(ctx, &harness.Run{}, &harness.Turn{No: 1, Text: "  \n "}); err != nil {
			t.Fatalf("OnTurn 失败：%v", err)
		}
		if n := len(sink.ofType("assistant_text")); n != 0 {
			t.Errorf("空正文不发 assistant_text，实际 %d 条", n)
		}
		if s.Delivery().Summary != "" {
			t.Errorf("空正文不该成为最终答复：%q", s.Delivery().Summary)
		}
	})
}
