package hunt

import (
	"xhunter/llm"
	"xhunter/workspace"
)

// ============================================================ 原语名

// PrimitiveName 是一个业务原语的名字。它是契约（对模型、对事件流），不是行为。
// 名字由业务层决定，harness 不做白名单。
type PrimitiveName string

const (
	// PrimCheckpoint 是检查点原语：模型用它表达「这里值得留检查点」的意图。
	// 它不直接写文件，语义只有一件事——把意图交给业务层；何时真正提交、提交信息
	// 怎么合成，由业务层的检查点钩子决定。
	PrimCheckpoint PrimitiveName = "checkpoint"
)

// ============================================================ 调用

// Call 是模型发出的一次业务调用。Target 是工作区内的相对路径，模型这一侧不存在
// 绝对路径、字节区间、分支名、凭据这些「资源层」概念。
//
// 三个具名参数各有槽位：NewName 是重命名的目标名、Gate 是要运行的具名门禁条目、
// Summary 是检查点的意图说明。
type Call struct {
	ID        ToolCallID
	Primitive PrimitiveName
	Target    string
	Selector  Selector
	Content   string
	NewName   string
	Gate      string
	Summary   string

	// Writes 由执行体在查表之后回填（模型给不出它：它不在参数形状里）。策略据此决定要
	// 不要做路径裁决；放在 Call 上而不是给 Policy 加参数，是为了让公开接口的签名保持稳定。
	Writes bool
}

// Selector 是原语的定位参数：字面量是内容寻址，限定名是符号寻址，行范围是区间读取。
// 模型用「表达方式」选择内部路径，不需要知道目标环境支持哪条。
type Selector struct {
	Literal  string               // 文本寻址：字面量片段
	Symbol   string               // 符号寻址：限定名
	InSymbol string               // 符号内相对定位
	Range    *workspace.LineRange // 行范围（读取）
	FileView bool                 // 读取符号视图（大纲）
	Scope    string               // 限定范围：文件 / 目录 / 包
}

// ============================================================ 结果与写记录

// Result 是一次调用的执行结果。
type Result struct {
	CallID  ToolCallID
	OK      bool
	Summary string
	Ops     []WriteOp
	Err     *llm.Fault
}

// WriteOp 是一次写操作记录：交付物、检查点与恢复都建立在它之上。
type WriteOp struct {
	Primitive PrimitiveName
	File      string
	ByteRange workspace.ByteRange
	Before    string
	After     string
	Turn      TurnNo
}
