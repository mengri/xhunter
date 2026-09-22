package hunt

import (
	"context"
	"encoding/json"
	"errors"
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

func (r *recordingSession) RecordTurn(rec harness.Turn) { r.turns = append(r.turns, rec) }
func (r *recordingSession) Snapshot() error             { return nil }

// 加工必须发生在落历史之前：历史与材料都是值拷贝，抢在后面改的过滤器等于白做——
// 模型下一轮看不到改动，而过滤器作者会以为生效了。
func TestOnTurn_FiltersRunBeforeRecording(t *testing.T) {
	ctxLog := &recordingContext{}
	rec := &recordingSession{}
	s := NewSession(Config{
		Context: ctxLog,
		Session: rec,
		Filters: []ResultFilter{
			func(_ context.Context, turn *harness.Turn) error { turn.Text += "-甲"; return nil },
			func(_ context.Context, turn *harness.Turn) error { turn.Text += "-乙"; return nil },
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
		Filters: []ResultFilter{
			func(_ context.Context, _ *harness.Turn) error { return errors.New("脱敏失败") },
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
func (stubBaselineGit) Diff(context.Context, string) ([]string, error) { return nil, nil }
func (stubBaselineGit) Patch(context.Context, string) (string, error)  { return "", nil }
func (stubBaselineGit) Clean(context.Context) error                    { return nil }

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
