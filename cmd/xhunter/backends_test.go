package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"xhunter/ext"
	"xhunter/ext/syntax"
	"xhunter/harness"
	"xhunter/workspace"
)

// 本文件钉住「符号后端由部署事实选出」这一件事：选谁、指纹与宿主同源、
// 选的过程不启动进程，以及换后端不改工具面（IA-7.1 的另一半）。

// mapLookup 把一份 map 当成环境来源：用例因此不依赖跑它的那台机器上真有什么变量。
func mapLookup(env map[string]string) lookupEnv {
	return func(name string) (string, bool) {
		v, ok := env[name]
		return v, ok
	}
}

// mustChooseExt 选出后端，写错即用例失败。
func mustChooseExt(t *testing.T, env map[string]string) extChoice {
	t.Helper()
	choice, err := chooseExt(mapLookup(env))
	if err != nil {
		t.Fatalf("选择符号后端失败：%v", err)
	}
	return choice
}

func openTestWorkspace(t *testing.T) workspace.Workspace {
	t.Helper()
	ws, err := defaultWorkspaces().Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开工作区失败：%v", err)
	}
	return ws
}

// 不配外挂命令 = 用内置语法级后端（FR-13.9）：那是首发后端，也是"没有外部依赖"的那一档。
func TestChooseExt_UnconfiguredIsTheBuiltInBackend(t *testing.T) {
	choice := mustChooseExt(t, nil)
	if !slices.Equal(choice.fp, syntax.FingerprintTokens()) {
		t.Fatalf("指纹应与内置后端同源，实际 %v", choice.fp)
	}
	caps := choice.new(openTestWorkspace(t)).Capabilities(context.Background())
	if !caps.Available {
		t.Fatal("内置后端应可用：否则这一次装配其实等于没有符号能力")
	}
	if caps.Precision != ext.PrecisionSyntactic {
		t.Errorf("内置后端是语法级，实际 %q", caps.Precision)
	}
}

// 配了外挂命令 = 走外挂通道。**选择本身不启动进程**（懒启动，FR-13.5）：
// 为一个还没用上的能力开子进程，会让"没用过符号能力"的运行也拖上一个进程。
//
// 反证用的是副作用：`touch <marker>` 只有在进程真的被拉起时才会留痕。
func TestChooseExt_ConfiguredIsTheExternalChannelAndDoesNotStartIt(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	choice := mustChooseExt(t, map[string]string{envExtCommand: "touch", envExtArgs: marker})

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("选后端不该启动进程：懒启动要求首次符号调用才拉起（FR-13.5）")
	}
	// 指纹来自装配事实而不是问来的——问就得先握手，握手就得先启动。
	if !slices.Contains(choice.fp, "ext:touch") {
		t.Fatalf("外挂后端的指纹应来自装配事实，实际 %v", choice.fp)
	}
	// 反证另一半：这个外挂后端此刻确实没在线（命令没被拉起），能力如实报不可用——
	// 那是可判定的事实，符号原语据此给结构化错误，而不是让符号能力假装在线。
	if choice.new(nil).Capabilities(context.Background()).Available {
		t.Fatal("尚未拉起的外挂后端应报不可用")
	}
}

// 「工具面恒定」的另一半（IA-7.1）：换**后端种类**也不改工具面。
//
// 同一份装配在「内置语法级后端 / 外挂 MCP 后端」两情形下，模型的工具面必须完全相同——
// 名字集合与 schema 逐项相同。工具面随后端增删，会让同一份提示词在不同机器上指向不同能力。
func TestChooseExt_ToolFaceIsIdenticalAcrossBackends(t *testing.T) {
	ws := openTestWorkspace(t)
	builtin := defaultTools(ws, mustChooseExt(t, nil).new(ws), nil)
	external := defaultTools(ws, mustChooseExt(t, map[string]string{envExtCommand: "touch"}).new(ws), nil)

	if len(builtin) != len(external) {
		t.Fatalf("工具面数量随后端变化：内置 %d / 外挂 %d", len(builtin), len(external))
	}
	for i := range builtin {
		a, b := builtin[i].Decl(), external[i].Decl()
		if a.Name != b.Name {
			t.Fatalf("第 %d 个原语名字随后端变化：%q vs %q", i+1, a.Name, b.Name)
		}
		if string(a.Schema) != string(b.Schema) {
			t.Errorf("%s 的参数形状随后端变化：%s vs %s", a.Name, a.Schema, b.Schema)
		}
		if a.Description != b.Description {
			t.Errorf("%s 的说明随后端变化", a.Name)
		}
	}
}

// 部署事实的读法：不配取默认，配了就原样带过去。
func TestParseExtConfig_ReadsDeploymentFacts(t *testing.T) {
	cfg, err := parseExtConfig(mapLookup(map[string]string{
		envExtCommand:   "xhunter-ext",
		envExtArgs:      "--stdio  --lang=go",
		envExtTimeout:   "15s",
		envExtLanguages: "go, rust",
		envExtEnv:       "PATH",
		"PATH":          "/usr/bin:/bin",
	}))
	if err != nil {
		t.Fatalf("解析部署事实失败：%v", err)
	}
	if cfg.Command != "xhunter-ext" {
		t.Errorf("命令 = %q", cfg.Command)
	}
	if !slices.Equal(cfg.Args, []string{"--stdio", "--lang=go"}) {
		t.Errorf("入参 = %v", cfg.Args)
	}
	if cfg.Timeout != 15*time.Second {
		t.Errorf("超时 = %v", cfg.Timeout)
	}
	if !slices.Equal(cfg.Languages, []string{"go", "rust"}) {
		t.Errorf("语言清单 = %v", cfg.Languages)
	}
	// 环境是**点名授予**：只拿点名且存在的那一项，值沿用当前环境里的那一份。
	want := []string{"PATH=/usr/bin:/bin"}
	if !slices.Equal(cfg.Env, want) {
		t.Errorf("授予的环境 = %v，期望 %v", cfg.Env, want)
	}
}

// 「不配」与「配错」必须分开：配错即启动期失败（退出 1）。
// 一个拼错的部署事实若被当成"没配"，会静默退化成"符号能力不可用"——
// 而那时模型已经按符号寻址失败改走文本路径，没人会回头看那行环境变量。
func TestParseExtConfig_RejectsMalformedFacts(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"超时不是时间长度", map[string]string{envExtTimeout: "abc"}},
		{"超时为 0", map[string]string{envExtTimeout: "0"}},
		{"超时为负", map[string]string{envExtTimeout: "-1s"}},
		{"语言清单有空项", map[string]string{envExtLanguages: "go,,rust"}},
		{"环境变量名有空项", map[string]string{envExtEnv: "PATH,,HOME"}},
		{"点名了不存在的环境变量", map[string]string{envExtEnv: "NOPE_NOT_SET"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{envExtCommand: "xhunter-ext"}
			for k, v := range tc.env {
				env[k] = v
			}
			if _, err := parseExtConfig(mapLookup(env)); err == nil {
				t.Fatalf("应拒绝这一组部署事实：%v", env)
			}
		})
	}
}

// 选错后端同样在启动期失败：chooseExt 是装配层的入口，错误从这里出、不再往里走。
func TestChooseExt_RejectsMalformedDeploymentFacts(t *testing.T) {
	if _, err := chooseExt(mapLookup(map[string]string{
		envExtCommand: "xhunter-ext", envExtTimeout: "很快",
	})); err == nil {
		t.Fatal("部署事实写错应即失败")
	}
}

// 生效配置快照报的是**实际装配的那个**后端（指纹与宿主同源）：换后端而不体现在快照里，
// 精度档位变了却报不出来，是诊断时最难查的一类事（FR-13.7 / IA-6.5）。
func TestAssemblyFacts_ReportsTheAssembledBackend(t *testing.T) {
	builtin := assemblyFacts(harness.DefaultConfig(), mustChooseExt(t, nil))
	external := assemblyFacts(harness.DefaultConfig(), mustChooseExt(t, map[string]string{
		envExtCommand: "xhunter-ext", envExtLanguages: "go",
	}))

	if !slices.Equal(builtin.Ext, syntax.FingerprintTokens()) {
		t.Fatalf("内置后端的快照指纹应与它自述的一致，实际 %v", builtin.Ext)
	}
	if slices.Equal(builtin.Ext, external.Ext) {
		t.Fatalf("换后端后快照应报出不同指纹，实际两边都是 %v", builtin.Ext)
	}
	if !slices.Contains(external.Ext, "ext:xhunter-ext") {
		t.Errorf("外挂后端的指纹应带上后端标识，实际 %v", external.Ext)
	}
	if !slices.Contains(external.Ext, "lang:go") {
		t.Errorf("外挂后端的指纹应带上覆盖语言，实际 %v", external.Ext)
	}
}
