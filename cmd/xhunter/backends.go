package main

import (
	"xhunter/harness"
	"xhunter/internal/git/cli"
	"xhunter/internal/workspace/osfs"
)

// 本文件装配两个"真正动东西"的协作者：**文件操作**与 **git 操作**。
//
// 它们是两套契约的实现，而契约本身在内核里——内核对"文件系统是本地磁盘"、
// "版本控制是 git 命令行"这两件事一无所知，它只知道工作区长什么样（只读视图 +
// 唯一写入原语）、历史怎么动（取基线、提交、取 diff、回收）。因此：
//
//   - 换后端（内存镜像、远端快照，或别的版本控制）只改这里一行；
//   - 原语只面对 Workspace 接口，写盘只经过 Committer——它们连 Storage 都拿不到；
//   - 装配的是"参数归一后的实现"，真正的动作发生在任务运行期
//     （那时才知道是哪个仓库、哪条分支、哪个基线）。

// defaultWorkspaces 给出文件操作的实现：本地文件系统。
func defaultWorkspaces() harness.WorkspaceOpener {
	return osfs.Opener{}
}

// defaultGit 给出 git 操作的实现：命令行。
func defaultGit() harness.GitWorktree {
	return cli.New(cli.Config{})
}
