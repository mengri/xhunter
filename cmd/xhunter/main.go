// Command xhunter 是 Xhunter 的入口。
//
// 用法：
//
//	xhunter --bounty <path> [--log-file <path>] [--result <path>] [--patch <path>]
//	                                              执行一次 Hunt
//	xhunter models update [--source URL] [--dir PATH] [--timeout DURATION]
//	xhunter models status [--dir PATH]
//	xhunter version
//
// 退出码：0 成功 / 1 任务失败 / 2 环境问题（可重试）/ 3 取消。
package main

import (
	"context"
	"errors"
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
	"xhunter/internal/modelcatalog"
	"xhunter/workspace"
)

const (
	exitOK        = 0
	exitFailed    = 1
	exitEnv       = 2
	exitCancelled = 3
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
	case args[0] == "models":
		return modelsCmd(args[1:])
	case args[0] == "version":
		fmt.Printf("xhunter %s\n", version)
		return exitOK
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
  xhunter models update [--source URL] [--dir PATH] [--timeout DURATION]
  xhunter models status [--dir PATH]
  xhunter version

部署事实由环境变量给出：XHUNTER_REPO_URL / XHUNTER_REPO_BASE_COMMIT（必填）、
XHUNTER_REPO_BRANCH / XHUNTER_BOUNTY_ID / XHUNTER_SESSION_ID（可选）、
XHUNTER_PROVIDER / XHUNTER_MODEL / XHUNTER_PROVIDER_CONFIG（模型接入）、
XHUNTER_MAX_TURNS / XHUNTER_MAX_TOKENS / XHUNTER_MAX_WALL_CLOCK（预算上限，可选）。
`)
}

// huntCmd 是执行入口：装配业务执行体与循环协作者，然后跑一次循环。
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

	selection := selectionFromEnv(os.LookupEnv)
	if err := selection.validate(); err != nil {
		fmt.Fprintf(os.Stderr, "%v（任务与仓库事实已就绪，缺的是模型接入）\n", err)
		return exitEnv
	}
	provider, err := openProvider(selection)
	if err != nil {
		fmt.Fprintf(os.Stderr, "构造 Provider 失败：%v\n", err)
		return exitEnv
	}

	logs := io.Writer(os.Stderr)
	if *logFile != "" {
		f, err := os.Create(*logFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "无法打开日志文件：%v\n", err)
			return exitEnv
		}
		defer f.Close()
		logs = f
	}
	// 通道分工是外部契约：事件流独占 stdout（机器消费），人类日志走 stderr。
	// 写错方向不会让任何单测失败，却会让所有按文档实现的驱动者读不到事件。
	sink := &eventSink{events: os.Stdout, logs: logs, start: time.Now()}

	// 业务执行体：向循环提供三组 handler，同时是原语看到的 Facts。上下文、会话材料与
	// 事件出口都由它自己持有——循环不认识这些东西。
	session := hunt.NewSession(hunt.Config{
		Bounty: bounty,
		Tools: func(ws workspace.Workspace) []hunt.Primitive {
			return defaultTools(ws, nil) // 一期符号扩展未接入
		},
		Policy:        defaultPolicy(bounty.Budget),
		Opener:        defaultWorkspaces(),
		Git:           defaultGit(),
		Context:       &contextBuilder{},
		Session:       &sessionRecorder{},
		Sink:          sink,
		SystemPlugins: defaultSystemPlugins,
		UserPlugins:   defaultUserPlugins,
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

	fmt.Fprintf(os.Stderr, "已装配：任务 %q 仓库 %s 分支 %s 基线 %.12s 模型 %s/%s\n",
		firstLine(bounty.Task), bounty.Repo.Remote, bounty.Repo.Branch, bounty.Repo.BaseCommit,
		selection.ProviderID, selection.ModelID)

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
	if err := writeRunOutputs(*resultPath, *patchPath, bounty, outcome, session.Delivery()); err != nil {
		fmt.Fprintf(os.Stderr, "%v（终态已定：%s/%s）\n", err, outcome.Status, outcome.Reason)
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

func modelsCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "用法：xhunter models <update|status>")
		return exitEnv
	}
	switch args[0] {
	case "update":
		return modelsUpdate(args[1:])
	case "status":
		return modelsStatus(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "未知子命令：models %s\n", args[0])
		return exitEnv
	}
}

func modelsUpdate(args []string) int {
	fs := flag.NewFlagSet("models update", flag.ContinueOnError)
	source := fs.String("source", "", "上游目录地址（默认 LiteLLM 目录）")
	dir := fs.String("dir", "", "快照目录（默认 ~/.xhunter）")
	timeout := fs.Duration("timeout", 60*time.Second, "单次请求超时")
	reserve := fs.Int("output-reserve", 0, "上游未区分输入输出时的输出预留 token 数")
	if err := fs.Parse(args); err != nil {
		return exitEnv
	}

	snap, err := modelcatalog.Update(context.Background(), modelcatalog.UpdateOptions{
		Source:        *source,
		Dir:           *dir,
		Timeout:       *timeout,
		OutputReserve: *reserve,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "更新模型目录失败：%v\n", err)
		return exitEnv
	}

	target := *dir
	if target == "" {
		if d, err := modelcatalog.DefaultDir(); err == nil {
			target = d
		}
	}
	rep := snap.Meta.Report
	fmt.Printf("模型目录已更新：%s\n", modelcatalog.SnapshotPath(target))
	fmt.Printf("  来源      %s\n", snap.Meta.Source)
	fmt.Printf("  抓取时间  %s\n", snap.Meta.FetchedAt.Format(time.RFC3339))
	fmt.Printf("  上游规模  %d 条 / %.1f MB（sha256 %s…）\n",
		rep.UpstreamEntries, float64(snap.Meta.UpstreamBytes)/(1<<20), snap.Meta.UpstreamSHA256[:12])
	fmt.Printf("  转换结果  %d 个模型 / %d 个供应商\n", rep.Converted, rep.Providers)
	fmt.Printf("  跳过      sample_spec %d · 缺供应商 %d · 无关模式 %d · 缺上限 %d · 窗口小于默认值 %d · 同名冲突 %d\n",
		rep.SkippedSampleSpec, rep.SkippedNoProvider, rep.SkippedMode, rep.SkippedNoLimits, rep.SkippedIncoherent, rep.Collisions)
	return exitOK
}

func modelsStatus(args []string) int {
	fs := flag.NewFlagSet("models status", flag.ContinueOnError)
	dir := fs.String("dir", "", "快照目录（默认 ~/.xhunter）")
	if err := fs.Parse(args); err != nil {
		return exitEnv
	}
	target := *dir
	if target == "" {
		d, err := modelcatalog.DefaultDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return exitEnv
		}
		target = d
	}

	snap, err := modelcatalog.Load(target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		if errors.Is(err, modelcatalog.ErrNoSnapshot) {
			return exitEnv
		}
		return exitFailed
	}
	total := 0
	for _, p := range snap.Catalog.Provider {
		total += len(p.Models)
	}
	fmt.Printf("模型目录：%s\n", modelcatalog.SnapshotPath(target))
	fmt.Printf("  来源      %s\n", snap.Meta.Source)
	fmt.Printf("  抓取时间  %s\n", snap.Meta.FetchedAt.Format(time.RFC3339))
	fmt.Printf("  可用模型  %d 个 / %d 个供应商\n", total, len(snap.Catalog.Provider))
	return exitOK
}
