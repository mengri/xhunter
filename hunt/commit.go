package hunt

import (
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

// Commit 对每个 FileEdit 依次做六件事：
//
//	① 判断新建还是改写    ② 改写的必须已读且指纹未变    ③ 读出被替换的内容
//	④ 执行区间替换        ⑤ 更新台账为新指纹            ⑥ 产出写操作记录
//
// 第 ② 步是核心：它同时挡住「凭想象编辑没读过的文件」和「基于读过之后又被改动的
// 内容落笔」两种情形。
func (cm *Committer) Commit(edits []workspace.FileEdit) ([]WriteOp, error) {
	ops := make([]WriteOp, 0, len(edits))
	for _, e := range edits {
		info, err := cm.storage.Stat(e.File)
		if err != nil {
			return nil, err
		}

		if !info.Exists {
			if e.ByteRange != (workspace.ByteRange{Start: 0, End: 0}) {
				return nil, &llm.Fault{Kind: "not_found",
					Message: "目标不存在，且区间非空：" + e.File, Retryable: true}
			}
		} else {
			fp, ok := cm.ledger.Fingerprint(e.File)
			if !ok {
				return nil, &llm.Fault{Kind: "not_read",
					Message: "未读即写：" + e.File + " 必须先读取再修改", Retryable: true}
			}
			cur, err := cm.storage.Read(e.File, workspace.LineRange{})
			if err != nil {
				return nil, err
			}
			if cur.Fingerprint != fp {
				return nil, &llm.Fault{Kind: "stale_read",
					Message: "文件在读取之后已被改动，请重新读取：" + e.File, Retryable: true}
			}
		}

		before := ""
		if info.Exists {
			if cur, err := cm.storage.Read(e.File, workspace.LineRange{}); err == nil {
				before = sliceBytes(cur.Raw, e.ByteRange)
			}
		}

		newFP, err := cm.storage.WriteRange(e.File, e.ByteRange, e.NewContent)
		if err != nil {
			return nil, err
		}
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

// sliceBytes 取字节区间；越界返回空串（调用方只把它当「改前内容」的留档）。
func sliceBytes(s string, br workspace.ByteRange) string {
	if br.Start < 0 || br.End > len(s) || br.Start > br.End {
		return ""
	}
	return s[br.Start:br.End]
}
