package hunt

import (
	"fmt"

	"xhunter/llm"
	"xhunter/workspace"
)

// Committer 把原语产出的编辑计划落到盘上，是「写盘唯一入口」。
//
// 三个写类原语都只产出 FileEdit，执行全部汇到这里，所以「写盘只有一个地方」是结构
// 事实。它只依赖 workspace.Storage 接口——落在哪个后端上是注入的事实。
type Committer struct {
	storage workspace.Storage
	ledger  *Ledger
}

func newCommitter(storage workspace.Storage, ledger *Ledger) *Committer {
	return &Committer{storage: storage, ledger: ledger}
}

// Commit 对编辑计划**先全部校验、再依次落盘**：
//
//	① 判断新建还是改写    ② 改写的必须已读且指纹未变    ③ 校验字节区间
//	④ 执行区间替换        ⑤ 更新台账为新指纹            ⑥ 产出写操作记录
//
// 第 ② 步是核心：它同时挡住「凭想象编辑没读过的文件」和「基于读过之后又被改动的
// 内容落笔」两种情形。
//
// 校验与落盘分两趟：一次调用可能带多份编辑，若边校验边写，"第三份不合法"会让前两份
// 已经落盘却因为返回错误而**不被记入台账与写操作记录**——盘上有改动、台账里没有，
// 这正是"写盘只有一个入口"要防的那类不一致。两趟之后，校验失败即零落盘。
func (cm *Committer) Commit(edits []workspace.FileEdit) ([]WriteOp, error) {
	plan := make([]writePlan, 0, len(edits))
	for _, e := range edits {
		step, err := cm.prepare(e)
		if err != nil {
			return nil, err
		}
		plan = append(plan, step)
	}

	ops := make([]WriteOp, 0, len(plan))
	for _, step := range plan {
		newFP, err := cm.storage.WriteRange(step.edit.File, step.edit.ByteRange, step.edit.NewContent)
		if err != nil {
			return nil, err
		}
		cm.ledger.Mark(step.edit.File, newFP)
		ops = append(ops, WriteOp{
			File:      step.edit.File,
			ByteRange: step.edit.ByteRange,
			Before:    step.before,
			After:     step.edit.NewContent,
		})
	}
	return ops, nil
}

// writePlan 是一份通过校验、等待落盘的编辑。
type writePlan struct {
	edit   workspace.FileEdit
	before string
}

// prepare 校验一份编辑并读出被替换的内容。
func (cm *Committer) prepare(e workspace.FileEdit) (writePlan, error) {
	info, err := cm.storage.Stat(e.File)
	if err != nil {
		return writePlan{}, err
	}

	if !info.Exists {
		// 新建只表达为「往空文件的开头插入」：区间非空说明调用方以为文件已经存在。
		if e.ByteRange != (workspace.ByteRange{Start: 0, End: 0}) {
			return writePlan{}, &llm.Fault{Kind: "not_found",
				Message: "目标不存在，且区间非空：" + e.File, Retryable: true}
		}
		return writePlan{edit: e}, nil
	}

	fp, ok := cm.ledger.Fingerprint(e.File)
	if !ok {
		return writePlan{}, &llm.Fault{Kind: "not_read",
			Message: "未读即写：" + e.File + " 必须先读取再修改", Retryable: true}
	}
	cur, err := cm.storage.Read(e.File, workspace.LineRange{})
	if err != nil {
		return writePlan{}, err
	}
	if cur.Fingerprint != fp {
		return writePlan{}, &llm.Fault{Kind: "stale_read",
			Message: "文件在读取之后已被改动，请重新读取：" + e.File, Retryable: true}
	}
	if br := e.ByteRange; br.Start < 0 || br.End > len(cur.Raw) || br.Start > br.End {
		return writePlan{}, &llm.Fault{Kind: "bad_range",
			Message: fmt.Sprintf("字节区间 [%d,%d) 超出 %s 的长度 %d", br.Start, br.End, e.File, len(cur.Raw))}
	}
	return writePlan{edit: e, before: sliceBytes(cur.Raw, e.ByteRange)}, nil
}

// sliceBytes 取字节区间；越界返回空串（调用方只把它当「改前内容」的留档）。
func sliceBytes(s string, br workspace.ByteRange) string {
	if br.Start < 0 || br.End > len(s) || br.Start > br.End {
		return ""
	}
	return s[br.Start:br.End]
}
