// Package cli 是"git 操作"这件事的实现落点：通过 git 命令行完成基线获取、
// 任务分支、提交与差异。它是 harness 里 GitWorktree 契约的一个实现。
//
// 为什么用命令行而不是库：契约要的是少数几个语义明确的动作（浅克隆、建分支、
// 提交、取 diff），而这些动作的**语义与 git 版本无关、与实现无关**；用命令行
// 换来零依赖与"人和工具跑的是同一条命令"，出问题时可以照着日志手工复现。
//
// 三条不变量由此包负责（它们在上层无法表达）：
//
//	分支与推送目标只在这里被决定——模型的工具面上没有任何 git 操作；
//	只允许 fast-forward——绝不 force push、绝不改写已推送历史；
//	提交累积自上一个检查点的全部改动——因此单次失败可在下一轮自愈。
package cli

import (
	"context"
	"os"
	"strings"

	"xhunter/harness"
)

// Config 是装配层给出的部署事实。
//
// 它只装"与环境有关、与任务无关"的部分：仓库地址、分支名、基线 commit 都随任务
// 逐次给出（见 RepoRef），因此不进这里。
type Config struct {
	// WorkDir 是工作区所在的父目录：每次任务在其中建一个临时工作树，
	// 任务结束即回收。传空则由系统临时目录兜底。
	WorkDir string
	// Remote 是远端名，默认 origin。
	Remote string
}

// Git 是命令行实现的 git 工作树。
type Git struct {
	workDir string
	remote  string
}

// New 构造 git 实现。它只做参数归一，不执行任何命令——
// 真正的动作都发生在任务运行期（那时才知道要哪个仓库、哪条分支）。
func New(cfg Config) *Git {
	workDir := cfg.WorkDir
	if strings.TrimSpace(workDir) == "" {
		workDir = os.TempDir()
	}
	remote := cfg.Remote
	if remote == "" {
		remote = "origin"
	}
	return &Git{workDir: workDir, remote: remote}
}

var _ harness.GitWorktree = (*Git)(nil)

// PrepareBaseline 获取基线、建立任务分支，并返回工作树根。
//
// **实现待补**。要做的四件事（顺序不能变）：
//
//	① 在 workDir 下建一个临时工作树（**必须**在临时目录里，绝不能拿用户当前
//	   仓库当工作区——那会污染用户未提交的改动）；
//	② 按 BaseCommit 取基线：能按 SHA 浅克隆就浅克隆，不可得时回退完整克隆；
//	③ 在基线 commit 上创建并推送任务分支（远端已存在且 tip 是基线的后继 →
//	   视为续跑，幂等成功；否则环境错误）；
//	④ checkout 分支 tip（续跑时就是最后一个检查点）。
//
// 失败一律是**环境问题**：网络、鉴权、推送权限、基线不可达——修好之后重跑有意义。
func (g *Git) PrepareBaseline(_ context.Context, repo harness.RepoRef) (string, error) {
	return "", notImplemented("PrepareBaseline", repo)
}

// Commit 提交自上一个检查点以来的全部改动并推送。
//
// **实现待补**：`add -A`（会话材料目录用 `-f` 强制加入，即使 .gitignore 忽略了它）
// → `commit`（提交信息由引擎给，本包只负责原样使用）→ 推送，且**只接受
// fast-forward**：远端 tip 不是本地父提交就拒绝，退出码 2。
func (g *Git) Commit(_ context.Context, repo harness.RepoRef, _ string) (harness.Commit, error) {
	return harness.Commit{}, notImplemented("Commit", repo)
}

// Diff 产出相对基线的改动文件清单（会话材料目录排除在外）。
func (g *Git) Diff(_ context.Context, _ string) ([]string, error) {
	return nil, notImplemented("Diff", harness.RepoRef{})
}

// Clean 回收临时工作树。
//
// **实现待补**：删目录即可。失败只记录、不阻断——它影响的是磁盘占用，
// 不是任务结论。
func (g *Git) Clean(_ context.Context) error {
	return notImplemented("Clean", harness.RepoRef{})
}

// notImplemented 给出明确的结构化失败，而不是静默返回零值：
// 零值会让上层以为"基线已经就绪"，然后在一个不存在的工作区里继续跑。
func notImplemented(op string, repo harness.RepoRef) error {
	target := repo.Remote
	if target == "" {
		target = "仓库"
	}
	return &harness.ToolError{
		Kind:      "not_implemented",
		Message:   "git 操作尚未实现：" + op + "（" + target + "）",
		Retryable: false,
	}
}
