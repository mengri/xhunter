package hunt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"xhunter/git"
	"xhunter/harness"
	"xhunter/llm"
	"xhunter/workspace"
)

// 记录桩：只关心「本轮是什么时候、以什么内容被记下来的」。
type recordingContext struct{ appended []harness.Turn }

func (c *recordingContext) SetPrompt([]llm.Message) {}
func (c *recordingContext) Assemble() []llm.Message { return nil }
func (c *recordingContext) Append(rec harness.Turn) { c.appended = append(c.appended, rec) }

type recordingSession struct{ turns []harness.Turn }

func (r *recordingSession) Open(string) error           { return nil }
func (r *recordingSession) RecordTurn(rec harness.Turn) { r.turns = append(r.turns, rec) }
func (r *recordingSession) RecordOp(WriteOp)            {}
func (r *recordingSession) RecordUsage(llm.Usage)       {}
func (r *recordingSession) Ops() []WriteOp              { return nil }
func (r *recordingSession) Snapshot() error             { return nil }

// 加工必须发生在落历史之前：历史与材料都是值拷贝，抢在后面改的过滤器等于白做——
// 模型下一轮看不到改动，而过滤器作者会以为生效了。过滤器的名字进生效配置快照。
func TestOnTurn_FiltersRunBeforeRecording(t *testing.T) {
	ctxLog := &recordingContext{}
	rec := &recordingSession{}
	s := NewSession(Config{
		Context: ctxLog,
		Session: rec,
		Filters: []NamedFilter{
			{Name: "甲", Run: func(_ context.Context, turn *harness.Turn) error { turn.Text += "-甲"; return nil }},
			{Name: "乙", Run: func(_ context.Context, turn *harness.Turn) error { turn.Text += "-乙"; return nil }},
		},
	})

	turn := &harness.Turn{No: 1, Text: "看见了"}
	cont, err := s.OnTurn(context.Background(), &harness.Run{}, turn)
	if err != nil || !cont {
		t.Fatalf("OnTurn = (%v, %v)，期望 (true, nil)", cont, err)
	}

	if turn.Text != "看见了-甲-乙" {
		t.Errorf("过滤器必须按装配顺序生效：%q", turn.Text)
	}
	if len(ctxLog.appended) != 1 || ctxLog.appended[0].Text != "看见了-甲-乙" {
		t.Errorf("落历史的必须是加工后的内容：%+v", ctxLog.appended)
	}
	if len(rec.turns) != 1 || rec.turns[0].Text != "看见了-甲-乙" {
		t.Errorf("会话材料必须是加工后的内容：%+v", rec.turns)
	}
}

// 过滤器失败不得静默放行：脱敏、截断一类过滤器挂掉时静默继续，正好把它们要防的内容
// 原样送进下一轮上下文。
func TestOnTurn_FilterFailureStopsTurn(t *testing.T) {
	ctxLog := &recordingContext{}
	s := NewSession(Config{
		Context: ctxLog,
		Filters: []NamedFilter{
			{Name: "脱敏", Run: func(_ context.Context, _ *harness.Turn) error { return errors.New("脱敏失败") }},
		},
	})

	cont, err := s.OnTurn(context.Background(), &harness.Run{}, &harness.Turn{No: 1, Text: "原始内容"})
	if err == nil {
		t.Fatal("过滤器失败必须上抛给循环")
	}
	if cont {
		t.Error("失败时不该返回「继续」")
	}
	if len(ctxLog.appended) != 0 {
		t.Errorf("失败的这一轮不该落进历史：%+v", ctxLog.appended)
	}
}

// 已答复的调用不再重复执行：排在 Session.OnTurn 前面的 handler 可以直接答复某次调用
// （缓存命中、一眼可见的非法规格），这条通道不成立的话「短路」就只是句空话。
func TestOnTurn_AnsweredCallIsNotReExecuted(t *testing.T) {
	s := NewSession(Config{}) // 刻意不装工具：一旦被真的执行，就会多出一条「没有这个工具」的结果
	turn := &harness.Turn{
		No:      1,
		Calls:   []llm.ToolCall{{ID: "c1", Name: "read", Arguments: json.RawMessage(`{"path":"a.go"}`)}},
		Results: []llm.ToolResult{{CallID: "c1", Output: "缓存命中"}},
	}

	if _, err := s.OnTurn(context.Background(), &harness.Run{}, turn); err != nil {
		t.Fatalf("OnTurn 失败：%v", err)
	}
	if len(turn.Results) != 1 || turn.Results[0].Output != "缓存命中" {
		t.Errorf("已答复的调用不该被重复执行：%+v", turn.Results)
	}
	if turn.Failed {
		t.Errorf("已答复的调用不该被记成失败：%+v", turn.Results)
	}
}

// 首轮两段的摆法与组装：system 在前、user 在后；插件正文为空时不占位置，
// 但内核那两块（内核条款 / 环境事实）**照旧存在**——它们不由插件提供。
func TestFirstPrompt_KeepsOrderAndAppendsKernelBlocks(t *testing.T) {
	bounty := Bounty{Repo: gitRepoRef()}
	got := firstPrompt("PLUGIN_SYS", "PLUGIN_USER", bounty)
	if len(got) != 2 || got[0].Role != llm.RoleSystem || got[1].Role != llm.RoleUser {
		t.Fatalf("两段都必须存在且 system 在前：%+v", got)
	}
	// system = 插件正文 + 内核条款（条款在末尾，插件改不动它）
	if !strings.Contains(got[0].Content, "PLUGIN_SYS") || !strings.Contains(got[0].Content, kernelClauseMarker) {
		t.Errorf("system 段应含插件正文与内核条款：%q", got[0].Content)
	}
	if !strings.Contains(got[0].Content, "## 未验证") {
		t.Errorf("内核条款必须陈述「## 未验证」这一节，否则模型不会写、采集再正确也拿不到东西：%q", got[0].Content)
	}
	if strings.Index(got[0].Content, "PLUGIN_SYS") > strings.Index(got[0].Content, kernelClauseMarker) {
		t.Error("内核条款必须在插件正文之后（末尾追加）")
	}
	// user = 环境事实 + 插件正文
	if !strings.Contains(got[1].Content, environmentFactMarker) || !strings.Contains(got[1].Content, "PLUGIN_USER") {
		t.Errorf("user 段应含环境事实与插件正文：%q", got[1].Content)
	}
	if strings.Index(got[1].Content, environmentFactMarker) > strings.Index(got[1].Content, "PLUGIN_USER") {
		t.Error("环境事实必须在插件正文之前")
	}

	// 插件两段都空：不留空行、不产出多余消息，但内核两块仍在。
	empty := firstPrompt("  \n ", "", bounty)
	if len(empty) != 2 {
		t.Fatalf("内核两块必须各自成段：%+v", empty)
	}
	if strings.TrimSpace(empty[0].Content) != strings.TrimSpace(kernelClauses()) {
		t.Errorf("没有插件正文时 system 段应恰为内核条款：%q", empty[0].Content)
	}
	if !strings.Contains(empty[1].Content, environmentFactMarker) {
		t.Errorf("user 段应恰为环境事实：%q", empty[1].Content)
	}
	if strings.Contains(empty[0].Content, "\n\n\n") || strings.HasPrefix(empty[0].Content, "\n") {
		t.Errorf("空插件段不该留下多余空行：%q", empty[0].Content)
	}
}

// gitRepoRef 给组装用例一个最小仓库事实。
func gitRepoRef() git.RepoRef {
	return git.RepoRef{Remote: "r", Branch: "xhunter/x", BaseCommit: "0123456789abcdef0123456789abcdef"}
}

// 装配缺件必须在首轮推理之前显式失败，而且要说清缺哪一项（FR-1.8、IA-12.5）：
// 此前 Git / Opener 缺失是调用即 panic（收敛成一句 "invalid memory address"），
// 而 Policy 缺失被**静默跳过**——同一份"装配校验"的说法，三种行为。
func TestPrepare_IncompleteAssemblyFailsLoudly(t *testing.T) {
	sink := &captureSink{}
	full := Config{
		Bounty: Bounty{ID: "b1", Task: "t", Repo: gitRepoRef()},
		Git:    &stubBaselineGit{}, Opener: stubOpener{}, Policy: allowAll{}, Sink: sink,
	}
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantSub string
	}{
		{"缺 git", func(c *Config) { c.Git = nil }, "缺少 git"},
		{"缺工作区打开点", func(c *Config) { c.Opener = nil }, "缺少工作区打开点"},
		{"缺策略", func(c *Config) { c.Policy = nil }, "缺少策略"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := full
			tc.mutate(&cfg)
			err := NewSession(cfg).Prepare(context.Background(), &harness.Run{})
			if err == nil {
				t.Fatal("缺件必须显式失败")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("错误信息应指明缺哪一项（%q）：%v", tc.wantSub, err)
			}
		})
	}

	// 缺上下文组装器不判死，但要留痕：模型会看不到自己上一轮做过什么。
	cfg := full
	cfg.Context = nil
	if err := NewSession(cfg).Prepare(context.Background(), &harness.Run{}); err != nil {
		t.Fatalf("缺上下文不该判死（降级而非缺件）：%v", err)
	}
	warned := false
	for _, line := range sink.logs {
		if strings.Contains(line, "上下文组装器") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("缺上下文必须留下可见的降级记录：%v", sink.logs)
	}
}

// stubBaselineGit 只提供 PrepareBaseline：装配用例不需要真的仓库。
type stubBaselineGit struct{}

func (stubBaselineGit) PrepareBaseline(context.Context, git.RepoRef) (string, error) {
	return "root", nil
}
func (stubBaselineGit) Commit(context.Context, git.RepoRef, string) (git.Commit, error) {
	return git.Commit{}, nil
}
func (stubBaselineGit) Diff(context.Context, git.RepoRef) ([]string, error) { return nil, nil }
func (stubBaselineGit) Patch(context.Context, git.RepoRef) (string, error)  { return "", nil }
func (stubBaselineGit) Clean(context.Context) error                         { return nil }

// stubOpener 交出一个内存工作区。
type stubOpener struct{}

func (stubOpener) Open(string) (workspace.Storage, error) {
	return &memStorage{files: map[string]string{}}, nil
}

// 净化是格式不可伪造的保证：只取首行、剥控制与格式字符、折叠空白、按上限截断。
func TestSanitizeIntent(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"只取首行", "第一行\n第二行", "第一行"},
		{"剥零宽字符", "a\u200bb", "ab"},
		{"折叠空白", "  a\t\t b  ", "a b"},
		{"空内容即无理由", "\n", ""},
	}
	for _, c := range cases {
		if got := sanitizeIntent(c.in); got != c.want {
			t.Errorf("%s：sanitizeIntent(%q) = %q，期望 %q", c.name, c.in, got, c.want)
		}
	}

	long := make([]rune, maxIntentRunes*2)
	for i := range long {
		long[i] = 'x'
	}
	got := sanitizeIntent(string(long))
	if runes := []rune(got); len(runes) != maxIntentRunes+1 || runes[maxIntentRunes] != '…' {
		t.Errorf("超长必须截断到 %d 字并加省略号，实得 %d 字", maxIntentRunes, len(runes))
	}
}

// 检查点是空操作时日志不能报"已创建"（FR-1.3c：无新改动不提交）。
// 假装干了活，比不干活更难查。
func TestCheckpoint_LogsNoOpWhenNothingWasCommitted(t *testing.T) {
	for _, tc := range []struct {
		name        string
		created     bool
		wantMessage string
	}{
		{"真提交", true, "已创建检查点"},
		{"空操作", false, "未产生提交"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &captureSink{}
			s := NewSession(Config{
				Bounty: Bounty{ID: "b", Task: "t", Repo: gitRepoRef()},
				Git:    &recordingCommitGit{created: tc.created},
				Policy: allowAll{}, Sink: sink,
			})
			s.ops = []WriteOp{{File: "a.txt"}} // 有改动，检查点才会走到提交
			s.RequestCheckpoint("模型认为这里自洽")    // 模型显式请求：不依赖结构判据
			s.checkpoint(context.Background(), &harness.Turn{No: 1})

			found := false
			for _, line := range sink.logs {
				if strings.Contains(line, tc.wantMessage) {
					found = true
				}
				if tc.created == false && strings.Contains(line, "已创建检查点") {
					t.Errorf("空操作不得报已创建：%v", sink.logs)
				}
			}
			if !found {
				t.Errorf("日志应含 %q：%v", tc.wantMessage, sink.logs)
			}
		})
	}
}

// 自动检查点只在结构完整点上产生。默认判据是**不可判定**（符号扩展未接入）：那时我们并**没有
// 判过**，只是没法判——日志必须这么说，不能报成「未落在结构完整点」（那是把"没法判"说成"判过了
// 没通过"）。仍然不提交，也不谎报「已创建检查点」。
func TestCheckpoint_NoAutoCheckpointOffStructuralPoint(t *testing.T) {
	sink := &captureSink{}
	g := &recordingCommitGit{created: true}
	s := NewSession(Config{
		Bounty: Bounty{ID: "b", Task: "t", Repo: gitRepoRef()},
		Git:    g, Policy: allowAll{}, Sink: sink,
	})
	s.ops = []WriteOp{{File: "a.txt"}} // 有改动，但没人请求
	s.checkpoint(context.Background(), &harness.Turn{No: 1})

	if g.calls != 0 {
		t.Errorf("不该调用提交：calls=%d", g.calls)
	}
	joined := strings.Join(sink.logs, "\n")
	if !strings.Contains(joined, "结构判据不可判定") {
		t.Errorf("不可判定必须如实措辞：%v", sink.logs)
	}
	if strings.Contains(joined, "未落在结构完整点") {
		t.Errorf("不可判定不得报成「未落在结构完整点」：%v", sink.logs)
	}
	if strings.Contains(joined, "已创建检查点") {
		t.Errorf("不得谎报已创建：%v", sink.logs)
	}
	if n := countEvent(sink, "degraded"); n != 1 {
		t.Errorf("不可判定应发恰好一条 degraded：%v", sink.events)
	}
}

// 判据可用但未通过 → 不提交、日志说「未落在结构完整点」，与"不可判定"区分开、且**不发** degraded。
func TestCheckpoint_StructuralFailDoesNotCommit(t *testing.T) {
	sink := &captureSink{}
	g := &recordingCommitGit{created: true}
	s := NewSession(Config{
		Bounty: Bounty{ID: "b", Task: "t", Repo: gitRepoRef()},
		Git:    g, Policy: allowAll{}, Sink: sink,
	})
	s.structuralJudge = func() structuralVerdict { return structuralFail }
	s.ops = []WriteOp{{File: "a.txt"}}
	s.checkpoint(context.Background(), &harness.Turn{No: 1})

	if g.calls != 0 {
		t.Errorf("未通过判据不该提交：calls=%d", g.calls)
	}
	joined := strings.Join(sink.logs, "\n")
	if !strings.Contains(joined, "未落在结构完整点") {
		t.Errorf("应如实记录跳过原因：%v", sink.logs)
	}
	if strings.Contains(joined, "不可判定") {
		t.Errorf("未通过不等于不可判定：%v", sink.logs)
	}
	if n := countEvent(sink, "degraded"); n != 0 {
		t.Errorf("判据可用时不发 degraded：%v", sink.events)
	}
}

// 判据通过 ＋ 有改动 ＋ 无模型请求 → 照常提交（三态没把"通过"这条堵死）。
func TestCheckpoint_StructuralPassCommits(t *testing.T) {
	sink := &captureSink{}
	g := &recordingCommitGit{created: true}
	s := NewSession(Config{
		Bounty: Bounty{ID: "b", Task: "t", Repo: gitRepoRef()},
		Git:    g, Policy: allowAll{}, Sink: sink,
	})
	s.structuralJudge = func() structuralVerdict { return structuralPass }
	s.ops = []WriteOp{{File: "a.txt"}}
	s.checkpoint(context.Background(), &harness.Turn{No: 1})

	if g.calls != 1 {
		t.Errorf("判据通过应提交一次：calls=%d", g.calls)
	}
	if !strings.Contains(strings.Join(sink.logs, "\n"), "已创建检查点") {
		t.Errorf("应记录已创建：%v", sink.logs)
	}
}

// 不可判定的降级**每次运行最多一条**：它是运行级常量事实，不是每轮新闻。
func TestCheckpoint_UndecidableReportsDegradedOnce(t *testing.T) {
	sink := &captureSink{}
	s := NewSession(Config{
		Bounty: Bounty{ID: "b", Task: "t", Repo: gitRepoRef()},
		Git:    &recordingCommitGit{created: true}, Policy: allowAll{}, Sink: sink,
	})
	s.ops = []WriteOp{{File: "a.txt"}}
	for n := 1; n <= 3; n++ { // 连跑三轮，每轮都有改动、都不在结构点上
		s.checkpoint(context.Background(), &harness.Turn{No: n})
	}

	if n := countEvent(sink, "degraded"); n != 1 {
		t.Errorf("不可判定的降级每次运行最多一条，实际 %d 条：%v", n, sink.events)
	}
	if deg := sink.ofType("degraded"); len(deg) == 1 && deg[0]["scope"] != "checkpoint" {
		t.Errorf("degraded.scope 应为 checkpoint：%v", deg[0])
	}
}

// recordingCommitGit 记录一次提交的返回值，用于断言日志口径。
type recordingCommitGit struct {
	stubBaselineGit
	created bool
	calls   int
}

func (g *recordingCommitGit) Commit(context.Context, git.RepoRef, string) (git.Commit, error) {
	g.calls++
	return git.Commit{SHA: "sha", Branch: "b", Created: g.created}, nil
}

// failingCommitGit 的 Commit 恒失败：模拟远端不可达。
type failingCommitGit struct {
	stubBaselineGit
	calls int
}

func (g *failingCommitGit) Commit(context.Context, git.RepoRef, string) (git.Commit, error) {
	g.calls++
	return git.Commit{}, errors.New("远端不可达")
}

// flakyCommitGit 前 failTimes 次 Commit 失败，之后成功。
type flakyCommitGit struct {
	stubBaselineGit
	failTimes int
	calls     int
}

func (g *flakyCommitGit) Commit(context.Context, git.RepoRef, string) (git.Commit, error) {
	g.calls++
	if g.calls <= g.failTimes {
		return git.Commit{}, errors.New("临时失败")
	}
	return git.Commit{SHA: "sha", Branch: "b", Created: true}, nil
}

// scriptedCommitGit 按脚本决定每次 Commit 成败（true = 失败）。
type scriptedCommitGit struct {
	stubBaselineGit
	fail  []bool
	calls int
}

func (g *scriptedCommitGit) Commit(context.Context, git.RepoRef, string) (git.Commit, error) {
	i := g.calls
	g.calls++
	if i < len(g.fail) && g.fail[i] {
		return git.Commit{}, errors.New("失败")
	}
	return git.Commit{SHA: "sha", Branch: "b", Created: true}, nil
}

// writingPrim 每次 Execute 写一个**新文件**并返回一份编辑计划（让 checkpoint 有改动可提交）。
type writingPrim struct{ n int }

func (p *writingPrim) Decl() llm.ToolDecl { return llm.ToolDecl{Name: "writer"} }
func (p *writingPrim) Writes() bool       { return true }

func (p *writingPrim) Execute(context.Context, Call, Facts) (Result, []workspace.FileEdit, error) {
	p.n++
	return Result{Summary: "写入"}, []workspace.FileEdit{{File: fmt.Sprintf("f%d.txt", p.n), NewContent: "x"}}, nil
}

// 连败达上限 → 本轮结束即收敛为环境错误（退出 1、原因前缀 checkpoint_failed_streak），
// 且**不跑完剩余轮次**（假上游的 infer 次数恰好等于上限）。
func TestCheckpoint_StreakLimitConvergesAsEnvError(t *testing.T) {
	g := &failingCommitGit{}
	s := NewSession(Config{
		Bounty: Bounty{ID: "b1", Task: "t", Repo: gitRepoRef()},
		Git:    g,
		Opener: stubOpener{},
		Policy: allowAll{},
		Sink:   &captureSink{},
		Tools:  func(workspace.Workspace) []Primitive { return []Primitive{&writingPrim{}} },
	})
	// 判据设为"通过"，让本轮改动真的走到提交（默认不可判定会直接跳过提交）。
	s.structuralJudge = func() structuralVerdict { return structuralPass }

	provider := &stubProvider{turns: []turnScript{
		{calls: []llm.ToolCall{call("c1", "writer", `{}`)}},
		{calls: []llm.ToolCall{call("c2", "writer", `{}`)}},
		{calls: []llm.ToolCall{call("c3", "writer", `{}`)}},
		{calls: []llm.ToolCall{call("c4", "writer", `{}`)}}, // 第 4 轮不该发生
	}}
	engine, err := harness.New(provider,
		[]harness.PrepareHandler{s.Prepare},
		[]harness.OnTurnHandler{s.OnTurn},
		[]harness.FinalHandler{s.Finalize},
	)
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}

	out := engine.Run(context.Background(), harness.Input{})
	if out.Status != harness.StatusFailed || out.ExitCode != harness.ExitEnv {
		t.Errorf("终态 = %s/%d，期望 failed/%d", out.Status, out.ExitCode, harness.ExitEnv)
	}
	if !strings.HasPrefix(out.Reason, "checkpoint_failed_streak") {
		t.Errorf("原因应以 checkpoint_failed_streak 开头：%q", out.Reason)
	}
	if provider.infers != checkpointFailStreakLimit {
		t.Errorf("连败达上限应本轮结束即收敛，不跑完剩余轮次：infer 次数 = %d，期望 %d", provider.infers, checkpointFailStreakLimit)
	}
	// 连败计数恰为上限：Finalize 的**交付提交**失败不计入（它已到收尾，语义不同）。
	if s.commitFailStreak != checkpointFailStreakLimit {
		t.Errorf("连败计数 = %d，期望 %d", s.commitFailStreak, checkpointFailStreakLimit)
	}
}

// 单次失败自愈：失败 1 次后下一次成功 → 连败归零、不收敛、仍保留"下一轮重试"的 warn。
func TestCheckpoint_SingleFailureSelfHeals(t *testing.T) {
	sink := &captureSink{}
	g := &flakyCommitGit{failTimes: 1}
	s := NewSession(Config{
		Bounty: Bounty{ID: "b", Task: "t", Repo: gitRepoRef()},
		Git:    g, Policy: allowAll{}, Sink: sink,
	})
	s.structuralJudge = func() structuralVerdict { return structuralPass }
	s.ops = []WriteOp{{File: "a.txt"}}

	s.checkpoint(context.Background(), &harness.Turn{No: 1}) // 失败
	if s.commitFailStreak != 1 {
		t.Errorf("失败后连败应为 1：%d", s.commitFailStreak)
	}
	if !strings.Contains(strings.Join(sink.logs, "\n"), "将在下一轮重试") {
		t.Errorf("单次失败应保留重试 warn：%v", sink.logs)
	}

	s.checkpoint(context.Background(), &harness.Turn{No: 2}) // 成功
	if s.commitFailStreak != 0 {
		t.Errorf("一次成功即自愈、连败归零：%d", s.commitFailStreak)
	}
	if s.commit == nil {
		t.Error("成功应记下提交")
	}
}

// 连败是**连续**失败数，不是累计失败数：交错序列不该达到上限。
func TestCheckpoint_SuccessResetsStreak(t *testing.T) {
	s := NewSession(Config{
		Bounty: Bounty{ID: "b", Task: "t", Repo: gitRepoRef()},
		Git:    &scriptedCommitGit{fail: []bool{true, true, false, true, true, false}},
		Policy: allowAll{}, Sink: &captureSink{},
	})
	s.structuralJudge = func() structuralVerdict { return structuralPass }
	s.ops = []WriteOp{{File: "a.txt"}}
	for n := 1; n <= 6; n++ {
		s.checkpoint(context.Background(), &harness.Turn{No: n})
		if s.commitFailStreak >= checkpointFailStreakLimit {
			t.Fatalf("交错序列不该达到连败上限（第 %d 轮 streak=%d）：连败是连续失败数，不是累计失败数", n, s.commitFailStreak)
		}
	}
}
