package hunt

import (
	"encoding/json"
	"testing"

	"xhunter/llm"
	"xhunter/workspace"
)

// 绑定层的职责只有一件：把模型给的「名字 + 参数 JSON」原样搬进业务槽位。
// 它**不判断名字存不存在**（那是查表的事），但形状不成立必须显式报错——
// 模型以为调了 A、实际执行了 B 的那类错误，要到很久以后才会以"任务做错了"暴露。

func TestBindToolCall_MapsSlots(t *testing.T) {
	call, fault := BindToolCall(llm.ToolCall{
		ID:   "c1",
		Name: "edit",
		Arguments: []byte(`{"path":"a/b.go","literal":"old","symbol":"pkg.F","in_symbol":"body",
			"file_view":true,"scope":"pkg","content":"new","name":"gate","new_name":"New","summary":"为什么",
			"include":["node_modules","vendor"]}`),
	})
	if fault != nil {
		t.Fatalf("合法调用不该报错：%v", fault)
	}
	if call.ID != "c1" || call.Primitive != "edit" || call.Target != "a/b.go" {
		t.Errorf("基本槽位不对：%+v", call)
	}
	if call.Selector.Literal != "old" || call.Selector.Symbol != "pkg.F" ||
		call.Selector.InSymbol != "body" || !call.Selector.FileView || call.Selector.Scope != "pkg" {
		t.Errorf("定位槽位不对：%+v", call.Selector)
	}
	if len(call.Selector.Include) != 2 || call.Selector.Include[0] != "node_modules" ||
		call.Selector.Include[1] != "vendor" {
		t.Errorf("include 槽位不对：%+v", call.Selector.Include)
	}
	if call.Content != "new" || call.Gate != "gate" || call.NewName != "New" || call.Summary != "为什么" {
		t.Errorf("具名槽位不对：%+v", call)
	}
}

func TestBindToolCall_RangeSlot(t *testing.T) {
	call, fault := BindToolCall(llm.ToolCall{ID: "c", Name: "read", Arguments: []byte(`{"path":"a","range":{"from":3,"to":9}}`)})
	if fault != nil {
		t.Fatalf("不该报错：%v", fault)
	}
	if call.Selector.Range == nil || call.Selector.Range.From != 3 || call.Selector.Range.To != 9 {
		t.Errorf("行范围槽位不对：%+v", call.Selector.Range)
	}
	// 省略 range 时常量零值可用（不是 nil 之外的另一套语义）。
	call, _ = BindToolCall(llm.ToolCall{ID: "c", Name: "read", Arguments: []byte(`{"path":"a"}`)})
	if call.Selector.Range != nil {
		t.Errorf("未给 range 时不该造出区间：%+v", call.Selector.Range)
	}
}

// 空参数与 null 都按"没有参数"处理：上游在无参数时未必给 {}。
func TestBindToolCall_EmptyArgumentsMeanNone(t *testing.T) {
	for _, args := range []string{"", "   ", "null"} {
		call, fault := BindToolCall(llm.ToolCall{ID: "c", Name: "checkpoint", Arguments: []byte(args)})
		if fault != nil {
			t.Errorf("args=%q 应按无参数处理：%v", args, fault)
		}
		if call.Target != "" || call.Content != "" {
			t.Errorf("args=%q 不该产生槽位值：%+v", args, call)
		}
	}
}

// 形状不成立必须显式报错，且**不判断名字存不存在**：名字的知识属于装配层。
func TestBindToolCall_RejectsShapesThatWouldExecuteTheWrongThing(t *testing.T) {
	cases := []struct {
		name string
		args string
	}{
		{"非对象", `[1,2]`},
		{"截断的 JSON", `{"path":`},
		{"未知字段", `{"path":"a","pth":"笔误"}`},
		{"对象之后还有内容", `{"path":"a"}{"path":"b"}`},
		{"对象之后跟着文本", `{"path":"a"} 这里不该有东西`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, fault := BindToolCall(llm.ToolCall{ID: "c", Name: "read", Arguments: []byte(tc.args)})
			if fault == nil {
				t.Fatalf("args=%q 必须被拒", tc.args)
			}
			if fault.Kind != "invalid_arguments" {
				t.Errorf("错误种类 = %q，期望 invalid_arguments", fault.Kind)
			}
			if fault.Message == "" {
				t.Error("错误信息要能让人看懂哪里错了")
			}
		})
	}
}

// 名字不认识不是绑定层的判断：它照原样交给查表，由执行器报"没有这个工具"。
func TestBindToolCall_DoesNotKnowToolNames(t *testing.T) {
	call, fault := BindToolCall(llm.ToolCall{ID: "c", Name: "完全不存在的工具", Arguments: []byte(`{"path":"a"}`)})
	if fault != nil {
		t.Fatalf("绑定层不该判断工具名是否存在：%v", fault)
	}
	if call.Primitive != "完全不存在的工具" {
		t.Errorf("名字应原样搬运：%q", call.Primitive)
	}
}

// 回灌历史时要还原成"当初发的那次调用"：只写非空字段，空字段不凭空补出来。
func TestUnbindToolCall_RoundTripsNonEmptyFields(t *testing.T) {
	original := Call{
		ID: "c1", Primitive: "edit", Target: "a.txt",
		Selector: Selector{Literal: "old", Scope: "pkg", FileView: true,
			Range: &workspace.LineRange{From: 1, To: 2}, Include: []string{"node_modules"}},
		Content: "new", NewName: "New", Gate: "unit", Summary: "为什么",
	}
	out := UnbindToolCall(original)
	if out.ID != "c1" || out.Name != "edit" {
		t.Fatalf("调用标识与名字应保留：%+v", out)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Arguments, &got); err != nil {
		t.Fatalf("参数不是合法 JSON：%v", err)
	}
	for _, k := range []string{"path", "literal", "scope", "file_view", "range", "content", "new_name", "name", "summary", "include"} {
		if _, ok := got[k]; !ok {
			t.Errorf("字段 %q 丢失：%v", k, got)
		}
	}
	// 再绑一次应得到同一份业务调用（历史回灌与当初执行的一致性）。
	back, fault := BindToolCall(out)
	if fault != nil {
		t.Fatalf("还原结果应可再绑定：%v", fault)
	}
	if back.Target != original.Target || back.Content != original.Content ||
		back.Selector.Literal != original.Selector.Literal || back.Summary != original.Summary ||
		len(back.Selector.Include) != 1 || back.Selector.Include[0] != "node_modules" {
		t.Errorf("往返不一致：%+v vs %+v", back, original)
	}
}

// 结果回灌文本要区分"可重试"与"不可重试"：无人值守时模型据此决定换不换做法。
func TestUnbindToolResult_CarriesRetryability(t *testing.T) {
	fault := UnbindToolResult(Result{CallID: "c1", Summary: "补充说明",
		Err: &llm.Fault{Kind: "stale_read", Message: "文件被改过", Retryable: true}})
	if !fault.IsError || fault.CallID != "c1" {
		t.Errorf("失败结果应带 IsError 与 CallID：%+v", fault)
	}
	if want := "工具执行失败[stale_read，可重试]：文件被改过（补充说明）"; fault.Output != want {
		t.Errorf("回灌文本 = %q，期望 %q", fault.Output, want)
	}

	no := UnbindToolResult(Result{CallID: "c2", Err: &llm.Fault{Kind: "not_implemented", Message: "未实现"}})
	if no.Output != "工具执行失败[not_implemented，不可重试]：未实现" {
		t.Errorf("不可重试的写法不对：%q", no.Output)
	}
}
