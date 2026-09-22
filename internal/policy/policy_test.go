package policy

import (
	"context"
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
		d, _ := p.Decide(context.Background(), hunt.Call{Primitive: name, Target: "src/main.go"})
		if d.Verdict != hunt.VerdictAllow {
			t.Errorf("%s 写工作区文件应当放行", name)
		}
	}
}

func TestDecide_WriteToControlDirDenied(t *testing.T) {
	p := New(Config{})
	for _, target := range []string{
		".xhunter/session.jsonl",
		".xhunter/abc/session.jsonl",
		".xhunter/gates.yml",
		".xhunter/skills/release/SKILL.md",
	} {
		d, _ := p.Decide(context.Background(), hunt.Call{Primitive: "write", Target: target})
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
		d, _ := p.Decide(context.Background(), hunt.Call{Primitive: "write", Target: target})
		if d.Verdict != hunt.VerdictAllow {
			t.Errorf("写 %s 应当放行（书写/生效分离的落点）", target)
		}
	}
}

func TestDecide_PathEscapeDenied(t *testing.T) {
	p := New(Config{})
	for _, target := range []string{"../etc/passwd", "a/../../outside", "..", "/abs/path", `C:\windows\system32`} {
		d, _ := p.Decide(context.Background(), hunt.Call{Primitive: "write", Target: target})
		if d.Verdict != hunt.VerdictDeny {
			t.Errorf("写 %s 应当被拒绝（路径越界）", target)
		}
	}
}

func TestDecide_UnknownPrimitiveDenied(t *testing.T) {
	p := New(Config{})
	d, _ := p.Decide(context.Background(), hunt.Call{Primitive: "rm", Target: "x"})
	if d.Verdict != hunt.VerdictDeny {
		t.Error("未识别的原语应当默认拒绝")
	}
}

func TestDecide_RenameWithoutTargetAllowed(t *testing.T) {
	p := New(Config{})
	d, _ := p.Decide(context.Background(), hunt.Call{Primitive: "symbol_rename"})
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
