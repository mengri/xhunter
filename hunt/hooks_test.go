package hunt

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"xhunter/harness"
	"xhunter/llm"
)

// 记录桩：只关心「本轮是什么时候、以什么内容被记下来的」。
type recordingContext struct{ appended []harness.Turn }

func (c *recordingContext) SetPrompt([]llm.Message) {}
func (c *recordingContext) Assemble() []llm.Message { return nil }
func (c *recordingContext) Append(rec harness.Turn) { c.appended = append(c.appended, rec) }

type recordingSession struct{ turns []harness.Turn }

func (r *recordingSession) RecordTurn(rec harness.Turn) { r.turns = append(r.turns, rec) }
func (r *recordingSession) Snapshot() error             { return nil }

// 加工必须发生在落历史之前：历史与材料都是值拷贝，抢在后面改的过滤器等于白做——
// 模型下一轮看不到改动，而过滤器作者会以为生效了。
func TestOnTurn_FiltersRunBeforeRecording(t *testing.T) {
	ctxLog := &recordingContext{}
	rec := &recordingSession{}
	s := NewSession(Config{
		Context: ctxLog,
		Session: rec,
		Filters: []ResultFilter{
			func(_ context.Context, turn *harness.Turn) error { turn.Text += "-甲"; return nil },
			func(_ context.Context, turn *harness.Turn) error { turn.Text += "-乙"; return nil },
		},
	})

	turn := &harness.Turn{No: 1, Text: "看见了"}
	cont, err := s.OnTurn(context.Background(), &harness.Run{}, turn)
	if err != nil || !cont {
		t.Fatalf("OnTurn = (%v, %v)，期望 (true, nil)", cont, err)
	}

	if turn.Text != "看见了-甲-乙" {
		t.Errorf("过滤器必须按装配顺序生效：%q", turn.Text)
	}
	if len(ctxLog.appended) != 1 || ctxLog.appended[0].Text != "看见了-甲-乙" {
		t.Errorf("落历史的必须是加工后的内容：%+v", ctxLog.appended)
	}
	if len(rec.turns) != 1 || rec.turns[0].Text != "看见了-甲-乙" {
		t.Errorf("会话材料必须是加工后的内容：%+v", rec.turns)
	}
}

// 过滤器失败不得静默放行：脱敏、截断一类过滤器挂掉时静默继续，正好把它们要防的内容
// 原样送进下一轮上下文。
func TestOnTurn_FilterFailureStopsTurn(t *testing.T) {
	ctxLog := &recordingContext{}
	s := NewSession(Config{
		Context: ctxLog,
		Filters: []ResultFilter{
			func(_ context.Context, _ *harness.Turn) error { return errors.New("脱敏失败") },
		},
	})

	cont, err := s.OnTurn(context.Background(), &harness.Run{}, &harness.Turn{No: 1, Text: "原始内容"})
	if err == nil {
		t.Fatal("过滤器失败必须上抛给循环")
	}
	if cont {
		t.Error("失败时不该返回「继续」")
	}
	if len(ctxLog.appended) != 0 {
		t.Errorf("失败的这一轮不该落进历史：%+v", ctxLog.appended)
	}
}

// 已答复的调用不再重复执行：排在 Session.OnTurn 前面的 handler 可以直接答复某次调用
// （缓存命中、一眼可见的非法规格），这条通道不成立的话「短路」就只是句空话。
func TestOnTurn_AnsweredCallIsNotReExecuted(t *testing.T) {
	s := NewSession(Config{}) // 刻意不装工具：一旦被真的执行，就会多出一条「没有这个工具」的结果
	turn := &harness.Turn{
		No:      1,
		Calls:   []llm.ToolCall{{ID: "c1", Name: "read", Arguments: json.RawMessage(`{"path":"a.go"}`)}},
		Results: []llm.ToolResult{{CallID: "c1", Output: "缓存命中"}},
	}

	if _, err := s.OnTurn(context.Background(), &harness.Run{}, turn); err != nil {
		t.Fatalf("OnTurn 失败：%v", err)
	}
	if len(turn.Results) != 1 || turn.Results[0].Output != "缓存命中" {
		t.Errorf("已答复的调用不该被重复执行：%+v", turn.Results)
	}
	if turn.Failed {
		t.Errorf("已答复的调用不该被记成失败：%+v", turn.Results)
	}
}

// 首轮两段正文的摆法与组装：system 在前、user 在后，空段不占位置。
func TestFirstPrompt_KeepsOrderAndSkipsEmpty(t *testing.T) {
	if got := firstPrompt("S", "U"); len(got) != 2 || got[0].Role != llm.RoleSystem || got[1].Role != llm.RoleUser {
		t.Errorf("两段都在时必须 system 在前 user 在后：%+v", got)
	}
	if got := firstPrompt("S", "  \n "); len(got) != 1 || got[0].Role != llm.RoleSystem {
		t.Errorf("空段不该占位置：%+v", got)
	}
	if got := firstPrompt("", ""); len(got) != 0 {
		t.Errorf("两段都空时不产出消息：%+v", got)
	}
}

// 净化是格式不可伪造的保证：只取首行、剥控制与格式字符、折叠空白、按上限截断。
func TestSanitizeIntent(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"只取首行", "第一行\n第二行", "第一行"},
		{"剥零宽字符", "a\u200bb", "ab"},
		{"折叠空白", "  a\t\t b  ", "a b"},
		{"空内容即无理由", "\n", ""},
	}
	for _, c := range cases {
		if got := sanitizeIntent(c.in); got != c.want {
			t.Errorf("%s：sanitizeIntent(%q) = %q，期望 %q", c.name, c.in, got, c.want)
		}
	}

	long := make([]rune, maxIntentRunes*2)
	for i := range long {
		long[i] = 'x'
	}
	got := sanitizeIntent(string(long))
	if runes := []rune(got); len(runes) != maxIntentRunes+1 || runes[maxIntentRunes] != '…' {
		t.Errorf("超长必须截断到 %d 字并加省略号，实得 %d 字", maxIntentRunes, len(runes))
	}
}
