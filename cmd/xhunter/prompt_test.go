package main

import (
	"testing"

	"xhunter/harness"
	"xhunter/prompt/agentsmd"
	"xhunter/prompt/skills"
)

// 装配顺序就是执行顺序：这份清单是"提示词由哪几段、以什么次序拼成"的唯一定义处，
// 顺序一变提示词就变，因此把它钉住。
func TestDefaultPromptPlugins_OrderIsThePromptOrder(t *testing.T) {
	system, user := defaultPromptPlugins()

	want := []string{"agentsmd", "skills"}
	if len(system) != len(want) {
		t.Fatalf("system 段插件数 = %d，期望 %d", len(system), len(want))
	}
	for i, p := range system {
		if got := kind(p); got != want[i] {
			t.Errorf("system 段第 %d 个插件 = %s，期望 %s", i+1, got, want[i])
		}
	}
	if len(user) != 0 {
		t.Errorf("user 段插件数 = %d，期望 0（本轮只接 system 段）", len(user))
	}
}

// 同一个插件实例只应出现一次：重复挂上会让同一份内容进提示词两遍，
// 白白吃掉预算，还可能让模型以为那是一条更重要的指令。
func TestDefaultPromptPlugins_NoDuplicate(t *testing.T) {
	system, _ := defaultPromptPlugins()
	seen := map[string]bool{}
	for _, p := range system {
		k := kind(p)
		if seen[k] {
			t.Errorf("插件 %s 在 system 段出现多次", k)
		}
		seen[k] = true
	}
}

// kind 由类型给出插件身份。刻意不给 harness.PromptPlugin 加 Name 方法——
// 插件身份要到真有人消费时才值得进公开契约，眼下只有装配可读性需要它。
func kind(p harness.PromptPlugin) string {
	switch p.(type) {
	case *agentsmd.Plugin:
		return "agentsmd"
	case *skills.Plugin:
		return "skills"
	default:
		return "unknown"
	}
}
