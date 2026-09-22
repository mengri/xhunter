// Package workspace 提供文件操作的抽象能力：读文件、列目录、字节区间替换写盘。
//
// 它是「基础能力包」，不是业务层：只定义抽象接口与坐标类型，不关心谁在消费它
// （原语、插件都是消费者），也不关心底层是本地磁盘、远端快照还是内存镜像——
// 具体实现由装配层注入（见 internal/workspace/osfs）。
//
// 写入只有一条路径：上层产出 FileEdit，落盘统一走 Storage.WriteRange。因此「写盘
// 只有一个地方」是结构事实，而不是每个调用方各自遵守的纪律。
package workspace

import "errors"

// LineRange 是 1-based 闭区间行范围。零值表示整份内容。
type LineRange struct {
	From int
	To   int
}

// ByteRange 是半开字节区间 [Start, End)。它是「替换某个区间」的唯一表达。
type ByteRange struct {
	Start int
	End   int
}

// FileEdit 是唯一写入原语的输入：目标文件 + 字节区间 + 新内容。
//
// 形状刻意单一——只有「替换某个区间」这一种表达。若允许多种写入形态
// （整文件覆盖、追加、删除……），写入的语义就会分散到各个调用方里去。
type FileEdit struct {
	File       string
	ByteRange  ByteRange
	NewContent string
}

// FileContent 是一次读取的结果。Raw 可能只是文件的一部分（截断时 Truncated 置位）。
type FileContent struct {
	Path        string
	Raw         string
	Fingerprint string // 内容指纹，用于「改前必读」校验
	Truncated   bool   // Raw 是否只是文件的一部分
	TotalLines  int    // 截断时给出总量，让调用方知道丢了多少
}

// FileInfo 是一次 Stat 的结果，用于区分「新建」与「改写」。
type FileInfo struct {
	Path   string
	Size   int64
	Exists bool
}

// Workspace 是文件视图的只读部分：读、查、列。
//
// 所有入参都是工作区**相对路径**——「绝对路径」这个概念根本不在接口上，
// 所以调用方没有办法表达「读工作区之外的东西」，这不是被检查拦下来的，
// 而是没有写法。
type Workspace interface {
	Read(rel string, r LineRange) (FileContent, error)
	Stat(rel string) (FileInfo, error)
	List(pattern string) ([]string, error)
}

// Storage 是完整文件视图：只读部分加上唯一的写入原语。它是 Workspace 的超集，
// 只交给落盘逻辑使用，不给普通调用方。
type Storage interface {
	Workspace
	WriteRange(rel string, br ByteRange, content string) (string, error)
}

// WorkspaceOpener 把「工作树根」变成一个可读写的工作区。
//
// 它是文件操作的装配点：抽象只规定工作区长什么样（Workspace / Storage），
// 具体由谁实现由装配层决定并在这里注入。工作区根是运行期产物，因此它的创建
// 只能发生在「拿到根路径」这个时点——构造期根本没有根可传。
type WorkspaceOpener interface {
	Open(root string) (Storage, error)
}

// ErrNotExist 用于「目标不存在」这一类可自愈的情形；具体实现可以给出更细的
// 结构化错误，调用方按错误类型处理即可。
var ErrNotExist = errors.New("文件不存在")
