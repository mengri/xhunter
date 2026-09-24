package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"xhunter/git"
	"xhunter/hunt"
	"xhunter/internal/git/cli"
)

// runUsage 是 run 子命令的用法行。缺参时打印它给出"下一步指引"，因此只有一处定义。
const runUsage = "用法：xhunter run --repo <path> --task <text> [--out <path>] [--log-file <path>] [--result <path>] [--patch <path>]"

// runCmd 是本地驱动入口（FR-1.10）：给定本地仓库路径与任务文本，**只读**探测仓库事实、
// 组装一份 Bounty，然后用**同一份装配**（executeHunt）执行一次 Hunt。
//
// 它不是第二条执行路径：探测只产出 Bounty（与平台投递的形态完全一致），执行一律经
// executeHunt。工作区由 Xhunter 自己 clone 到临时目录（PrepareBaseline 的既有行为），
// 用户的仓库目录只被只读探测，绝不会被当成工作区——用户未提交的改动不受影响。
//
// 不做远端可推送性预检：不推一次无法可靠判定，实际取不到基线由既有 PrepareBaseline 报错。
func runCmd(args []string) int {
	fs := flag.NewFlagSet("xhunter run", flag.ContinueOnError)
	repoPath := fs.String("repo", "", "本地仓库路径（只读探测远端与基线）")
	taskText := fs.String("task", "", "任务正文（文本）")
	outPath := fs.String("out", "", "Bounty 清单输出路径（可选；JSON）")
	logFile := fs.String("log-file", "", "人类可读日志路径（默认 stderr）")
	resultPath := fs.String("result", "", "结果文件路径（JSON）")
	patchPath := fs.String("patch", "", "补丁文件路径（相对基线的统一 diff）")
	if err := fs.Parse(args); err != nil {
		return exitEnv
	}

	task := strings.TrimSpace(*taskText)
	if *repoPath == "" || task == "" {
		fmt.Fprintln(os.Stderr, "缺少 --repo/--task 参数")
		fmt.Fprintln(os.Stderr, runUsage)
		return exitEnv
	}

	probe, err := cli.ProbeLocalRepo(context.Background(), *repoPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, probeGuidance(err))
		return exitEnv
	}

	bounty, err := bountyForLocalRepo(task, probe)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitEnv
	}
	if *outPath != "" {
		if err := writeBountyManifest(*outPath, bounty, probe.GateCandidate); err != nil {
			fmt.Fprintf(os.Stderr, "写 Bounty 清单失败：%v\n", err)
			return exitEnv
		}
	}
	return executeHunt(bounty, runOptions{logFile: *logFile, resultPath: *resultPath, patchPath: *patchPath})
}

// bountyForLocalRepo 把一次探测结果组装成 Bounty。分支名、材料目录、预算口径都复用既有
// 唯一来源（branchFor / materialDirFor / parseBudget），不另造规则。
func bountyForLocalRepo(task string, probe cli.LocalRepo) (hunt.Bounty, error) {
	id := shortSHA(probe.BaseCommit)
	sessionID := id // 本地驱动不给会话标识：本次投递自成一次新会话（与环境投递同口径）
	bounty := hunt.Bounty{
		ID:      hunt.BountyID(id),
		Task:    task,
		TraceID: id,
		Repo: git.RepoRef{
			Remote:      probe.RemoteURL,
			Branch:      branchFor(sessionID),
			BaseCommit:  probe.BaseCommit,
			MaterialDir: materialDirFor(sessionID),
		},
	}
	budget, err := parseBudget(os.LookupEnv)
	if err != nil {
		return hunt.Bounty{}, err
	}
	bounty.Budget = budget
	return bounty, nil
}

// probeGuidance 把探测失败翻成"报什么 ＋ 下一步怎么做"：分类要能分开，
// 每一条都给出可执行的动作，而不是把一句 git 报错原样丢回去。
func probeGuidance(err error) string {
	var pe *cli.ProbeError
	if !errors.As(err, &pe) {
		return fmt.Sprintf("探测本地仓库失败：%v", err)
	}
	switch pe.Kind {
	case cli.ProbePathMissing:
		return fmt.Sprintf("本地仓库路径不存在：%s\n把 --repo 指向一个存在的目录", pe.Path)
	case cli.ProbeNotARepo:
		return fmt.Sprintf("不是 git 仓库：%s\n--repo 指向仓库根（含 .git）的目录", pe.Path)
	case cli.ProbeNoRemote:
		return fmt.Sprintf("本地仓库没有远端：无法确定可推送的仓库地址\n"+
			"运行 `git -C %s remote add origin <可推送远端>`（需写权限）", pe.Path)
	case cli.ProbeNoCommit:
		return fmt.Sprintf("仓库没有提交：无法确定基线 commit（取 HEAD）\n先在 %s 里产生一次提交", pe.Path)
	case cli.ProbeGitUnavailable:
		detail := ""
		if pe.Err != nil {
			detail = "：" + pe.Err.Error()
		}
		return fmt.Sprintf("无法执行 git%s\n确认 PATH 中有 git", detail)
	default:
		return fmt.Sprintf("探测本地仓库失败：%v", err)
	}
}

// bountyManifest 是本地驱动生成的 Bounty 清单（`--out`）：它是**便利产物**，不是执行输入——
// 真正的执行输入是内存里的 Bounty，两者同源。字段取最小集：任务、可推送远端、基线、分支、
// 两个标识与门禁候选。
type bountyManifest struct {
	Task          string `json:"task"`
	Remote        string `json:"remote"`
	BaseCommit    string `json:"base_commit"`
	Branch        string `json:"branch"`
	BountyID      string `json:"bounty_id"`
	SessionID     string `json:"session_id"`
	GateCandidate bool   `json:"gate_candidate"`
}

// writeBountyManifest 把 Bounty 清单写成 JSON。写不出来属环境问题（交不出驱动者要的产物）。
func writeBountyManifest(path string, bounty hunt.Bounty, gateCandidate bool) error {
	m := bountyManifest{
		Task:          bounty.Task,
		Remote:        bounty.Repo.Remote,
		BaseCommit:    bounty.Repo.BaseCommit,
		Branch:        bounty.Repo.Branch,
		BountyID:      string(bounty.ID),
		SessionID:     SessionID(bounty),
		GateCandidate: gateCandidate,
	}
	buf, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	buf = append(buf, '\n')
	return writeFile(path, buf)
}
