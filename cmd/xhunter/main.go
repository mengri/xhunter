// Command xhunter 是 Xhunter 的入口。
//
// 用法：
//
//	xhunter --bounty <path> [--log-file <path>]   执行一次 Hunt（FR-1.1）
//	xhunter models update [--source URL] [--dir PATH] [--timeout DURATION]
//	xhunter models status [--dir PATH]
//	xhunter version
//
// 退出码遵循需求 §6.4：0 成功 / 1 任务失败 / 2 环境问题（可重试）/ 3 取消。
// models 子命令属于环境侧动作：网络或磁盘问题一律以 2 退出。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"xhunter/harness"
	"xhunter/internal/modelcatalog"
)

const (
	exitOK        = 0
	exitFailed    = 1
	exitEnv       = 2
	exitCancelled = 3
)

// version 是本构建的版本号，客户端标识（User-Agent）与 `xhunter version` 都读它。
//
// 缺省为 "devel"（本地开发构建）；发布时用
//
//	go build -ldflags "-X xhunter/cmd/xhunter.version=v0.1.0"
//
// 注入正式版本号。它没有默认的"有意义"值：骨架期不假装有版本，上游从标识里
// 看到 devel 就知道这是开发构建。
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

  xhunter --bounty <path> [--log-file <path>]   执行一次 Hunt（<path> 是任务正文文件）
  xhunter models update [--source URL] [--dir PATH] [--timeout DURATION]
  xhunter models status [--dir PATH]
  xhunter version

部署事实由环境变量给出：XHUNTER_REPO_URL / XHUNTER_REPO_BASE_COMMIT（必填）、
XHUNTER_REPO_BRANCH / XHUNTER_BOUNTY_ID / XHUNTER_SESSION_ID（可选）、
XHUNTER_PROVIDER / XHUNTER_MODEL / XHUNTER_PROVIDER_CONFIG（模型接入）。
同一个 XHUNTER_SESSION_ID 的多次投递共享记忆与分支；不传时本次自成新会话。
`)
}

// huntCmd 是执行入口。
//
// 投递形态：`--bounty` 指向**任务正文文件**，其余事实从环境变量读（见 bounty.go）。
// 任务、仓库事实与 Provider 在这一步都真正装配起来（因此缺什么会在启动期报出来，
// 而不是跑起来才发现），但除 Provider 之外的协作者（上下文、工具、策略、事件出口、
// 会话、扩展、git）尚未实现，所以这里**显式失败**而不是假装跑通。
func huntCmd(args []string) int {
	fs := flag.NewFlagSet("xhunter", flag.ContinueOnError)
	bountyPath := fs.String("bounty", "", "任务正文文件路径")
	logFile := fs.String("log-file", "", "人类可读日志路径（默认 stderr）")
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

	// 模型接入：三要素齐备才构造 Provider——它的构造期校验（缺上限、缺端点、
	// 凭据引用解析为空）正是"启动期显式失败"该覆盖的那一类问题。
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

	// 提示词插件在装配期排定顺序——顺序即执行顺序，因此这里构造成功本身就说明
	// "首轮提示词由哪几段拼成"已经确定。
	system, user := defaultPromptPlugins()

	// 装配：把已经实现的协作者装进 Deps。**还缺什么由内核自己算**——
	// 手写一份"尚未实现"的清单迟早会烂掉，而内核的装配校验不会。
	deps := harness.Deps{
		Provider:   provider,
		Git:        defaultGit(),
		Workspaces: defaultWorkspaces(),
		// 工具运行时、上下文、策略、事件出口、会话、扩展尚未实现
	}

	fmt.Fprintf(os.Stderr,
		"已装配：任务 %q（%d 字）会话 %s 仓库 %s 分支 %s 基线 %.12s 模型 %s/%s 日志 %s；"+
			"提示词插件 system %d / user %d；文件操作与 git 由装配层注入（本地文件系统 / git 命令行）\n",
		firstLine(bounty.Task), len([]rune(bounty.Task)), SessionID(bounty),
		bounty.Repo.Remote, bounty.Repo.Branch, bounty.Repo.BaseCommit,
		selection.ProviderID, selection.ModelID, logTarget(*logFile),
		len(system), len(user))
	if err := deps.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "%v，因此不开始执行\n", err)
		return exitEnv
	}
	return exitEnv
}

// logTarget 给出日志去向的显示值（空表示默认 stderr）。
func logTarget(path string) string {
	if path == "" {
		return "stderr"
	}
	return path
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
	reserve := fs.Int("output-reserve", 0,
		"上游未区分输入输出时的输出预留 token 数（默认 8192；输出预留是预算参数，不是模型事实）")
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
	fmt.Printf("  跳过      sample_spec %d · 无关模式 %d · 缺上限 %d · 窗口小于默认值 %d · 同名冲突 %d\n",
		rep.SkippedSampleSpec, rep.SkippedMode, rep.SkippedNoLimits, rep.SkippedIncoherent, rep.Collisions)
	fmt.Printf("  输出预留  上游未区分输入输出的 %d 条使用策略默认值 %d（已打标，可审计）\n",
		rep.UsedPolicyDefault, rep.PolicyDefaultReserve)
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
