// Package gate 是门禁领域：提供 check 原语、门禁执行器与检查点联动的落点。
//
// 门禁不是一个普通原语，而是一个跨生命周期的领域：
//   - check 原语——模型显式运行一个具名门禁条目（工具面）；
//   - 执行器——跑命令、按门禁自己声明的判据判定、缓存结论（本文件与 runner.go）；
//   - 检查点联动——门禁通过后本轮必提交，未通过则抑制后续自动检查点（由执行体接线）；
//   - 交付门禁——收尾阶段补跑尚未通过的必需门禁（由执行体接线）。
//
// 命令与判据都在业务侧（清单里），模型只给门禁名——因此它无法通过改命令或改判据让自己
// 更容易通过，也不可能借它把 shell 请回来。
package gate

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"xhunter/hunt"
	"xhunter/llm"
	"xhunter/workspace"
)

// Check 是 check 原语对模型可见的名字。
const Check = hunt.PrimitiveName("check")

// CheckTool 构造 check 原语。门禁是具名条目，名字之外没有定位信息——模型的参数面只有
// 一个名字，命令、判据都在业务侧。
func CheckTool(runner hunt.GateRunner) hunt.Primitive { return checkPrim{runner: runner} }

type checkPrim struct{ runner hunt.GateRunner }

// Writes 报告 check 不写工作盘：它跑什么由任务侧清单决定、不经模型之手，因此不属于
// 「模型写工作区」这件事，策略不为它做路径裁决。
func (checkPrim) Writes() bool { return false }

func (checkPrim) Decl() llm.ToolDecl {
	return llm.ToolDecl{
		Name:        string(Check),
		Description: "运行一个具名校验条目（清单由任务侧提供），并把判定结论作为自检证据。",
		Schema: llm.ObjectSchema(`{
			"name": {"type": "string", "description": "清单里的门禁条目名"}
		}`, "name"),
	}
}

func (p checkPrim) Execute(ctx context.Context, call hunt.Call, facts hunt.Facts) (hunt.Result, []workspace.FileEdit, error) {
	name := strings.TrimSpace(call.Gate)
	if name == "" {
		return hunt.Result{CallID: call.ID, Err: &llm.Fault{
			Kind: "bad_args", Message: "check 需要一个门禁名", Retryable: false}}, nil, nil
	}
	var target hunt.Gate
	found := false
	for _, g := range facts.Gates() {
		if g.Name == name {
			target, found = g, true
			break
		}
	}
	if !found {
		// 名字不在清单里是**结论**（这次没有这条门禁），不是环境问题——重试也不会有。
		return hunt.Result{CallID: call.ID, Err: &llm.Fault{
			Kind: "unknown_gate", Message: "本次的门禁清单里没有 " + strconv.Quote(name), Retryable: false}}, nil, nil
	}
	if p.runner == nil {
		return hunt.Result{CallID: call.ID, Err: &llm.Fault{
			Kind: "gate_unavailable", Message: "未装配门禁执行器", Retryable: true}}, nil, nil
	}

	res, err := p.runner.Run(ctx, facts.WorkRoot(), target, facts.ChangeFingerprint())
	if err != nil {
		// 执行失败或判据不可判定都是**环境问题**：修好配置就能重跑，不该被读成"代码质量差"。
		return hunt.Result{CallID: call.ID, Err: &llm.Fault{
			Kind: "gate_unavailable", Message: err.Error(), Retryable: true}}, nil, nil
	}
	res.CallID = string(call.ID)
	facts.RecordGateResult(res)
	if res.Passed {
		// 门禁通过是一个语义自洽点：这一刻的改动是验证过的，值得钉在分支上。
		facts.RequestCheckpoint("门禁 " + res.Name + " 通过")
	}
	return hunt.Result{CallID: call.ID, Summary: formatGateResult(res)}, nil, nil
}

// formatGateResult 把**结论**交给模型：门禁在内部跑完并按自己声明的判据判完，交回的是判定
// 结果与必要的证据，不是原始输出。
//
// 让模型自己读几十兆构建日志去判断过没过，既浪费上下文，也不可复现——同一个门禁两次跑，
// 结论必须相同，而"模型读完日志后的判断"不满足这一点。
func formatGateResult(res hunt.GateResult) string {
	verdict := "不通过"
	if res.Passed {
		verdict = "通过"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "门禁 %s：%s（判据：%s；exit %d，%dms", res.Name, verdict, res.Summary, res.ExitCode, res.DurationMS)
	if res.Cached {
		sb.WriteString("，缓存结果")
	}
	sb.WriteString("）")
	if !res.Passed && strings.TrimSpace(res.Evidence) != "" {
		// 不通过时给证据：模型要能据此修；通过时不给——那只是噪音。
		sb.WriteString("\n")
		sb.WriteString(res.Evidence)
	}
	return sb.String()
}
