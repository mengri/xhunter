package harness

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Config 是引擎的运行参数。
type Config struct {
	MaxDeniedStreak int // 连续被策略拒绝多少轮算异常
	MaxFailStreak   int // 连续多少轮工具失败即止损
	MaxTurnsHard    int // 轮次硬上限，兜住编排缺陷导致的死循环
}

func (c Config) withDefaults() Config {
	if c.MaxDeniedStreak <= 0 {
		c.MaxDeniedStreak = 5
	}
	if c.MaxFailStreak <= 0 {
		c.MaxFailStreak = 3
	}
	if c.MaxTurnsHard <= 0 {
		c.MaxTurnsHard = 1000
	}
	return c
}

// Deps 是引擎的全部协作者。
//
// 注意这里没有工作区：工作区根要等基线准备好之后才存在。它是运行期产物，
// 不是装配期依赖——把它塞进构造参数，就只能在别处再开一个后门传进来。
type Deps struct {
	Provider Provider
	Context  ContextBuilder
	Tools    ToolRuntime
	Policy   Policy
	Sink     EventSink
	Session  SessionRecorder
	Ext      ExtHost
	Git      GitWorktree
	// Workspaces 把基线准备好的工作树根变成可读写的工作区。它是文件操作的装配点：
	// 内核只规定工作区长什么样，实现由装配层注入（本地文件系统只是其中一种）。
	Workspaces WorkspaceOpener
	// SystemPlugins / UserPlugins 是首轮两段正文的构造插件，按**给定顺序**拼接。
	// 这是扩展点：写什么、约定怎么发现、技能清单从哪来，全部由插件决定；
	// 内核只固定"两段的位置、取一次冻结、内核条款在末尾追加、不得越界"。
	//
	// 顺序即执行顺序：由装配层排定，因此"哪段在前"是可复现的装配事实。
	// 两段都为空表示不接——首轮就只剩内核条款与环境事实，会记一条 warn，
	// 生产装配应当接上，否则模型看不到任何项目约定。
	SystemPlugins []PromptPlugin
	UserPlugins   []PromptPlugin
}

// Validate 报告装配缺了哪些协作者，**一次说全**。
//
// 缺一项报一项会让装配变成打地鼠：补一个、再跑、再报下一个。而全部列出来，
// 装配的人一眼就知道还差几件事。
//
// 两条刻意的例外：Ext 可以缺席（符号路径不可用是合法的运行状态，工具面不变），
// 提示词插件可以给空清单（那会记一条告警，任务照常跑）。
func (d Deps) Validate() error {
	missing := make([]string, 0, 8)
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"Provider", d.Provider != nil},
		{"Context", d.Context != nil},
		{"Tools", d.Tools != nil},
		{"Policy", d.Policy != nil},
		{"Sink", d.Sink != nil},
		{"Session", d.Session != nil},
		{"Git", d.Git != nil},
		{"Workspaces", d.Workspaces != nil},
	} {
		if !c.ok {
			missing = append(missing, c.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("装配不完整：缺少协作者 %s", strings.Join(missing, "、"))
	}
	return nil
}

// Engine 驱动三条链：pre 一次、turn 每轮一次、post 一次。
type Engine struct {
	deps Deps
	cfg  Config
	pre  []Stage
	turn []Stage
	post []Stage
}

// New 是唯一构造入口：编排校验、链编译与协作者校验都在这里完成，
// 非法编排或缺失实现都不可能活到运行期。
func New(deps Deps, cfg Config, p Pipeline, reg map[StageID]HandlerFunc) (*Engine, error) {
	if err := deps.Validate(); err != nil {
		return nil, err
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	pl := p.Normalize()

	pre, err := build(pl.Pre, reg)
	if err != nil {
		return nil, err
	}
	guards, err := build(pl.Turn.Guards, reg)
	if err != nil {
		return nil, err
	}
	steps, err := build(pl.Turn.Steps, reg)
	if err != nil {
		return nil, err
	}
	post, err := build(pl.Post, reg)
	if err != nil {
		return nil, err
	}

	return &Engine{
		deps: deps,
		cfg:  cfg.withDefaults(),
		pre:  pre,
		// 每轮一条链：守卫与步骤拼接在一起。守卫命中即收敛，链自然停住，
		// 后面的步骤不会因为"守卫失败了但没通知"而照跑。
		turn: append(guards, steps...),
		post: post,
	}, nil
}

// Run 执行一次任务：pre → 轮循环 → post。任何分支都会收敛到显式终态。
func (e *Engine) Run(ctx context.Context, b Bounty) (Outcome, error) {
	h := &Hunt{Bounty: b, ledger: NewLedger(), started: time.Now()}

	// ① 初始化链，任务级一次。失败也照常进入收尾。
	e.drive(e.round(ctx, h, 0), e.pre)

	// ② 轮循环。每轮新建 Context——轮级状态因此天然不跨轮。
	// 用"记住哪些字段要清空"来维持边界是行不通的，那种纪律迟早会被打破，
	// 而打破的代价是上一轮待执行的调用被再来一遍（也就是重复写盘）。
	for turn := TurnNo(1); h.Terminal == nil && turn <= TurnNo(e.cfg.MaxTurnsHard); turn++ {
		// 检查点意图是"每轮至多一次"的旗标，轮边界复位；理由一并清掉——
		// 上一轮没被兑现的请求不该混进下一轮的提交信息（那会让 git 历史说谎）。
		h.checkpointRequested, h.checkpointSummary = false, ""
		c := e.round(ctx, h, turn)
		e.drive(c, e.turn)
		e.settle(c)
	}
	if h.Terminal == nil {
		h.Terminal = &Terminal{StatusFailed, "turn_limit_exceeded", ExitFailed}
	}

	// ③ 收尾链是另一条链，重置推进位置后照常执行。
	// 所以"失败也照常交付"不需要在每个失败分支里小心维护，它是结构决定的。
	pc := e.round(ctx, h, 0)
	e.drive(pc, e.post)
	return pc.outcome(), nil
}

func (e *Engine) round(ctx context.Context, h *Hunt, turn TurnNo) *Context {
	return &Context{Ctx: ctx, Hunt: h, Deps: e.deps, Cfg: e.cfg, TurnNo: turn}
}

// drive 执行一条链，并把任何 panic 收敛为环境问题。
//
// 恢复放在驱动层而不是做成一个环节，有两个原因：一是"panic 之后仍要走收尾"
// 属于引擎的知识，不是某个环节的知识；二是放在这里不会参与位置约束，
// 也就不会让环节清单表达的时间顺序变得不可信。
func (e *Engine) drive(c *Context, chain []Stage) {
	defer func() {
		if r := recover(); r != nil {
			c.Terminate(StatusFailed, fmt.Sprintf("panic: %v", r), ExitEnv)
		}
	}()
	c.handlers, c.index = chain, -1
	c.next()
}

// settle 是轮边界收敛，刻意不占环节。
//
// 判断标准是"输入是不是整轮的结果"：止损看的是这一轮累积的失败，
// 终止判定看的是这一轮有没有发起调用——它们都不属于任何单个环节的视角。
// 硬做成环节只会得到一堆"必须排在最后"的约束，而约束本身没有表达力。
func (e *Engine) settle(c *Context) {
	h := c.Hunt
	if h.Terminal != nil {
		return
	}

	// 事件通道断裂：这是对端已经不在的信号，继续跑只会生产无人接收的进度。
	if h.WriteErr != nil {
		c.Terminate(StatusFailed, "channel_write_failed: "+h.WriteErr.Error(), ExitEnv)
		return
	}

	// 止损：连续失败累积到阈值即停。
	if c.Failed {
		h.FailStreak++
		if h.FailStreak >= e.cfg.MaxFailStreak {
			c.Terminate(StatusFailed, fmt.Sprintf("stop_loss: 连续 %d 轮工具失败", h.FailStreak), ExitFailed)
			return
		}
	} else if len(c.Results) > 0 {
		h.FailStreak = 0
	}

	// 终止判定：本轮没有发起任何调用，说明模型认为做完了。
	//
	// 但"没有调用"不等于"有产出"。一次调用都没发过、文件一个没改，
	// 却报成功，会让下游拿到一个空交付物——所以没有写操作时判失败。
	// 本轮有失败结果（例如模型发出的调用形状不成立）时同样不算做完：
	// 它还需要一轮机会来换做法。
	if len(c.Pending) == 0 && !c.Failed {
		if len(e.deps.Session.Ops()) == 0 {
			c.Terminate(StatusFailed, "no_output: 本轮无调用且全程无任何写操作", ExitFailed)
			return
		}
		c.Terminate(StatusSucceeded, "no_tool_call", ExitOK)
	}
}
