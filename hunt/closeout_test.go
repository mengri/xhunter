package hunt

import (
	"context"
	"errors"
	"strings"
	"testing"

	"xhunter/harness"
	"xhunter/llm"
	"xhunter/workspace"
)

// MS-2 收口：`Policy.Charge` 收到的**增量**、以及插件失败**收敛成正确退出码与终态**——
// 这两条此前只有间接证据（usage 事件的增量、Prepare 返回了错误），本次补直接断言。

// recordingPolicy 记录每次 Charge 收到的用量。
type recordingPolicy struct {
	charges []llm.Usage
}

func (p *recordingPolicy) Decide(context.Context, Call) (Decision, error) {
	return Decision{Verdict: VerdictAllow, Reason: "测试放行"}, nil
}
func (p *recordingPolicy) Charge(u llm.Usage)              { p.charges = append(p.charges, u) }
func (p *recordingPolicy) Exhausted(TurnNo) (bool, string) { return false, "" }
func (p *recordingPolicy) ObserveFailure(string) (StopLoss, string) {
	return StopContinue, ""
}
func (p *recordingPolicy) DeniedCount() (int, bool) { return 0, false }

// Policy.Charge 拿的是**本轮增量**：累计值被反复当增量上报，预算会被自己的重报耗尽。
func TestPolicy_ChargeReceivesIncrements(t *testing.T) {
	pol := &recordingPolicy{}
	s := NewSession(Config{Policy: pol})
	ctx := context.Background()

	run := &harness.Run{Usage: llm.Usage{InputTokens: 100, OutputTokens: 40}}
	if _, err := s.OnTurn(ctx, run, &harness.Turn{No: 1}); err != nil {
		t.Fatalf("OnTurn 失败：%v", err)
	}
	run.Usage = llm.Usage{InputTokens: 150, OutputTokens: 70} // 本轮增量 50 / 30
	if _, err := s.OnTurn(ctx, run, &harness.Turn{No: 2}); err != nil {
		t.Fatalf("OnTurn 失败：%v", err)
	}
	// 第三轮用量没有增长：不应产生一次正增量上报。
	if _, err := s.OnTurn(ctx, run, &harness.Turn{No: 3}); err != nil {
		t.Fatalf("OnTurn 失败：%v", err)
	}

	if len(pol.charges) != 2 {
		t.Fatalf("Charge 调用次数 = %d，期望 2（无增长的那一轮不上报）：%v", len(pol.charges), pol.charges)
	}
	if pol.charges[0] != (llm.Usage{InputTokens: 100, OutputTokens: 40}) {
		t.Errorf("第 1 轮 Charge = %+v，期望增量 {100 40}", pol.charges[0])
	}
	if pol.charges[1] != (llm.Usage{InputTokens: 50, OutputTokens: 30}) {
		t.Errorf("第 2 轮 Charge = %+v，期望本轮增量 {50 30}，而不是累计值再来一遍", pol.charges[1])
	}
}

// failingPlugin 的 Build 永远失败。
type failingPlugin struct{}

func (failingPlugin) Name() string { return "failing" }
func (failingPlugin) Build(context.Context, PromptInput) (PromptPart, error) {
	return PromptPart{}, errors.New("插件构建失败")
}

// 插件失败按环境错误收敛：Prepare 失败 → 循环不开始 → 终态 failed / 原因前缀 prepare_failed /
// **退出码 1**（未进入对话 = 环境问题，可重跑）。断言的是收敛结果，不是"返回了错误"。
func TestPrepare_PluginFailureConvergesAsEnvError(t *testing.T) {
	s := NewSession(Config{
		Bounty: Bounty{ID: "b1", Task: "t", Repo: gitRepoRef()},
		Git:    &stubBaselineGit{},
		Opener: stubOpener{},
		Policy: allowAll{},
		Sink:   &captureSink{},
		SystemPlugins: func(workspace.Workspace) []PromptPlugin {
			return []PromptPlugin{failingPlugin{}}
		},
	})
	provider := &stubProvider{turns: []turnScript{{text: "不该跑到这里"}}}
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
	if !strings.HasPrefix(out.Reason, "prepare_failed") {
		t.Errorf("原因应以 prepare_failed 开头：%q", out.Reason)
	}
	if provider.infers != 0 {
		t.Errorf("Prepare 失败时循环不该开始：infer 次数 = %d", provider.infers)
	}
}
