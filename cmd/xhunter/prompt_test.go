package main

import "testing"

// 装配顺序就是执行顺序：这份清单是"提示词由哪几段、以什么次序拼成"的唯一定义处，
// 顺序一变提示词就变，因此把它钉住。
//
// 两段各自的内容也是契约：system 段放跨任务稳定的素材（约定 + 技能清单），
// user 段放每次投递不同的任务陈述（FR-7.1、FR-7.6）。
func TestDefaultPromptPlugins_OrderIsThePromptOrder(t *testing.T) {
	system, user := defaultSystemPlugins(nil), defaultUserPlugins(nil)

	wantSystem := []string{"agentsmd", "skills"}
	if len(system) != len(wantSystem) {
		t.Fatalf("system 段插件数 = %d，期望 %d", len(system), len(wantSystem))
	}
	for i, p := range system {
		if got := p.Name(); got != wantSystem[i] {
			t.Errorf("system 段第 %d 个插件 = %s，期望 %s", i+1, got, wantSystem[i])
		}
	}

	wantUser := []string{"task"}
	if len(user) != len(wantUser) {
		t.Fatalf("user 段插件数 = %d，期望 %d（任务陈述不可缺）", len(user), len(wantUser))
	}
	for i, p := range user {
		if got := p.Name(); got != wantUser[i] {
			t.Errorf("user 段第 %d 个插件 = %s，期望 %s", i+1, got, wantUser[i])
		}
	}
}

// 同一个插件实例只应出现一次：重复挂上会让同一份内容进提示词两遍，
// 白白吃掉预算，还可能让模型以为那是一条更重要的指令。
func TestDefaultPromptPlugins_NoDuplicate(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range defaultSystemPlugins(nil) {
		k := p.Name()
		if seen[k] {
			t.Errorf("插件 %s 在两段中重复出现", k)
		}
		seen[k] = true
	}
	for _, p := range defaultUserPlugins(nil) {
		k := p.Name()
		if seen[k] {
			t.Errorf("插件 %s 在两段中重复出现", k)
		}
		seen[k] = true
	}
}
