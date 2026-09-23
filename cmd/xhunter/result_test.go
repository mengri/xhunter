package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

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
		if err := writeRunOutputs(path, "", bounty, out, hunt.Delivery{}, declared, hunt.EffectiveConfig{}); err != nil {
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
		if err := writeRunOutputs(path, "", bounty, out, hunt.Delivery{}, hunt.Declared{}, hunt.EffectiveConfig{}); err != nil {
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

// 生效配置快照写进结果文件（FR-11.6）：评审者据此回答"这次用的是哪套规则"。原语顺序
// 必须如实反映定格后的工具面（含殿后的 checkpoint）；预算以可读形状出现（0 = 不限、
// 墙钟换算成毫秒）；没有快照（未进入对话）时字段省略，不摆空壳。
func TestResultFile_EffectiveConfigIsWritten(t *testing.T) {
	bounty := hunt.Bounty{ID: "b1", Repo: git.RepoRef{Branch: "xhunter/x", BaseCommit: "abc"}}
	out := harness.Outcome{Status: harness.StatusSucceeded, ExitCode: harness.ExitOK}

	t.Run("有快照", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "r.json")
		ec := hunt.EffectiveConfig{
			Primitives:    []string{"read", "write", "edit", "checkpoint"},
			SystemPlugins: []string{"agentsmd", "skills"},
			UserPlugins:   []string{"task"},
			Filters:       []string{},
			Policy:        map[string]any{"default": "deny"},
			Budget:        hunt.Budget{MaxTurns: 5, MaxWallClock: 90 * time.Minute},
			Checkpoint:    "on_structure",
			Ext:           []string{},
			Platform:      "linux/amd64",
		}
		if err := writeRunOutputs(path, "", bounty, out, hunt.Delivery{}, hunt.Declared{}, ec); err != nil {
			t.Fatalf("写结果文件失败：%v", err)
		}

		got := readResultFile(t, path)
		if got.EffectiveConfig == nil {
			t.Fatal("有快照时必须写出 effective_config")
		}
		want := []string{"read", "write", "edit", "checkpoint"}
		if !slices.Equal(got.EffectiveConfig.Primitives, want) {
			t.Errorf("primitives = %v，期望 %v（顺序即工具面顺序）", got.EffectiveConfig.Primitives, want)
		}
		if got.EffectiveConfig.Policy["default"] != "deny" {
			t.Errorf("policy 未如实写出：%v", got.EffectiveConfig.Policy)
		}

		// 预算的可读形状：0 = 不限；墙钟换算成毫秒，而不是一串纳秒数字。
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读结果文件失败：%v", err)
		}
		var probe struct {
			EC struct {
				Budget map[string]any `json:"budget"`
			} `json:"effective_config"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			t.Fatalf("解析结果文件失败：%v", err)
		}
		if probe.EC.Budget["max_turns"] != float64(5) {
			t.Errorf("budget.max_turns = %v，期望 5", probe.EC.Budget["max_turns"])
		}
		if probe.EC.Budget["max_wall_clock_ms"] != float64(5400000) {
			t.Errorf("budget.max_wall_clock_ms = %v，期望 5400000（90 分钟的毫秒数）", probe.EC.Budget["max_wall_clock_ms"])
		}
	})

	t.Run("无快照字段省略", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "r.json")
		if err := writeRunOutputs(path, "", bounty, out, hunt.Delivery{}, hunt.Declared{}, hunt.EffectiveConfig{}); err != nil {
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
		if _, ok := probe["effective_config"]; ok {
			t.Error("无快照时不得出现 effective_config——空壳会被读成另一句话")
		}
	})
}
