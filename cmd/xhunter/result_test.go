package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"xhunter/git"
	"xhunter/harness"
	"xhunter/hunt"
)

func readResultFile(t *testing.T, path string) resultFile {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读结果文件失败：%v", err)
	}
	var r resultFile
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("解析结果文件失败：%v", err)
	}
	return r
}

// 结果文件里的两类清单是三态（FR-6.3、使用手册 §6）：
//   - 有自陈 → 原样写出，且 blocked 不带 error（它不是失败）；
//   - 没提供 → **写成 null，不是 []、也不是缺字段**：空数组会被读成"没有需要补全的条件"，
//     那是另一句话。
func TestResultFile_DeclarationsAreThreeState(t *testing.T) {
	bounty := hunt.Bounty{ID: "b1", Repo: git.RepoRef{Branch: "xhunter/x", BaseCommit: "abc"}}

	t.Run("有自陈", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "r.json")
		out := harness.Outcome{
			Status: harness.StatusBlocked, Reason: "needs_input", ExitCode: harness.ExitOK,
		}
		declared := hunt.Declared{
			Needs:       []string{"缺 A"},
			Assumptions: []string{"假定 B"},
		}
		if err := writeRunOutputs(path, "", bounty, out, hunt.Delivery{}, declared); err != nil {
			t.Fatalf("写结果文件失败：%v", err)
		}

		got := readResultFile(t, path)
		if got.Needs == nil || len(*got.Needs) != 1 || (*got.Needs)[0] != "缺 A" {
			t.Errorf("needs = %v，期望 [缺 A]", got.Needs)
		}
		if got.Assumptions == nil || len(*got.Assumptions) != 1 || (*got.Assumptions)[0] != "假定 B" {
			t.Errorf("assumptions = %v，期望 [假定 B]", got.Assumptions)
		}
		if got.Status != string(harness.StatusBlocked) || got.ExitCode != int(harness.ExitOK) {
			t.Errorf("blocked 必须是退出码 0：status=%s exit=%d", got.Status, got.ExitCode)
		}
		if got.Error != nil {
			t.Errorf("blocked 不是失败，不该带 error：%+v", got.Error)
		}
	})

	t.Run("没自陈写成 null", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "r.json")
		out := harness.Outcome{Status: harness.StatusSucceeded, Reason: "no_tool_call", ExitCode: harness.ExitOK}
		if err := writeRunOutputs(path, "", bounty, out, hunt.Delivery{}, hunt.Declared{}); err != nil {
			t.Fatalf("写结果文件失败：%v", err)
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读结果文件失败：%v", err)
		}
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(raw, &probe); err != nil {
			t.Fatalf("解析结果文件失败：%v", err)
		}
		for _, key := range []string{"needs", "assumptions"} {
			v, ok := probe[key]
			if !ok {
				t.Fatalf("%s 字段必须存在——三态靠 null 表达，不是靠缺字段", key)
			}
			if string(v) != "null" {
				t.Errorf("%s = %s，期望 null（不是 []）", key, v)
			}
		}
	})
}
