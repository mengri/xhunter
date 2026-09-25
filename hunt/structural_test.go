package hunt

import (
	"context"
	"strings"
	"testing"

	"xhunter/ext"
	"xhunter/harness"
	"xhunter/workspace"
)

// fakeExt 是结构判据的替身宿主：按脚本回答「语法完整吗」「有声明包含它吗」这两个**问题**，
// 而不是替 Session 直接给出三态——三态怎么收敛是被测代码的事，替身只负责给出后端事实。
type fakeExt struct {
	// parses 依次回答 Parse；用尽后重复最后一个（空 = 一律 ParseUnknown）。
	parses []ext.ParseVerdict
	// enclosed 依次回答 Enclose 的"是否有声明包含它"；用尽后重复最后一个（空 = 有）。
	enclosed []bool
	// encloseErr 非 nil 时 Enclose 一律判不了（模拟语言未注册、读不到）。
	encloseErr error
	// files 是被问过语法的文件（按序，去重前）——判据①"同一文件只问一次"由它来钉。
	files []string
	// encloses 是被问过封闭性的区间（按序）。
	encloses []workspace.ByteRange
	// closed 记下被回收的次数：外挂后端是子进程，漏回收就是漏进程。
	closed int
}

func (f *fakeExt) Capabilities(context.Context) ext.ExtCaps {
	return ext.ExtCaps{Available: true, Languages: []string{"go"}, Precision: ext.PrecisionSyntactic}
}

func (f *fakeExt) Locate(context.Context, ext.LocateRequest) (ext.Prepared, error) {
	return ext.Prepared{}, ext.ErrUnavailable
}

func (f *fakeExt) Parse(_ context.Context, file string) ext.ParseVerdict {
	f.files = append(f.files, file)
	if len(f.parses) == 0 {
		return ext.ParseUnknown
	}
	if len(f.files)-1 < len(f.parses) {
		return f.parses[len(f.files)-1]
	}
	return f.parses[len(f.parses)-1]
}

func (f *fakeExt) Enclose(_ context.Context, req ext.EncloseRequest) (ext.Prepared, bool, error) {
	f.encloses = append(f.encloses, req.ByteRange)
	if f.encloseErr != nil {
		return ext.Prepared{}, false, f.encloseErr
	}
	if len(f.enclosed) == 0 {
		return ext.Prepared{File: req.File, ByteRange: req.ByteRange}, true, nil
	}
	if len(f.encloses)-1 < len(f.enclosed) {
		return ext.Prepared{File: req.File, ByteRange: req.ByteRange}, f.enclosed[len(f.encloses)-1], nil
	}
	return ext.Prepared{File: req.File, ByteRange: req.ByteRange}, f.enclosed[len(f.enclosed)-1], nil
}

func (f *fakeExt) Fingerprint() []string { return []string{"fake/1", "lang:go"} }
func (f *fakeExt) Close() error          { f.closed++; return nil }

// extFactory 把替身宿主包成装配层那种工厂。走引擎的用例**必须**用它注入：Prepare 会用
// 工厂造宿主，直接给 s.ext 赋值会被装配覆盖——那等于没测到注入路径。
func extFactory(host ext.ExtHost) ExtHostFactory {
	return func(workspace.Workspace) ext.ExtHost { return host }
}

// 符号能力宿主随 Hunt 一起回收（FR-13.5）：外挂后端是一个子进程，不回收就留在系统里。
func TestFinalize_ClosesTheExtHost(t *testing.T) {
	sink := &captureSink{}
	s := newFinalizeSession(sink)
	host := &fakeExt{}
	s.ext = host

	run := &harness.Run{}
	run.SetTerminal(harness.Terminal{Status: harness.StatusSucceeded, Reason: "no_tool_call", Code: harness.ExitOK})
	if err := s.Finalize(context.Background(), run); err != nil {
		t.Fatalf("Finalize 失败：%v", err)
	}
	if host.closed != 1 {
		t.Fatalf("宿主回收次数 = %d，期望 1", host.closed)
	}
	// 回收排在终态之后：回收卡住也不该把 hunt_end 吞掉——平台靠它记账。
	if i := indexOf(sink, "hunt_end"); i != len(sink.events)-1 {
		t.Fatalf("hunt_end 应是最后一条事件（下标 %d / 共 %d）", i, len(sink.events))
	}
}

// Prepare 没走到造宿主那一步时 s.ext 是零值（nil），而收尾在 Prepare 失败后也会跑一次。
// 回收一处 nil 会炸在收尾里——那会把一个已经定下的终态变成 panic。
func TestFinalize_WithoutExtHostDoesNotPanic(t *testing.T) {
	s := newFinalizeSession(&captureSink{})
	run := &harness.Run{}
	run.SetTerminal(harness.Terminal{Status: harness.StatusFailed, Reason: "boom", Code: harness.ExitAborted})
	if err := s.Finalize(context.Background(), run); err != nil {
		t.Fatalf("没有宿主时收尾应照常完成，实际：%v", err)
	}
}

// structuralSession 给出一台只差判据的 Session：宿主可替、提交动作可数。
func structuralSession(g *countingGit, host ext.ExtHost) *Session {
	s := NewSession(Config{
		Bounty: Bounty{ID: "b", Task: "t", Repo: gitRepoRef()},
		Git:    g, Policy: allowAll{}, Sink: &captureSink{},
	})
	s.ext = host
	return s
}

// 判据①翻转：同一处改动先残缺（不提交）、补齐语法后（提交）。
// 它钉的是"判据问的是**此刻**的内容"——如果判据只在首次取样、或把 Unknown 读成 Broken，
// 第二次就不会提交。
func TestStructural_ParseOKFlipCommits(t *testing.T) {
	g := &countingGit{}
	s := structuralSession(g, &fakeExt{parses: []ext.ParseVerdict{ext.ParseBroken, ext.ParseOK}})
	s.ops = []WriteOp{{File: "a.go"}}

	s.checkpoint(context.Background(), &harness.Turn{No: 1})
	if g.commits != 0 {
		t.Fatalf("语法不完整时不该提交：commits=%d", g.commits)
	}
	s.checkpoint(context.Background(), &harness.Turn{No: 2})
	if g.commits != 1 {
		t.Fatalf("补齐语法后应提交一次：commits=%d", g.commits)
	}
}

// 语法不完整 → 不提交，且措辞是"未落在结构完整点"（判过、没通过），与"不可判定"分得开。
func TestStructural_IncompleteSyntaxSuppresses(t *testing.T) {
	sink := &captureSink{}
	g := &countingGit{}
	s := NewSession(Config{
		Bounty: Bounty{ID: "b", Task: "t", Repo: gitRepoRef()},
		Git:    g, Policy: allowAll{}, Sink: sink,
	})
	s.ext = &fakeExt{parses: []ext.ParseVerdict{ext.ParseBroken}}
	s.ops = []WriteOp{{File: "a.go"}}
	s.checkpoint(context.Background(), &harness.Turn{No: 1})

	if g.commits != 0 {
		t.Errorf("语法不完整不该提交：commits=%d", g.commits)
	}
	joined := strings.Join(sink.logs, "\n")
	if !strings.Contains(joined, "未落在结构完整点") {
		t.Errorf("应如实记录跳过原因：%v", sink.logs)
	}
	if strings.Contains(joined, "不可判定") {
		t.Errorf("判过不等于没法判：%v", sink.logs)
	}
	if n := countEvent(sink, "degraded"); n != 0 {
		t.Errorf("判据可用时不发 degraded：%v", sink.events)
	}
}

// 判不了（语言未注册）→ 不提交 ＋ 如实上报一次降级，且**不说**"未落在结构完整点"。
func TestStructural_UndecidableDoesNotCommit(t *testing.T) {
	sink := &captureSink{}
	g := &countingGit{}
	s := NewSession(Config{
		Bounty: Bounty{ID: "b", Task: "t", Repo: gitRepoRef()},
		Git:    g, Policy: allowAll{}, Sink: sink,
	})
	s.ext = &fakeExt{parses: []ext.ParseVerdict{ext.ParseUnknown}}
	s.ops = []WriteOp{{File: "a.txt"}}
	s.checkpoint(context.Background(), &harness.Turn{No: 1})

	if g.commits != 0 {
		t.Errorf("判不了时不该提交：commits=%d", g.commits)
	}
	joined := strings.Join(sink.logs, "\n")
	if !strings.Contains(joined, "结构判据不可判定") {
		t.Errorf("不可判定必须如实措辞：%v", sink.logs)
	}
	if strings.Contains(joined, "未落在结构完整点") {
		t.Errorf("不可判定不得报成「未落在结构完整点」：%v", sink.logs)
	}
	if n := countEvent(sink, "degraded"); n != 1 {
		t.Errorf("不可判定应发恰好一条 degraded：%v", sink.events)
	}
}

// 判据②：改动不落在任何声明里（判过、确实没封闭）→ 不提交，且**不发**降级（那是结论，不是没法判）。
func TestStructural_EditOutsideAnySymbolSuppresses(t *testing.T) {
	sink := &captureSink{}
	g := &countingGit{}
	s := NewSession(Config{
		Bounty: Bounty{ID: "b", Task: "t", Repo: gitRepoRef()},
		Git:    g, Policy: allowAll{}, Sink: sink,
	})
	host := &fakeExt{parses: []ext.ParseVerdict{ext.ParseOK}, enclosed: []bool{false}}
	s.ext = host

	// 走真实链路：判据②在落盘前就地判，结论与写操作平行记下。
	verdicts := s.judgeEnclosure(context.Background(), []workspace.FileEdit{{
		File: "a.go", ByteRange: workspace.ByteRange{Start: 4, End: 9}, NewContent: "x",
	}})
	s.ops = append(s.ops, WriteOp{File: "a.go", ByteRange: workspace.ByteRange{Start: 4, End: 9}})
	s.opEnclose = append(s.opEnclose, verdicts...)
	if len(host.encloses) != 1 {
		t.Fatalf("判据②应问一次：%v", host.encloses)
	}

	s.checkpoint(context.Background(), &harness.Turn{No: 1})
	if g.commits != 0 {
		t.Errorf("改动不封闭在任何符号内时不该提交：commits=%d", g.commits)
	}
	if n := countEvent(sink, "degraded"); n != 0 {
		t.Errorf("判过且不通过不发 degraded：%v", sink.events)
	}
}

// 整文件写入（含新建）**没有"封闭在哪个符号内"可言**：判据②对它不适用，由判据①单独覆盖。
// 少了这条豁免，新建一个完整的文件反而永远留不下检查点。
func TestStructural_WholeFileWriteIsNotJudgedAsUnenclosed(t *testing.T) {
	s := structuralSession(&countingGit{}, &fakeExt{
		parses:   []ext.ParseVerdict{ext.ParseOK},
		enclosed: []bool{false}, // 即便后端答"没有声明包含它"
	})
	// 零区间 = 整文件写入（Committer 的口径）：不该去问封闭性。
	verdicts := s.judgeEnclosure(context.Background(), []workspace.FileEdit{{
		File: "new.go", NewContent: "package main\n",
	}})
	if len(verdicts) != 1 || verdicts[0] != structuralPass {
		t.Fatalf("整文件写入的判据②应为通过（不适用）：%v", verdicts)
	}
}

// 已提交的改动**离开判据窗口**：判据问的是"这批待提交的改动"，不是"这一路走来的每一笔"。
// 少了这个窗口，一次早年的越界编辑会永久关掉自动检查点。
func TestStructural_CommittedOpsLeaveTheJudgementWindow(t *testing.T) {
	sink := &captureSink{}
	g := &countingGit{}
	s := NewSession(Config{
		Bounty: Bounty{ID: "b", Task: "t", Repo: gitRepoRef()},
		Git:    g, Policy: allowAll{}, Sink: sink,
	})
	s.ext = &fakeExt{parses: []ext.ParseVerdict{ext.ParseOK}}

	// 第 1 轮：一处越界改动，由模型显式请求带走（显式请求不看判据）。
	s.ops = []WriteOp{{File: "a.go"}}
	s.opEnclose = []structuralVerdict{structuralFail}
	s.RequestCheckpoint("模型认为这里自洽")
	s.checkpoint(context.Background(), &harness.Turn{No: 1})
	if g.commits != 1 {
		t.Fatalf("显式请求不看判据，应提交一次：commits=%d", g.commits)
	}

	// 第 2 轮：新改动封闭且语法完整 → 自动检查点照常（早先那笔越界编辑不该继续压着）。
	s.ops = append(s.ops, WriteOp{File: "b.go"})
	s.opEnclose = append(s.opEnclose, structuralPass)
	s.checkpoint(context.Background(), &harness.Turn{No: 2})
	if g.commits != 2 {
		t.Errorf("已提交的改动应离开判据窗口，第 2 轮应再提交一次：commits=%d", g.commits)
	}
}

// 判据①问的是**文件**，不是改动笔数：同一文件的多处改动只问一次语法。
func TestStructural_ParseIsAskedOncePerFile(t *testing.T) {
	host := &fakeExt{parses: []ext.ParseVerdict{ext.ParseOK}}
	s := structuralSession(&countingGit{}, host)
	s.ops = []WriteOp{
		{File: "a.go"}, {File: "a.go"}, {File: "b.go"},
	}
	s.opEnclose = []structuralVerdict{structuralPass, structuralPass, structuralPass}

	if v := s.judgeStructural(context.Background()); v != structuralPass {
		t.Fatalf("三处改动都封闭、语法完整 → 应通过：%d", v)
	}
	if len(host.files) != 2 {
		t.Errorf("判据①应每文件问一次，实际问了 %d 次：%v", len(host.files), host.files)
	}
}

// 判不出来时**不阻断写盘**：判据只决定"值不值得在这里留检查点"，不决定"能不能改"。
func TestStructural_UndecidableDoesNotBlockWriting(t *testing.T) {
	s := NewSession(Config{})
	if verdicts := s.judgeEnclosure(context.Background(), []workspace.FileEdit{{
		File: "a.go", ByteRange: workspace.ByteRange{Start: 1, End: 2},
	}}); len(verdicts) != 1 || verdicts[0] != structuralUndecidable {
		t.Fatalf("未装配符号能力时应判不了：%v", verdicts)
	}
	s.ext = &fakeExt{encloseErr: ext.ErrUnavailable}
	if verdicts := s.judgeEnclosure(context.Background(), []workspace.FileEdit{{
		File: "a.go", ByteRange: workspace.ByteRange{Start: 1, End: 2},
	}}); len(verdicts) != 1 || verdicts[0] != structuralUndecidable {
		t.Fatalf("后端判不了时应判不了（而非判成不封闭）：%v", verdicts)
	}
}
