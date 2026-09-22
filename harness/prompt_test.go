package harness

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// stubPrompt 是一个构造插件的桩：记录被调用次数与最近一次看到的输入，
// 用于验证"只取一次"以及"插件确实拿到了任务事实"。
type stubPrompt struct {
	part   PromptPart
	err    error
	calls  int
	lastIn PromptInput
}

func (s *stubPrompt) Build(_ context.Context, in PromptInput) (PromptPart, error) {
	s.calls++
	s.lastIn = in
	return s.part, s.err
}

func newPromptContext(system, user []PromptPlugin, sink EventSink) *Context {
	return &Context{
		Ctx:  context.Background(),
		Hunt: &Hunt{ledger: NewLedger()},
		Deps: Deps{SystemPlugins: system, UserPlugins: user, Sink: sink},
	}
}

// 同一阶段的多个插件按装配顺序拼接。顺序反了就会变成"任务描述排在工作区约定之前"
// 这类只在读提示词时才看得出来的问题，所以它必须是装配期的事实，而不是运行期的偶然。
func TestPromptBuild_ConcatenatesPluginsInAssemblyOrder(t *testing.T) {
	a := &stubPrompt{part: PromptPart{Body: " 第一段 ", Sources: []string{"base:aaa"}}}
	b := &stubPrompt{part: PromptPart{Body: "第二段", Sources: []string{"base:bbb"}}}
	c := &stubPrompt{part: PromptPart{Body: "任务描述", Sources: []string{"bounty"}}}
	ctx := newPromptContext([]PromptPlugin{a, b}, []PromptPlugin{c}, &stubSink{})

	handlePromptBuild(ctx)

	if ctx.Hunt.Terminal != nil {
		t.Fatalf("构造成功不该有终态：%+v", ctx.Hunt.Terminal)
	}
	// 每段各调用一次：初始化阶段取一次，此后整任务内不再刷新。
	if a.calls != 1 || b.calls != 1 || c.calls != 1 {
		t.Errorf("调用次数 = %d/%d/%d，期望各 1 次", a.calls, b.calls, c.calls)
	}
	if got, want := ctx.Hunt.Prompt.System.Body, "第一段\n\n第二段"; got != want {
		t.Errorf("system 正文 = %q，期望 %q", got, want)
	}
	if got, want := ctx.Hunt.Prompt.User.Body, "任务描述"; got != want {
		t.Errorf("user 正文 = %q，期望 %q", got, want)
	}
	if got, want := ctx.Hunt.Prompt.System.Sources, []string{"base:aaa", "base:bbb"}; !reflect.DeepEqual(got, want) {
		t.Errorf("system 来源 = %v，期望 %v", got, want)
	}
	// 两段互不串味。
	if strings.Contains(ctx.Hunt.Prompt.System.Body, "任务描述") {
		t.Error("system 正文里混进了 user 段的内容")
	}
}

// 同一个插件挂在两段上是合法用法（约定进 system、任务背景进 user 可以是同一实现）。
// 调用两次是"每段各取一次"，不是重复取——两段本来就是两次独立的构造。
func TestPromptBuild_SamePluginCanServeBothStages(t *testing.T) {
	p := &stubPrompt{part: PromptPart{Body: "同一段文本"}}
	ctx := newPromptContext([]PromptPlugin{p}, []PromptPlugin{p}, &stubSink{})

	handlePromptBuild(ctx)

	if p.calls != 2 {
		t.Errorf("调用次数 = %d，期望 2（每段各调用一次）", p.calls)
	}
	if ctx.Hunt.Prompt.System.Body != "同一段文本" || ctx.Hunt.Prompt.User.Body != "同一段文本" {
		t.Errorf("两段正文 = %q / %q", ctx.Hunt.Prompt.System.Body, ctx.Hunt.Prompt.User.Body)
	}
}

// 插件靠任务事实决定读哪份约定，靠工具面决定怎么写工具纪律——
// 工具面必须是定格后的那一份，否则就会出现"正文陈述的工具面"与"实际注册的工具面"
// 不一致的漂移，而这正是把构造排在 ext.caps 之后要消除的东西。
func TestPromptBuild_PassesTaskFactsAndFrozenToolFace(t *testing.T) {
	p := &stubPrompt{part: PromptPart{Body: "x"}}
	ctx := newPromptContext([]PromptPlugin{p}, nil, &stubSink{})
	ctx.Hunt.Bounty = Bounty{ID: "B-1", Task: "改个 bug"}
	ctx.Hunt.ToolDecls = []ToolDecl{{Name: "read"}}

	handlePromptBuild(ctx)

	if p.lastIn.Bounty.ID != "B-1" || p.lastIn.Bounty.Task != "改个 bug" {
		t.Errorf("插件看到的任务 = %+v", p.lastIn.Bounty)
	}
	if len(p.lastIn.Tools) != 1 || p.lastIn.Tools[0].Name != "read" {
		t.Errorf("插件看到的工具面 = %+v", p.lastIn.Tools)
	}
}

// 不接插件不是错误（本地试跑、测试都可能不接），但必须显式记一条 warn——
// 静默地少掉项目约定，产出会与仓库约定不符，而这类偏差要到评审时才看得出来。
func TestPromptBuild_WithoutPluginsOnlyWarns(t *testing.T) {
	sink := &stubSink{}
	ctx := newPromptContext(nil, nil, sink)

	handlePromptBuild(ctx)

	if ctx.Hunt.Terminal != nil {
		t.Errorf("未接入插件不该判死：%+v", ctx.Hunt.Terminal)
	}
	if ctx.Hunt.Prompt.System.Body != "" || ctx.Hunt.Prompt.User.Body != "" {
		t.Errorf("没有插件时不该凭空有正文：%+v", ctx.Hunt.Prompt)
	}
	if !sink.warned() {
		t.Error("不接插件必须告警")
	}
}

// 降级记录必须进事件流，而且**不随空正文一起被丢掉**：
// 一个把候选素材全部判为非法、因而输出空正文的插件，恰恰最需要被看见。
func TestPromptBuild_NoticesAreEmittedEvenWithEmptyBody(t *testing.T) {
	sink := &stubSink{}
	ctx := newPromptContext([]PromptPlugin{
		&stubPrompt{part: PromptPart{Notices: []Notice{
			{Scope: "prompt.skills", Subject: ".xhunter/skills/bad/SKILL.md", Reason: "缺少 description"},
		}}},
	}, nil, sink)

	handlePromptBuild(ctx)

	var got []ExternalEvent
	for _, ev := range sink.events {
		if ev.Type == "degraded" {
			got = append(got, ev)
		}
	}
	if len(got) != 1 {
		t.Fatalf("降级事件数 = %d，期望 1（空正文不该吞掉降级记录）", len(got))
	}
	if got[0].Payload["scope"] != "prompt.skills" ||
		got[0].Payload["subject"] != ".xhunter/skills/bad/SKILL.md" ||
		got[0].Payload["reason"] != "缺少 description" {
		t.Errorf("降级事件载荷不完整：%+v", got[0].Payload)
	}
}

// 插件挂着、但还没给出内容，与根本没接插件的后果完全一样：模型看不到项目约定。
// 所以告警看的是"两段是否都为空"，而不是"插件清单是否为空"——否则一个入口就位、
// 实现待补的插件会安安静静地少掉一整份约定。
func TestPromptBuild_PluginsWithoutContentStillWarn(t *testing.T) {
	sink := &stubSink{}
	ctx := newPromptContext([]PromptPlugin{
		&stubPrompt{},                              // 入口在，正文待补
		&stubPrompt{part: PromptPart{Body: "   "}}, // 只有空白也算没内容
	}, nil, sink)

	handlePromptBuild(ctx)

	if ctx.Hunt.Terminal != nil {
		t.Errorf("空正文不该判死：%+v", ctx.Hunt.Terminal)
	}
	if !sink.warned() {
		t.Error("两段正文都为空时必须告警——否则'缺了项目约定'与'这个仓库本来没有约定'长得一模一样")
	}
}

// 一个插件失败即整段作废：留着已经拼好的前半段继续跑，等于在少一份约定的情况下
// 开工，且没有任何一步会报错。
func TestPromptBuild_PluginFailureIsEnvError(t *testing.T) {
	bad := &stubPrompt{err: errors.New("基线不可读")}
	good := &stubPrompt{part: PromptPart{Body: "第二段"}}
	ctx := newPromptContext([]PromptPlugin{bad, good}, nil, &stubSink{})

	handlePromptBuild(ctx)

	if ctx.Hunt.Terminal == nil {
		t.Fatal("构造失败必须显式收敛，不能带着半段提示词继续跑")
	}
	if ctx.Hunt.Terminal.Code != ExitEnv {
		t.Errorf("退出码 = %v，期望 %v（环境问题，修好后重派有意义）", ctx.Hunt.Terminal.Code, ExitEnv)
	}
	if !strings.Contains(ctx.Hunt.Terminal.Reason, "prompt_build_failed: system") {
		t.Errorf("原因 = %q，应指明是哪一段失败的", ctx.Hunt.Terminal.Reason)
	}
	if ctx.Hunt.Prompt.System.Body != "" {
		t.Errorf("失败时不该留下半段正文：%q", ctx.Hunt.Prompt.System.Body)
	}
}

func TestJoinPromptParts_SkipsEmptyContributions(t *testing.T) {
	got := joinPromptParts([]PromptPart{
		{Body: "   ", Sources: []string{"nothing"}},
		{Body: "甲"},
		{Body: ""},
		{Body: "乙", Sources: []string{"base:abc"}},
	})
	if want := "甲\n\n乙"; got.Body != want {
		t.Errorf("拼接结果 = %q，期望 %q（没有内容的插件不该留下空行）", got.Body, want)
	}
	if want := []string{"base:abc"}; !reflect.DeepEqual(got.Sources, want) {
		t.Errorf("来源 = %v，期望 %v（没贡献正文的插件，其来源也不该进快照）", got.Sources, want)
	}
}

// 生效配置快照要能回答"这次用的是哪份约定、哪个版本的提示词"——来源因此一并定格，
// 事后不必回溯当时的工作区状态。带上阶段前缀，才能看出某份约定到底挂了没有。
func TestConfigSnapshot_RecordsPromptSources(t *testing.T) {
	sys := &stubPrompt{part: PromptPart{Body: "约定", Sources: []string{"base:abc123"}}}
	usr := &stubPrompt{part: PromptPart{Body: "任务", Sources: []string{"bounty:B-1"}}}
	sink := &stubSink{}
	ctx := newPromptContext([]PromptPlugin{sys}, []PromptPlugin{usr}, sink)

	handlePromptBuild(ctx)
	handleConfigSnapshot(ctx)

	var got []string
	for _, ev := range sink.events {
		if ev.Type == "config_snapshot" {
			got, _ = ev.Payload["prompt_sources"].([]string)
		}
	}
	want := []string{"system:base:abc123", "user:bounty:B-1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("快照里的提示词来源 = %v，期望 %v", got, want)
	}
}
