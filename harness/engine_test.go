package harness

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"xhunter/llm"
)

// ============================================================ 路径边界

// 工作区之外的位置必须在解析阶段就被挡住，而且是三道都挡。
func TestWorkspaceRejectsEscape(t *testing.T) {
	ws := testFS(t)

	for _, rel := range []string{
		"",                // 空路径
		"/etc/passwd",     // 绝对路径
		"../outside.txt",  // 上跳一层
		"a/../../b.txt",   // 藏在中段的上跳
		"..\\outside.txt", // 反斜杠形式
	} {
		if _, err := ws.Read(rel, LineRange{}); err == nil {
			t.Errorf("应当拒绝路径 %q，但被接受了", rel)
		}
	}
}

// 同一个工作区内的正常读写必须畅通——校验不该把合法用法一起挡掉。
func TestWorkspaceAllowsRelativePath(t *testing.T) {
	ws := testFS(t)

	if _, err := ws.WriteRange("sub/dir/a.txt", ByteRange{0, 0}, "hello"); err != nil {
		t.Fatalf("工作区内新建失败：%v", err)
	}
	fc, err := ws.Read("sub/dir/a.txt", LineRange{})
	if err != nil {
		t.Fatalf("读取失败：%v", err)
	}
	if fc.Raw != "hello" {
		t.Fatalf("内容 = %q，期望 hello", fc.Raw)
	}
}

// ============================================================ 写入前置条件

// 改一个没读过的文件必须被拒绝——否则就是凭想象编辑。
func TestCommitRequiresPriorRead(t *testing.T) {
	ws := testFS(t)
	if _, err := ws.WriteRange("a.txt", ByteRange{0, 0}, "hello"); err != nil {
		t.Fatal(err)
	}

	cm := &Committer{storage: ws, ledger: NewLedger()}
	_, err := cm.Commit([]FileEdit{{File: "a.txt", ByteRange: ByteRange{0, 5}, NewContent: "HELLO"}})
	if err == nil {
		t.Fatal("未读即写应当被拒绝")
	}

	// 读一次之后，同样的编辑就能通过。
	fc, err := ws.Read("a.txt", LineRange{})
	if err != nil {
		t.Fatal(err)
	}
	cm.ledger.Mark("a.txt", fc.Fingerprint)
	ops, err := cm.Commit([]FileEdit{{File: "a.txt", ByteRange: ByteRange{0, 5}, NewContent: "HELLO"}})
	if err != nil {
		t.Fatalf("已读之后应当可以改写：%v", err)
	}
	if len(ops) != 1 || ops[0].Before != "hello" {
		t.Fatalf("写操作记录不正确：%+v", ops)
	}

	// 内容已变，落回文件应能读出改写结果。
	after, _ := ws.Read("a.txt", LineRange{})
	if after.Raw != "HELLO" {
		t.Fatalf("改写后内容 = %q", after.Raw)
	}
}

// 读完之后文件被改动过，落笔也要被拒绝——否则会覆盖掉别人的改动。
func TestCommitRejectsStaleRead(t *testing.T) {
	ws := testFS(t)
	if _, err := ws.WriteRange("a.txt", ByteRange{0, 0}, "hello"); err != nil {
		t.Fatal(err)
	}
	fc, _ := ws.Read("a.txt", LineRange{})

	ledger := NewLedger()
	ledger.Mark("a.txt", fc.Fingerprint)

	// 模拟"读取之后文件又被改动"。
	if _, err := ws.WriteRange("a.txt", ByteRange{5, 5}, " world"); err != nil {
		t.Fatal(err)
	}

	cm := &Committer{storage: ws, ledger: ledger}
	if _, err := cm.Commit([]FileEdit{{File: "a.txt", ByteRange: ByteRange{0, 5}, NewContent: "HELLO"}}); err == nil {
		t.Fatal("基于过期内容的写入应当被拒绝")
	}
}

// 改写已有文件时，区间之外的字节必须原样保留。
func TestWriteRangePreservesOutsideBytes(t *testing.T) {
	ws := testFS(t)
	if _, err := ws.WriteRange("a.txt", ByteRange{0, 0}, "abcdef"); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.WriteRange("a.txt", ByteRange{2, 4}, "XY"); err != nil {
		t.Fatal(err)
	}
	fc, _ := ws.Read("a.txt", LineRange{})
	if fc.Raw != "abXYef" {
		t.Fatalf("内容 = %q，期望 abXYef", fc.Raw)
	}
}

// ============================================================ 端到端

// 用桩协作者跑完一整轮流程：初始化 → 第一轮写文件 → 第二轮收工 → 收尾。
// 全程不接触模型、网络与真实仓库。
func TestEngineEndToEnd(t *testing.T) {
	root := t.TempDir()
	provider := &stubProvider{}
	sink := &stubSink{}
	rec := &stubSessionRecorder{}
	ext := stubExt{}
	policy := stubPolicy{}
	tools, err := NewRuntime(policy, ext, testTools())
	if err != nil {
		t.Fatalf("构造运行时失败：%v", err)
	}

	eng, err := New(
		Deps{
			Provider: provider,
			Context:  stubContextBuilder{},
			Tools:    tools,
			Policy:   policy,
			Sink:     sink,
			Session:  rec,
			Ext:      ext,
			Git:      &stubGit{root: root},
			Workspaces: testWorkspaces{},
		},
		Config{},
		Pipeline{}, // 零值即默认编排
		DefaultStages,
	)
	if err != nil {
		t.Fatalf("构造引擎失败：%v", err)
	}

	out, err := eng.Run(context.Background(), Bounty{
		ID:   "b-1",
		Task: "新建一个文件",
		Repo: RepoRef{Remote: "origin", Branch: "task/b-1", BaseCommit: "abc"},
	})
	if err != nil {
		t.Fatalf("运行返回错误：%v", err)
	}

	if out.Status != StatusSucceeded || out.ExitCode != ExitOK {
		t.Fatalf("期望成功终态，实际 %s / %d（原因：%s）", out.Status, out.ExitCode, out.Reason)
	}
	if len(rec.ops) != 1 {
		t.Fatalf("期望记录 1 次写操作，实际 %d", len(rec.ops))
	}
	if out.CommitSHA == "" {
		t.Fatal("成功交付应当带有提交标识")
	}

	// 事件流里必须能看到终态上报，且它是最后一个事件。
	last := sink.events[len(sink.events)-1]
	if last.Type != "hunt_end" {
		t.Fatalf("最后一个事件应当是终态上报，实际 %q", last.Type)
	}
}

// 全程没有任何写操作却报成功，会让下游拿到空交付物，因此必须判失败。
func TestEngineFailsWhenNoOutput(t *testing.T) {
	provider := &stubProvider{noToolCall: true}

	tools2, err := NewRuntime(stubPolicy{}, stubExt{}, testTools())
	if err != nil {
		t.Fatal(err)
	}

	eng, err := New(
		Deps{
			Provider: provider,
			Context:  stubContextBuilder{},
			Tools:    tools2,
			Policy:   stubPolicy{},
			Sink:     &stubSink{},
			Session:  &stubSessionRecorder{},
			Ext:      stubExt{},
			Git:      &stubGit{root: t.TempDir()},
			Workspaces: testWorkspaces{},
		},
		Config{},
		Pipeline{},
		DefaultStages,
	)
	if err != nil {
		t.Fatal(err)
	}

	out, err := eng.Run(context.Background(), Bounty{ID: "b-2", Task: "什么都不做"})
	if err != nil {
		t.Fatalf("运行返回错误：%v", err)
	}
	if out.Status != StatusFailed || out.ExitCode != ExitFailed {
		t.Fatalf("无产出应当判失败，实际 %s / %d", out.Status, out.ExitCode)
	}
	if !strings.Contains(out.Reason, "no_output") {
		t.Fatalf("原因应当说明是无产出，实际 %q", out.Reason)
	}
}

// ============================================================ 桩协作者

type stubProvider struct {
	noToolCall bool
	calls      int
}

func (p *stubProvider) Capabilities() Caps { return Caps{MaxContextTokens: 8000} }

// 第一轮发出一次写文件的调用，之后不再发起任何调用——模拟"做完就收工"。
//
// 注意这里发出的是**原始调用**（名字 + 参数 JSON）：模型侧不认识工具集，
// 把它翻译成领域调用是绑定层的事，桩也照这个边界来。
func (p *stubProvider) Infer(_ context.Context, _ llm.Request) (Session, error) {
	ch := make(chan Event, 4)
	if !p.noToolCall && p.sent() {
		ch <- Event{Kind: EvToolUse, Call: llm.ToolCall{
			ID:        "c-1",
			Name:      string(testWrite),
			Arguments: json.RawMessage(`{"path":"note.txt","content":"hello"}`),
		}}
	}
	ch <- Event{Kind: EvEnd}
	close(ch)
	p.calls++
	return &stubSession{ch: ch}, nil
}

func (p *stubProvider) sent() bool { return p.calls == 0 }

type stubSession struct{ ch chan Event }

func (s *stubSession) Events() <-chan Event { return s.ch }
func (s *stubSession) Cancel() error        { return nil }

type stubPolicy struct{}

func (stubPolicy) Decide(_ context.Context, _ Call, _ Route) (Decision, error) {
	return Decision{Verdict: VerdictAllow}, nil
}
func (stubPolicy) Charge(Usage)                    {}
func (stubPolicy) Exhausted(TurnNo) (bool, string) { return false, "" }

type stubExt struct{}

func (stubExt) Capabilities(context.Context) ExtCaps {
	return ExtCaps{Available: true, Languages: []string{"go"}, CanResolve: true}
}
func (stubExt) Locate(context.Context, Call) (Prepared, error) {
	return Prepared{}, nil
}
func (stubExt) Close() error { return nil }

type logEntry struct{ level, msg string }

type stubSink struct {
	events []ExternalEvent
	logs   []logEntry
}

func (s *stubSink) Emit(ev ExternalEvent) error {
	s.events = append(s.events, ev)
	return nil
}
func (s *stubSink) Log(level, msg string, _ ...any) {
	s.logs = append(s.logs, logEntry{level, msg})
}
func (s *stubSink) Heartbeat(Phase) error { return nil }

// warned 报告是否记过 warn 级日志。"缺了什么必须留下痕迹"这类断言靠它——
// 静默地少掉一份项目约定，产出偏差要到评审时才看得出来。
func (s *stubSink) warned() bool {
	for _, l := range s.logs {
		if l.level == "warn" {
			return true
		}
	}
	return false
}

type stubSessionRecorder struct{ ops []WriteOp }

func (s *stubSessionRecorder) RecordTurn(TurnRecord) {}
func (s *stubSessionRecorder) RecordOp(op WriteOp)   { s.ops = append(s.ops, op) }
func (s *stubSessionRecorder) Ops() []WriteOp        { return s.ops }
func (s *stubSessionRecorder) Snapshot() error       { return nil }

type stubContextBuilder struct{}

func (stubContextBuilder) Assemble(_ context.Context, h *Hunt, _ TurnNo) ([]Message, error) {
	return []Message{{Role: RoleUser, Content: h.Bounty.Task}}, nil
}
func (stubContextBuilder) Append(TurnRecord)                        {}
func (stubContextBuilder) Restore(context.Context, []Message) error { return nil }
func (stubContextBuilder) TokenCount() int                          { return 0 }

type stubGit struct {
	root string
	// messages 记录每次提交的提交信息，供检查点测试断言"信息是引擎合成的"。
	messages []string
	// fail 非 nil 时 Commit 失败，用来验证"单次失败不终止"。
	fail error
}

func (g *stubGit) PrepareBaseline(context.Context, RepoRef) (string, error) { return g.root, nil }
func (g *stubGit) Commit(_ context.Context, repo RepoRef, msg string) (Commit, error) {
	g.messages = append(g.messages, msg)
	if g.fail != nil {
		return Commit{}, g.fail
	}
	return Commit{SHA: "deadbeef", Branch: repo.Branch}, nil
}
func (g *stubGit) Diff(context.Context, string) ([]string, error) { return []string{"note.txt"}, nil }
func (g *stubGit) Clean(context.Context) error                    { return nil }
