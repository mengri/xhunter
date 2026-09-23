package hunt

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"xhunter/git"
	"xhunter/harness"
	"xhunter/llm"
	"xhunter/workspace"
)

// MS-4 的三条验收（IA-11.8 / IA-11.10 / IA-11.13）守的是最容易被一次重构无声破坏的东西：
// git 动作的**调用时机**、git 能力的**不可见性**、模型意图的**兑现次数**。

// ============================================================ IA-11.8 时序

// orderGit 在取基线时留痕。
type orderGit struct {
	stubBaselineGit
	order *[]string
}

func (g *orderGit) PrepareBaseline(context.Context, git.RepoRef) (string, error) {
	*g.order = append(*g.order, "prepare_baseline")
	return "root", nil
}

// orderOpener 在打开工作区时留痕。
type orderOpener struct{ order *[]string }

func (o orderOpener) Open(string) (workspace.Storage, error) {
	*o.order = append(*o.order, "open")
	return &memStorage{files: map[string]string{}}, nil
}

// orderPlugin 在构造正文时留痕。
type orderPlugin struct{ order *[]string }

func (p orderPlugin) Name() string { return "order-probe" }

func (p orderPlugin) Build(context.Context, PromptInput) (PromptPart, error) {
	*p.order = append(*p.order, "build")
	return PromptPart{Body: "x"}, nil
}

// `PrepareBaseline` 必须是第一个动作：先于打开工作区、构造原语与构造正文。
func TestWiring_PrepareBaselineRunsBeforeAnyTool(t *testing.T) {
	var order []string
	s := NewSession(Config{
		Bounty: Bounty{ID: "b1", Task: "t", Repo: gitRepoRef()},
		Git:    &orderGit{order: &order},
		Opener: orderOpener{order: &order},
		Policy: allowAll{},
		Sink:   &captureSink{},
		Tools: func(workspace.Workspace) []Primitive {
			order = append(order, "tools")
			return nil
		},
		SystemPlugins: func(workspace.Workspace) []PromptPlugin {
			return []PromptPlugin{orderPlugin{order: &order}}
		},
	})
	if err := s.Prepare(context.Background(), &harness.Run{}); err != nil {
		t.Fatalf("Prepare 失败：%v", err)
	}

	want := []string{"prepare_baseline", "open", "tools", "build"}
	if len(order) != len(want) {
		t.Fatalf("调用顺序 = %v，期望 %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("第 %d 个动作 = %q，期望 %q（%v）", i+1, order[i], want[i], order)
		}
	}
}

// orderRecordingGit 记录每次 Commit 的**类型**（检查点 / 交付）与提交信息。
type orderRecordingGit struct {
	stubBaselineGit
	order *[]string
	msgs  []string
}

func (g *orderRecordingGit) Commit(_ context.Context, _ git.RepoRef, msg string) (git.Commit, error) {
	g.msgs = append(g.msgs, msg)
	kind := "delivery"
	if strings.HasPrefix(msg, "turn ") {
		kind = "checkpoint"
	}
	*g.order = append(*g.order, "commit:"+kind)
	return git.Commit{SHA: "sha", Branch: "b", Created: true}, nil
}

// orderedWritingPrim 写一个新文件，并在调用序列里记一次 "tool"。
type orderedWritingPrim struct {
	n     int
	order *[]string
}

func (p *orderedWritingPrim) Decl() llm.ToolDecl { return llm.ToolDecl{Name: "writer"} }
func (p *orderedWritingPrim) Writes() bool       { return true }

func (p *orderedWritingPrim) Execute(context.Context, Call, Facts) (Result, []workspace.FileEdit, error) {
	p.n++
	*p.order = append(*p.order, "tool")
	return Result{Summary: "写入"}, []workspace.FileEdit{{File: fmt.Sprintf("f%d.txt", p.n), NewContent: "x"}}, nil
}

// turnsBudgetPolicy 在指定轮次后耗尽轮数预算（让运行在该轮之后停下、进入收尾）。
type turnsBudgetPolicy struct{ limit int }

func (turnsBudgetPolicy) Decide(context.Context, Call) (Decision, error) {
	return Decision{Verdict: VerdictAllow, Reason: "放行"}, nil
}
func (turnsBudgetPolicy) Charge(llm.Usage) {}

func (p turnsBudgetPolicy) Exhausted(turn TurnNo) (bool, string) {
	if int(turn) >= p.limit {
		return true, "turns"
	}
	return false, ""
}

// git 动作只在轮边界与收尾发生：`Prepare` 期间零次 Commit；每轮的检查点提交在该轮工具之后、
// 下一轮工具之前；交付提交在最后。断言整条序列，而不只是次数。
func TestCheckpoint_CommitOnlyAtTurnBoundaryAndFinalize(t *testing.T) {
	var order []string
	g := &orderRecordingGit{order: &order}
	s := NewSession(Config{
		Bounty: Bounty{ID: "b1", Task: "t", Repo: gitRepoRef()},
		Git:    g,
		Opener: stubOpener{},
		Policy: turnsBudgetPolicy{limit: 2}, // 第 2 轮后耗尽预算 → 进入收尾（交付提交）
		Sink:   &captureSink{},
		Tools:  func(workspace.Workspace) []Primitive { return []Primitive{&orderedWritingPrim{order: &order}} },
	})
	s.structuralJudge = func() structuralVerdict { return structuralPass }

	provider := &stubProvider{turns: []turnScript{
		{calls: []llm.ToolCall{call("c1", "writer", `{}`)}},
		{calls: []llm.ToolCall{call("c2", "writer", `{}`)}},
	}}
	engine, err := harness.New(provider,
		[]harness.PrepareHandler{s.Prepare},
		[]harness.OnTurnHandler{s.OnTurn},
		[]harness.FinalHandler{s.Finalize},
	)
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	engine.Run(context.Background(), harness.Input{})

	want := []string{"tool", "commit:checkpoint", "tool", "commit:checkpoint", "commit:delivery"}
	if len(order) != len(want) {
		t.Fatalf("调用序列 = %v，期望 %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("第 %d 步 = %q，期望 %q（%v）", i+1, order[i], want[i], order)
		}
	}
	if len(g.msgs) < 2 || !strings.HasPrefix(g.msgs[0], "turn 1 检查点") ||
		!strings.HasPrefix(g.msgs[len(g.msgs)-1], "任务改动：") {
		t.Errorf("提交信息口径不对：%v", g.msgs)
	}
}

// ============================================================ IA-11.10 时间语义

// 提交的是**本轮已应用的改动**：第 1 轮的检查点提交在第 2 轮工具之前（不含下一轮内容）；
// 末轮（第 2 轮）未通过判据 → 它的改动不丢，由收尾的交付提交带上（且是交付口径）。
func TestCheckpoint_CommitsOnlyThisTurnsChanges(t *testing.T) {
	var order []string
	g := &orderRecordingGit{order: &order}
	s := NewSession(Config{
		Bounty: Bounty{ID: "b1", Task: "t", Repo: gitRepoRef()},
		Git:    g,
		Opener: stubOpener{},
		Policy: turnsBudgetPolicy{limit: 2},
		Sink:   &captureSink{},
		Tools:  func(workspace.Workspace) []Primitive { return []Primitive{&orderedWritingPrim{order: &order}} },
	})
	// 判据：第 1 轮通过、第 2 轮未通过（让末轮改动落在检查点之外，只能靠交付提交带走）。
	calls := 0
	s.structuralJudge = func() structuralVerdict {
		calls++
		if calls == 1 {
			return structuralPass
		}
		return structuralFail
	}

	provider := &stubProvider{turns: []turnScript{
		{calls: []llm.ToolCall{call("c1", "writer", `{}`)}},
		{calls: []llm.ToolCall{call("c2", "writer", `{}`)}},
	}}
	engine, err := harness.New(provider,
		[]harness.PrepareHandler{s.Prepare},
		[]harness.OnTurnHandler{s.OnTurn},
		[]harness.FinalHandler{s.Finalize},
	)
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	engine.Run(context.Background(), harness.Input{})

	// tool(t1) → commit(检查点 t1) → tool(t2) → commit(交付)：检查点提交排在 tool(t2) 之前，
	// 说明它只含第 1 轮的改动，不含下一轮内容。
	want := []string{"tool", "commit:checkpoint", "tool", "commit:delivery"}
	if len(order) != len(want) {
		t.Fatalf("调用序列 = %v，期望 %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("第 %d 步 = %q，期望 %q（%v）", i+1, order[i], want[i], order)
		}
	}
	if len(g.msgs) < 2 {
		t.Fatalf("应至少两次提交（检查点 ＋ 交付）：%v", g.msgs)
	}
	if !strings.HasPrefix(g.msgs[0], "turn 1 检查点：") {
		t.Errorf("第 1 轮检查点信息 = %q", g.msgs[0])
	}
	if !strings.HasPrefix(g.msgs[len(g.msgs)-1], "任务改动：") {
		t.Errorf("末轮改动应由交付提交带上（交付口径）：%v", g.msgs)
	}
}

// ============================================================ IA-11.13 意图兑现

// msgRecordingGit 记录每次 Commit 的提交信息。
type msgRecordingGit struct {
	stubBaselineGit
	msgs  []string
	calls int
}

func (g *msgRecordingGit) Commit(_ context.Context, _ git.RepoRef, msg string) (git.Commit, error) {
	g.calls++
	g.msgs = append(g.msgs, msg)
	return git.Commit{SHA: "sha", Branch: "b", Created: true}, nil
}

// 模型意图的兑现：同一轮重复请求只兑现一次且不覆盖首次理由；一次请求只兑现一次；
// 「无改动 → 不产生空提交」与「意图照样被消费」是**两件事**，必须分开断言。
func TestCheckpoint_ModelRequestIsConsumedOnceAndSkipsEmptyCommit(t *testing.T) {
	g := &msgRecordingGit{}
	s := NewSession(Config{
		Bounty: Bounty{ID: "b1", Task: "任务首行", Repo: gitRepoRef()},
		Git:    g, Policy: allowAll{}, Sink: &captureSink{},
	})

	// 同一轮重复请求：第二次返回"已请求过"，且不覆盖首次理由。
	first := s.executeCheckpoint(Call{ID: "c1", Primitive: PrimCheckpoint, Summary: "第一次的理由"})
	second := s.executeCheckpoint(Call{ID: "c2", Primitive: PrimCheckpoint, Summary: "第二次的理由"})
	if !strings.Contains(first.Output, "已记录检查点意图") {
		t.Errorf("首次请求应被记录：%q", first.Output)
	}
	if !strings.Contains(second.Output, "已请求过") {
		t.Errorf("重复请求应返回已请求过：%q", second.Output)
	}
	if s.checkpointSummary != "第一次的理由" {
		t.Errorf("重复请求不得覆盖首次理由：%q", s.checkpointSummary)
	}

	// 一次请求只兑现一次：消费后意图清空，提交信息里是首次理由。
	s.ops = []WriteOp{{File: "a.txt"}}
	s.checkpoint(context.Background(), &harness.Turn{No: 1})
	if s.CheckpointRequested() {
		t.Error("checkpoint() 应消费掉意图（一次请求只兑现一次）")
	}
	if g.calls != 1 {
		t.Fatalf("模型请求应提交一次：calls=%d", g.calls)
	}
	if !strings.Contains(g.msgs[0], "（模型请求）") || !strings.Contains(g.msgs[0], "第一次的理由") {
		t.Errorf("提交信息应含首次理由与请求标记：%q", g.msgs[0])
	}

	// 无改动 → 不产生空提交，但意图照样被消费——两者分开断言。
	s.RequestCheckpoint("第二次请求")
	s.ops = nil
	s.checkpoint(context.Background(), &harness.Turn{No: 2})
	if g.calls != 1 {
		t.Errorf("无改动不该产生空提交：calls=%d", g.calls)
	}
	if s.CheckpointRequested() {
		t.Error("无改动时意图照样被消费（模型说了就算数，与有没有东西可提交是两码事）")
	}
}

// 提交信息是**执行体合成**的产物：前缀与分隔符由执行体固定，模型改不了。
func TestCheckpointMessage_Composition(t *testing.T) {
	b := Bounty{ID: "b", Task: "给仓库加一段说明\n验收：能跑通"}

	cases := []struct {
		name string
		got  string
		want string
	}{
		{"非请求", checkpointMessage(b, 2, false, ""), "turn 2 检查点：给仓库加一段说明"},
		{"模型请求", checkpointMessage(b, 2, true, "这里自洽"), "turn 2 检查点（模型请求）：给仓库加一段说明｜这里自洽"},
		{"请求但无理由不带分隔符", checkpointMessage(b, 2, true, ""), "turn 2 检查点（模型请求）：给仓库加一段说明"},
		{"交付", deliveryMessage(b), "任务改动：给仓库加一段说明"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s：%q，期望 %q", c.name, c.got, c.want)
		}
	}
	if strings.Contains(cases[2].got, "｜") {
		t.Errorf("理由为空时不该留悬空分隔符：%q", cases[2].got)
	}

	// 理由里的零宽/控制字符在进入最终信息前已被净化（净化本身见 TestSanitizeIntent）：
	// 这里只断言进入最终信息的形状——前缀与分隔符由执行体固定。
	got := checkpointMessage(b, 2, true, sanitizeIntent("理由\u200b带零宽"))
	if got != "turn 2 检查点（模型请求）：给仓库加一段说明｜理由带零宽" {
		t.Errorf("净化后的理由形状不对：%q", got)
	}
	if strings.Count(got, "｜") != 1 {
		t.Errorf("分隔符应恰好一个：%q", got)
	}

	// 任务正文超长：只取首行并按既有上限截断（与 taskSubject 同一口径）。
	long := Bounty{Task: strings.Repeat("字", 100)}
	if got := checkpointMessage(long, 2, false, ""); got != "turn 2 检查点："+taskSubject(long) {
		t.Errorf("超长任务应按 taskSubject 口径截断：%q", got)
	}
	if n := len([]rune(taskSubject(long))); n != 61 {
		t.Errorf("截断后应为 60 字 + 省略号（共 61）：%d", n)
	}
}
