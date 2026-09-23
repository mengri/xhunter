package hunt

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"xhunter/harness"
	"xhunter/workspace"
)

// 生效配置快照是"本次 Hunt 实际用了哪套规则"的只读事实。本文件用假 sink、假 git/工作区
// 装配一次 Prepare（不联网、不碰真实模型），断言快照与起飞事件。

// namedPlugin 是带自述名的提示词插件桩：快照里报的就是这个名字。
type namedPlugin struct {
	name string
	body string
}

func (p namedPlugin) Name() string { return p.name }
func (p namedPlugin) Build(context.Context, PromptInput) (PromptPart, error) {
	return PromptPart{Body: p.body}, nil
}

// prepareConfig 给出一份可跑通 Prepare 的装配：真 git/工作区换成本包内桩，其余都是
// 快照要观察的件——工具面、两段插件、过滤器链、装配层注入事实。
func prepareConfig(sink EventSink) Config {
	return Config{
		Bounty: Bounty{
			ID: "b1", Task: "首行任务\n第二行细节", Repo: gitRepoRef(),
			Budget: Budget{MaxTurns: 3, MaxTokens: 1000},
		},
		Git:    &stubBaselineGit{},
		Opener: stubOpener{},
		Policy: allowAll{},
		Sink:   sink,
		Tools: func(workspace.Workspace) []Primitive {
			return []Primitive{
				stubPrim{name: "read"}, stubPrim{name: "write"}, stubPrim{name: "edit"},
				stubPrim{name: "find"}, stubPrim{name: "glob"},
			}
		},
		SystemPlugins: func(workspace.Workspace) []PromptPlugin {
			return []PromptPlugin{namedPlugin{name: "agentsmd", body: "约定"}, namedPlugin{name: "skills", body: "清单"}}
		},
		UserPlugins: func(workspace.Workspace) []PromptPlugin {
			return []PromptPlugin{namedPlugin{name: "task", body: "任务"}}
		},
		Filters: []NamedFilter{
			{Name: "redact", Run: func(context.Context, *harness.Turn) error { return nil }},
			{Name: "truncate", Run: func(context.Context, *harness.Turn) error { return nil }},
		},
		Assembly: AssemblyFacts{
			Policy:     map[string]any{"default": "deny", "write_protected": ".xhunter"},
			Checkpoint: "on_structure",
			Ext:        []string{"go"},
			Platform:   "linux/amd64",
		},
	}
}

// 快照里的原语顺序 == 定格后的工具面顺序，且含殿后追加的 checkpoint。
func TestEffectiveConfig_ListsPrimitivesInToolFaceOrder(t *testing.T) {
	s := NewSession(prepareConfig(&captureSink{}))
	if err := s.Prepare(context.Background(), &harness.Run{}); err != nil {
		t.Fatalf("Prepare 失败：%v", err)
	}

	got := s.EffectiveConfig().Primitives
	want := []string{"read", "write", "edit", "find", "glob", string(PrimCheckpoint)}
	if !slices.Equal(got, want) {
		t.Errorf("primitives = %v，期望 %v（顺序即工具面顺序，checkpoint 殿后）", got, want)
	}
}

// 两段插件名与顺序、过滤器链名如实上报：装配顺序即生效顺序。
func TestEffectiveConfig_CarriesPluginAndFilterNames(t *testing.T) {
	s := NewSession(prepareConfig(&captureSink{}))
	if err := s.Prepare(context.Background(), &harness.Run{}); err != nil {
		t.Fatalf("Prepare 失败：%v", err)
	}
	got := s.EffectiveConfig()

	if !slices.Equal(got.SystemPlugins, []string{"agentsmd", "skills"}) {
		t.Errorf("system_plugins = %v，期望 [agentsmd skills]", got.SystemPlugins)
	}
	if !slices.Equal(got.UserPlugins, []string{"task"}) {
		t.Errorf("user_plugins = %v，期望 [task]", got.UserPlugins)
	}
	if !slices.Equal(got.Filters, []string{"redact", "truncate"}) {
		t.Errorf("filters = %v，期望 [redact truncate]（顺序即生效顺序）", got.Filters)
	}
}

// 装配层注入的策略/检查点/扩展/平台原样透传——Session 不猜这些事实。
func TestEffectiveConfig_CarriesAssemblyFacts(t *testing.T) {
	s := NewSession(prepareConfig(&captureSink{}))
	if err := s.Prepare(context.Background(), &harness.Run{}); err != nil {
		t.Fatalf("Prepare 失败：%v", err)
	}
	got := s.EffectiveConfig()

	if got.Policy["default"] != "deny" || got.Policy["write_protected"] != ".xhunter" {
		t.Errorf("policy 未如实透传：%v", got.Policy)
	}
	if got.Checkpoint != "on_structure" {
		t.Errorf("checkpoint = %q，期望 on_structure", got.Checkpoint)
	}
	if !slices.Equal(got.Ext, []string{"go"}) {
		t.Errorf("ext = %v，期望 [go]", got.Ext)
	}
	if got.Platform != "linux/amd64" {
		t.Errorf("platform = %q，期望 linux/amd64", got.Platform)
	}
	// 预算也进快照：0 = 不限这一口径要能被读出来（此处两项非 0，墙钟不限）。
	if got.Budget.MaxTurns != 3 || got.Budget.MaxTokens != 1000 || got.Budget.MaxWallClock != 0 {
		t.Errorf("budget = %+v，期望 {3 1000 0}", got.Budget)
	}
}

// Prepare 成功后立即发一条（且只发一条）hunt_start，载荷含生效配置快照；失败不发。
func TestPrepare_EmitsHuntStart(t *testing.T) {
	ctx := context.Background()

	t.Run("成功即发", func(t *testing.T) {
		sink := &captureSink{}
		s := NewSession(prepareConfig(sink))
		if err := s.Prepare(ctx, &harness.Run{}); err != nil {
			t.Fatalf("Prepare 失败：%v", err)
		}

		starts := sink.ofType("hunt_start")
		if len(starts) != 1 {
			t.Fatalf("应恰好一条 hunt_start，实际 %d：%v", len(starts), sink.events)
		}
		p := starts[0]
		if p["session_id"] != "b1" {
			t.Errorf("session_id = %v，期望 b1", p["session_id"])
		}
		if p["base_commit"] != gitRepoRef().BaseCommit {
			t.Errorf("base_commit = %v，期望 %q", p["base_commit"], gitRepoRef().BaseCommit)
		}
		if p["branch"] != gitRepoRef().Branch {
			t.Errorf("branch = %v，期望 %q", p["branch"], gitRepoRef().Branch)
		}
		if p["task"] != "首行任务" {
			t.Errorf("task 应取正文首行并限长：%v", p["task"])
		}
		ec, ok := p["effective_config"].(EffectiveConfig)
		if !ok {
			t.Fatalf("载荷应带 effective_config 快照，实得 %T", p["effective_config"])
		}
		if !slices.Equal(ec.Primitives, s.EffectiveConfig().Primitives) {
			t.Errorf("事件里的快照必须与 Session 冻结的那份同源：%v vs %v", ec.Primitives, s.EffectiveConfig().Primitives)
		}
	})

	t.Run("失败不发", func(t *testing.T) {
		sink := &captureSink{}
		cfg := prepareConfig(sink)
		cfg.Git = nil // 装配缺件：Prepare 在起飞前失败
		s := NewSession(cfg)
		if err := s.Prepare(ctx, &harness.Run{}); err == nil {
			t.Fatal("缺 git 必须失败")
		}
		if n := len(sink.ofType("hunt_start")); n != 0 {
			t.Errorf("Prepare 失败不得发起飞事件，实际 %d 条", n)
		}
		if len(s.EffectiveConfig().Primitives) != 0 {
			t.Errorf("Prepare 未成功时快照应为零值：%+v", s.EffectiveConfig())
		}
	})
}

// 快照里的集合在"空"时也必须是**空数组**而非 nil：nil 序列化成 null，被读成"未提供／不知道有没有"。
// 三期集合由 `pluginNames` / 构造保证非 nil；`ext` 由装配层给，冻结处兜底归一。
func TestEffectiveConfig_AbsentCollectionsAreEmptyArraysNotNil(t *testing.T) {
	s := NewSession(Config{
		Bounty: Bounty{ID: "b1", Task: "t", Repo: gitRepoRef()},
		Git:    &stubBaselineGit{},
		Opener: stubOpener{},
		Policy: allowAll{},
		Sink:   &captureSink{},
		// 刻意不给 Tools / 插件 / 过滤器 / Assembly.Ext：三方集合都应是空数组。
	})
	if err := s.Prepare(context.Background(), &harness.Run{}); err != nil {
		t.Fatalf("Prepare 失败：%v", err)
	}

	got := s.EffectiveConfig()
	for _, c := range []struct {
		name string
		v    []string
	}{
		{"system_plugins", got.SystemPlugins},
		{"user_plugins", got.UserPlugins},
		{"filters", got.Filters},
		{"ext", got.Ext},
	} {
		if c.v == nil {
			t.Errorf("%s 必须是空数组而非 nil（nil → null，被读成「未提供」）", c.name)
		}
	}

	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	for _, key := range []string{"system_plugins", "user_plugins", "filters", "ext"} {
		if strings.Contains(string(b), `"`+key+`":null`) {
			t.Errorf("%s 不得序列化成 null（空数组 = 已知的「没有」）：%s", key, b)
		}
	}
	if !strings.Contains(string(b), `"ext":[]`) {
		t.Errorf("ext 应序列化成 []：%s", b)
	}
}
