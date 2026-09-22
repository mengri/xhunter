package harness

import (
	"context"
	"errors"
	"strings"
	"testing"

	"xhunter/llm"
)

// scriptedProvider 按轮次回放：每轮给一段正文、若干工具调用、一个可选流错误。
// 引擎不认识工具，所以"有工具调用"在它眼里只是 turn.Calls 非空——这正是要钉住的边界。
type scriptedProvider struct {
	turns []scriptedTurn
	calls int
}

type scriptedTurn struct {
	text  string
	calls []llm.ToolCall
	fault *llm.Fault
	err   error
}

func (p *scriptedProvider) Capabilities() llm.Caps { return llm.Caps{} }

func (p *scriptedProvider) Infer(_ context.Context, _ llm.Request) (llm.Session, error) {
	p.calls++
	if p.calls > len(p.turns) {
		return nil, errors.New("脚本已用尽：引擎发起了超出预期的推理")
	}
	t := p.turns[p.calls-1]
	if t.err != nil {
		return nil, t.err
	}

	ch := make(chan llm.Event, len(t.calls)+3)
	if t.text != "" {
		ch <- llm.Event{Kind: llm.EvText, Text: t.text}
	}
	for _, c := range t.calls {
		ch <- llm.Event{Kind: llm.EvToolUse, Call: c}
	}
	ch <- llm.Event{Kind: llm.EvUsage, Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}}
	if t.fault != nil {
		ch <- llm.Event{Kind: llm.EvError, Err: t.fault}
	}
	close(ch)
	return &stubSession{ch: ch}, nil
}

// stubSession 是一段「已经跑完的流」：事件一次性入队后关闭。
type stubSession struct {
	ch        chan llm.Event
	cancelled bool
}

func (s *stubSession) Events() <-chan llm.Event { return s.ch }
func (s *stubSession) Cancel() error            { s.cancelled = true; return nil }

func aCall(id string) llm.ToolCall { return llm.ToolCall{ID: id, Name: "read"} }

// 构造期缺模型是装配错误：不该等到第一次推理才发现。
func TestNew_RequiresProvider(t *testing.T) {
	if _, err := New(nil, nil, nil, nil); err == nil {
		t.Fatal("缺 provider 必须构造失败")
	}
}

// 初始化失败即终止：**一次推理都不发起**，且收尾照样跑。
func TestEngine_PrepareFailureStopsBeforeInference(t *testing.T) {
	p := &scriptedProvider{}
	finals := 0
	eng, err := New(p,
		[]PrepareHandler{func(context.Context, *Run) error { return errors.New("工作区打不开") }},
		nil,
		[]FinalHandler{func(context.Context, *Run) error { finals++; return nil }},
	)
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}

	out := eng.Run(context.Background(), Input{})
	if out.Status != StatusFailed || out.ExitCode != ExitEnv {
		t.Errorf("初始化失败应收敛为环境错误：%+v", out)
	}
	if !strings.Contains(out.Reason, "prepare_failed") {
		t.Errorf("原因应指明是初始化失败：%q", out.Reason)
	}
	if p.calls != 0 {
		t.Errorf("初始化失败不得发起推理，实际 %d 次", p.calls)
	}
	if finals != 1 {
		t.Errorf("收尾必须跑一次，实际 %d 次", finals)
	}
}

// 本轮无工具调用 = 模型认为做完了：成功路径，且 handler 能拿到整轮内容。
func TestEngine_NoToolCallSucceedsAndCollectsTurn(t *testing.T) {
	p := &scriptedProvider{turns: []scriptedTurn{{text: "我先看一眼"}}}
	var seen *Turn
	eng, _ := New(p,
		[]PrepareHandler{func(_ context.Context, run *Run) error {
			run.Messages = []llm.Message{{Role: llm.RoleUser, Content: "任务"}}
			return nil
		}},
		[]OnTurnHandler{func(_ context.Context, _ *Run, turn *Turn) (bool, error) {
			seen = turn
			return true, nil
		}},
		nil,
	)

	out := eng.Run(context.Background(), Input{})
	if out.Status != StatusSucceeded || out.ExitCode != ExitOK {
		t.Fatalf("无工具调用应成功收敛：%+v", out)
	}
	if out.Reason != "no_tool_call" {
		t.Errorf("原因 = %q，期望 no_tool_call", out.Reason)
	}
	if out.Usage.InputTokens != 10 || out.Usage.OutputTokens != 5 {
		t.Errorf("用量应累计进 Outcome：%+v", out.Usage)
	}
	if seen == nil {
		t.Fatal("轮边界 handler 必须被调用")
	}
	if seen.No != 1 || seen.Text != "我先看一眼" {
		t.Errorf("本轮内容不对：%+v", seen)
	}
	if len(seen.Messages) != 1 || seen.Messages[0].Content != "任务" {
		t.Errorf("turn.Messages 应是「本轮发送的」：%+v", seen.Messages)
	}
}

// 轮数上限是机制性硬顶：兜住编排缺陷导致的死循环，与业务预算无关。
func TestEngine_TurnLimitIsMechanicalCap(t *testing.T) {
	p := &scriptedProvider{turns: []scriptedTurn{
		{calls: []llm.ToolCall{aCall("c1")}},
		{calls: []llm.ToolCall{aCall("c2")}},
		{calls: []llm.ToolCall{aCall("c3")}},
		{calls: []llm.ToolCall{aCall("c4")}},
	}}
	eng, _ := New(p, nil,
		[]OnTurnHandler{func(context.Context, *Run, *Turn) (bool, error) { return true, nil }}, nil)
	eng.WithConfig(Config{MaxTurns: 3, MaxFailStreak: 2})

	out := eng.Run(context.Background(), Input{})
	if out.Status != StatusFailed || out.ExitCode != ExitFailed {
		t.Fatalf("达轮数上限应失败：%+v", out)
	}
	if out.Reason != "turn_limit_exceeded" {
		t.Errorf("原因 = %q，期望 turn_limit_exceeded", out.Reason)
	}
	if p.calls != 3 {
		t.Errorf("推理次数 = %d，期望恰好等于上限 3", p.calls)
	}
}

// 连续失败止损：不管原因，只在同一堵墙前反复撞时抽身。
func TestEngine_FailStreakStopsLoss(t *testing.T) {
	p := &scriptedProvider{turns: []scriptedTurn{
		{calls: []llm.ToolCall{aCall("c1")}},
		{calls: []llm.ToolCall{aCall("c2")}},
		{calls: []llm.ToolCall{aCall("c3")}},
	}}
	eng, _ := New(p, nil, []OnTurnHandler{func(_ context.Context, _ *Run, turn *Turn) (bool, error) {
		turn.Failed = true
		return true, nil
	}}, nil)
	eng.WithConfig(Config{MaxTurns: 10, MaxFailStreak: 2})

	out := eng.Run(context.Background(), Input{})
	if out.Status != StatusFailed {
		t.Fatalf("连续失败应止损：%+v", out)
	}
	if !strings.Contains(out.Reason, "stop_loss") {
		t.Errorf("原因应标明止损：%q", out.Reason)
	}
	if p.calls != 2 {
		t.Errorf("推理次数 = %d，期望 2（阈值）", p.calls)
	}
}

// handler 自己置的终态优先于「返回 false 即失败」这条兜底：业务可比引擎说得更准。
func TestEngine_HandlerTerminalWinsOverStop(t *testing.T) {
	p := &scriptedProvider{turns: []scriptedTurn{{calls: []llm.ToolCall{aCall("c1")}}}}
	eng, _ := New(p, nil, []OnTurnHandler{func(_ context.Context, run *Run, _ *Turn) (bool, error) {
		run.SetTerminal(Terminal{Status: StatusSucceeded, Reason: "业务自己收敛", Code: ExitOK})
		return false, nil
	}}, nil)

	out := eng.Run(context.Background(), Input{})
	if out.Status != StatusSucceeded || out.Reason != "业务自己收敛" {
		t.Errorf("handler 的终态应被保留：%+v", out)
	}
}

// handler 报错即失败，并带上是谁报的。
func TestEngine_OnTurnErrorIsFailed(t *testing.T) {
	p := &scriptedProvider{turns: []scriptedTurn{{calls: []llm.ToolCall{aCall("c1")}}}}
	eng, _ := New(p, nil, []OnTurnHandler{func(context.Context, *Run, *Turn) (bool, error) {
		return false, errors.New("渲染器崩了")
	}}, nil)

	out := eng.Run(context.Background(), Input{})
	if out.Status != StatusFailed || out.ExitCode != ExitFailed {
		t.Fatalf("轮边界报错应失败：%+v", out)
	}
	if !strings.Contains(out.Reason, "on_turn") || !strings.Contains(out.Reason, "渲染器崩了") {
		t.Errorf("原因应指明阶段与原因：%q", out.Reason)
	}
}

// 取消优先级最高，且取消时不发起新的推理。
func TestEngine_CancelledBeforeFirstTurn(t *testing.T) {
	p := &scriptedProvider{turns: []scriptedTurn{{text: "不该被调用"}}}
	eng, _ := New(p, nil, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := eng.Run(ctx, Input{})
	if out.Status != StatusCancelled || out.ExitCode != ExitCancelled {
		t.Fatalf("取消应收敛为 cancelled：%+v", out)
	}
	if p.calls != 0 {
		t.Errorf("取消后不得发起推理，实际 %d 次", p.calls)
	}
}

// 推理失败是环境问题（可重试），不是任务本身失败。
func TestEngine_InferFailureIsEnvError(t *testing.T) {
	p := &scriptedProvider{turns: []scriptedTurn{{err: errors.New("连接被重置")}}}
	eng, _ := New(p, nil, nil, nil)

	out := eng.Run(context.Background(), Input{})
	if out.Status != StatusFailed || out.ExitCode != ExitEnv {
		t.Fatalf("推理失败应判环境错误：%+v", out)
	}
	if !strings.Contains(out.Reason, "infer_failed") {
		t.Errorf("原因 = %q", out.Reason)
	}
}

// 流里的错误不得被当成正常结束。
func TestEngine_StreamErrorIsEnvError(t *testing.T) {
	p := &scriptedProvider{turns: []scriptedTurn{{fault: &llm.Fault{Kind: "stream_truncated", Message: "断了"}}}}
	onTurn := 0
	eng, _ := New(p, nil, []OnTurnHandler{func(context.Context, *Run, *Turn) (bool, error) {
		onTurn++
		return true, nil
	}}, nil)

	out := eng.Run(context.Background(), Input{})
	if out.Status != StatusFailed || out.ExitCode != ExitEnv {
		t.Fatalf("流错误应判环境错误：%+v", out)
	}
	if !strings.Contains(out.Reason, "stream_error") {
		t.Errorf("原因应带错误种类：%q", out.Reason)
	}
	if onTurn != 0 {
		t.Error("流中断时不该进入轮边界（那一轮没有完整结果）")
	}
}

// 推理在途中被取消：各协议实现都会回一个"请求被取消"的错误（HTTP 层如此），
// 但归因必须仍是 cancelled/3——否则一次主动取消会被判成"环境问题、可重派"，
// 平台会重跑一个用户刚取消的任务。
func TestEngine_CancelDuringInferIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := &cancelMidInferProvider{cancel: cancel}
	finals := 0
	eng, _ := New(p, nil, nil, []FinalHandler{func(context.Context, *Run) error { finals++; return nil }})

	out := eng.Run(ctx, Input{})
	if out.Status != StatusCancelled || out.ExitCode != ExitCancelled {
		t.Fatalf("推理途中取消应收敛为 cancelled/3：%+v", out)
	}
	if finals != 1 {
		t.Errorf("取消后收尾仍必须跑：%d", finals)
	}
}

// cancelMidInferProvider 在自己的 Infer 里触发取消，并像真实协议实现那样返回取消错误
// （HTTP 请求被 ctx 打断时就是这种形状）。
type cancelMidInferProvider struct{ cancel context.CancelFunc }

func (p *cancelMidInferProvider) Capabilities() llm.Caps { return llm.Caps{} }

func (p *cancelMidInferProvider) Infer(context.Context, llm.Request) (llm.Session, error) {
	p.cancel()
	return nil, errors.New("请求被取消：context canceled")
}

// panic 收敛为环境错误：不让进程带着半截状态崩掉，且收尾照跑。
func TestEngine_PanicBecomesEnvFailureAndFinalStillRuns(t *testing.T) {
	p := &scriptedProvider{turns: []scriptedTurn{{calls: []llm.ToolCall{aCall("c1")}}}}
	finals := 0
	eng, _ := New(p, nil, []OnTurnHandler{func(context.Context, *Run, *Turn) (bool, error) {
		panic("工具实现炸了")
	}}, []FinalHandler{func(context.Context, *Run) error { finals++; return nil }})

	out := eng.Run(context.Background(), Input{})
	if out.Status != StatusFailed || out.ExitCode != ExitEnv {
		t.Fatalf("panic 应收敛为环境错误：%+v", out)
	}
	if !strings.Contains(out.Reason, "panic") {
		t.Errorf("原因应标明 panic：%q", out.Reason)
	}
	if finals != 1 {
		t.Errorf("panic 之后收尾也必须跑：%d", finals)
	}
}
