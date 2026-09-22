package adapter

import (
	"strings"
	"testing"

	"xhunter/llm"
)

// ============================================================ 流式分帧

func TestSSEReader_FramesEvents(t *testing.T) {
	in := ": 注释行被忽略\n" +
		"event: message\n" +
		"retry: 100\n" +
		`data: {"a":1}` + "\n" +
		"\n" +
		// 一个事件的载荷可以分多个 data 行，按换行拼接。
		`data: {"b":` + "\n" +
		`data: 2}` + "\n" +
		"\n" +
		// 冒号后没有空格也是合法的。
		"data:[DONE]\n" +
		"\n" +
		// 末尾事件没有空行收尾：不能把它丢掉。
		`data: {"c":3}`

	want := []string{`{"a":1}`, "{\"b\":\n2}", "[DONE]", `{"c":3}`}

	r := NewSSEReader(strings.NewReader(in))
	var got []string
	for {
		data, ok, err := r.Next()
		if err != nil {
			t.Fatalf("读取失败：%v", err)
		}
		if !ok {
			break
		}
		got = append(got, string(data))
	}

	if len(got) != len(want) {
		t.Fatalf("事件数 = %d（%q），期望 %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 个事件 = %q，期望 %q", i+1, got[i], want[i])
		}
	}
}

func TestSSEReader_HandlesCRLFAndEmptyInput(t *testing.T) {
	r := NewSSEReader(strings.NewReader("data: {\"a\":1}\r\n\r\n"))
	data, ok, err := r.Next()
	if err != nil || !ok {
		t.Fatalf("CRLF 分帧失败：ok=%v err=%v", ok, err)
	}
	if string(data) != `{"a":1}` {
		t.Errorf("事件 = %q，回车符不该混进载荷", data)
	}

	r = NewSSEReader(strings.NewReader(""))
	if data, ok, err := r.Next(); ok || err != nil {
		t.Errorf("空输入应直接结束：ok=%v data=%q err=%v", ok, data, err)
	}
}

// ============================================================ 调用拼装

func TestCallBuilder_AssemblesFragmentsByPosition(t *testing.T) {
	b := NewCallBuilder()
	// 参数分两片到达，中间没有任何分隔；标识与函数名只在第一片出现。
	b.Add(0, "call_1", "read", `{"pa`)
	b.Add(0, "", "", `th":"a.go"}`)
	// 第二个调用与前一个交错出现。
	b.Add(1, "call_2", "glob", `{"path":"**/*.go"}`)

	evs := b.Events()
	if len(evs) != 2 {
		t.Fatalf("事件数 = %d，期望 2", len(evs))
	}
	if evs[0].Kind != llm.EvToolUse || evs[0].Call.Name != "read" || evs[0].Call.ID != "call_1" {
		t.Errorf("第一个调用 = %+v", evs[0].Call)
	}
	if string(evs[0].Call.Arguments) != `{"path":"a.go"}` {
		t.Errorf("第一个参数 = %s，期望分片拼成完整 JSON", evs[0].Call.Arguments)
	}
	if evs[1].Call.Name != "glob" || string(evs[1].Call.Arguments) != `{"path":"**/*.go"}` {
		t.Errorf("第二个调用 = %+v", evs[1].Call)
	}
}

func TestCallBuilder_PrefersStreamedArguments(t *testing.T) {
	cases := []struct {
		name     string
		streamed bool
		full     string
		want     string
	}{
		{name: "只有流式分片", streamed: true, want: `{"path":"a.go"}`},
		{name: "只有完整值", full: `{"path":"a.go"}`, want: `{"path":"a.go"}`},
		{name: "两者都有（流式优先）", streamed: true, full: `{"path":"ignored"}`, want: `{"path":"a.go"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := NewCallBuilder()
			b.Add(0, "c1", "read", "")
			if tc.streamed {
				b.Add(0, "", "", `{"path":"a.go"}`)
			}
			if tc.full != "" {
				b.SetFull(0, tc.full)
			}
			evs := b.Events()
			if len(evs) != 1 {
				t.Fatalf("事件数 = %d", len(evs))
			}
			if string(evs[0].Call.Arguments) != tc.want {
				t.Errorf("参数 = %s，期望 %s", evs[0].Call.Arguments, tc.want)
			}
		})
	}
}

func TestCallBuilder_DoesNotInterpretArguments(t *testing.T) {
	// 参数不合法也照原样交出：协议层不认识工具集，解释是绑定层的事。
	b := NewCallBuilder()
	b.Add(0, "c1", "shell", `{"path":`)

	evs := b.Events()
	if len(evs) != 1 || evs[0].Err != nil {
		t.Fatalf("事件 = %+v，协议层不该对参数下判断", evs)
	}
	if evs[0].Call.Name != "shell" || string(evs[0].Call.Arguments) != `{"path":` {
		t.Errorf("原始调用 = %+v / %s", evs[0].Call, evs[0].Call.Arguments)
	}
}

func TestCallBuilder_Empty(t *testing.T) {
	if got := NewCallBuilder().Events(); len(got) != 0 {
		t.Errorf("空 builder 不该产出事件：%+v", got)
	}
	if got := NewCallBuilder().Len(); got != 0 {
		t.Errorf("Len = %d，期望 0", got)
	}
}
