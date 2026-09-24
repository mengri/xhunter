// Package cli 是「git 操作」这件事的实现落点：通过 git 命令行完成基线获取、任务分支、
// 提交与差异。它是 xhunter/git 里 GitWorktree 契约的一个实现。
//
// 为什么用命令行而不是库：契约要的是少数几个语义明确的动作，而这些动作的语义与 git
// 版本无关、与实现无关；用命令行换来零依赖与「人和工具跑的是同一条命令」，出问题时
// 可以照着日志手工复现。
//
// 时序与不变量（架构 INV-11、FR-1.3）：
//   - PrepareBaseline 是初始化期的第一个动作，先于任何模型推理与任何写操作；
//     它在**临时工作树**里取基线、建任务分支、checkout，并立刻把分支推给远端
//     （把基线钉在远端）；
//   - 任务分支的创建/推送只发生在初始化期；Commit 只在轮边界与收尾被调用，
//     且**只允许 fast-forward**，绝不 force push；
//   - 工具面上没有任何 git 能力，模型既看不到也改不了分支、推送目标与已推送历史。
//
// 状态：一次 Hunt 一个实例（装配层每次执行构造一个）。工作树根由 PrepareBaseline
// 交出并记在这里，因为契约上的 Commit / Diff / Clean 只拿得到 RepoRef。
package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"xhunter/git"
	"xhunter/llm"
)

// Config 是装配层给出的部署事实，只装「与环境有关、与任务无关」的部分。
type Config struct {
	WorkDir string // 工作区父目录，空则用系统临时目录
	Remote  string // 远端名，默认 origin
}

// Git 是命令行实现的 git 工作树。
type Git struct {
	workDir string
	remote  string

	// root 是 PrepareBaseline 建出的临时工作树。契约上的其它动作只拿得到 RepoRef，
	// 因此这个事实由本实例记住。
	root string
}

// New 构造 git 实现。它只做参数归一，不执行任何命令。
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

var _ git.GitWorktree = (*Git)(nil)

// PrepareBaseline 获取基线、建立任务分支、推送并返回工作树根。
//
// 步骤：建临时工作树 → 取基线（按 SHA 浅取，不可得则完整 fetch）→ 判定任务分支状态
// （不存在则以基线创建并推送；已存在且 tip 为基线或其后代则按续跑 checkout 其 tip；
// 分叉即环境错误）→ 干净校验。
func (g *Git) PrepareBaseline(ctx context.Context, repo git.RepoRef) (string, error) {
	if strings.TrimSpace(repo.Remote) == "" {
		return "", envFault("baseline_unreachable", "缺少仓库远端地址：基线不可获取")
	}
	if strings.TrimSpace(repo.BaseCommit) == "" {
		return "", envFault("baseline_unreachable", "缺少基线 commit：无法确定从哪里开始")
	}
	if strings.TrimSpace(repo.Branch) == "" {
		return "", envFault("branch_missing", "缺少任务分支名：分支由初始化阶段创建，不能缺省")
	}
	if err := os.MkdirAll(g.workDir, 0o755); err != nil {
		return "", envFault("workspace_unavailable", "工作区父目录不可用："+err.Error())
	}
	// 同一实例重复调用时先回收上一棵：契约允许库使用者多次取基线，留下孤儿工作树
	// 既占磁盘，也会让"这次的工作区在哪"变得含糊。
	_ = g.Clean(ctx)
	root, err := os.MkdirTemp(g.workDir, "xhunter-work-*")
	if err != nil {
		return "", envFault("workspace_unavailable", "无法创建工作树："+err.Error())
	}
	// 失败路径不留临时工作树：半成品工作区既占地方，也会让下一次运行看见脏状态。
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(root)
		}
	}()

	if err := g.initWorktree(ctx, root); err != nil {
		return "", err
	}
	if err := g.attachRemote(ctx, root, repo.Remote); err != nil {
		return "", err
	}
	if err := g.checkoutTaskBranch(ctx, root, repo); err != nil {
		return "", err
	}

	// 初始化期结束时工作区必须是干净的：此后一切改动都由执行体经唯一写盘入口产生。
	out, err := g.run(ctx, root, "status", "--porcelain")
	if err != nil {
		return "", envFault("workspace_dirty", "无法检查工作区状态："+err.Error())
	}
	if out != "" {
		return "", envFault("workspace_dirty", "工作区在初始化后不是干净的："+firstLine(out))
	}

	g.root = root
	ok = true
	return root, nil
}

// Commit 提交自上一个检查点以来的全部改动并推送。**只允许 fast-forward**：
// 远端 tip 不是本地父提交时，push 会被拒绝，这里如实上报为环境错误，绝不 force push。
//
// 无改动时不产生空提交（FR-1.3c「无新写操作不提交」），返回当前 HEAD——调用方据此
// 仍能得到一个有效的 Commit 事实。
func (g *Git) Commit(ctx context.Context, repo git.RepoRef, msg string) (git.Commit, error) {
	if g.root == "" {
		return git.Commit{}, envFault("no_worktree", "尚未获取基线：不能提交")
	}
	if _, err := g.run(ctx, g.root, "add", "-A"); err != nil {
		return git.Commit{}, envFault("commit_failed", "暂存改动失败："+err.Error())
	}
	// 材料目录**强制加入**：仓库忽略 `.xhunter/` 是常见做法，而 `add -A` 不会加入被忽略的路径——
	// 那样材料会写在工作区、却不进任何提交，"唯一状态源"悄悄失效且不报错（IA-6.1c）。材料目录
	// 尚未落盘（Open 失败等）不是错误：跳过即可，不把整个提交判死。
	if md := strings.TrimSpace(repo.MaterialDir); md != "" && g.worktreeHas(md) {
		if _, err := g.run(ctx, g.root, "add", "-f", "--", md); err != nil {
			return git.Commit{}, envFault("commit_failed", "强制加入材料目录失败："+err.Error())
		}
	}
	// `diff --cached --quiet`：退出码 0 表示没有已暂存的改动。
	if _, code, err := g.runCode(ctx, g.root, "diff", "--cached", "--quiet"); err != nil {
		return git.Commit{}, envFault("commit_failed", "检查暂存区失败："+err.Error())
	} else if code == 0 {
		sha, err := g.revParse(ctx, g.root, "HEAD")
		if err != nil {
			return git.Commit{}, err
		}
		return git.Commit{SHA: sha, Branch: repo.Branch}, nil
	}

	// 提交身份与签名策略由本实现给出：临时工作树没有全局配置，而交付不该依赖
	// 跑任务那台机器的 ~/.gitconfig。
	if _, err := g.run(ctx, g.root, "-c", "user.name=xhunter", "-c", "user.email=xhunter@localhost",
		"-c", "commit.gpgsign=false", "commit", "-q", "-m", msg); err != nil {
		return git.Commit{}, envFault("commit_failed", "提交失败："+err.Error())
	}
	sha, err := g.revParse(ctx, g.root, "HEAD")
	if err != nil {
		return git.Commit{}, err
	}
	if _, err := g.run(ctx, g.root, "push", "-q", g.remote,
		"HEAD:refs/heads/"+repo.Branch); err != nil {
		// 非快进、无推送权限、远端不可达都落在这里：都是环境问题，且都不能改写远端。
		return git.Commit{}, envFault("push_rejected",
			"推送任务分支失败（只允许 fast-forward，绝不 force push）："+err.Error())
	}
	return git.Commit{SHA: sha, Branch: repo.Branch, Created: true}, nil
}

// Diff 产出相对基线的改动文件清单。
//
// 失败不阻断收尾：调用方（Finalize）只把它当作附带交付物，拿不到就只产出补丁。
func (g *Git) Diff(ctx context.Context, repo git.RepoRef) ([]string, error) {
	if g.root == "" {
		return nil, envFault("no_worktree", "尚未获取基线：无法产出差异")
	}
	if strings.TrimSpace(repo.BaseCommit) == "" {
		return nil, envFault("baseline_unreachable", "缺少基线 commit：无法产出差异")
	}
	out, err := g.run(ctx, g.root, diffArgs("--name-only", repo)...)
	if err != nil {
		return nil, envFault("diff_failed", "产出差异失败："+err.Error())
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// Patch 产出相对基线的统一 diff（`git apply` 兼容）。
//
// 与 Diff 同一范围、同一时点：都是附带交付物，主交付始终是分支 tip。
// 必须在 Clean 之前调用——工作树回收之后就没有可 diff 的对象了。
func (g *Git) Patch(ctx context.Context, repo git.RepoRef) (string, error) {
	if g.root == "" {
		return "", envFault("no_worktree", "尚未获取基线：无法产出补丁")
	}
	if strings.TrimSpace(repo.BaseCommit) == "" {
		return "", envFault("baseline_unreachable", "缺少基线 commit：无法产出补丁")
	}
	out, err := g.runRaw(ctx, g.root, diffArgs("--no-color", repo)...)
	if err != nil {
		return "", envFault("diff_failed", "产出补丁失败："+err.Error())
	}
	return out, nil
}

// ReadFileAtCommit 读取基线 commit 里的文件内容。
//
// 门禁清单必须**从基线读**而不是从工作区读：判据是这次交付的验收标准，让它运行中途变，这次
// 的结论就不可复现（文件本身不禁改，改了照样进交付 diff 给人 review，只是本次不生效）。
func (g *Git) ReadFileAtCommit(ctx context.Context, repo git.RepoRef, path string) ([]byte, bool, error) {
	if g.root == "" {
		return nil, false, envFault("no_worktree", "尚未获取基线：无法读取基线文件")
	}
	base := strings.TrimSpace(repo.BaseCommit)
	if base == "" {
		return nil, false, envFault("baseline_unreachable", "缺少基线 commit：无法读取基线文件")
	}
	if strings.TrimSpace(path) == "" {
		return nil, false, envFault("baseline_read_failed", "读取基线文件失败：路径为空")
	}
	// 先问存在、再取内容：cat-file -e 的非零退出是**结论**（那个提交里没有这个文件），不是执行
	// 失败。两步分开，才不会把"没有声明"读成"读不出来"。
	_, code, err := g.runCode(ctx, g.root, "cat-file", "-e", base+":"+path)
	if err != nil {
		return nil, false, envFault("baseline_read_failed", "读取基线文件失败："+err.Error())
	}
	if code != 0 {
		return nil, false, nil
	}
	out, err := g.runRaw(ctx, g.root, "show", base+":"+path)
	if err != nil {
		return nil, false, envFault("baseline_read_failed", "读取基线文件 "+path+" 失败："+err.Error())
	}
	return []byte(out), true, nil
}

// diffArgs 组装 diff / patch 的公共参数：基准 commit ＋（材料目录非空时）排除本次会话材料目录的
// pathspec。**只排本次会话的材料目录**——`.xhunter/` 下的其它路径（如 `skills.draft/**`）是交付
// 内容，不能被一起藏掉；MaterialDir 为空时不加 pathspec（保持旧行为）。
func diffArgs(extra string, repo git.RepoRef) []string {
	args := []string{"diff"}
	if extra != "" {
		args = append(args, extra)
	}
	args = append(args, repo.BaseCommit)
	if md := strings.TrimSpace(repo.MaterialDir); md != "" {
		args = append(args, "--", ":(exclude)"+md)
	}
	return args
}

// worktreeHas 报告工作树里是否存在某个相对路径：材料目录可能尚未落盘（Open 失败走降级口径）。
func (g *Git) worktreeHas(rel string) bool {
	_, err := os.Stat(filepath.Join(g.root, rel))
	return err == nil
}

// Clean 回收临时工作树。可重复调用：没有工作树时是空操作。
func (g *Git) Clean(_ context.Context) error {
	if g.root == "" {
		return nil
	}
	root := g.root
	g.root = ""
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("回收工作树失败：%w", err)
	}
	return nil
}

// ============================================================ 初始化步骤

// initWorktree 建出工作树与提交身份。command-line git 在临时目录里没有全局配置，
// 身份因此显式写进该仓库的 config。
func (g *Git) initWorktree(ctx context.Context, root string) error {
	if _, err := g.run(ctx, "", "init", "-q", root); err != nil {
		return envFault("workspace_unavailable", "初始化工作树失败："+err.Error())
	}
	for _, kv := range [][2]string{{"user.name", "xhunter"}, {"user.email", "xhunter@localhost"}} {
		if _, err := g.run(ctx, root, "config", kv[0], kv[1]); err != nil {
			return envFault("workspace_unavailable", "写入 git 身份失败："+err.Error())
		}
	}
	return nil
}

func (g *Git) attachRemote(ctx context.Context, root, url string) error {
	if _, err := g.run(ctx, root, "remote", "add", g.remote, url); err != nil {
		return envFault("baseline_unreachable", "登记远端失败："+err.Error())
	}
	return nil
}

// checkoutTaskBranch 判定任务分支状态并 checkout 到正确的位置。
func (g *Git) checkoutTaskBranch(ctx context.Context, root string, repo git.RepoRef) error {
	out, err := g.run(ctx, root, "ls-remote", "--heads", g.remote, repo.Branch)
	if err != nil {
		return envFault("baseline_unreachable", "远端不可达："+err.Error())
	}

	if out != "" {
		// 分支已存在：必须判"续跑"还是"分叉"，因此需要完整历史。
		if _, err := g.run(ctx, root, "fetch", "-q", g.remote,
			"+refs/heads/*:refs/remotes/"+g.remote+"/*"); err != nil {
			return envFault("baseline_unreachable", "取远端分支失败："+err.Error())
		}
		tipRef := g.remote + "/" + repo.Branch
		tip, err := g.revParse(ctx, root, tipRef)
		if err != nil {
			return envFault("baseline_unreachable", "任务分支不可解析："+err.Error())
		}
		if !g.commitExists(ctx, root, repo.BaseCommit) {
			return envFault("baseline_unreachable",
				"基线 commit 在远端不可达："+repo.BaseCommit)
		}
		if _, code, err := g.runCode(ctx, root, "merge-base", "--is-ancestor", repo.BaseCommit, tip); err != nil {
			return envFault("baseline_unreachable", "判定分支关系失败："+err.Error())
		} else if code != 0 {
			// 分叉（非后代）：既不能续跑也不能覆盖，属环境错误。
			return envFault("branch_diverged",
				fmt.Sprintf("任务分支与基线分叉：%s 不是 %s 的后代", tip, repo.BaseCommit))
		}
		if _, err := g.run(ctx, root, "checkout", "-q", "-B", repo.Branch, tipRef); err != nil {
			return envFault("baseline_unreachable", "checkout 任务分支失败："+err.Error())
		}
		return nil
	}

	// 分支不存在：以基线创建并立刻推送，把基线钉在远端。
	if !g.fetchBase(ctx, root, repo.BaseCommit) {
		return envFault("baseline_unreachable", "基线 commit 不可获取："+repo.BaseCommit)
	}
	if _, err := g.run(ctx, root, "checkout", "-q", "-B", repo.Branch, repo.BaseCommit); err != nil {
		return envFault("baseline_unreachable", "以基线创建任务分支失败："+err.Error())
	}
	if _, err := g.run(ctx, root, "push", "-q", g.remote, "HEAD:refs/heads/"+repo.Branch); err != nil {
		return envFault("push_rejected",
			"推送任务分支失败（需要写权限）："+err.Error())
	}
	return nil
}

// fetchBase 取到基线 commit：先尝试按 SHA 浅取（省时间与带宽），不可得时退化为完整 fetch。
// 返回 false 表示两种方式都没拿到该 commit。
func (g *Git) fetchBase(ctx context.Context, root, base string) bool {
	if _, err := g.run(ctx, root, "fetch", "-q", "--depth", "1", g.remote, base); err == nil {
		if g.commitExists(ctx, root, base) {
			return true
		}
	}
	if _, err := g.run(ctx, root, "fetch", "-q", g.remote,
		"+refs/heads/*:refs/remotes/"+g.remote+"/*"); err != nil {
		return false
	}
	// 某些远端只暴露分支：完整 fetch 之后基线可能随分支一起到达。
	return g.commitExists(ctx, root, base)
}

// ============================================================ 命令执行

// run 执行一条 git 命令，返回裁剪后的标准输出；非零退出即失败。
// 失败时把 stderr 带进错误里——git 的诊断信息是定位环境问题的第一手依据。
func (g *Git) run(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := g.runRaw(ctx, dir, args...)
	return strings.TrimSpace(out), err
}

// runRaw 同 run，但**原样**交出标准输出。
//
// 补丁必须走这条：diff 的最后一个换行是内容的一部分，裁剪过的补丁会变成
// "corrupt patch"（`git apply` 明确拒绝）。
func (g *Git) runRaw(ctx context.Context, dir string, args ...string) (string, error) {
	out, errText, code, err := g.exec(ctx, dir, args...)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("git %s：%s", strings.Join(args, " "), firstLine(errText))
	}
	return out, nil
}

// runCode 额外交出退出码：有些判定（`--is-ancestor`、`--quiet`、`cat-file -e`）用退出码
// 表达结论，非零退出是**结论**而不是执行失败。err 只在"命令起不来"时非空。
func (g *Git) runCode(ctx context.Context, dir string, args ...string) (string, int, error) {
	out, _, code, err := g.exec(ctx, dir, args...)
	if err != nil {
		return "", -1, err
	}
	return strings.TrimSpace(out), code, nil
}

func (g *Git) exec(ctx context.Context, dir string, args ...string) (string, string, int, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	out := stdout.String()
	errText := strings.TrimSpace(stderr.String())
	if err == nil {
		return out, errText, 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// 命令跑起来了：退出码交给调用方解释（结论还是失败）。
		return out, errText, exitErr.ExitCode(), nil
	}
	// 起不来（PATH 里没有 git、上下文已取消……）——同样要如实上报。
	return out, errText, -1, fmt.Errorf("无法执行 git %s：%w", strings.Join(args, " "), err)
}

func (g *Git) revParse(ctx context.Context, dir, ref string) (string, error) {
	sha, err := g.run(ctx, dir, "rev-parse", ref)
	if err != nil {
		return "", envFault("baseline_unreachable", "解析 "+ref+" 失败："+err.Error())
	}
	return sha, nil
}

func (g *Git) commitExists(ctx context.Context, dir, sha string) bool {
	_, code, err := g.runCode(ctx, dir, "cat-file", "-e", sha+"^{commit}")
	return err == nil && code == 0
}

// envFault 把失败归类为环境问题（FR-1.3、IA-11.3）：可重试的部署/权限/连通性问题。
func envFault(kind, msg string) *llm.Fault {
	return &llm.Fault{Kind: kind, Message: msg, Retryable: true}
}

// firstLine 取首行用于诊断输出：git 的诊断可能很长，事件与日志只留最关键的一句。
func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	const limit = 200
	if len(s) > limit {
		s = s[:limit] + "…"
	}
	if s == "" {
		return "无输出"
	}
	return s
}
