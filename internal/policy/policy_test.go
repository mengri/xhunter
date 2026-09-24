package policy

import (
	"context"
	"strings"
	"testing"
	"time"

	"xhunter/hunt"
	"xhunter/llm"
)

// 策略的裁决与预算都可以脱离模型独立测试：这就是"无人类场景下顶替人"的那个位置，
// 它错了等于权限边界错了，所以这里的每一条规则都要有一句话钉住。

func TestDecide_ReadAndGateAllowed(t *testing.T) {
	p := New(Config{})
	for _, name := range []hunt.PrimitiveName{"read", "find", "glob", "check"} {
		d, err := p.Decide(context.Background(), hunt.Call{Primitive: name, Target: "a/b.go"})
		if err != nil || d.Verdict != hunt.VerdictAllow {
			t.Errorf("%s 应当放行，got %v err=%v", name, d.Verdict, err)
		}
	}
}

func TestDecide_WriteAllowed(t *testing.T) {
	p := New(Config{})
	for _, name := range []hunt.PrimitiveName{"write", "edit"} {
		d, _ := p.Decide(context.Background(), hunt.Call{Primitive: name, Writes: true, Target: "src/main.go"})
		if d.Verdict != hunt.VerdictAllow {
			t.Errorf("%s 写工作区文件应当放行", name)
		}
	}
}

func TestDecide_WriteToControlDirDenied(t *testing.T) {
	p := New(Config{})
	// 控制目录里禁写的是**引擎自己的材料与冻结配置**。门禁清单不在这个目录里
	// （它是仓库级、别的工具也会读的配置，落在仓库根 `gates.yml`），所以这里没有它。
	for _, target := range []string{
		".xhunter/session.jsonl",
		".xhunter/abc/session.jsonl",
		".xhunter/skills/release/SKILL.md",
	} {
		d, _ := p.Decide(context.Background(), hunt.Call{Primitive: "write", Writes: true, Target: target})
		if d.Verdict != hunt.VerdictDeny {
			t.Errorf("写 %s 应当被拒绝（引擎自有材料不可被模型篡改）", target)
		}
	}
}

func TestDecide_WriteToSkillsDraftAllowed(t *testing.T) {
	p := New(Config{})
	for _, target := range []string{
		".xhunter/skills.draft/foo/SKILL.md",
		".xhunter/skills.draft/a.md",
	} {
		d, _ := p.Decide(context.Background(), hunt.Call{Primitive: "write", Writes: true, Target: target})
		if d.Verdict != hunt.VerdictAllow {
			t.Errorf("写 %s 应当放行（书写/生效分离的落点）", target)
		}
	}
}

func TestDecide_PathEscapeDenied(t *testing.T) {
	p := New(Config{})
	for _, target := range []string{"../etc/passwd", "a/../../outside", "..", "/abs/path", `C:\windows\system32`} {
		d, _ := p.Decide(context.Background(), hunt.Call{Primitive: "write", Writes: true, Target: target})
		if d.Verdict != hunt.VerdictDeny {
			t.Errorf("写 %s 应当被拒绝（路径越界）", target)
		}
	}
}

// 只读调用不走路径边界——这不是防线缺口，是分工：
//   - 越界由机制层（工作区）拒绝：它只接受相对路径，`..` 与绝对路径根本表达不出来；
//   - 而 `.xhunter/**` 的**读必须放行**：skill 正文正是靠 read 按需加载的（FR-15.3）。
//
// 名字不认识的原语也不再由策略兜底：执行体在查表处就挡下了（`unknown_tool`），到不了这里。
func TestDecide_NonWritingCallSkipsPathRules(t *testing.T) {
	p := New(Config{})
	for _, target := range []string{"../etc/passwd", ".xhunter/skills/release/SKILL.md"} {
		d, _ := p.Decide(context.Background(), hunt.Call{Primitive: "read", Target: target})
		if d.Verdict != hunt.VerdictAllow {
			t.Errorf("读 %s 不该被策略拦下（越界归工作区层，控制目录的读要放行）", target)
		}
	}
}

func TestDecide_RenameWithoutTargetAllowed(t *testing.T) {
	p := New(Config{})
	d, _ := p.Decide(context.Background(), hunt.Call{Primitive: "symbol_rename", Writes: true})
	if d.Verdict != hunt.VerdictAllow {
		t.Error("symbol_rename 不指定文件是合法的符号级调用，不应被路径规则拦截")
	}
}

func TestChargeAndExhausted_Tokens(t *testing.T) {
	p := New(Config{Budget: hunt.Budget{MaxTokens: 100}})
	p.Charge(llm.Usage{InputTokens: 60, OutputTokens: 20})
	if yes, dim := p.Exhausted(1); yes {
		t.Fatalf("80 token 未达 100 上限，不应耗尽（dim=%s）", dim)
	}
	p.Charge(llm.Usage{InputTokens: 20, OutputTokens: 0})
	if yes, dim := p.Exhausted(1); !yes || dim != "tokens" {
		t.Fatalf("累计 100 token 应报 tokens 耗尽，got %v/%s", yes, dim)
	}
}

func TestExhausted_Turns(t *testing.T) {
	p := New(Config{Budget: hunt.Budget{MaxTurns: 3}})
	if yes, dim := p.Exhausted(3); !yes || dim != "turns" {
		t.Fatalf("第 3 轮应报 turns 耗尽，got %v/%s", yes, dim)
	}
	if yes, _ := p.Exhausted(2); yes {
		t.Fatal("第 2 轮不应耗尽")
	}
}

func TestExhausted_WallClock(t *testing.T) {
	now := time.Unix(100, 0)
	p := New(Config{Budget: hunt.Budget{MaxWallClock: time.Minute}, Now: func() time.Time { return now }})
	// 构造时 started 已定格为 100；把时钟拨到 61 秒后，墙钟预算即应耗尽。
	now = time.Unix(161, 0)
	if yes, dim := p.Exhausted(1); !yes || dim != "wall_clock" {
		t.Fatalf("墙钟 60s 已到应报 wall_clock 耗尽，got %v/%s", yes, dim)
	}
}

func TestExhausted_ZeroMeansUnlimited(t *testing.T) {
	p := New(Config{})
	for turn := hunt.TurnNo(1); turn <= 1000; turn++ {
		if yes, dim := p.Exhausted(turn); yes {
			t.Fatalf("零值预算应视为不限，turn %d 却报了 %s", turn, dim)
		}
	}
}

// ============================================================ MS-3 止损（两段式）

// 两段式的三态演进：同一类失败连续喂——第 1 次继续、第 2 次换策略（含"换一种做法"）、
// 第 3 次终止（诊断尾巴含 kind 与计数）。
func TestObserveFailure_SwitchThenTerminate(t *testing.T) {
	p := New(Config{}).(*engine)

	if sl, _ := p.ObserveFailure("not_found"); sl != hunt.StopContinue {
		t.Errorf("第 1 次同类失败应 continue，got %q", sl)
	}
	sl, msg := p.ObserveFailure("not_found")
	if sl != hunt.StopSwitch {
		t.Fatalf("第 2 次同类失败应 switch（达阈值先换策略），got %q", sl)
	}
	if !strings.Contains(msg, "换一种做法") {
		t.Errorf("switch 提示必须含「换一种做法」：%q", msg)
	}
	sl, msg = p.ObserveFailure("not_found")
	if sl != hunt.StopTerminate {
		t.Fatalf("第 3 次同类失败应 terminate（超上限），got %q", sl)
	}
	if !strings.Contains(msg, "not_found") {
		t.Errorf("terminate 诊断尾巴必须含失败类别：%q", msg)
	}
}

// 归零条件：一次成功（空串）把连续同类失败归零；换一类失败从 1 重新起计。
func TestObserveFailure_ResetsOnSuccessAndKind(t *testing.T) {
	p := New(Config{}).(*engine)

	p.ObserveFailure("a") // 同类 1
	p.ObserveFailure("a") // 同类 2 → switch
	p.ObserveFailure("")  // 成功：归零
	if sl, _ := p.ObserveFailure("a"); sl != hunt.StopContinue {
		t.Errorf("成功应把连续同类失败归零（下一次回到第 1 次 → continue），got %q", sl)
	}

	q := New(Config{}).(*engine)
	q.ObserveFailure("a") // kind=a，第 1 次
	if sl, _ := q.ObserveFailure("b"); sl != hunt.StopContinue {
		t.Errorf("换一类失败应从 1 重新起计（continue），got %q", sl)
	}
	if sl, _ := q.ObserveFailure("b"); sl != hunt.StopSwitch {
		t.Errorf("同类第 2 次应 switch，got %q", sl)
	}
}

// 同类 = 同 kind；policy_denied 被忽略：既不加同类计数，也不改状态（拒绝是另一条轴）。
func TestObserveFailure_KindScopedAndDenialIgnored(t *testing.T) {
	p := New(Config{}).(*engine)

	p.ObserveFailure("not_found") // 同类计数 1
	if sl, _ := p.ObserveFailure("policy_denied"); sl != hunt.StopContinue {
		t.Errorf("policy_denied 应被忽略（continue），got %q", sl)
	}
	if p.failKind != "not_found" || p.failStreak != 1 {
		t.Errorf("policy_denied 不得改同类失败状态：kind=%q streak=%d", p.failKind, p.failStreak)
	}
}

// 连续拒绝达阈值即报"已超阈值"。
func TestDeniedCount_TerminatesAfterThreshold(t *testing.T) {
	p := New(Config{}).(*engine)
	deny := hunt.Call{Primitive: "write", Writes: true, Target: ".xhunter/session.jsonl"}

	for i := 1; i <= 2; i++ {
		if _, err := p.Decide(context.Background(), deny); err != nil {
			t.Fatalf("Decide 失败：%v", err)
		}
		if _, over := p.DeniedCount(); over {
			t.Errorf("第 %d 次拒绝不该达阈值", i)
		}
	}
	p.Decide(context.Background(), deny) // 第 3 次
	if n, over := p.DeniedCount(); !over || n != 3 {
		t.Errorf("第 3 次拒绝应达阈值：n=%d over=%v", n, over)
	}
}

// 阈值本身的字面值就是契约（取 3）：用字面量钉死，改常量会立刻变红。理由见常量定义——
// 它必须不大于机制止损的收敛轮数（harness 默认 3），否则 stop_loss_denied 永不可达。
func TestDeniedStreakLimitIsThree(t *testing.T) {
	if MaxDeniedStreak != 3 {
		t.Fatalf("连续拒绝阈值 = %d，契约是「连续 3 次」", MaxDeniedStreak)
	}
}

// 两段式的两个阈值：达阈值先换策略（2）、超上限才终止（3）。用字面量钉死。
func TestSameKindLimitsAreTwoAndThree(t *testing.T) {
	if MaxSameKindSwitch != 2 {
		t.Fatalf("换策略阈值 = %d，契约是「连续 2 次」", MaxSameKindSwitch)
	}
	if MaxSameKindStreak != 3 {
		t.Fatalf("终止上限 = %d，契约是「连续 3 次」", MaxSameKindStreak)
	}
}

// 一次放行把连续拒绝归零——三种放行路径（只读、符号级写、工作区内写）都要归零，漏一条就会把
// 「夹在拒绝之间的正常调用」误判成连拒。
func TestDecide_DeniedStreakResetsOnAllow(t *testing.T) {
	deny := hunt.Call{Primitive: "write", Writes: true, Target: ".xhunter/session.jsonl"}
	ctx := context.Background()

	// 只读放行
	p := New(Config{}).(*engine)
	p.Decide(ctx, deny)
	p.Decide(ctx, deny)
	p.Decide(ctx, hunt.Call{Primitive: "read", Target: ".xhunter/skills/x.md"})
	if n, _ := p.DeniedCount(); n != 0 {
		t.Errorf("只读放行应把连续拒绝归零：%d", n)
	}

	// 符号级写（无 Target）放行
	p = New(Config{}).(*engine)
	p.Decide(ctx, deny)
	p.Decide(ctx, hunt.Call{Primitive: "symbol_rename", Writes: true})
	if n, _ := p.DeniedCount(); n != 0 {
		t.Errorf("符号级写放行应把连续拒绝归零：%d", n)
	}

	// 工作区内写放行
	p = New(Config{}).(*engine)
	p.Decide(ctx, deny)
	p.Decide(ctx, hunt.Call{Primitive: "write", Writes: true, Target: "src/main.go"})
	if n, _ := p.DeniedCount(); n != 0 {
		t.Errorf("工作区内写放行应把连续拒绝归零：%d", n)
	}
}

// Facts 必须自述两个止损阈值（config_snapshot 据此报给平台）——值等于字面量。
func TestFacts_CarriesStopLossLimits(t *testing.T) {
	f := Facts()
	if f["max_denied_streak"] != 3 {
		t.Errorf("max_denied_streak = %v，期望 3", f["max_denied_streak"])
	}
	if f["max_same_kind_streak"] != 3 {
		t.Errorf("max_same_kind_streak = %v，期望 3", f["max_same_kind_streak"])
	}
}
