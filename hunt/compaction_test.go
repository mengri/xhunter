package hunt

import (
	"context"
	"testing"

	"xhunter/ext"
	"xhunter/git"
	"xhunter/harness"
	"xhunter/llm"
	"xhunter/workspace"
)

// 本文件钉住压缩的**交接面**：压缩发生在 `ContextBuilder` 内部，事件出口在 Session 上，
// 中间靠 `Compaction` / `TakeCompactions` 交接——执行体只搬运，不重算、不合并。

// compactionContext 是带压缩上报面的上下文替身：每次取走交出一批预设结论。
type compactionContext struct {
	prompt   []llm.Message
	turns    []harness.Turn
	batch    []Compaction
	overHard bool
}

func (c *compactionContext) SetPrompt(msgs []llm.Message) { c.prompt = msgs }
func (c *compactionContext) Assemble() []llm.Message      { return c.prompt }
func (c *compactionContext) Append(rec harness.Turn)      { c.turns = append(c.turns, rec) }
func (c *compactionContext) TakeCompactions() []Compaction {
	out := c.batch
	c.batch = nil
	return out
}
func (c *compactionContext) OverHardLimit() bool { return c.overHard }

// plainContext 只实现 `ContextBuilder`：不做压缩，因此也不实现上报面——
// "没有压缩"的如实形态是**不发事件**，不是发一条空事件。
type plainContext struct {
	prompt []llm.Message
	turns  []harness.Turn
}

func (c *plainContext) SetPrompt(msgs []llm.Message) { c.prompt = msgs }
func (c *plainContext) Assemble() []llm.Message      { return c.prompt }
func (c *plainContext) Append(rec harness.Turn)      { c.turns = append(c.turns, rec) }

// 压缩事件的载荷：层、释放量、触发水位一并发（FR-14.7）；**取走即清空**——
// 同一条结论发两次等于把一次压缩记成两次。
func TestCompaction_EventCarriesLevelAndReleasedTokens(t *testing.T) {
	sink := &captureSink{}
	ctx := &compactionContext{batch: []Compaction{
		{Level: "L2", ReleasedTokens: 1200, Watermark: "warn"},
		{Level: "L3", ReleasedTokens: 3400, Watermark: "warn"},
	}}
	s := NewSession(Config{
		Bounty: Bounty{ID: "b", Task: "t", Repo: gitRepoRef()},
		Git:    &countingGit{}, Policy: allowAll{}, Sink: sink, Context: ctx,
	})
	s.emitCompactions()

	ev := sink.ofType("context_compacted")
	if len(ev) != 2 {
		t.Fatalf("每层一条，实际 %d 条：%v", len(ev), sink.events)
	}
	if ev[0]["level"] != "L2" || ev[0]["released_tokens"] != 1200 || ev[0]["watermark"] != "warn" {
		t.Errorf("载荷 = %v，期望 level=L2 / released_tokens=1200 / watermark=warn", ev[0])
	}
	if ev[1]["level"] != "L3" || ev[1]["released_tokens"] != 3400 {
		t.Errorf("第二层载荷 = %v", ev[1])
	}
	// 取走即清空：再搬一次不该重复发。
	s.emitCompactions()
	if n := len(sink.ofType("context_compacted")); n != 2 {
		t.Errorf("同一条结论不得发两次：%d 条", n)
	}
}

// 上报面是**可选**的：不实现的 `ContextBuilder` 不产生事件（也不报错）。
func TestCompaction_NoReporterMeansNoEvent(t *testing.T) {
	sink := &captureSink{}
	s := NewSession(Config{
		Bounty: Bounty{ID: "b", Task: "t", Repo: gitRepoRef()},
		Git:    &countingGit{}, Policy: allowAll{}, Sink: sink, Context: &plainContext{},
	})
	s.emitCompactions()
	if n := len(sink.ofType("context_compacted")); n != 0 {
		t.Errorf("没有上报面就不该有压缩事件：%v", sink.events)
	}
}

// 接线：轮边界组装下一轮输入之后立刻搬运（压缩是"这一轮刚发生"的事实，不能攒到收尾）。
func TestCompaction_EmittedOnTurnBoundary(t *testing.T) {
	sink := &captureSink{}
	ctx := &compactionContext{batch: []Compaction{{Level: "L0", ReleasedTokens: 80, Watermark: "warn"}}}
	s := NewSession(Config{
		Bounty: Bounty{ID: "b1", Task: "t", Repo: gitRepoRef()},
		Git:    &countingGit{}, Opener: stubOpener{}, Policy: allowAll{}, Sink: sink,
		Context: ctx,
		Tools:   func(workspace.Workspace, ext.ExtHost) []Primitive { return []Primitive{&writingPrim{}} },
	})
	provider := &stubProvider{turns: []turnScript{{calls: []llm.ToolCall{call("c1", "writer", `{}`)}}}}
	engine, err := harness.New(provider,
		[]harness.PrepareHandler{s.Prepare},
		[]harness.OnTurnHandler{s.OnTurn},
		[]harness.FinalHandler{s.Finalize})
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	engine.Run(context.Background(), harness.Input{})

	ev := sink.ofType("context_compacted")
	if len(ev) != 1 {
		t.Fatalf("轮边界应发出一条压缩事件，实际 %d 条：%v", len(ev), sink.events)
	}
	if ev[0]["level"] != "L0" || ev[0]["released_tokens"] != 80 {
		t.Errorf("载荷 = %v", ev[0])
	}
}

// deliveryGit 给出一份能取出交付物的 git 替身（改动清单与补丁来自仓库，不来自上下文）。
type deliveryGit struct{ stubBaselineGit }

func (deliveryGit) Diff(context.Context, git.RepoRef) ([]string, error) { return []string{"a.go"}, nil }
func (deliveryGit) Patch(context.Context, git.RepoRef) (string, error) {
	return "diff --git a/a.go", nil
}

// 交付物不依赖上下文（FR-14.6）：上下文被压到只剩提示词时，改动清单、补丁与自陈照旧交出——
// 清单与补丁来自仓库，自陈在**说出的当轮**就登记了，不靠收尾从上下文里捞。
func TestDeliverables_DoNotDependOnContext(t *testing.T) {
	sink := &captureSink{}
	// droppingContext 收下每一轮、但组装时只给提示词：等价于"历史被压空"。
	ctx := &plainContext{}
	s := NewSession(Config{
		Bounty: Bounty{ID: "b1", Task: "t", Repo: gitRepoRef()},
		Git:    deliveryGit{}, Opener: stubOpener{}, Policy: allowAll{}, Sink: sink,
		Context: ctx,
		Tools:   func(workspace.Workspace, ext.ExtHost) []Primitive { return []Primitive{&writingPrim{}} },
	})
	provider := &stubProvider{turns: []turnScript{{
		text:  "先改着\n\n## 需要补全\n- 缺数据库地址\n",
		calls: []llm.ToolCall{call("c1", "writer", `{}`)},
	}}}
	engine, err := harness.New(provider,
		[]harness.PrepareHandler{s.Prepare},
		[]harness.OnTurnHandler{s.OnTurn},
		[]harness.FinalHandler{s.Finalize})
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	engine.Run(context.Background(), harness.Input{})

	if len(ctx.turns) == 0 {
		t.Fatal("历史应当被收下（否则这条用例没有压到被测路径）")
	}
	for _, m := range ctx.Assemble() {
		if m.Role == llm.RoleAssistant || m.Role == llm.RoleTool {
			t.Errorf("上下文被压空时不该再带历史：%v", m.Role)
		}
	}
	d := s.Delivery()
	if len(d.Files) != 1 || d.Files[0] != "a.go" {
		t.Errorf("改动清单来自仓库，与上下文无关：%v", d.Files)
	}
	if d.Patch == "" {
		t.Error("补丁来自仓库，与上下文无关")
	}
	if needs := s.Declared().Needs; len(needs) != 1 || needs[0] != "缺数据库地址" {
		t.Errorf("自陈在当轮登记，不该被压缩带走：%v", needs)
	}
}

// 压完仍不低于硬上限 → 退出 2（usage§7“上下文达硬上限 → 按预算耗尽处理”）：
// 那是“任务自身的量不够了、重跑一样”，不是环境问题，因此不重派。
// 只把它记成 `watermark: hard` 却继续发，换来的是上游的报错——这一档必须真的会停。
func TestCompaction_OverHardLimitStopsTheRun(t *testing.T) {
	sink := &captureSink{}
	ctx := &compactionContext{
		overHard: true,
		batch:    []Compaction{{Level: "L3", ReleasedTokens: 900, Watermark: "hard"}},
	}
	s := NewSession(Config{
		Bounty: Bounty{ID: "b1", Task: "t", Repo: gitRepoRef()},
		Git:    &countingGit{}, Opener: stubOpener{}, Policy: allowAll{}, Sink: sink,
		Context: ctx,
		Tools:   func(workspace.Workspace, ext.ExtHost) []Primitive { return []Primitive{&writingPrim{}} },
	})
	provider := &stubProvider{turns: []turnScript{{calls: []llm.ToolCall{call("c1", "writer", `{}`)}}}}
	engine, err := harness.New(provider,
		[]harness.PrepareHandler{s.Prepare},
		[]harness.OnTurnHandler{s.OnTurn},
		[]harness.FinalHandler{s.Finalize})
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	engine.Run(context.Background(), harness.Input{})

	// 压了几层是**已发生的事实**，事件照发——终止不改写发生过什么。
	if n := len(sink.ofType("context_compacted")); n != 1 {
		t.Errorf("压缩事件照发，实际 %d 条：%v", n, sink.events)
	}
	ev := sink.ofType("hunt_end")
	if len(ev) != 1 {
		t.Fatalf("应有一条 hunt_end，实际 %d 条", len(ev))
	}
	if ev[0]["status"] != "failed" {
		t.Errorf("硬上限压不下来应判失败，实际 %v", ev[0]["status"])
	}
	if ev[0]["reason"] != "budget_exhausted:context" {
		t.Errorf("原因应为 budget_exhausted:context，实际 %v", ev[0]["reason"])
	}
}
