package cli

import (
	"context"
	"os"
	"strings"
)

// 本文件是本地驱动的**只读探测**：给定一个本地仓库路径，读出生成一份 Bounty 所需的仓库事实。
// 它与 git 四动作（PrepareBaseline / Commit / Diff / Patch）共用同一套命令执行，但只用**只读**
// 命令——探测不得改动用户的仓库。

// LocalRepo 是一次本地仓库探测的结果：远端地址、基线 commit 与门禁候选。
type LocalRepo struct {
	RemoteURL     string // 远端地址（探测得到的、采用的远端 URL）
	BaseCommit    string // 基线 commit（取 HEAD）
	GateCandidate bool   // 基线 commit 的仓库根是否存在 gates.yml
}

// 探测失败的类别：调用方据此给出可执行的下一步指引——"缺远端"与"不是仓库"必须能分开报。
const (
	ProbePathMissing    = "path_missing"
	ProbeNotARepo       = "not_a_repo"
	ProbeGitUnavailable = "git_unavailable"
	ProbeNoRemote       = "no_remote"
	ProbeNoCommit       = "no_commit"
)

// ProbeError 是一次探测失败。Kind 是类别（取值为上面的常量），Path 指向被探测的路径，
// Err 保留底层原因（git 起不来时为可 unwrap 的原始错误）。
type ProbeError struct {
	Kind string
	Path string
	Err  error
}

func (e *ProbeError) Error() string {
	if e.Err != nil {
		return e.Kind + "：" + e.Err.Error()
	}
	return e.Kind + "：" + e.Path
}

// Unwrap 让调用方能用 errors.As / errors.Is 拿到底层原因。
func (e *ProbeError) Unwrap() error { return e.Err }

// ProbeLocalRepo 只读探测一个本地仓库：仓库根、远端地址、基线 commit（HEAD）与门禁候选。
//
// 它**只用只读命令**（rev-parse / remote / cat-file -e），绝不 checkout / add / commit /
// fetch / push——探测不得改动用户的仓库（fetch 会写 .git，一并禁用）。门禁候选从**基线
// commit** 读（`HEAD:gates.yml`），不读工作区：与门禁口径同源（判据在运行前定死，本次
// 不受工作区改动影响）。
func ProbeLocalRepo(ctx context.Context, path string) (LocalRepo, error) {
	abs := strings.TrimSpace(path)
	if abs == "" {
		return LocalRepo{}, &ProbeError{Kind: ProbePathMissing, Path: path}
	}
	st, err := os.Stat(abs)
	if err != nil || !st.IsDir() {
		return LocalRepo{}, &ProbeError{Kind: ProbePathMissing, Path: abs, Err: err}
	}

	g := New(Config{})

	rootOut, _, code, err := g.exec(ctx, abs, "rev-parse", "--show-toplevel")
	if err != nil {
		return LocalRepo{}, &ProbeError{Kind: ProbeGitUnavailable, Path: abs, Err: err}
	}
	if code != 0 {
		// 解析不到仓库根：多半不是 git 仓库（命令跑起来了，只是结论为否）。
		return LocalRepo{}, &ProbeError{Kind: ProbeNotARepo, Path: abs}
	}
	root := strings.TrimSpace(rootOut)

	remotesOut, _, code, err := g.exec(ctx, root, "remote")
	if err != nil {
		return LocalRepo{}, &ProbeError{Kind: ProbeGitUnavailable, Path: abs, Err: err}
	}
	if code != 0 {
		return LocalRepo{}, &ProbeError{Kind: ProbeNotARepo, Path: abs}
	}
	name, ok := chooseRemote(remotesOut)
	if !ok {
		return LocalRepo{}, &ProbeError{Kind: ProbeNoRemote, Path: abs}
	}
	urlOut, _, code, err := g.exec(ctx, root, "remote", "get-url", name)
	if err != nil {
		return LocalRepo{}, &ProbeError{Kind: ProbeGitUnavailable, Path: abs, Err: err}
	}
	if code != 0 || strings.TrimSpace(urlOut) == "" {
		return LocalRepo{}, &ProbeError{Kind: ProbeNoRemote, Path: abs}
	}
	url := strings.TrimSpace(urlOut)

	headOut, _, code, err := g.exec(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return LocalRepo{}, &ProbeError{Kind: ProbeGitUnavailable, Path: abs, Err: err}
	}
	if code != 0 || strings.TrimSpace(headOut) == "" {
		return LocalRepo{}, &ProbeError{Kind: ProbeNoCommit, Path: abs}
	}
	head := strings.TrimSpace(headOut)

	// cat-file -e 用退出码表达结论：0 = 存在。命令起不来（err != nil）才是执行失败。
	_, _, gateCode, err := g.exec(ctx, root, "cat-file", "-e", "HEAD:gates.yml")
	if err != nil {
		return LocalRepo{}, &ProbeError{Kind: ProbeGitUnavailable, Path: abs, Err: err}
	}

	return LocalRepo{
		RemoteURL:     url,
		BaseCommit:    head,
		GateCandidate: gateCode == 0,
	}, nil
}

// chooseRemote 从 `git remote` 的输出里选一个采用：优先 origin，否则取第一项。
func chooseRemote(out string) (string, bool) {
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			names = append(names, s)
		}
	}
	for _, n := range names {
		if n == "origin" {
			return n, true
		}
	}
	if len(names) == 0 {
		return "", false
	}
	return names[0], true
}
