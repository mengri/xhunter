package harness

import (
	"context"
	"sync"
	"testing"
	"time"

	"xhunter/llm"
)

// 接收段看门狗：相邻两事件之间（含首个事件之前）允许的最长静默。本文件用可精确编排事件时序的
// 假流来钉住口径——默认值、关闭、活动重置、取消优先，以及"超时绝不落进 no_tool_call"。

// timedEvent 是脚本里的一条事件：先等 after，再发 ev。
type timedEvent struct {
	after time.Duration
	ev    llm.Event
}

// stagedSession 是一段受测试控制的流：事件由后台 goroutine 按脚本推进，Cancel 关闭 cancelC。
type stagedSession struct {
	ch      chan llm.Event
	cancelC chan struct{}
	once    sync.Once
}

func (s *stagedSession) Events() <-chan llm.Event { return s.ch }

func (s *stagedSession) Cancel() error {
	s.once.Do(func() { close(s.cancelC) })
	return nil
}

func (s *stagedSession) cancelled() bool {
	select {
	case <-s.cancelC:
		return true
	default:
		return false
	}
}

// stagedProvider 返回一段按脚本推进的流。hold = true 时脚本走完也不关流（模拟上游挂起，只有
// 看门狗能把它停住）；hold = false 时脚本走完即关流（接收段正常结束）。
type stagedProvider struct {
	events []timedEvent
	hold   bool
	sess   *stagedSession
}

func (p *stagedProvider) Capabilities() llm.Caps { return llm.Caps{} }

func (p *stagedProvider) Infer(ctx context.Context, _ llm.Request) (llm.Session, error) {
	s := &stagedSession{ch: make(chan llm.Event, 16), cancelC: make(chan struct{})}
	p.sess = s
	go func() {
		if !p.hold {
			defer close(s.ch)
		}
		for _, te := range p.events {
			select {
			case <-time.After(te.after):
			case <-s.cancelC:
				return
			case <-ctx.Done():
				return
			}
			select {
			case s.ch <- te.ev:
			case <-s.cancelC:
				return
			case <-ctx.Done():
				return
			}
		}
		if p.hold {
			select {
			case <-s.cancelC:
			case <-ctx.Done():
			}
		}
	}()
	return s, nil
}

// 默认值的唯一来源：装配层不设（0）即 120s。
func TestDefaultConfig_StreamIdleTimeoutDefaultsTo120s(t *testing.T) {
	if got := DefaultConfig().StreamIdleTimeout; got != 120*time.Second {
		t.Errorf("DefaultConfig().StreamIdleTimeout = %v，期望字面量 120s", got)
	}
}

// 0 = 取默认；负数原样保留 = 关闭看门狗（"不配"与"关掉"是两件事）；正数不被改写。
func TestConfig_ZeroIdleTimeoutTakesDefaultNegativeStays(t *testing.T) {
	if got := (Config{}).withDefaults().StreamIdleTimeout; got != 120*time.Second {
		t.Errorf("0 应取默认 120s，实得 %v", got)
	}
	if got := (Config{StreamIdleTimeout: -time.Second}).withDefaults().StreamIdleTimeout; got != -time.Second {
		t.Errorf("负数应原样保留（关闭看门狗），实得 %v", got)
	}
	if got := (Config{StreamIdleTimeout: 45 * time.Second}).withDefaults().StreamIdleTimeout; got != 45*time.Second {
		t.Errorf("正数不应被改写，实得 %v", got)
	}
}

// 接收段一直静默（上游挂起）→ 看门狗判超时：环境错误（退出 1），且对上游调用 Cancel。
func TestWatchdog_IdleStreamCancelsAsEnvError(t *testing.T) {
	p := &stagedProvider{hold: true}
	eng, err := New(p, nil, nil, nil)
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}
	eng.WithConfig(Config{StreamIdleTimeout: 30 * time.Millisecond})

	out := eng.Run(context.Background(), Input{})
	if out.Status != StatusFailed || out.ExitCode != ExitEnv {
		t.Fatalf("静默流应收敛为环境错误：%+v", out)
	}
	if out.Reason != "stream_idle_timeout" {
		t.Errorf("reason = %q，期望字面量 stream_idle_timeout", out.Reason)
	}
	if p.sess == nil || !p.sess.cancelled() {
		t.Error("超时后必须取消上游（sess.Cancel）")
	}
}

// 超时的终态绝不是"本轮无工具调用"：先落终态、再取消的顺序使然（结构保证，不靠记得写终态）。
func TestWatchdog_TimeoutCannotBeNoToolCall(t *testing.T) {
	p := &stagedProvider{hold: true}
	eng, _ := New(p, nil, nil, nil)
	eng.WithConfig(Config{StreamIdleTimeout: 30 * time.Millisecond})

	out := eng.Run(context.Background(), Input{})
	if out.Reason == "no_tool_call" || out.Status == StatusSucceeded {
		t.Fatalf("超时不得落进 no_tool_call/成功：%+v", out)
	}
	if out.Reason != "stream_idle_timeout" {
		t.Errorf("reason = %q，期望 stream_idle_timeout", out.Reason)
	}
}

// **任何**流内事件都算一次活动（不限于文本）：相邻间隔都小于上界、但总时长超过上界的流，
// 不该被判超时。文本与**非文本**（用量）两种事件都覆盖——把"活动"缩成只在文本事件上重置，
// 用量事件那条就会超时变红。
func TestWatchdog_ActivityResetsTimer(t *testing.T) {
	steps := func(ev llm.Event) []timedEvent {
		var out []timedEvent
		for i := 0; i < 40; i++ {
			out = append(out, timedEvent{after: 10 * time.Millisecond, ev: ev})
		}
		return out
	}
	cases := map[string]llm.Event{
		"文本事件":      {Kind: llm.EvText, Text: "x"},
		"非文本事件（用量）": {Kind: llm.EvUsage, Usage: llm.Usage{InputTokens: 1, OutputTokens: 1}},
	}
	for name, ev := range cases {
		t.Run(name, func(t *testing.T) {
			p := &stagedProvider{events: steps(ev)}
			eng, _ := New(p, nil, nil, nil)
			// 上界 200ms：单个间隔（10ms）是它的 1/20，而总时长（约 400ms）大于它——单次事件被
			// 拖延 >200ms 才会误判（余量 20×）；计时器不重置仍会被抓。
			eng.WithConfig(Config{StreamIdleTimeout: 200 * time.Millisecond})

			out := eng.Run(context.Background(), Input{})
			if out.Status != StatusSucceeded || out.Reason != "no_tool_call" {
				t.Fatalf("有活动的流应正常结束（no_tool_call）：%+v", out)
			}
			if p.sess == nil || p.sess.cancelled() {
				t.Error("正常结束不该取消上游")
			}
		})
	}
}

// 首次事件之前的等待也算不活动，且计时器从 `receiveTurn` 入口就起算（不是"来了第一个事件才
// 起算"）：首个事件故意晚到 500ms，30ms 的上界必须在它之前就收敛——所以 Run 必须**很快**返回，
// 而不是等到 500ms 收到事件后才开始计时。把计时器挪到首事件之后，elapsed 会逼近 500ms 而变红。
func TestWatchdog_WaitsForFirstEvent(t *testing.T) {
	p := &stagedProvider{hold: true, events: []timedEvent{
		{after: 500 * time.Millisecond, ev: llm.Event{Kind: llm.EvText, Text: "迟到"}},
	}}
	eng, _ := New(p, nil, nil, nil)
	eng.WithConfig(Config{StreamIdleTimeout: 30 * time.Millisecond})

	start := time.Now()
	out := eng.Run(context.Background(), Input{})
	elapsed := time.Since(start)

	if out.Reason != "stream_idle_timeout" {
		t.Fatalf("首事件之前的静默也计时：%+v", out)
	}
	// 30ms 上界：正常约 30ms 返回；若计时器推迟到首事件之后，则要等到约 500ms。留 10× 余量。
	if elapsed > 300*time.Millisecond {
		t.Errorf("Run 花了 %v 才返回——计时器没有从接收段入口起算（首事件之前的静默未被计时）", elapsed)
	}
}

// 负数 = 关闭看门狗：同一段"静默 150ms 再关流"的流，小上界会判超时，负数则不判。
func TestWatchdog_DisabledWhenNegative(t *testing.T) {
	steps := []timedEvent{{after: 150 * time.Millisecond, ev: llm.Event{Kind: llm.EvText, Text: "慢"}}}

	t.Run("小上界会判超时", func(t *testing.T) {
		p := &stagedProvider{events: steps}
		eng, _ := New(p, nil, nil, nil)
		eng.WithConfig(Config{StreamIdleTimeout: 30 * time.Millisecond})
		if out := eng.Run(context.Background(), Input{}); out.Reason != "stream_idle_timeout" {
			t.Fatalf("小上界应收敛超时：%+v", out)
		}
	})
	t.Run("负数关闭看门狗不判超时", func(t *testing.T) {
		p := &stagedProvider{events: steps}
		eng, _ := New(p, nil, nil, nil)
		eng.WithConfig(Config{StreamIdleTimeout: -time.Second})
		if out := eng.Run(context.Background(), Input{}); out.Status != StatusSucceeded {
			t.Fatalf("关闭看门狗后应正常结束：%+v", out)
		}
	})
}

// 取消优先于超时：ctx 已取消、计时器又同时可能就绪时，终态必须是 cancelled，不是环境错误。
// 直接反复触发"两路同时就绪"——若少了超时分支里的 ctx 复查，这里必然变红。
func TestWatchdog_CancelOutranksTimeout(t *testing.T) {
	for i := 0; i < 50; i++ {
		eng, _ := New(&stagedProvider{}, nil, nil, nil)
		eng.WithConfig(Config{StreamIdleTimeout: time.Nanosecond}) // 计时器几乎立刻就绪

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // 与计时器同时就绪

		sess := &stagedSession{ch: make(chan llm.Event), cancelC: make(chan struct{})}
		run := &Run{}
		eng.receiveTurn(ctx, run, &Turn{No: 1}, sess)

		if run.Terminal == nil || run.Terminal.Status != StatusCancelled || run.Terminal.Code != ExitCancelled {
			t.Fatalf("第 %d 次：取消必须优先于超时，实得 %+v", i, run.Terminal)
		}
		if !sess.cancelled() {
			t.Fatalf("第 %d 次：取消路径也要 Cancel 上游", i)
		}
	}
}

// 超时在轮边界之前就收敛：OnTurn 一次都不该被调用。
func TestWatchdog_TimeoutStopsBeforeOnTurn(t *testing.T) {
	p := &stagedProvider{hold: true}
	onTurn := 0
	eng, _ := New(p, nil, []OnTurnHandler{func(context.Context, *Run, *Turn) (bool, error) {
		onTurn++
		return true, nil
	}}, nil)
	eng.WithConfig(Config{StreamIdleTimeout: 30 * time.Millisecond})

	out := eng.Run(context.Background(), Input{})
	if out.Reason != "stream_idle_timeout" {
		t.Fatalf("应超时收敛：%+v", out)
	}
	if onTurn != 0 {
		t.Errorf("超时不得进入轮边界，OnTurn 被调用 %d 次", onTurn)
	}
}
