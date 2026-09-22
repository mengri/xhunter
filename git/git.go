// Package git 提供版本控制的抽象能力：基线获取、任务分支、提交、差异、回收。
//
// 它是「基础能力包」：只定义抽象接口与引用类型，不实现任何命令。底层用 git 命令行、
// 远端 API 还是别的版本控制，由装配层注入（见 internal/git/cli）。上层（业务层）只认
// 这套抽象，看不见「本地磁盘」「命令行」这些事实。
package git

import "context"

// RepoRef 指向远端仓库、任务分支与基线提交。
// 任务分支由业务层创建并推送——模型既看不到也改不了它，工具面上没有任何 git 操作。
type RepoRef struct {
	Remote     string
	Branch     string
	BaseCommit string
}

// Commit 是一次提交的结果。
type Commit struct {
	SHA    string
	Branch string
	// Created 报告这次调用**是否真的产生了新提交**。轮边界的检查点在"本轮无新改动"
	// 时是空操作（FR-1.3c「无新写操作不提交」），调用方据此决定怎么记日志与记账——
	// 空操作却打印"已创建检查点"是在假装干了活。
	Created bool
}

// GitWorktree 负责基线获取、任务分支与提交。
//
// PrepareBaseline 返回**工作区根路径**——这是它唯一一次向外交出这个信息，
// 拿到它的人是业务层内部，不是模型侧的任何组件。
type GitWorktree interface {
	PrepareBaseline(ctx context.Context, repo RepoRef) (workRoot string, err error)
	Commit(ctx context.Context, repo RepoRef, msg string) (Commit, error)
	// Diff 给出相对基线的改动文件清单；Patch 给出同一范围的统一 diff
	// （git apply 兼容）。两者都是附带交付物，失败不阻断主交付（分支 tip）。
	Diff(ctx context.Context, baseCommit string) ([]string, error)
	Patch(ctx context.Context, baseCommit string) (string, error)
	Clean(ctx context.Context) error
}
