package hunt

import (
	"context"

	"xhunter/llm"
	"xhunter/workspace"
)

// Primitive 是一个业务原语：模型可见的声明 + 执行一次调用的实现。
//
// 原语**自己决定寻址与降级**：写限定名走符号、写字面量走文本、符号不可用时降级——
// 这些是原语自身的性质，不经过任何统一分发层。因此一个原语是否支持符号化，看它的
// 实现，而不是一张寻址性质表。
//
// 原语只产出结果与编辑计划，绝不自己落盘——写盘统一由执行器（见 Session）经
// workspace 的写入原语完成，这样「写盘只有一个地方」是结构事实。
type Primitive interface {
	// Decl 返回模型可见的工具声明。
	Decl() llm.ToolDecl
	// Execute 执行一次调用。Edits 是编辑计划（可空），由执行器统一落盘。
	Execute(ctx context.Context, call Call, facts Facts) (Result, []workspace.FileEdit, error)
	// Writes 报告这个原语是否会产生写操作——**由原语自己声明**，而不是由策略侧维护一张
	// 跨包镜像表。理由与寻址、降级相同：它是原语自身的性质，换一份实现就可能换一个答案；
	// 镜像表与实现分处两个包，漂开之后不会以编译错误的形式暴露，只会让写原语被当成只读、
	// 绕开路径边界。
	Writes() bool
}

// Facts 是原语执行时能看到的任务级事实，刻意收窄：门禁清单、读台账、检查点意图。
// 工作区**不在这里**——原语在构造时就拿到工作区（见 basic/symbolic 的 Tool 构造），
// 它是构造参数，不是每次执行才补的运行时事实。
type Facts interface {
	Gates() []Gate
	Ledger() *Ledger
	RequestCheckpoint(summary string)
	CheckpointRequested() bool
	// WorkRoot 给出工作区根的**绝对路径**。原语拿不到它——工作区接口只接受相对路径，
	// 这是刻意的设计（模型写不出"工作区之外的路径"）；但门禁要在工作区里跑命令，
	// 命令需要一个真实目录，所以这一条事实由执行体给出、且只有执行体给得出。
	WorkRoot() string
	// ChangeFingerprint 给出**待提交改动**的内容指纹：门禁结果按它缓存——改动没变，
	// 就没必要再跑一次几十秒的测试。
	ChangeFingerprint() string
	// RecordGateResult 把一次门禁结论交回执行体：终态、检查点联动与结果文件都读它，
	// 因此"跑过什么、结论是什么"只有这一个来源。
	RecordGateResult(res GateResult)
}

// Ledger 记录「模型看过哪些文件、当时的内容指纹是什么」。
//
// 它同时解决两个问题：改一个没读过的文件应当被拒绝（避免凭想象编辑）；
// 读完之后文件又被改动过，落笔也应当被拒绝（避免基于过期内容覆盖别人的改动）。
// 零值即可用。
type Ledger struct {
	entries map[string]string
}

// Mark 登记一次读取，或一次写入后的新指纹。
func (l *Ledger) Mark(file, fingerprint string) {
	if l.entries == nil {
		l.entries = map[string]string{}
	}
	l.entries[file] = fingerprint
}

// Fingerprint 返回文件最近登记的内容指纹。
func (l *Ledger) Fingerprint(file string) (string, bool) {
	fp, ok := l.entries[file]
	return fp, ok
}
