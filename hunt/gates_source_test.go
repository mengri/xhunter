package hunt

import (
	"context"
	"errors"
	"strings"
	"testing"

	"xhunter/git"
)

// stubGateSource 交出一份固定的仓库声明清单，并记下自己有没有被问到。
type stubGateSource struct {
	gates     []Gate
	source    string
	err       error
	asked     bool
	askedWith git.RepoRef
}

func (s *stubGateSource) Load(_ context.Context, repo git.RepoRef) ([]Gate, string, error) {
	s.asked = true
	s.askedWith = repo
	return s.gates, s.source, s.err
}

// Parse 是工作区豁免档走的解析口子；本文件的用例不经过它（它们只裁决来源），
// 但它属于契约的一部分——接口加了方法，桩必须跟着实现。
func (s *stubGateSource) Parse([]byte) ([]Gate, error) { return s.gates, nil }

// 来源优先级：Bounty 下发 > 仓库声明 > 无。下发的清单覆盖仓库声明——验收标准归平台说了算。
func TestResolveGates_BountyOverridesRepoDeclaration(t *testing.T) {
	src := &stubGateSource{gates: []Gate{{Name: "repo-gate", Argv: []string{"true"}, Expect: Expect{Kind: ExpectExitZero}}}, source: GateSourceRepo}
	bounty := Bounty{Gates: []Gate{{Name: "bounty-gate", Argv: []string{"true"}, Expect: Expect{Kind: ExpectExitZero}}}}
	s := NewSession(Config{Gates: src, Bounty: bounty})

	gates, source, err := s.resolveGates(context.Background())
	if err != nil {
		t.Fatalf("裁决失败：%v", err)
	}
	if source != GateSourceBounty {
		t.Errorf("期望档位 %q，实际 %q", GateSourceBounty, source)
	}
	if len(gates) != 1 || gates[0].Name != "bounty-gate" {
		t.Fatalf("应按下发的清单生效，实际 %+v", gates)
	}
	if src.asked {
		t.Error("下发的清单已经够了：不该再去读仓库声明（判据应当只有一个来源）")
	}
}

func TestResolveGates_RepoDeclarationIsUsedWhenNothingIsHandedDown(t *testing.T) {
	src := &stubGateSource{
		gates:  []Gate{{Name: "fmt", Argv: []string{"true"}, Expect: Expect{Kind: ExpectEmptyOutput}}},
		source: GateSourceRepo,
	}
	s := NewSession(Config{Gates: src})

	gates, source, err := s.resolveGates(context.Background())
	if err != nil {
		t.Fatalf("裁决失败：%v", err)
	}
	if source != GateSourceRepo || len(gates) != 1 {
		t.Errorf("期望仓库声明档位与 1 条门禁，实际 %q / %d", source, len(gates))
	}
	if !src.asked {
		t.Error("没有下发清单时应当去读仓库声明")
	}
}

func TestResolveGates_NoSourceAtAllMeansNone(t *testing.T) {
	s := NewSession(Config{})
	gates, source, err := s.resolveGates(context.Background())
	if err != nil {
		t.Fatalf("没有来源不该报错：%v", err)
	}
	if source != GateSourceNone || gates != nil {
		t.Errorf("期望 none 档位与空清单，实际 %q / %v", source, gates)
	}
}

// 下发的清单同样要过"判据可判定"这一关：换来源不等于换标准。
func TestResolveGates_UndecidableHandedDownListIsRejected(t *testing.T) {
	s := NewSession(Config{Bounty: Bounty{Gates: []Gate{{Name: "lint", Argv: []string{"true"}}}}})
	_, _, err := s.resolveGates(context.Background())
	if err == nil {
		t.Fatal("没有判据的下发清单应当被拒")
	}
	if !strings.Contains(err.Error(), "门禁清单不可用") {
		t.Errorf("错误信息应说明清单不可用：%v", err)
	}
}

// 读不到清单（远端不可达、清单非法）是环境问题：带着一份判不了的清单跑完，只会得到
// 一个"看起来通过了"的结论。
func TestResolveGates_LoadFailureStopsTheRun(t *testing.T) {
	src := &stubGateSource{err: errors.New("基线里那份清单读不出来")}
	s := NewSession(Config{Gates: src})
	_, _, err := s.resolveGates(context.Background())
	if err == nil {
		t.Fatal("加载失败应当报错")
	}
	if !strings.Contains(err.Error(), "加载门禁清单失败") {
		t.Errorf("错误信息应带上阶段：%v", err)
	}
}

// 一次报全：只报第一条会让平台反复重派才看得全。
func TestValidateGates_ReportsEveryProblemAtOnce(t *testing.T) {
	err := ValidateGates([]Gate{
		{Name: "", Argv: []string{"true"}},
		{Name: "ok", Argv: []string{"true"}, Expect: Expect{Kind: ExpectExitZero}},
		{Name: "sh", Argv: []string{"bash", "-c", "true"}, Expect: Expect{Kind: ExpectExitZero}},
	})
	if err == nil {
		t.Fatal("期望报错")
	}
	if !strings.Contains(err.Error(), "缺少 name") || !strings.Contains(err.Error(), "不得把 shell 请回来") {
		t.Errorf("两条问题都该出现在同一条错误里：%v", err)
	}
	if err := ValidateGates(nil); err != nil {
		t.Errorf("空清单不是错误：%v", err)
	}
}
