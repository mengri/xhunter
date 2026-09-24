package hunt

import (
	"context"
	"strings"
	"testing"

	"xhunter/ext"
	"xhunter/harness"
)

// 本文件补的是「检查点连败上限」里**用常量符号断言就漏掉**的两处：
// 阈值本身的字面值、以及守卫次序（连败必须在预算之前）。
// 二者都用变异实验验证过：改常量 / 换次序会让这里的用例变红。

// 连败上限的阈值本身就是要守住的契约（连续 3 次），不是实现细节。既有
// TestCheckpoint_StreakLimitConvergesAsEnvError 拿 checkpointFailStreakLimit 当期望值，
// 改常量会跟着漂——这里把字面值钉死。
func TestCheckpointFailStreakLimitIsThree(t *testing.T) {
	if checkpointFailStreakLimit != 3 {
		t.Fatalf("连败上限 = %d，契约是「连续 3 次」", checkpointFailStreakLimit)
	}
}

// 阈值边界：连续第 2 次失败**不**收敛，第 3 次才收敛。用字面轮次驱动守卫（不引用常量），
// 因此把上限从 3 改成别的值会立刻变红。
func TestOnTurn_CommitStreakConvergesExactlyAtThirdFailure(t *testing.T) {
	s := NewSession(Config{
		Bounty: Bounty{ID: "b1", Task: "t", Repo: gitRepoRef()},
		Policy: allowAll{},
		Sink:   &captureSink{},
	})

	// 连续第 2 次失败：未达上限 → 继续、不设终态。
	s.commitFailStreak = 2
	run := &harness.Run{}
	cont, err := s.OnTurn(context.Background(), run, &harness.Turn{No: 2})
	if err != nil {
		t.Fatalf("OnTurn 失败：%v", err)
	}
	if !cont || run.Terminal != nil {
		t.Errorf("连续 2 次失败不该收敛：cont=%v terminal=%+v", cont, run.Terminal)
	}

	// 连续第 3 次：达上限 → 本轮结束即收敛为环境错误（退出 1）。
	s.commitFailStreak = 3
	run = &harness.Run{}
	cont, err = s.OnTurn(context.Background(), run, &harness.Turn{No: 3})
	if err != nil {
		t.Fatalf("OnTurn 失败：%v", err)
	}
	if cont {
		t.Error("连续 3 次失败应在本轮结束即收敛")
	}
	out := run.Outcome()
	if !strings.HasPrefix(out.Reason, checkpointFailedStreak) {
		t.Errorf("原因应以 %q 开头：%q", checkpointFailedStreak, out.Reason)
	}
	if out.ExitCode != harness.ExitEnv {
		t.Errorf("退出码 = %d，期望 %d（环境问题、可重跑）", out.ExitCode, harness.ExitEnv)
	}
}

// 守卫次序：连败达上限与预算耗尽**同时**成立时，终态必须是更根因的 checkpoint_failed_streak，
// 而不是 budget_exhausted——"远端不可达"是修好环境就能接着跑的诊断（退出 1、可重派），被
// "预算耗尽"（退出 2、不重派）盖掉会让平台误判。次序固定为「取消 → 通道 → 提交连败 → 预算」。
func TestOnTurn_CommitStreakOutranksBudget(t *testing.T) {
	s := NewSession(Config{
		Bounty: Bounty{ID: "b1", Task: "t", Repo: gitRepoRef()},
		Policy: turnsBudgetPolicy{limit: 1}, // 预算本轮也正好耗尽
		Sink:   &captureSink{},
	})
	s.commitFailStreak = checkpointFailStreakLimit // 连败也已达上限

	run := &harness.Run{}
	cont, err := s.OnTurn(context.Background(), run, &harness.Turn{No: 1})
	if err != nil {
		t.Fatalf("OnTurn 失败：%v", err)
	}
	if cont {
		t.Error("达上限应停")
	}
	out := run.Outcome()
	if !strings.HasPrefix(out.Reason, checkpointFailedStreak) {
		t.Errorf("终态原因 = %q，期望以 %q 开头（连败是更根因的诊断，不该被预算盖掉）",
			out.Reason, checkpointFailedStreak)
	}
	if out.ExitCode != harness.ExitEnv {
		t.Errorf("退出码 = %d，期望 %d", out.ExitCode, harness.ExitEnv)
	}
}

// 「一次成功的往返即自愈」里的"成功"指提交这个**动作成立**，不是"真的造出了提交"：
// `Created=false` 的空操作（本轮无新改动）同样把连败归零。判据若错取成"是否 Created"，
// 空操作的下一轮就会被上一次的失败连累、误判成连败。
func TestCheckpoint_StreakResetsOnEmptyCommitToo(t *testing.T) {
	s := NewSession(Config{
		Bounty: Bounty{ID: "b", Task: "t", Repo: gitRepoRef()},
		Git:    &recordingCommitGit{created: false}, // 提交不报错，但没有造出新提交
		Policy: allowAll{},
		Sink:   &captureSink{},
	})
	s.ext = &fakeExt{parses: []ext.ParseVerdict{ext.ParseOK}}
	s.ops = []WriteOp{{File: "a.txt"}}
	s.commitFailStreak = 2 // 先摆一个连败水位

	s.checkpoint(context.Background(), &harness.Turn{No: 1})
	if s.commitFailStreak != 0 {
		t.Errorf("Created=false 的空操作也是一次成功往返，连败应归零：%d", s.commitFailStreak)
	}
}
