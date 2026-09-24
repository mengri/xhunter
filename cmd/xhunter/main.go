// Command xhunter 是 Xhunter 的入口。
//
// 用法：
//
//	xhunter --bounty <path> [--log-file <path>] [--result <path>] [--patch <path>]
//	                                              执行一次 Hunt
//	xhunter run --repo <path> --task <text> [--out <path>] [--log-file|--result|--patch <path>]
//	                                              本地驱动：探测仓库 → 生成 Bounty → 执行一次 Hunt
//	xhunter version
//
// 退出码：0 = 模型正常完成对话（成败另看 status）；1 = 未进入对话 / 被上游与环境打断（环境问题，
// 修好可重试）；2 = 被引擎中止（预算耗尽、止损、轮数硬顶、引擎侧错误）；3 = 被取消。
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"xhunter/harness"
	"xhunter/hunt"
	"xhunter/hunt/gate"
	"xhunter/internal/gates"
	"xhunter/providerconfig"
	"xhunter/workspace"
)

const (
	// 退出码只有一处定义（契约在 harness），这里只是短别名：同一组数字两处定义迟早会漂。
	exitOK        = int(harness.ExitOK)
	exitEnv       = int(harness.ExitEnv)
	exitAborted   = int(harness.ExitAborted)
	exitCancelled = int(harness.ExitCancelled)
)

// version 是本构建的版本号。
var version = "devel"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return exitEnv
	}
	switch {
	case args[0] == "version":
		fmt.Printf("xhunter %s\n", version)
		return exitOK
	case args[0] == "run":
		return runCmd(args[1:])
	case strings.HasPrefix(args[0], "-"):
		return huntCmd(args)
	default:
		usage()
		return exitEnv
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `xhunter

  xhunter --bounty <path> [--log-file <path>] [--result <path>] [--patch <path>]
        执行一次 Hunt（<path> 是任务正文文件）
        --result  结束时写结果文件（JSON；无论成败）
        --patch   写相对基线的统一 diff（git apply 兼容）
  xhunter run --repo <path> --task <text> [--out <path>] [--log-file|--result|--patch <path>]
        本地驱动：探测本地仓库 → 生成 Bounty（--out 可选）→ 用同一份装配执行一次 Hunt
  xhunter version

部署事实由环境变量给出。仓库：XHUNTER_REPO_URL / XHUNTER_REPO_BASE_COMMIT（必填）、
XHUNTER_REPO_BRANCH / XHUNTER_BOUNTY_ID / XHUNTER_SESSION_ID（可选）。
模型接入：XHUNTER_MODEL / XHUNTER_BASE_URL / XHUNTER_MODEL_CONTEXT_TOKENS /
XHUNTER_MODEL_OUTPUT_TOKENS（必填）、XHUNTER_PROTOCOL（协议取值，缺省对话补全）、
XHUNTER_API_KEY / XHUNTER_HEADERS（可选）。预算上限（可选）：XHUNTER_BUDGET_TURNS /
XHUNTER_BUDGET_TOKENS / XHUNTER_BUDGET_WALL_CLOCK。心跳间隔（可选）：
XHUNTER_HEARTBEAT_INTERVAL（Go duration，缺省 30s）。接收段不活动超时（可选）：
XHUNTER_STREAM_IDLE_TIMEOUT（Go duration，缺省 120s；非 duration / 0 / 负 → 启动期退出 1）。
`)
}

// huntCmd 是「环境投递」入口：把它读成一份 Bounty（任务正文文件 ＋ 部署事实环境变量），
// 再交给 executeHunt 执行。它自己不装配执行体——装配与执行只有 executeHunt 一条路径。
func huntCmd(args []string) int {
	fs := flag.NewFlagSet("xhunter", flag.ContinueOnError)
	bountyPath := fs.String("bounty", "", "任务正文文件路径")
	logFile := fs.String("log-file", "", "人类可读日志路径（默认 stderr）")
	resultPath := fs.String("result", "", "结果文件路径（JSON；FR-1.5）")
	patchPath := fs.String("patch", "", "补丁文件路径（相对基线的统一 diff）")
	if err := fs.Parse(args); err != nil {
		return exitEnv
	}
	if *bountyPath == "" {
		fmt.Fprintln(os.Stderr, "缺少 --bounty 参数（任务正文文件路径）")
		return exitEnv
	}

	task, err := loadTask(*bountyPath, os.ReadFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitEnv
	}
	bounty, err := bountyFromEnv(task, os.LookupEnv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitEnv
	}

	return executeHunt(bounty, runOptions{logFile: *logFile, resultPath: *resultPath, patchPath: *patchPath})
}

// runOptions 是一次 Hunt 的三条输出路径（Bounty 之外的装配参数）：日志、结果文件、补丁。
// 两条驱动路径（环境投递 / 本地探测）都用它交给 executeHunt，输出契约因此只有一处。
type runOptions struct {
	logFile    string
	resultPath string
	patchPath  string
}

// executeHunt 用**同一份装配**执行一次 Hunt：解析部署事实 → 构造 Provider 与出口 →
// 装配业务执行体与循环 → 跑一次 → 写出交付记录。两条驱动路径都只调它——
// 不存在第二条执行路径（外部不得自建一条绕过不变量的后门）。
func executeHunt(bounty hunt.Bounty, opts runOptions) int {
	// 心跳间隔是部署事实：写错即启动期失败（退出 1），不留到运行期才发现。
	heartbeat, err := parseHeartbeatInterval(os.LookupEnv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitEnv
	}
	// 接收段不活动超时也是部署事实：写错即启动期失败（退出 1）。
	idle, err := parseStreamIdleTimeout(os.LookupEnv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitEnv
	}

	// 模型接入事实完全来自环境变量：一次报出全部缺项（退出 1），
	// 不留到第一次推理才发现——那时候已经烧掉了轮次与预算。
	resolved, err := providerconfig.FromEnv(os.LookupEnv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v（任务与仓库事实已就绪，缺的是模型接入）\n", err)
		return exitEnv
	}
	provider, err := openProvider(resolved)
	if err != nil {
		fmt.Fprintf(os.Stderr, "构造 Provider 失败：%v\n", err)
		return exitEnv
	}

	logs := io.Writer(os.Stderr)
	if opts.logFile != "" {
		f, err := os.Create(opts.logFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "无法打开日志文件：%v\n", err)
			return exitEnv
		}
		defer f.Close()
		logs = f
	}
	// 通道分工是外部契约：事件流独占 stdout（机器消费），人类日志走 stderr。
	// 写错方向不会让任何单测失败，却会让所有按文档实现的驱动者读不到事件。
	// 信封里的追踪标识来自投递事实（缺省已回填为 bounty id）。
	sink := &eventSink{events: os.Stdout, logs: logs, start: time.Now(),
		bountyID: string(bounty.ID), traceID: bounty.TraceID}

	// 机制硬顶用同一份配置贯穿两处：装配层报进 config_snapshot、循环按它执行。在两处各写一遍
	// 3/1000 迟早会漂，所以这里是唯一来源。
	hcfg := harness.DefaultConfig()
	// 部署侧可覆写接收段不活动超时；未设置则不动（默认仍由 DefaultConfig 提供）。
	if idle > 0 {
		hcfg.StreamIdleTimeout = idle
	}

	// 业务执行体：向循环提供三组 handler，同时是原语看到的 Facts。上下文、会话材料与
	// 事件出口都由它自己持有——循环不认识这些东西。
	// 门禁的两端共用同一份执行器：模型主动调用与收尾补跑跑的是同一条命令、同一份判据。
	gateRunner := gate.NewRunner()
	gitBackend := defaultGit()
	session := hunt.NewSession(hunt.Config{
		Bounty: bounty,
		Tools: func(ws workspace.Workspace) []hunt.Primitive {
			// 一期符号扩展未接入：装 panic 哨兵（未冻结期口径，走到即炸）；符号原语声明不实现，走不到。
			return defaultTools(ws, defaultExt(ws), gateRunner)
		},
		Policy: defaultPolicy(bounty.Budget),
		Opener: defaultWorkspaces(),
		Git:    gitBackend,
		// 门禁清单从基线 commit 读（判据必须在运行开始前定死）。
		Gates:         gates.New(gitBackend, ""),
		GateRunner:    gateRunner,
		Context:       &contextBuilder{},
		Session:       &sessionRecorder{bounty: bounty},
		Sink:          sink,
		SystemPlugins: defaultSystemPlugins,
		UserPlugins:   defaultUserPlugins,
		// 生效配置快照里「只有装配层知道」的那部分：装配它就等于声明"本次生效的是什么"。
		Assembly: assemblyFacts(hcfg),
		// 心跳间隔来自部署事实（0 = 取 Session 默认 30s）。
		Heartbeat: heartbeat,
	})

	// 循环只拿到「模型 + 三组 handler」：换一套 handler 就是换一套业务。
	engine, err := harness.New(provider,
		[]harness.PrepareHandler{session.Prepare},
		[]harness.OnTurnHandler{session.OnTurn},
		[]harness.FinalHandler{session.Finalize},
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "装配不完整：%v\n", err)
		return exitEnv
	}
	// 把与 config_snapshot 同源的机制硬顶交给循环：默认配置已够用，这里显式覆盖是为了让
	// "报出的"与"实际执行的"是同一份。
	engine = engine.WithConfig(hcfg)

	fmt.Fprintf(os.Stderr, "已装配：任务 %q 仓库 %s 分支 %s 基线 %.12s 协议 %s 模型 %s\n",
		firstLine(bounty.Task), bounty.Repo.Remote, bounty.Repo.Branch, bounty.Repo.BaseCommit,
		resolved.Protocol, resolved.ModelID)

	// 取消入口：循环内任何阻塞调用都靠 ctx 打断。信号只在**运行段**接管——
	// 启动期的读文件、建 Provider 仍走默认处置（那里还没有任何状态需要保住，
	// 接管反而会让一个卡住的启动过程变得杀不掉）。
	ctx, stop := signalContext()
	defer stop()

	outcome := engine.Run(ctx, harness.Input{Meta: map[string]any{
		"bounty_id":  string(bounty.ID),
		"session_id": SessionID(bounty),
	}})

	// 交付记录在终态之后写：无论成败都要留档（FR-1.5），写不出来属环境问题。
	// 生效配置快照由 Session 在装配完成后冻结，这里取同一份写进结果文件。
	if err := writeRunOutputs(opts.resultPath, opts.patchPath, bounty, outcome, session.Delivery(), session.Declared(), session.EffectiveConfig()); err != nil {
		fmt.Fprintf(os.Stderr, "%v（终态已定：%s/%s）\n", err, outcome.Status, outcome.Reason)
		return exitEnv
	}
	// 通道断裂即环境错误（FR-10.4）：消费者已不在通道上，继续跑只是在自说自话。
	// 事件出口记着第一次写失败，这里收口——不把"写不出去"降级成一条 warn。
	if err := sink.Failed(); err != nil {
		fmt.Fprintf(os.Stderr, "事件通道写入失败：%v（终态已定：%s/%s）\n", err, outcome.Status, outcome.Reason)
		return exitEnv
	}
	return int(outcome.ExitCode)
}

// signalContext 返回一个在 SIGINT / SIGTERM 时取消的上下文。
//
// 它是"外部打断"进引擎的唯一通道：取消后循环不再发起新的推理与工具调用，
// Finalize 照常跑（交付提交、材料落盘、清理、`hunt_end`），终态是 cancelled / 退出 3。
// 不接信号时，进程被默认处置直接杀掉——没有终态事件、没有清理、退出码也不是 3。
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// firstLine 取正文首行用于诊断输出。
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const limit = 60
	if len([]rune(s)) > limit {
		return string([]rune(s)[:limit]) + "…"
	}
	return s
}
