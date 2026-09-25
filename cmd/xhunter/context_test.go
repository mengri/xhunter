package main

import (
	"strings"
	"testing"

	"xhunter/harness"
	"xhunter/hunt"
	"xhunter/llm"
)

// 本文件钉住上下文压缩（FR-14）：水位触发与冷却、分层下压、投影可复现，以及**永不丢**那几条。
// 期望值一律用字面量——引用被测常量做期望值会把"常量被改"变绿。

// bigOutput 造一条足够长的工具输出（长度是断言的对象，用字面量重复串拼出来）。
func bigOutput(n int) string { return strings.Repeat("0123456789abcdef", n/16+1)[:n] }

// compactionFixture 给出一台「提示词 + N 轮历史」的组装器，每轮带一次工具调用与一条长输出。
func compactionFixture(cfg hunt.CompactionConfig, turns int, outLen int) *contextBuilder {
	c := &contextBuilder{cfg: cfg}
	c.SetPrompt([]llm.Message{{Role: llm.RoleSystem, Content: "任务：把 Alpha 的返回值改成 new\n验收：改动出现在交付提交里"}})
	for i := 0; i < turns; i++ {
		c.Append(harness.Turn{
			No:    i + 1,
			Text:  "我先读一下这个文件，看看它的结构。",
			Calls: []llm.ToolCall{{ID: "c" + string(rune('a'+i)), Name: "read", Arguments: []byte(`{"path":"big.txt"}`)}},
			Results: []llm.ToolResult{{
				CallID: "c" + string(rune('a'+i)),
				Output: bigOutput(outLen),
			}},
		})
	}
	return c
}

// 水位不触发就不压缩：低于预警线时既不下压也不发事件。
func TestWatermark_TriggersAtWarnAndCoolsDown(t *testing.T) {
	// 输出很短 → 远低于预警线。
	cfg := hunt.CompactionConfig{WarnAt: 100000, TargetAt: 70000, HardAt: 130000, Cooldown: 3}
	c := compactionFixture(cfg, 4, 64)
	c.Assemble()
	if got := c.TakeCompactions(); len(got) != 0 {
		t.Fatalf("未到预警线不该压缩：%+v", got)
	}

	// 同一台组装器把水位压到"必然命中"，并让输出足够长。
	c2 := compactionFixture(hunt.CompactionConfig{WarnAt: 40, TargetAt: 20, HardAt: 60, Cooldown: 3}, 4, 400)
	c2.Assemble()
	first := c2.TakeCompactions()
	if len(first) == 0 {
		t.Fatal("到预警线应触发压缩")
	}
	if c2.cooldown != 3 {
		t.Errorf("压完应进冷却（3 轮），实际 %d", c2.cooldown)
	}

	// 冷却期内再组装：即便仍在水位之上也不再动已缓存的前缀。
	c2.Assemble()
	if got := c2.TakeCompactions(); len(got) != 0 {
		t.Errorf("冷却期内不该再压缩：%+v", got)
	}
	// 冷却走完（每轮 Append 递减）→ 可以再压。
	for i := 0; i < 3; i++ {
		c2.Append(harness.Turn{No: 10 + i, Text: "继续", Results: []llm.ToolResult{{Output: bigOutput(400)}}})
	}
	c2.Assemble()
	if got := c2.TakeCompactions(); len(got) == 0 {
		t.Error("冷却结束后应可再次压缩")
	}
}

// 分层下压：按序 L0→L1→L2→L3，**够用即停**——压到目标水位以下就不再动更深的层。
func TestCompaction_LayersDownToFit(t *testing.T) {
	// 目标水位定在"L0 就够"的位置：分层表有四项，实际只该压一层。
	cfg := hunt.CompactionConfig{WarnAt: 2000, TargetAt: 2500, HardAt: 100000, Cooldown: 3}
	c := compactionFixture(cfg, 6, 2000)
	c.Assemble()

	var levels []string
	for _, got := range c.TakeCompactions() {
		levels = append(levels, got.Level)
	}
	if len(levels) != 1 || levels[0] != "L0" {
		t.Fatalf("够用即停：应只压 L0，实际 %v", levels)
	}
	if after := c.measure(); after >= cfg.TargetAt {
		t.Errorf("压后应落到目标水位以下：%d >= %d", after, cfg.TargetAt)
	}
}

// 目标水位压不到时一路压到最深（当前轮永不压，所以它压不到 0），但**不超出分层表**。
func TestCompaction_PressesThroughAllLayersWhenTargetIsUnreachable(t *testing.T) {
	cfg := hunt.CompactionConfig{WarnAt: 40, TargetAt: 1, HardAt: 200, Cooldown: 3}
	c := compactionFixture(cfg, 6, 2000)
	c.Assemble()

	var levels []string
	for _, got := range c.TakeCompactions() {
		levels = append(levels, got.Level)
	}
	want := []string{"L0", "L1", "L2", "L3"}
	if len(levels) != len(want) {
		t.Fatalf("分层数 = %v，期望恰好四层 %v", levels, want)
	}
	for i, l := range levels {
		if l != want[i] {
			t.Errorf("第 %d 层 = %q，期望 %q（分层顺序固定）：%v", i+1, l, want[i], levels)
		}
	}
	if c.log == "" {
		t.Error("压到 L3 应产出折叠工作日志")
	}
}

// 每一层释放的量是**同一把尺子的前后差**：不另估一次，因此不可能与事件里的数字对不上。
func TestCompaction_ReleasedTokensIsAMeasuredDelta(t *testing.T) {
	cfg := hunt.CompactionConfig{WarnAt: 40, TargetAt: 12, HardAt: 200, Cooldown: 3}
	c := compactionFixture(cfg, 6, 2000)
	before := c.measure()
	c.Assemble()
	total := 0
	for _, got := range c.TakeCompactions() {
		total += got.ReleasedTokens
	}
	if got := before - c.measure(); got != total {
		t.Errorf("事件里释放量之和 = %d，与前后差 %d 不符", total, got)
	}
}

// 投影可复现（AC-18）：同一份记录投影两次必须逐字相同，且不调用模型。
func TestCompaction_ProjectionIsReproducible(t *testing.T) {
	cfg := hunt.CompactionConfig{WarnAt: 40, TargetAt: 12, HardAt: 200, Cooldown: 3}
	build := func() string {
		c := compactionFixture(cfg, 5, 2000)
		c.opsSrc = func() []hunt.WriteOp {
			return []hunt.WriteOp{
				{Primitive: "write", File: "a.go"},
				{Primitive: "edit", File: "a.go"},
				{Primitive: "write", File: "b.txt"},
			}
		}
		c.Assemble()
		return c.log
	}
	first, second := build(), build()
	if first == "" {
		t.Fatal("L3 折叠后应产出工作日志")
	}
	if first != second {
		t.Errorf("同一份记录两次投影不一致：\n%q\n%q", first, second)
	}
	if !strings.Contains(first, "a.go：edit ×1、write ×1") {
		t.Errorf("工作日志应有按文件与工具统计的已完成改动：\n%s", first)
	}
	if !strings.Contains(first, "b.txt：write ×1") {
		t.Errorf("工作日志应覆盖全部改动文件：\n%s", first)
	}
}

// 永不丢（AC-17 / FR-14.3）：Bounty 正文（首轮提示词）、**当前轮**、写操作记录。
func TestCompaction_NeverDropsBountyOrWriteOps(t *testing.T) {
	cfg := hunt.CompactionConfig{WarnAt: 40, TargetAt: 12, HardAt: 200, Cooldown: 3}
	c := compactionFixture(cfg, 5, 2000)
	// 当前轮：正文与结果都必须在压后原样出现（模型正踩在上面）。
	current := harness.Turn{
		No:      6,
		Text:    "这一轮我正在改 Alpha。",
		Calls:   []llm.ToolCall{{ID: "z1", Name: "write", Arguments: []byte(`{"path":"a.go"}`)}},
		Results: []llm.ToolResult{{CallID: "z1", Output: "写入完成"}},
	}
	c.Append(current)
	c.opsSrc = func() []hunt.WriteOp {
		return []hunt.WriteOp{{Primitive: "write", File: "a.go"}}
	}
	msgs := c.Assemble()

	joined := make([]string, 0, len(msgs))
	for _, m := range msgs {
		joined = append(joined, m.Content)
		for _, r := range m.Results {
			joined = append(joined, r.Output)
		}
	}
	all := strings.Join(joined, "\n")
	if !strings.Contains(all, "任务：把 Alpha 的返回值改成 new") {
		t.Error("Bounty 正文（首轮提示词）永不裁剪")
	}
	if !strings.Contains(all, "这一轮我正在改 Alpha。") {
		t.Error("当前轮正文永不裁剪")
	}
	if !strings.Contains(all, "写入完成") {
		t.Error("当前轮的工具结果永不裁剪")
	}
	if !strings.Contains(c.log, "a.go：write ×1") {
		t.Errorf("写操作记录必须留在工作日志里：\n%s", c.log)
	}
}

// 硬上限档：撞到硬上限时事件里记的是 "hard"，不是常规预警。
func TestCompaction_HardWatermarkIsReported(t *testing.T) {
	cfg := hunt.CompactionConfig{WarnAt: 40, TargetAt: 12, HardAt: 60, Cooldown: 3}
	c := compactionFixture(cfg, 6, 2000) // 远超 hard
	c.Assemble()
	var marks []string
	for _, got := range c.TakeCompactions() {
		marks = append(marks, got.Watermark)
		if got.Watermark != "hard" {
			t.Errorf("撞硬上限时 watermark = %q，期望 hard", got.Watermark)
		}
	}
	if len(marks) == 0 {
		t.Fatal("应触发压缩")
	}
}

// 只到预警线（未到硬上限）时记 "warn"：两档要能分开，否则平台看不出"差点撞墙"。
func TestCompaction_WarnWatermarkBelowHard(t *testing.T) {
	cfg := hunt.CompactionConfig{WarnAt: 40, TargetAt: 12, HardAt: 100000, Cooldown: 3}
	c := compactionFixture(cfg, 6, 2000)
	c.Assemble()
	got := c.TakeCompactions()
	if len(got) == 0 {
		t.Fatal("应触发压缩")
	}
	for _, g := range got {
		if g.Watermark != "warn" {
			t.Errorf("未撞硬上限时 watermark = %q，期望 warn", g.Watermark)
		}
	}
}

// L0 截断保留两头，并标注丢了多少：只留开头会把"结论/报错"那句丢掉。
func TestCompaction_L0KeepsBothEndsAndMarksTheDrop(t *testing.T) {
	cfg := hunt.CompactionConfig{WarnAt: 40, TargetAt: 4000, HardAt: 200, Cooldown: 3}
	c := compactionFixture(cfg, 4, 4000)
	msgs := c.Assemble()
	found := false
	for _, m := range msgs {
		for _, r := range m.Results {
			if strings.Contains(r.Output, "已截断") {
				found = true
				if !strings.HasPrefix(r.Output, "0123456789abcdef") {
					t.Errorf("截断应保留开头：%q", r.Output[:32])
				}
				if !strings.HasSuffix(r.Output, "0123456789abcdef") {
					t.Errorf("截断应保留结尾：%q", r.Output[len(r.Output)-32:])
				}
			}
		}
	}
	if !found {
		t.Error("L0 应产出带截断标注的输出")
	}
}

// L2 换掉的是内容，不是地址：摘要里必须能重新定位到"当时动的是谁"。
func TestCompaction_L2KeepsAddressability(t *testing.T) {
	// 目标水位定在 L2 就够的位置：老轮被压成可寻址摘要，但不会折叠成工作日志。
	cfg := hunt.CompactionConfig{WarnAt: 2000, TargetAt: 1500, HardAt: 100000, Cooldown: 3}
	c := compactionFixture(cfg, 6, 2000)
	msgs := c.Assemble()
	if c.log != "" {
		t.Fatalf("目标水位可由 L2 达成时不该折到 L3：%s", c.log)
	}
	found := false
	for _, m := range msgs {
		for _, r := range m.Results {
			if strings.Contains(r.Output, "可寻址摘要") {
				found = true
				if !strings.Contains(r.Output, "read big.txt") {
					t.Errorf("摘要应带可重新定位的地址：%q", r.Output)
				}
			}
		}
	}
	if !found {
		t.Error("应产出可寻址摘要")
	}
}

// L3 折叠时把模型的自陈捞回来：假设与"缺什么"是判断类事实，压掉就再也拿不回来。
func TestCompaction_FoldPreservesDeclaredSections(t *testing.T) {
	cfg := hunt.CompactionConfig{WarnAt: 40, TargetAt: 12, HardAt: 200, Cooldown: 3}
	c := compactionFixture(cfg, 4, 2000)
	c.turns[1].Text = "先按默认分支做\n\n## 需要补全\n- 缺少数据库地址\n\n## 假设\n- 表名沿用 users\n"
	c.Assemble()
	if !strings.Contains(c.log, "需要补全：缺少数据库地址") {
		t.Errorf("折叠应保留「需要补全」：\n%s", c.log)
	}
	if !strings.Contains(c.log, "假设：表名沿用 users") {
		t.Errorf("折叠应保留「假设」：\n%s", c.log)
	}
}

// 没有水位（未装配压缩参数）就**不压**：缺席要能被看出来，而不是悄悄压一堆。
func TestCompaction_NoWatermarksMeansNoCompaction(t *testing.T) {
	c := compactionFixture(hunt.CompactionConfig{}, 4, 4000)
	before := c.measure()
	c.Assemble()
	if got := c.TakeCompactions(); len(got) != 0 {
		t.Errorf("没有水位不该压缩：%+v", got)
	}
	if after := c.measure(); after != before {
		t.Errorf("没有水位时组装结果不该变：%d → %d", before, after)
	}
}

// 当前轮永不压：哪怕它是唯一"够厚"的一轮（模型正踩在上面）。
func TestCompaction_CurrentTurnIsNeverCompacted(t *testing.T) {
	cfg := hunt.CompactionConfig{WarnAt: 40, TargetAt: 1, HardAt: 60, Cooldown: 3}
	c := &contextBuilder{cfg: cfg}
	c.Append(harness.Turn{
		No:      1,
		Text:    "唯一的这一轮",
		Calls:   []llm.ToolCall{{ID: "z1", Name: "read", Arguments: []byte(`{"path":"big.txt"}`)}},
		Results: []llm.ToolResult{{CallID: "z1", Output: bigOutput(4000)}},
	})
	msgs := c.Assemble()
	if got := c.TakeCompactions(); len(got) != 0 {
		t.Errorf("只有当前轮时无从压缩：%+v", got)
	}
	all := ""
	for _, m := range msgs {
		all += m.Content
		for _, r := range m.Results {
			all += r.Output
		}
	}
	if !strings.Contains(all, "0123456789abcdef") {
		t.Error("当前轮不得被压缩")
	}
}

// 压不动时不进冷却：只有当前轮时无从压（当前轮永不压）。
// 冷却若在那时就上膛，等下一轮真的带来第一个可压的老轮，反而被自己的冷却挡住——
// 那是「该压的时候压不动」，与冷却「别反复改写已缓存前缀」的初衷正好相反。
func TestCompaction_NothingToCompactDoesNotArmCooldown(t *testing.T) {
	cfg := hunt.CompactionConfig{WarnAt: 40, TargetAt: 12, HardAt: 60, Cooldown: 3}
	c := &contextBuilder{cfg: cfg}
	c.Append(harness.Turn{
		No:    1,
		Calls: []llm.ToolCall{{ID: "z1", Name: "read", Arguments: []byte(`{"path":"big.txt"}`)}},
		// 结果必须配着调用：没有正文也没有调用的轮次不占上下文（空轮不占位），
		// 只给结果会让这一轮量不出来。
		Results: []llm.ToolResult{{CallID: "z1", Output: bigOutput(4000)}},
	})
	c.Assemble()
	if got := c.TakeCompactions(); len(got) != 0 {
		t.Fatalf("只有当前轮时无从压缩：%+v", got)
	}
	if c.cooldown != 0 {
		t.Fatalf("无从压缩时不该上冷却，实际 %d", c.cooldown)
	}
	// 下一轮带来第一个可压的老轮：此刻必须压得动（冷却不该挡住这次唯一的机会）。
	c.Append(harness.Turn{
		No:      2,
		Calls:   []llm.ToolCall{{ID: "z2", Name: "read", Arguments: []byte(`{"path":"big.txt"}`)}},
		Results: []llm.ToolResult{{CallID: "z2", Output: bigOutput(4000)}},
	})
	c.Assemble()
	if got := c.TakeCompactions(); len(got) == 0 {
		t.Fatal("老轮出现后应压得动")
	}
}

// 硬上限是**压完之后**才问的（FR-14.1“硬上限视为不可继续”）：压得下来就不终止，
// 哪怕压之前确实站在它之上——否则“撞过硬上限”会变成一次误杀。
func TestCompaction_HardLimitIsJudgedAfterPressing(t *testing.T) {
	// 压得下来：压之前在硬上限之上，压完落到它以下。
	loose := compactionFixture(hunt.CompactionConfig{WarnAt: 40, TargetAt: 1000, HardAt: 2500, Cooldown: 3}, 6, 2000)
	if before := loose.measure(); before < 2500 {
		t.Fatalf("这条用例要先站在硬上限之上才有意义：%d", before)
	}
	loose.Assemble()
	if loose.OverHardLimit() {
		t.Errorf("压完已落到硬上限以下，不该报硬上限：measure=%d", loose.measure())
	}
	// 压不下来：硬上限低到连当前轮自己都装不进去——那时压几层都无济于事，只能停。
	tight := compactionFixture(hunt.CompactionConfig{WarnAt: 40, TargetAt: 1, HardAt: 200, Cooldown: 3}, 6, 20000)
	tight.Assemble()
	if !tight.OverHardLimit() {
		t.Errorf("压完仍不低于硬上限就该报：measure=%d HardAt=200", tight.measure())
	}
}

// 恢复回灌走同一管线（FR-14.8）：回灌的历史同样会被压，规则与正常运行时一致。
func TestCompaction_ResumeUsesTheSamePipeline(t *testing.T) {
	cfg := hunt.CompactionConfig{WarnAt: 40, TargetAt: 12, HardAt: 200, Cooldown: 3}
	resumed := compactionFixture(cfg, 4, 2000)
	fresh := compactionFixture(cfg, 4, 2000)
	// 恢复路径：把读回的轮次逐条 Append（与 Prepare 回灌同一动作），再组装。
	replay := &contextBuilder{cfg: cfg}
	replay.SetPrompt(resumed.prompt)
	for _, t := range resumed.turns {
		replay.Append(t)
	}
	replay.Assemble()
	fresh.Assemble()
	if replay.log != fresh.log {
		t.Errorf("恢复回灌应与正常运行时投影一致：\n%q\n%q", replay.log, fresh.log)
	}
}
