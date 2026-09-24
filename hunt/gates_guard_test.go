package hunt

import (
	"context"
	"strings"
	"testing"

	"xhunter/git"
	"xhunter/workspace"
)

// 本文件钉住门禁清单的**护栏**：清单能不能用（元门禁）、豁免档有没有把门禁改松（强度）、
// 以及豁免生效时有没有如实上报。清单文件不限制模型写，所以这三条是唯一防线。

// guardGateSource 交出"基线那一份"清单，并按预设给解析结果——工作区那份的文本形状由用例控制。
type guardGateSource struct {
	baseline []Gate
	parsed   []Gate
	loadErr  error
	parseErr error
	loads    int
	seen     string
}

func (s *guardGateSource) Load(context.Context, git.RepoRef) ([]Gate, string, error) {
	s.loads++
	return s.baseline, GateSourceRepo, s.loadErr
}

func (s *guardGateSource) Parse(content []byte) ([]Gate, error) {
	s.seen = string(content)
	return s.parsed, s.parseErr
}

func sessionForGates(src GateSource, files map[string]string, sink EventSink) *Session {
	s := NewSession(Config{
		Bounty: Bounty{GatesSource: GateSourceWorkingTree},
		Gates:  src,
		Sink:   sink,
	})
	if files != nil {
		s.storage = &memStorage{files: files}
	}
	return s
}

// 判据不可判定的清单不许生效：它要么永真要么永假，比没有门禁更坏。
func TestMetaGate_RejectsUndecidableExpect(t *testing.T) {
	gates := []Gate{{Name: "lint", Argv: []string{"true"}, Expect: Expect{Kind: ExpectRegex}}}
	if err := MetaGate(gates); err == nil {
		t.Fatal("缺少 pattern 的 regex 判据应当被拒")
	}
}

// 命令起不来的门禁同样不可用：它会在真正被调用时才炸，而那时已经是"门禁没跑"了。
func TestMetaGate_RequiresTheCommandToBeStartable(t *testing.T) {
	ok := []Gate{{Name: "fmt", Argv: []string{"true"}, Expect: Expect{Kind: ExpectExitZero}}}
	if err := MetaGate(ok); err != nil {
		t.Fatalf("可启动的清单不该被拒：%v", err)
	}
	bad := []Gate{{Name: "ghost", Argv: []string{"definitely-not-a-command-zzz"}, Expect: Expect{Kind: ExpectExitZero}}}
	err := MetaGate(bad)
	if err == nil {
		t.Fatal("起不来的命令应当被拒")
	}
	if !strings.Contains(err.Error(), "无法启动") {
		t.Errorf("错误信息应说明命令起不来：%v", err)
	}
}

// 一次报全：这些问题同属一份配置，报一条就要重派一次。
func TestMetaGate_ReportsEveryProblemAtOnce(t *testing.T) {
	gates := []Gate{
		{Name: "a", Argv: []string{"true"}},
		{Name: "b", Argv: []string{"bash", "-c", "true"}, Expect: Expect{Kind: ExpectExitZero}},
		{Name: "c", Argv: []string{"definitely-not-a-command-zzz"}, Expect: Expect{Kind: ExpectExitZero}},
	}
	err := MetaGate(gates)
	if err == nil {
		t.Fatal("期望报错")
	}
	for _, want := range []string{"a", "b", "c"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("三条问题都该出现在同一条错误里，缺 %q：%v", want, err)
		}
	}
}

// 强度不得降低：基线里 required 的门禁不能被移除、也不能改命令或判据，只允许新增。
func TestCheckStrength_RejectsWeakening(t *testing.T) {
	baseline := []Gate{
		{Name: "fmt", Argv: []string{"true", "-l"}, Required: true, Expect: Expect{Kind: ExpectEmptyOutput}},
		{Name: "opt", Argv: []string{"true"}, Required: false, Expect: Expect{Kind: ExpectExitZero}},
	}
	cases := []struct {
		name      string
		candidate []Gate
		want      string
	}{
		{"移除必需门禁", []Gate{}, "被移除"},
		{"改命令", []Gate{{Name: "fmt", Argv: []string{"true", "-w"}, Required: true, Expect: Expect{Kind: ExpectEmptyOutput}}}, "改了命令"},
		{"改判据", []Gate{{Name: "fmt", Argv: []string{"true", "-l"}, Required: true, Expect: Expect{Kind: ExpectExitZero}}}, "改了判据"},
	}
	for _, c := range cases {
		err := CheckStrength(baseline, c.candidate)
		if err == nil {
			t.Errorf("%s：期望报错", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：错误信息应含 %q，实际 %v", c.name, c.want, err)
		}
	}

	// 新增不降强度；非必需门禁怎么改都不算降低。
	added := append([]Gate{}, baseline...)
	added = append(added, Gate{Name: "new", Argv: []string{"true"}, Required: true, Expect: Expect{Kind: ExpectExitZero}})
	if err := CheckStrength(baseline, added); err != nil {
		t.Errorf("新增门禁不该被拒：%v", err)
	}
	relaxed := []Gate{baseline[0], {Name: "opt", Argv: []string{"true", "--fast"}, Expect: Expect{Kind: ExpectExitZero}}}
	if err := CheckStrength(baseline, relaxed); err != nil {
		t.Errorf("改非必需门禁不算降低强度：%v", err)
	}
}

// 豁免只能由 Bounty 授予：没有 `gates_source: working_tree` 时，读的一定是基线那一份——
// 模型拿不到这个开关，否则"改判据让自己通过"就有一条正当路径。
func TestResolveGates_ExemptionRequiresABountyGrant(t *testing.T) {
	src := &guardGateSource{baseline: []Gate{{Name: "fmt", Argv: []string{"true"}, Expect: Expect{Kind: ExpectExitZero}}}}
	s := NewSession(Config{Gates: src, Bounty: Bounty{}})
	s.storage = &memStorage{files: map[string]string{GateManifestPath: "gates:\n  - name: new\n"}}

	gates, source, err := s.resolveGates(context.Background())
	if err != nil {
		t.Fatalf("未授予豁免时应照常读基线：%v", err)
	}
	if source != GateSourceRepo {
		t.Errorf("未授予豁免时来源应是 repo，实际 %q", source)
	}
	if len(gates) != 1 || gates[0].Name != "fmt" {
		t.Errorf("应拿到基线那份清单：%+v", gates)
	}
}

// 护栏之一（元门禁）：工作区那份判据不可判定时，整趟在启动期失败——
// 带着一份靠不住的清单跑完，只会得到一个"看起来通过了"的结论。
func TestResolveGates_WorkingTreeExemptionRequiresMetaGate(t *testing.T) {
	src := &guardGateSource{
		baseline: []Gate{{Name: "fmt", Argv: []string{"true"}, Expect: Expect{Kind: ExpectExitZero}}},
		// 判据缺 pattern：不可判定。
		parsed: []Gate{{Name: "lint", Argv: []string{"true"}, Expect: Expect{Kind: ExpectRegex}}},
	}
	s := sessionForGates(src, map[string]string{GateManifestPath: "gates: []\n"}, &captureSink{})

	if _, _, err := s.resolveGates(context.Background()); err == nil {
		t.Fatal("判据不可判定的豁免清单应当在启动期被拒")
	}
}

// 护栏之二（强度不得降低）：新清单把基线里 required 的门禁改松即失败，只允许新增。
func TestResolveGates_WorkingTreeCannotLowerStrength(t *testing.T) {
	src := &guardGateSource{
		baseline: []Gate{{Name: "fmt", Argv: []string{"true", "-l"}, Required: true, Expect: Expect{Kind: ExpectEmptyOutput}}},
		parsed:   []Gate{{Name: "fmt", Argv: []string{"true", "-l"}, Required: true, Expect: Expect{Kind: ExpectExitZero}}},
	}
	s := sessionForGates(src, map[string]string{GateManifestPath: "gates: []\n"}, &captureSink{})

	_, _, err := s.resolveGates(context.Background())
	if err == nil {
		t.Fatal("判据被改松应当在启动期被拒")
	}
	if !strings.Contains(err.Error(), "强度") {
		t.Errorf("错误应说明是强度问题：%v", err)
	}
}

// 护栏之三（显式上报）：豁免真的生效时必须发 `gate_config_changed`——
// 一份被改松的门禁与被改紧的门禁，在结果文件里长得一模一样，只有这条事件能区分。
func TestResolveGates_WorkingTreeEmitsGateConfigChanged(t *testing.T) {
	sink := &captureSink{}
	src := &guardGateSource{
		baseline: []Gate{{Name: "fmt", Argv: []string{"true", "-l"}, Required: true, Expect: Expect{Kind: ExpectEmptyOutput}}},
		parsed: []Gate{
			{Name: "fmt", Argv: []string{"true", "-l"}, Required: true, Expect: Expect{Kind: ExpectEmptyOutput}},
			{Name: "cover", Argv: []string{"true"}, Required: true, Expect: Expect{Kind: ExpectExitZero}},
		},
	}
	s := sessionForGates(src, map[string]string{GateManifestPath: "gates:\n  - name: fmt\n"}, sink)

	gates, source, err := s.resolveGates(context.Background())
	if err != nil {
		t.Fatalf("只增不改的豁免清单应当生效：%v", err)
	}
	if source != GateSourceWorkingTree {
		t.Errorf("来源档位应是 working_tree，实际 %q", source)
	}
	if len(gates) != 2 {
		t.Errorf("工作区那份应生效：%+v", gates)
	}
	events := sink.ofType("gate_config_changed")
	if len(events) != 1 {
		t.Fatalf("豁免生效必须显式上报一次，实际 %d 条", len(events))
	}
	if events[0]["source"] != GateSourceWorkingTree {
		t.Errorf("上报的 source 应是 working_tree：%v", events[0])
	}
	// 上报的门禁名必须来自真正生效的那份（与落地的清单同源，不另抄一遍）。
	names, _ := events[0]["gates"].([]string)
	if len(names) != 2 || names[0] != "fmt" || names[1] != "cover" {
		t.Errorf("上报的清单名应来自生效的那份：%v", events[0]["gates"])
	}
}

// 工作区里没有清单文件时，豁免档按"空清单"走护栏：基线若有 required 门禁，
// 那就是"移除"，应当失败——豁免不是"顺手把门禁清掉"的通道。
func TestResolveGates_WorkingTreeMissingManifestCannotDropRequiredGates(t *testing.T) {
	src := &guardGateSource{
		baseline: []Gate{{Name: "fmt", Argv: []string{"true"}, Required: true, Expect: Expect{Kind: ExpectExitZero}}},
	}
	s := sessionForGates(src, map[string]string{}, &captureSink{})

	if _, _, err := s.resolveGates(context.Background()); err == nil {
		t.Fatal("基线有必需门禁而工作区清单为空，应当被拒")
	}
}

// 读的是工作区那一份（不是基线）：豁免的意义就是让新写的清单本次生效。
func TestResolveGates_WorkingTreeReadsTheWorkspaceFile(t *testing.T) {
	src := &guardGateSource{parsed: []Gate{{Name: "fresh", Argv: []string{"true"}, Expect: Expect{Kind: ExpectExitZero}}}}
	s := sessionForGates(src, map[string]string{GateManifestPath: "gates:\n  - name: fresh\n"}, &captureSink{})

	if _, _, err := s.resolveGates(context.Background()); err != nil {
		t.Fatalf("豁免生效失败：%v", err)
	}
	if src.seen != "gates:\n  - name: fresh\n" {
		t.Errorf("解析的应是工作区那份文本，实际读到：%q", src.seen)
	}
	if src.loads != 1 {
		t.Errorf("基线那份只该读一次（用于强度比对），实际 %d 次", src.loads)
	}
}

// 装配缺件：给了豁免却没装配门禁来源，无法做强度比对——启动期失败，不静默放行。
func TestResolveGates_ExemptionWithoutGateSourceFails(t *testing.T) {
	s := NewSession(Config{Bounty: Bounty{GatesSource: GateSourceWorkingTree}})
	s.storage = &memStorage{files: map[string]string{}}
	if _, _, err := s.resolveGates(context.Background()); err == nil {
		t.Fatal("没有门禁来源却授予豁免，应当在启动期失败")
	}
}

// 编译期约束：会话执行体自己就是门禁来源的调用方，接口形状变了要在这条上先炸出来。
var _ workspace.Workspace = (*memStorage)(nil)
