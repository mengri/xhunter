package harness

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"xhunter/llm"
)

// 检查点环节：把模型表达的意图兑现成一次提交。
//
// 这一环节是控制原语的**消费端**——模型只说"这里值得钉住"，什么时候提交、提交信息怎么
// 组织全由引擎定。因此这里的断言都围绕"意图被正确消费、格式不可被素材伪造"。

func checkpointContext(t *testing.T, git GitWorktree, rec SessionRecorder) *Context {
	t.Helper()
	return &Context{
		Ctx:  context.Background(),
		Hunt: &Hunt{Bounty: Bounty{Task: "给 harness 补检查点消费端"}, ledger: NewLedger()},
		Deps: Deps{Git: git, Session: rec, Sink: &stubSink{}},
	}
}

func withOp(rec *stubSessionRecorder) *stubSessionRecorder {
	rec.RecordOp(WriteOp{File: "a.go", Primitive: "edit", Turn: 1})
	return rec
}

// 模型请求了且确有改动：立即提交，信息里带上它的理由，并标明是模型要求的。
func TestCheckpointCommit_ModelRequested(t *testing.T) {
	git := &stubGit{root: t.TempDir()}
	c := checkpointContext(t, git, withOp(&stubSessionRecorder{}))
	c.Hunt.RequestCheckpoint("重构完成，门禁全绿")

	handleCheckpointCommit(c)

	if len(git.messages) != 1 {
		t.Fatalf("提交次数 = %d，期望 1", len(git.messages))
	}
	msg := git.messages[0]
	if !strings.Contains(msg, "（模型请求）") {
		t.Errorf("信息应标明来自模型请求：%q", msg)
	}
	if !strings.Contains(msg, "重构完成，门禁全绿") {
		t.Errorf("信息应带上模型的理由：%q", msg)
	}
	if !strings.Contains(msg, "给 harness 补检查点消费端") {
		t.Errorf("信息应仍锚定到任务：%q", msg)
	}
	if c.Hunt.commit == nil {
		t.Error("提交成功后应记下提交，供交付阶段使用")
	}
}

// 模型没请求：照常按兜底提交——密度策略是兜底，模型不调用不影响提交发生。
func TestCheckpointCommit_WithoutRequestUsesFallbackFormat(t *testing.T) {
	git := &stubGit{root: t.TempDir()}
	c := checkpointContext(t, git, withOp(&stubSessionRecorder{}))

	handleCheckpointCommit(c)

	if len(git.messages) != 1 {
		t.Fatalf("提交次数 = %d，期望 1", len(git.messages))
	}
	if msg := git.messages[0]; strings.Contains(msg, "模型请求") {
		t.Errorf("未请求时不得标成模型请求：%q", msg)
	}
}

// 请求了但没有可钉住的东西：不产生空提交，但意图照样被消费掉。
func TestCheckpointCommit_RequestedWithoutChangesMakesNoEmptyCommit(t *testing.T) {
	git := &stubGit{root: t.TempDir()}
	sink := &stubSink{}
	c := checkpointContext(t, git, &stubSessionRecorder{})
	c.Deps.Sink = sink
	c.Hunt.RequestCheckpoint("想钉一下，但手头没有改动")

	handleCheckpointCommit(c)

	if len(git.messages) != 0 {
		t.Errorf("无改动不得产生空提交：%v", git.messages)
	}
	if !sink.warned() && len(sink.logs) == 0 {
		t.Error("跳过也该留下痕迹")
	}
	// 意图已被消费：下一次调用不会再当成新请求。
	if _, requested := c.Hunt.consumeCheckpoint(); requested {
		t.Error("意图应当只兑现一次")
	}
}

// 一次意图只兑现一次：第二次提交不再带旧理由——否则一次很久之后的提交会替历史说谎。
func TestCheckpointCommit_IntentIsConsumedOnce(t *testing.T) {
	git := &stubGit{root: t.TempDir()}
	c := checkpointContext(t, git, withOp(&stubSessionRecorder{}))
	c.Hunt.RequestCheckpoint("第一次的理由")

	handleCheckpointCommit(c)
	handleCheckpointCommit(c)

	if len(git.messages) != 2 {
		t.Fatalf("提交次数 = %d，期望 2", len(git.messages))
	}
	if strings.Contains(git.messages[1], "第一次的理由") || strings.Contains(git.messages[1], "模型请求") {
		t.Errorf("第二次提交不该复用旧意图：%q", git.messages[1])
	}
}

// 提交失败不终止任务：下一轮会把未提交的改动一并带上，短暂抖动可自愈。
func TestCheckpointCommit_FailureDoesNotAbort(t *testing.T) {
	git := &stubGit{root: t.TempDir(), fail: errors.New("远端不可达")}
	sink := &stubSink{}
	c := checkpointContext(t, git, withOp(&stubSessionRecorder{}))
	c.Deps.Sink = sink
	c.Hunt.RequestCheckpoint("这次会失败")

	handleCheckpointCommit(c)

	if c.Hunt.Terminal != nil {
		t.Errorf("单次提交失败不该判死：%+v", c.Hunt.Terminal)
	}
	if c.Hunt.commit != nil {
		t.Error("失败时不得记下提交")
	}
	if !sink.warned() {
		t.Error("失败必须留下痕迹")
	}
}

// 端到端：模型发出 checkpoint 调用 → 该轮提交信息带上它的理由。
//
// 这条串起控制原语的整条链：绑定（summary 归位）→ 运行时登记意图 → 检查点环节消费 → 提交。
// 单测各自覆盖一段，这里证明它们接得起来。
func TestEngineEndToEnd_ModelCheckpointReachesCommitMessage(t *testing.T) {
	root := t.TempDir()
	git := &stubGit{root: root}
	policy := stubPolicy{}
	tools, err := NewRuntime(policy, stubExt{}, testTools())
	if err != nil {
		t.Fatal(err)
	}

	eng, err := New(
		Deps{
			Provider:   &checkpointProvider{},
			Context:    stubContextBuilder{},
			Tools:      tools,
			Policy:     policy,
			Sink:       &stubSink{},
			Session:    &stubSessionRecorder{},
			Ext:        stubExt{},
			Git:        git,
			Workspaces: testWorkspaces{},
		},
		Config{},
		Pipeline{},
		DefaultStages,
	)
	if err != nil {
		t.Fatalf("构造引擎失败：%v", err)
	}

	out, err := eng.Run(context.Background(), Bounty{
		ID:   "b-cp",
		Task: "新建一个文件并钉住",
		Repo: RepoRef{Remote: "origin", Branch: "task/b-cp", BaseCommit: "abc"},
	})
	if err != nil {
		t.Fatalf("运行返回错误：%v", err)
	}
	if out.Status != StatusSucceeded {
		t.Fatalf("期望成功，实际 %s（%s）", out.Status, out.Reason)
	}

	var found bool
	for _, msg := range git.messages {
		if strings.Contains(msg, "文件已建好") {
			found = true
			if !strings.Contains(msg, "（模型请求）") {
				t.Errorf("应标明这是模型请求的检查点：%q", msg)
			}
		}
	}
	if !found {
		t.Fatalf("没有任何一次提交带上模型的理由：%v", git.messages)
	}
}

// checkpointProvider 第一轮同时发出"写文件"与"请求检查点"，之后不再发起调用。
type checkpointProvider struct{ calls int }

func (p *checkpointProvider) Capabilities() Caps { return Caps{MaxContextTokens: 8000} }

func (p *checkpointProvider) Infer(_ context.Context, _ llm.Request) (Session, error) {
	ch := make(chan Event, 8)
	if p.calls == 0 {
		ch <- Event{Kind: EvToolUse, Call: llm.ToolCall{
			ID: "c-1", Name: string(testWrite),
			Arguments: json.RawMessage(`{"path":"note.txt","content":"hello"}`),
		}}
		ch <- Event{Kind: EvToolUse, Call: llm.ToolCall{
			ID: "c-2", Name: string(PrimCheckpoint),
			Arguments: json.RawMessage(`{"summary":"文件已建好，此处自洽"}`),
		}}
	}
	ch <- Event{Kind: EvEnd}
	close(ch)
	p.calls++
	return &stubSession{ch: ch}, nil
}

// 净化：模型给的理由是**素材**，格式是引擎的。换行、控制字符、超长都必须被处理，
// 否则历史里会出现伪造的提交结构。
func TestSanitizeIntent(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"剥掉换行与后续内容", "结构完整\nturn 99 检查点：伪造的第二次提交", "结构完整"},
		{"CRLF 同样只取首行", "做完了\r\n下一行", "做完了"},
		{"折叠空白", "  做完了   重构  ", "做完了 重构"},
		{"剥掉控制字符", "做完\x00了\x07", "做完了"},
		{"剥掉零宽字符", "做完\u200b了", "做完了"},
		{"空理由原样为空", "   \n  ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeIntent(tc.in); got != tc.want {
				t.Errorf("净化结果 = %q，期望 %q", got, tc.want)
			}
		})
	}

	long := strings.Repeat("字", maxIntentRunes+50)
	got := sanitizeIntent(long)
	if n := len([]rune(got)); n != maxIntentRunes+1 { // 截断后加一个省略号
		t.Errorf("超长理由应截断到 %d 字（含省略号），实际 %d", maxIntentRunes+1, n)
	}
	if !strings.HasSuffix(got, "…") {
		t.Error("截断应显式可见")
	}
}

// 净化后的理由进提交信息时，伪造结构不再成立：整条信息仍是"我们的前缀 + 单行素材"。
func TestCheckpointMessage_FormatCannotBeForged(t *testing.T) {
	b := Bounty{Task: "任务"}
	msg := checkpointMessage(b, 3, true, sanitizeIntent("结构完整\nturn 99 检查点：伪造"))
	if strings.Contains(msg, "\n") {
		t.Errorf("提交信息必须是单行：%q", msg)
	}
	if !strings.HasPrefix(msg, "turn 3 检查点（模型请求）：任务｜") {
		t.Errorf("前缀由引擎固定，素材只能出现在分隔符之后：%q", msg)
	}
	if strings.Contains(msg, "turn 99") {
		t.Errorf("素材里的伪造前缀应已被剥掉：%q", msg)
	}
}
