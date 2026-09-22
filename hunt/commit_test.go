package hunt

import (
	"errors"
	"testing"

	"xhunter/llm"
	"xhunter/workspace"
)

// Committer 是「写盘唯一入口」，它挡的两件事（凭想象编辑、基于过期内容覆盖）此前
// 没有用例（架构 §14.2 自认）。这里把它守住的每条边界都钉住。

func newCommitterFor(t *testing.T, files map[string]string, reads ...string) (*Committer, *memStorage, *Ledger) {
	t.Helper()
	st := &memStorage{files: files}
	ledger := &Ledger{}
	cm := newCommitter(st, ledger)
	for _, f := range reads {
		fc, err := st.Read(f, workspace.LineRange{})
		if err != nil {
			t.Fatalf("预读 %s 失败：%v", f, err)
		}
		ledger.Mark(f, fc.Fingerprint)
	}
	return cm, st, ledger
}

// 未读即写被拒：这是"凭想象编辑"的唯一拦法。
func TestCommitter_RejectsWriteWithoutPriorRead(t *testing.T) {
	cm, st, _ := newCommitterFor(t, map[string]string{"a.txt": "hello"})

	_, err := cm.Commit([]workspace.FileEdit{{File: "a.txt", ByteRange: workspace.ByteRange{Start: 0, End: 5}, NewContent: "bye"}})
	var fault *llm.Fault
	if !errors.As(err, &fault) || fault.Kind != "not_read" {
		t.Fatalf("未读即写应报 not_read：%v", err)
	}
	if !fault.Retryable {
		t.Error("未读即写是可自愈的（读一次即可），应标可重试")
	}
	if st.files["a.txt"] != "hello" {
		t.Errorf("被拒的编辑不得落盘：%q", st.files["a.txt"])
	}
}

// 读后被外部改动：落笔会覆盖别人的改动，必须拒绝。
func TestCommitter_RejectsStaleRead(t *testing.T) {
	cm, st, _ := newCommitterFor(t, map[string]string{"a.txt": "hello"}, "a.txt")
	// 模拟"读过之后文件被别人改了"：绕过台账直接改盘。
	st.files["a.txt"] = "HELLO!"

	_, err := cm.Commit([]workspace.FileEdit{{File: "a.txt", ByteRange: workspace.ByteRange{Start: 0, End: 5}, NewContent: "bye"}})
	var fault *llm.Fault
	if !errors.As(err, &fault) || fault.Kind != "stale_read" {
		t.Fatalf("读后被改动应报 stale_read：%v", err)
	}
	if st.files["a.txt"] != "HELLO!" {
		t.Errorf("被拒的编辑不得落盘：%q", st.files["a.txt"])
	}

	// 重新读取之后即可落笔（自愈路径）。
	fc, _ := st.Read("a.txt", workspace.LineRange{})
	cm.ledger.Mark("a.txt", fc.Fingerprint)
	if _, err := cm.Commit([]workspace.FileEdit{{File: "a.txt", ByteRange: workspace.ByteRange{Start: 0, End: 5}, NewContent: "bye"}}); err != nil {
		t.Fatalf("重读后应可写入：%v", err)
	}
	if st.files["a.txt"] != "bye!" {
		t.Errorf("落盘结果 = %q，期望 bye!", st.files["a.txt"])
	}
}

// 只改写目标字节区间，其余字节原样保留；写操作记录带改前内容。
func TestCommitter_OnlyReplacesTargetRange(t *testing.T) {
	cm, st, _ := newCommitterFor(t, map[string]string{"a.txt": "0123456789"}, "a.txt")

	ops, err := cm.Commit([]workspace.FileEdit{{
		File: "a.txt", ByteRange: workspace.ByteRange{Start: 2, End: 4}, NewContent: "XY",
	}})
	if err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	if st.files["a.txt"] != "01XY456789" {
		t.Errorf("只应替换目标区间：%q", st.files["a.txt"])
	}
	if len(ops) != 1 {
		t.Fatalf("应有一条写操作记录：%+v", ops)
	}
	if ops[0].Before != "23" || ops[0].After != "XY" {
		t.Errorf("写操作记录应带改前/改后内容：%+v", ops[0])
	}
}

// 越界区间在落盘之前就被拒（校验与落盘分两趟）。
func TestCommitter_RejectsOutOfRange(t *testing.T) {
	cm, st, _ := newCommitterFor(t, map[string]string{"a.txt": "abc"}, "a.txt")
	_, err := cm.Commit([]workspace.FileEdit{{File: "a.txt", ByteRange: workspace.ByteRange{Start: 1, End: 99}, NewContent: "x"}})
	var fault *llm.Fault
	if !errors.As(err, &fault) || fault.Kind != "bad_range" {
		t.Fatalf("越界区间应报 bad_range：%v", err)
	}
	if st.files["a.txt"] != "abc" {
		t.Errorf("被拒的编辑不得落盘：%q", st.files["a.txt"])
	}
}

// 批量编辑是"先全部校验、再依次落盘"：后一份不合法时，前一份也不得落盘——
// 否则盘上有改动、台账与写操作记录里却没有，正是唯一写盘入口要防的不一致。
func TestCommitter_BatchIsAllOrNothingOnValidation(t *testing.T) {
	cm, st, _ := newCommitterFor(t, map[string]string{"a.txt": "abc"}, "a.txt")

	_, err := cm.Commit([]workspace.FileEdit{
		{File: "new.txt", ByteRange: workspace.ByteRange{}, NewContent: "按说会先写"},
		{File: "a.txt", ByteRange: workspace.ByteRange{Start: 0, End: 99}, NewContent: "越界"},
	})
	if err == nil {
		t.Fatal("批量里有一份不合法就该整体拒绝")
	}
	if _, exists := st.files["new.txt"]; exists {
		t.Errorf("校验阶段失败时不得有任何落盘：%+v", st.files)
	}
	if st.files["a.txt"] != "abc" {
		t.Errorf("原文件不该被改动：%q", st.files["a.txt"])
	}
}

// 新建只建新文件：目标已存在（且有区间）与目标不存在却给了区间，都要显式拒绝。
func TestCommitter_NewFileRules(t *testing.T) {
	cm, st, _ := newCommitterFor(t, map[string]string{})

	// 不存在 + 空区间 = 新建。
	ops, err := cm.Commit([]workspace.FileEdit{{File: "n.txt", ByteRange: workspace.ByteRange{}, NewContent: "hi"}})
	if err != nil || len(ops) != 1 {
		t.Fatalf("新建应成功：err=%v ops=%+v", err, ops)
	}
	if st.files["n.txt"] != "hi" {
		t.Errorf("新建内容不对：%q", st.files["n.txt"])
	}
	if ops[0].Before != "" {
		t.Errorf("新建的改前内容应为空：%+v", ops[0])
	}

	// 不存在 + 非空区间 = 调用方以为文件存在。
	_, err = cm.Commit([]workspace.FileEdit{{File: "m.txt", ByteRange: workspace.ByteRange{Start: 0, End: 3}, NewContent: "x"}})
	var fault *llm.Fault
	if !errors.As(err, &fault) || fault.Kind != "not_found" {
		t.Fatalf("应报 not_found：%v", err)
	}
}

// 写入后台账更新为新指纹：同一文件可以连续编辑，而不必每次重读。
func TestCommitter_MarksNewFingerprintAfterWrite(t *testing.T) {
	cm, st, ledger := newCommitterFor(t, map[string]string{"a.txt": "abc"}, "a.txt")

	if _, err := cm.Commit([]workspace.FileEdit{{File: "a.txt", ByteRange: workspace.ByteRange{Start: 3, End: 3}, NewContent: "d"}}); err != nil {
		t.Fatalf("首次写入失败：%v", err)
	}
	fp, ok := ledger.Fingerprint("a.txt")
	if !ok || fp != st.files["a.txt"] {
		t.Fatalf("台账应更新为新指纹：%q vs %q", fp, st.files["a.txt"])
	}
	if _, err := cm.Commit([]workspace.FileEdit{{File: "a.txt", ByteRange: workspace.ByteRange{Start: 4, End: 4}, NewContent: "e"}}); err != nil {
		t.Fatalf("同一文件连续编辑不该要求重读：%v", err)
	}
	if st.files["a.txt"] != "abcde" {
		t.Errorf("连续编辑结果 = %q", st.files["a.txt"])
	}
}
