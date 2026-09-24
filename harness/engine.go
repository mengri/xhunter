package harness

import (
	"context"
	"fmt"
	"time"

	"xhunter/llm"
)

// Config 是循环的运行参数。
type Config struct {
	MaxTurns      int // 轮次硬上限，兜住编排缺陷导致的死循环
	MaxFailStreak int // 连续多少轮工具失败即止损
	// StreamIdleTimeout 是**接收段的"不活动"超时**（FR-1.11①、架构 §6.3 L4）。读法：
	// **0 = 不限；非 0 = 不活动上限**——别读成"没配就是 120s"（默认由 withDefaults 给）。
	StreamIdleTimeout time.Duration
}

// DefaultConfig 返回与 withDefaults 之后**实际生效**同一份的默认配置。
//
// 装配层用它把机制侧硬顶（连续失败阈值 / 轮数硬顶）如实报进 config_snapshot——在 cmd 里另抄
// 一份 3/1000 迟早会与 withDefaults 漂开，而这两个数正是平台解释机制性终止所依据的。
func DefaultConfig() Config { return Config{}.withDefaults() }

func (c Config) withDefaults() Config {
	if c.MaxTurns <= 0 {
		c.MaxTurns = 1000
	}
	if c.MaxFailStreak <= 0 {
		c.MaxFailStreak = 3
	}
	// 文档写定的默认：不活动 120s。装配层不设时走这里。
	if c.StreamIdleTimeout <= 0 {
		c.StreamIdleTimeout = 120 * time.Second
	}
	return c
}

// Input 是循环的输入。
type Input struct {
	Meta map[string]any // 业务元数据，透传给 Run
}

// Run 是任务级状态，跨轮次存活。handler 读写它：Prepare 准备首轮消息与工具面，各阶段
// 写 Terminal 决定终态。任务本身（是什么、要做什么）不进这里——harness 不关心任务，
// 它只认「消息 + 工具面 + 轮次」。
type Run struct {
	// Messages 是下一次推理要发送的消息。业务在 handler 里维护它：Prepare 放首轮消息，
	// OnTurn 放下一轮的消息（本轮结果已在上下文里组装好）。harness 只原样交给模型。
	Messages []llm.Message
	Tools    []llm.ToolDecl
	Meta     map[string]any

	Usage    llm.Usage
	Terminal *Terminal
}

// Turn 是轮级状态：本轮发送的 + 本轮产生的。它只在轮边界存在——OnTurn 拿它处理成
// 下一轮的输入，然后丢弃。历史只住在业务（handler 的实现）里，不进这里。
type Turn struct {
	No       int              `json:"no"`       // 轮次号（1 起）
	Messages []llm.Message    `json:"messages"` // 本轮发送的（= 发送前的 Run.Messages）
	Text     string           `json:"text"`     // 本轮模型输出文本
	Calls    []llm.ToolCall   `json:"calls"`    // 本轮工具调用
	Results  []llm.ToolResult `json:"results"`  // 本轮工具结果（业务执行后填）
	Failed   bool             `json:"failed"`   // 本轮是否有失败结果（业务执行后填）
}

// PrepareHandler 在循环前执行一次。
type PrepareHandler func(ctx context.Context, run *Run) error

// OnTurnHandler 在每个轮边界执行：拿本轮结果（含模型输出与工具调用），由业务执行工具、
// 记录、组装下一轮消息、做守卫；返回 false 停止循环。
type OnTurnHandler func(ctx context.Context, run *Run, turn *Turn) (bool, error)

// FinalHandler 在循环后执行一次（无论成败）。
type FinalHandler func(ctx context.Context, run *Run) error

// SetTerminal 覆盖终态（供 handler 在收尾阶段用，如「全程无产出 → 失败」）。
func (r *Run) SetTerminal(t Terminal) { r.Terminal = &t }

// terminate 设置首个终态，后续不覆盖。机制性终止用它，避免「先超预算、后踩止损」
// 这类叠加情形让退出码取决于执行顺序而非事实本身。
func (r *Run) terminate(status Status, reason string, code ExitCode) {
	if r.Terminal == nil {
		r.Terminal = &Terminal{Status: status, Reason: reason, Code: code}
	}
}

// Outcome 组装对外结果。
func (r *Run) Outcome() Outcome {
	t := r.Terminal
	if t == nil {
		t = &Terminal{StatusFailed, "no_terminal", ExitEnv}
	}
	return Outcome{Status: t.Status, Reason: t.Reason, ExitCode: t.Code, Usage: r.Usage}
}

// Engine 驱动一次「模型调用循环」。
type Engine struct {
	provider llm.Provider
	prepare  []PrepareHandler
	onTurn   []OnTurnHandler
	final    []FinalHandler
	cfg      Config
}

// New 是唯一构造入口：只要模型与三组 handler。缺模型即装配错误，handler 可各为空。
func New(provider llm.Provider, prepare []PrepareHandler, onTurn []OnTurnHandler, final []FinalHandler) (*Engine, error) {
	if provider == nil {
		return nil, fmt.Errorf("缺少模型：Provider 不能为空")
	}
	return &Engine{
		provider: provider,
		prepare:  prepare,
		onTurn:   onTurn,
		final:    final,
		cfg:      Config{}.withDefaults(),
	}, nil
}

// WithConfig 覆盖循环参数（默认上限已够用，主要给测试收窄）。
func (e *Engine) WithConfig(cfg Config) *Engine {
	e.cfg = cfg.withDefaults()
	return e
}

// Run 执行一次循环：
//
//	Prepare → [Infer → Receive → OnTurn] → Final
//
// 任何分支都收敛到显式终态。失败也照常进入 Final——这是结构保证的，不依赖每个
// return 处小心处理。
func (e *Engine) Run(ctx context.Context, in Input) (out Outcome) {
	run := &Run{Meta: in.Meta}
	started := time.Now()
	turns := 0

	// 收尾放在 defer 里：它要在**每条退出路径**上都跑，包括 panic。业务把交付提交、
	// 材料落盘、终态上报都挂在收尾上，少跑一次就等于"有结论没发出"。
	defer func() {
		if r := recover(); r != nil {
			// panic 收敛为环境问题：任务本身不该因未捕获的 panic 崩掉进程。
			run.terminate(StatusFailed, fmt.Sprintf("panic: %v", r), ExitEnv)
		}
		// 轮数与耗时由循环自己统计：协议实现只知道 token 两项，而结果文件与预算
		// 都要这两个数——放在 defer 里才覆盖得到每条退出路径。
		run.Usage.Turns = turns
		run.Usage.Elapsed = time.Since(started)
		e.finalize(ctx, run)
		// 终态在收尾之后再取：收尾可能用 SetTerminal 覆盖它（如"全程无产出 → 失败"）。
		// 循环**只有**这一个返回值：每条路径都收敛到显式终态（panic 也收敛），
		// 因此不存在"有错误没有终态"的情形，也就没有 error 可返回。
		out = run.Outcome()
	}()

	for _, h := range e.prepare {
		if err := h(ctx, run); err != nil {
			run.terminate(StatusFailed, "prepare_failed: "+err.Error(), ExitEnv)
			break
		}
	}

	failStreak := 0
	for n := 1; run.Terminal == nil && n <= e.cfg.MaxTurns; n++ {
		turns = n
		if ctx.Err() != nil {
			run.terminate(StatusCancelled, "cancelled", ExitCancelled)
			break
		}

		sess, err := e.provider.Infer(ctx, llm.Request{
			Turn:     n,
			Messages: run.Messages,
			Tools:    run.Tools,
		})
		if err != nil {
			// 取消优先于任何失败归因：请求在途中被取消时，各协议实现都会返回一个
			// "请求被取消"的错误——若按推理失败处理，一次主动取消会被判成环境问题
			// （退出 1），与"取消收敛为 cancelled/3"直接冲突。
			if ctx.Err() != nil {
				run.terminate(StatusCancelled, "cancelled", ExitCancelled)
			} else {
				run.terminate(StatusFailed, "infer_failed: "+err.Error(), ExitEnv)
			}
			break
		}

		turn := &Turn{No: n, Messages: run.Messages}
	stream:
		for ev := range sess.Events() {
			// 流看门狗（FR-1.11①、架构 §6.3 L4）的**接入点**在这里：不活动超时 → `sess.Cancel()`
			// → 环境错误（退出 1）。计时器在 MS-12 落——本次只留接入点与口径，不实现 select 计时器。
			if ctx.Err() != nil {
				_ = sess.Cancel()
				run.terminate(StatusCancelled, "cancelled", ExitCancelled)
				break stream
			}
			switch ev.Kind {
			case llm.EvText:
				turn.Text += ev.Text
			case llm.EvToolUse:
				turn.Calls = append(turn.Calls, ev.Call)
			case llm.EvUsage:
				run.Usage.InputTokens += ev.Usage.InputTokens
				run.Usage.OutputTokens += ev.Usage.OutputTokens
				run.Usage.CachedInputTokens += ev.Usage.CachedInputTokens
			case llm.EvError:
				kind := "stream_error"
				if ev.Err != nil {
					kind = ev.Err.Kind
				}
				run.terminate(StatusFailed, "stream_error: "+kind, ExitEnv)
				break stream
			}
		}
		if run.Terminal != nil {
			break
		}

		// 轮边界：交给业务——执行工具、记录、组装下一轮消息、守卫。
		cont := true
		for _, h := range e.onTurn {
			c, err := h(ctx, run, turn)
			if err != nil {
				run.terminate(StatusFailed, "on_turn: "+err.Error(), ExitAborted)
				break
			}
			if !c {
				cont = false
				break
			}
		}
		if run.Terminal != nil {
			break
		}
		if !cont {
			run.terminate(StatusFailed, "hook_stopped", ExitAborted)
			break
		}

		// 止损：连续失败累积到阈值即停。
		if turn.Failed {
			failStreak++
			if failStreak >= e.cfg.MaxFailStreak {
				run.terminate(StatusFailed, fmt.Sprintf("stop_loss: 连续 %d 轮工具失败", failStreak), ExitAborted)
				break
			}
		} else {
			failStreak = 0
		}

		// 本轮没有发起任何工具调用，说明模型认为做完了。
		if len(turn.Calls) == 0 {
			run.terminate(StatusSucceeded, "no_tool_call", ExitOK)
			break
		}
	}

	if run.Terminal == nil {
		run.terminate(StatusFailed, "turn_limit_exceeded", ExitAborted)
	}

	// 裸 return：终态由函数开头的 defer 统一取（见那里的说明），这一行不自己再取一次。
	return
}

// finalize 跑收尾 handler。它自己再炸一次也不让进程崩掉——已经走到最后一步了，
// 崩在这里等于把「一定有结论发出」这条保证丢掉；终态由调用方在本函数之后取。
func (e *Engine) finalize(ctx context.Context, run *Run) {
	defer func() { _ = recover() }()
	for _, h := range e.final {
		_ = h(ctx, run)
	}
}
