package harness

import (
	"reflect"
	"strings"
	"testing"

	"xhunter/llm"
)

// 绑定层是"模型侧的中立调用"与"本场景的领域调用"之间的唯一转换点。
// 它同时承担两件必须在执行之前完成的事：工具名必须在工具面之内、参数必须能解释。

func TestBindToolCall_TranslatesArguments(t *testing.T) {
	cases := []struct {
		name string
		raw  llm.ToolCall
		want Call
	}{
		{
			name: "读取带行范围",
			raw:  llm.ToolCall{ID: "c1", Name: "read", Arguments: []byte(`{"path":"a.go","range":{"from":3,"to":9}}`)},
			want: Call{
				Primitive: testRead,
				Target:    "a.go",
				Selector:  Selector{Range: &LineRange{From: 3, To: 9}},
			},
		},
		{
			name: "按符号定位",
			raw:  llm.ToolCall{ID: "c1", Name: "edit", Arguments: []byte(`{"path":"a.go","symbol":"pkg.Fn","in_symbol":"inner","content":"new()"}`)},
			want: Call{
				Primitive: testEdit,
				Target:    "a.go",
				Selector:  Selector{Symbol: "pkg.Fn", InSymbol: "inner"},
				Content:   "new()",
			},
		},
		{
			name: "读取符号视图",
			raw:  llm.ToolCall{ID: "c1", Name: "read", Arguments: []byte(`{"path":"a.go","file_view":true}`)},
			want: Call{
				Primitive: testRead,
				Target:    "a.go",
				Selector:  Selector{FileView: true},
			},
		},
		{
			name: "限定检索范围",
			raw:  llm.ToolCall{ID: "c1", Name: "find", Arguments: []byte(`{"literal":"TODO","scope":"internal/"}`)},
			want: Call{
				Primitive: testFind,
				Selector:  Selector{Literal: "TODO", Scope: "internal/"},
			},
		},
		{
			name: "参数为空对象",
			raw:  llm.ToolCall{ID: "c1", Name: "glob", Arguments: []byte(`{}`)},
			want: Call{Primitive: testGlob},
		},
		{
			name: "参数为空串",
			raw:  llm.ToolCall{ID: "c1", Name: "glob"},
			want: Call{Primitive: testGlob},
		},
		{
			name: "参数为 null",
			raw:  llm.ToolCall{ID: "c1", Name: "glob", Arguments: []byte(`null`)},
			want: Call{Primitive: testGlob},
		},
		{
			name: "门禁条目名",
			raw:  llm.ToolCall{ID: "c1", Name: "check", Arguments: []byte(`{"name":"unit"}`)},
			want: Call{Primitive: testCheck, Gate: "unit"},
		},
		{
			name: "重命名的目标名",
			raw:  llm.ToolCall{ID: "c1", Name: "rename", Arguments: []byte(`{"path":"a.go","symbol":"pkg.Fn","new_name":"Fn2"}`)},
			want: Call{
				Primitive: testRename,
				Target:    "a.go",
				Selector:  Selector{Symbol: "pkg.Fn"},
				NewName:   "Fn2",
			},
		},
		{
			name: "检查点的意图说明",
			raw:  llm.ToolCall{ID: "c1", Name: "checkpoint", Arguments: []byte(`{"summary":"结构完整"}`)},
			want: Call{Primitive: PrimCheckpoint, Summary: "结构完整"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, fault := BindToolCall(tc.raw)
			if fault != nil {
				t.Fatalf("绑定失败：%+v", fault)
			}
			want := tc.want
			want.ID = "c1"
			if !reflect.DeepEqual(got, want) {
				t.Errorf("绑定结果 = %+v，期望 %+v", got, want)
			}
		})
	}
}

// 绑定只做形状翻译，不判断"这个名字存不存在"：有哪些原语由装配层决定，
// 框架手里没有白名单，也不该有——名字不认识由运行时查表时挡下并列出可用的名字。
func TestBindToolCall_DoesNotJudgeToolNames(t *testing.T) {
	call, fault := BindToolCall(llm.ToolCall{ID: "c1", Name: "deploy", Arguments: []byte(`{"path":"a.go"}`)})
	if fault != nil {
		t.Fatalf("绑定不该因名字而失败：%+v", fault)
	}
	if call.Primitive != "deploy" || call.Target != "a.go" {
		t.Errorf("名字与参数应原样搬运：%+v", call)
	}

	// 名字为空同样留给运行时判断（那里才知道清单是什么）。
	if _, fault := BindToolCall(llm.ToolCall{ID: "c2"}); fault != nil {
		t.Errorf("空名字也不该在绑定层失败：%+v", fault)
	}
}

func TestBindToolCall_RejectsShapesThatWouldExecuteTheWrongThing(t *testing.T) {
	cases := []struct {
		name     string
		raw      llm.ToolCall
		wantKind string
	}{
		{"参数不是合法 JSON", llm.ToolCall{ID: "c1", Name: "read", Arguments: []byte(`{"path":`)}, "invalid_arguments"},
		{"参数含未知字段", llm.ToolCall{ID: "c1", Name: "read", Arguments: []byte(`{"filename":"a.go"}`)}, "invalid_arguments"},
		{"参数类型不符", llm.ToolCall{ID: "c1", Name: "read", Arguments: []byte(`{"path":123}`)}, "invalid_arguments"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			call, fault := BindToolCall(tc.raw)
			if fault == nil {
				t.Fatal("不成立的调用必须返回结构化错误：尽力而为会让模型以为它调用了一个工具、" +
					"实际执行的是另一个意思，而错误要到很久以后才以「任务做错了」的形式暴露")
			}
			if fault.Kind != tc.wantKind {
				t.Errorf("错误类型 = %q，期望 %q（%s）", fault.Kind, tc.wantKind, fault.Message)
			}
			if call.ID != "c1" {
				t.Errorf("调用标识 = %q：形状不成立也要保留，失败结果要按它回灌", call.ID)
			}
		})
	}
}

func TestUnbindToolCall_OmitsAbsentFields(t *testing.T) {
	got := UnbindToolCall(Call{ID: "c1", Primitive: testRead, Target: "a.go"})
	if got.Name != "read" || got.ID != "c1" {
		t.Errorf("还原结果 = %+v", got)
	}
	if string(got.Arguments) != `{"path":"a.go"}` {
		t.Errorf("参数 = %s，只有给出的字段才该出现：零值会被上游读成「给了一个空值」", got.Arguments)
	}

	full := UnbindToolCall(Call{
		ID:        "c1",
		Primitive: testEdit,
		Target:    "a.go",
		Selector:  Selector{Symbol: "pkg.Fn", Range: &LineRange{From: 1, To: 2}, FileView: true},
		Content:   "x",
	})
	text := string(full.Arguments)
	for _, want := range []string{`"path":"a.go"`, `"symbol":"pkg.Fn"`, `"range":{"from":1,"to":2}`, `"file_view":true`, `"content":"x"`} {
		if !strings.Contains(text, want) {
			t.Errorf("还原结果 = %s，缺少 %s", text, want)
		}
	}
}

func TestUnbindToolResult_CarriesRetryability(t *testing.T) {
	ok := UnbindToolResult(Result{CallID: "c1", OK: true, Summary: "已读取 10 行"})
	if ok.Output != "已读取 10 行" || ok.IsError {
		t.Errorf("成功结果 = %+v", ok)
	}

	failed := UnbindToolResult(Result{
		CallID: "c2",
		Err:    &ToolError{Kind: "not_found", Message: "匹配不到唯一位置"},
	})
	if !failed.IsError {
		t.Error("失败结果必须标出 IsError：支持的协议会据此如实标注给模型")
	}
	for _, want := range []string{"not_found", "不可重试", "匹配不到唯一位置"} {
		if !strings.Contains(failed.Output, want) {
			t.Errorf("失败文本 = %q，缺少 %q：模型看不到日志，这是它唯一的输入", failed.Output, want)
		}
	}
}

func TestToLLMMessages_SplitsCallsAndResults(t *testing.T) {
	msgs := toLLMMessages([]Message{
		{Role: RoleAssistant, Content: "我来看看", Calls: []Call{
			{ID: "c1", Primitive: testRead, Target: "a.go"},
		}},
		{Role: RoleTool, Results: []Result{
			{CallID: "c1", OK: true, Summary: "已读取"},
		}},
	})
	if len(msgs) != 2 {
		t.Fatalf("消息数 = %d", len(msgs))
	}
	if msgs[0].Calls[0].Name != "read" || msgs[0].Content != "我来看看" {
		t.Errorf("助手消息 = %+v，正文与调用都要保留", msgs[0])
	}
	if msgs[1].Results[0].CallID != "c1" || msgs[1].Results[0].IsError {
		t.Errorf("结果消息 = %+v", msgs[1])
	}
}
