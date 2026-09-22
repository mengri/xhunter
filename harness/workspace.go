package harness

import "errors"

// 工作区契约：文件操作的**抽象**在这里，实现不在。
//
// 上层（原语、环节、引擎）只认这两个接口，看不见"本地文件系统"这件事——
// 换成远端快照、内存镜像或别的后端，是换一个实现，调用方一行不动。
//
// 写入只有一条路径：原语产出 FileEdit，落盘统一走 Committer。因此"写盘只有一处"
// 是结构事实，而不是每个原语各自遵守的纪律。

// Workspace 是原语能看到的文件视图，只有读操作。
//
// 所有入参都是工作区**相对路径**——"绝对路径"这个概念根本不在接口上，
// 所以原语实现没有办法表达"读工作区之外的东西"，这不是被检查拦下来的，
// 而是没有写法。
type Workspace interface {
	Read(rel string, r LineRange) (FileContent, error)
	Stat(rel string) (FileInfo, error)
	List(pattern string) ([]string, error)
}

// Storage 是引擎内部入口：读视图加上唯一的写入原语。
// 它是 Workspace 的超集，只交给落盘逻辑使用，不给原语。
type Storage interface {
	Workspace
	WriteRange(rel string, br ByteRange, content string) (string, error)
}

// WorkspaceOpener 把"工作树根"变成一个可读写的工作区。
//
// 它是**文件操作的装配点**：内核只规定工作区长什么样（Workspace / Storage），
// 具体由谁实现由装配层决定并在这里注入。工作区根是基线准备好之后才存在的运行期
// 产物，因此它的创建只能发生在这个时点——构造期根本没有根可传。
type WorkspaceOpener interface {
	Open(root string) (Storage, error)
}

type FileContent struct {
	Path        string
	Raw         string
	Fingerprint string
	Truncated   bool // Raw 是否只是文件的一部分
	TotalLines  int  // 截断时给出总量，让调用方知道丢了多少
}

type FileInfo struct {
	Path   string
	Size   int64
	Exists bool
}

// ErrNotExist 用于"目标不存在"这一类可自愈的情形；具体实现可以给出更细的
// 结构化错误（如带可重试标记），调用方按 ToolError 处理即可。
var ErrNotExist = errors.New("文件不存在")

// ============================================================ 唯一写入原语

// Committer 把原语产出的编辑计划落到盘上。
//
// 三个写类原语（新建、改写、重命名）都只产出 FileEdit，执行全部汇到这里，
// 所以"写盘只有一个地方"是结构事实。反过来若让每个原语自己写盘，
// 写入口就有了三个，任何一处漏掉校验都会成为缺口。
//
// 它只依赖 Storage 接口——**落在哪个后端上是注入的事实**，因此这套校验
// （改前必读、指纹过期、区间替换）对任何实现都一致。
type Committer struct {
	storage Storage
	ledger  *Ledger
}

// NewCommitter 构造写入器：它把落盘与台账绑在一起，两者必须同源。
func NewCommitter(storage Storage, ledger *Ledger) *Committer {
	return &Committer{storage: storage, ledger: ledger}
}

// Commit 对每个 FileEdit 依次做六件事：
//
//	① 判断新建还是改写    ② 改写的必须已读且指纹未变    ③ 读出被替换的内容
//	④ 执行区间替换        ⑤ 更新台账为新指纹            ⑥ 产出写操作记录
//
// 第 ② 步是这套机制的核心：它同时挡住了"凭想象编辑没读过的文件"和
// "基于读过之后又被改动的内容落笔"两种情形。
func (cm *Committer) Commit(edits []FileEdit) ([]WriteOp, error) {
	ops := make([]WriteOp, 0, len(edits))
	for _, e := range edits {
		info, err := cm.storage.Stat(e.File)
		if err != nil {
			return nil, err
		}

		if !info.Exists {
			// 新建只能是"往空文件的开头插入"；区间非空说明调用方以为是改写。
			if e.ByteRange != (ByteRange{Start: 0, End: 0}) {
				return nil, &ToolError{Kind: "not_found",
					Message: "目标不存在，且区间非空：" + e.File, Retryable: true}
			}
		} else {
			fp, ok := cm.ledger.Fingerprint(e.File)
			if !ok {
				return nil, &ToolError{Kind: "not_read",
					Message: "未读即写：" + e.File + " 必须先读取再修改", Retryable: true}
			}
			cur, err := cm.storage.Read(e.File, LineRange{})
			if err != nil {
				return nil, err
			}
			if cur.Fingerprint != fp {
				return nil, &ToolError{Kind: "stale_read",
					Message: "文件在读取之后已被改动，请重新读取：" + e.File, Retryable: true}
			}
		}

		before := ""
		if info.Exists {
			if cur, err := cm.storage.Read(e.File, LineRange{}); err == nil {
				before = sliceBytes(cur.Raw, e.ByteRange)
			}
		}

		newFP, err := cm.storage.WriteRange(e.File, e.ByteRange, e.NewContent)
		if err != nil {
			return nil, err
		}
		// 写完之后目标内容已经变了，台账要跟上，否则同一轮内的下一次编辑会被误判为过期。
		cm.ledger.Mark(e.File, newFP)

		ops = append(ops, WriteOp{
			File:      e.File,
			ByteRange: e.ByteRange,
			Before:    before,
			After:     e.NewContent,
		})
	}
	return ops, nil
}

// sliceBytes 取字节区间；越界返回空串（调用方只把它当"改前内容"的留档）。
func sliceBytes(s string, br ByteRange) string {
	if br.Start < 0 || br.End > len(s) || br.Start > br.End {
		return ""
	}
	return s[br.Start:br.End]
}
