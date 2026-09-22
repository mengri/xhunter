package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ToolRuntime 是引擎看到的工具执行接口。Runtime 是它的默认实现。
// 把接口与实现分开，是为了让测试可以整体替换。
type ToolRuntime interface {
	Execute(ctx context.Context, in ExecInput) Result
	// Decls 返回模型可见的工具声明，顺序即模型看到的顺序。
	//
	// 它由执行用的同一份原语清单推出——分开维护就会出现"说明了 A、实际执行了 B"
	// 的漂移，而这正是当初把工具面定格成一个集合的原因。
	Decls() []ToolDecl
}

type ExecInput struct {
	Call  Call
	Turn  TurnNo
	Caps  ExtCaps
	Facts TaskFacts
}

// Runtime 把模型发出的调用变成"结果 + 写操作记录"。
//
// 它是**无状态**的，任务级事实每次都从 ExecInput.Facts 取。这不是风格偏好，
// 而是被时序逼出来的：工作区根要等基线准备好才存在，门禁清单要等门禁环节读完
// 才存在，两者都是运行期产物，而 Runtime 在装配期就已经构造好了——
// 构造参数里根本放不下它们。
//
// 它**不认识任何具体原语**：有哪些原语、叫什么、寻址性质如何，全部由装配层在构造时
// 交进来。框架因此可以原样装上一套完全不同的工具集，而"这套工具集就是这 7 个"这类
// 业务约束留在装配层。
type Runtime struct {
	tools  map[PrimitiveName]Tool
	order  []PrimitiveName
	policy Policy
	ext    ExtHost
}

// NewRuntime 以装配层给的清单构造运行时。
//
// 这里只校验**结构自洽**，不校验业务合法性：名字非空、实现齐备、声明名字与键一致、
// 不重复。有没有某个原语、该不该有，是装配层的事——框架替业务做判断，等于把业务
// 知识写进框架，那条边界一旦模糊，框架就没法原样复用到别的工具集上。
func NewRuntime(policy Policy, ext ExtHost, tools []Tool) (*Runtime, error) {
	byName := make(map[PrimitiveName]Tool, len(tools))
	order := make([]PrimitiveName, 0, len(tools))
	for _, t := range tools {
		switch {
		case t.Name == "":
			return nil, errors.New("原语缺少名字")
		case t.Impl == nil:
			return nil, fmt.Errorf("原语 %q 没有实现", t.Name)
		case t.Decl.Name != string(t.Name):
			return nil, fmt.Errorf("原语 %q 的声明名字是 %q，两者必须一致", t.Name, t.Decl.Name)
		}
		if _, dup := byName[t.Name]; dup {
			return nil, fmt.Errorf("原语 %q 在清单里出现了两次", t.Name)
		}
		byName[t.Name] = t
		order = append(order, t.Name)
	}
	return &Runtime{tools: byName, order: order, policy: policy, ext: ext}, nil
}

// Decls 按装配顺序给出声明，控制原语殿后。
func (rt *Runtime) Decls() []ToolDecl {
	tools := make([]Tool, 0, len(rt.order))
	for _, name := range rt.order {
		tools = append(tools, rt.tools[name])
	}
	return VisibleToolDecls(tools)
}

// Execute 的步骤顺序是固定的：
//
//	Inspect → Dispatch → [Locate] → Decide → Plan → Commit
//
// 顺序里有两条不能动的约束：
//
//   - 分发是纯函数、没有副作用，所以可以放在裁决之前跑。正因如此，
//     裁决时才看得到"这次调用走的是哪条路径、影响面有多大"——
//     否则策略只能看到一个孤零零的工具名，拦不住"这次重命名会改 200 个文件"。
//
//   - 定位（符号路径的只读解析）也必须在裁决之前完成，它产出的影响面正是上面的输入。
//
// 写盘放到最后，且只有一处：原语只产出编辑计划，落盘统一由 Committer 执行。
//
// 唯一的岔路是控制原语（checkpoint）：它不读写工作区，前五步对它没有意义——
// 在查表之前就被单独接走，转成交给任务状态的检查点意图。
// 引擎收到意图后何时真正提交、信息怎么合成，全是引擎的事；
// 每轮最多生效一次，多余调用返回确认而不是错误。
func (rt *Runtime) Execute(ctx context.Context, in ExecInput) Result {
	call := in.Call

	if call.Primitive == PrimCheckpoint {
		return rt.executeCheckpoint(in)
	}

	tool, ok := rt.tools[call.Primitive]
	if !ok {
		// 名字不认识：模型会编工具名，上游偶尔也会把参数塞进名字字段。
		// 挡在这里并列出可用的名字——它换个名字就能继续，而不是对着同一堵墙重试。
		return failed(call, Route{}, "unknown_tool",
			fmt.Errorf("没有名为 %q 的工具；可用的是：%s", call.Primitive, strings.Join(rt.available(), "、")))
	}

	fs, err := rt.inspect(call, in.Caps)
	if err != nil {
		return failed(call, Route{}, "inspect_error", err)
	}
	route := Dispatch(call, tool.Address, in.Caps, fs)

	var prep Prepared
	if route.Path == PathSymbol {
		if rt.ext == nil {
			return failed(call, route, "ext_unavailable", errors.New("符号路径需要扩展，但扩展未接入"))
		}
		p, err := rt.ext.Locate(ctx, call)
		if err != nil {
			return failed(call, route, "locate_error", err)
		}
		prep = p
	}

	d, err := rt.policy.Decide(ctx, call, route)
	if err != nil {
		return failed(call, route, "policy_error", err)
	}
	if d.Verdict != VerdictAllow {
		return Result{
			CallID: call.ID,
			Route:  route,
			Err:    &ToolError{Kind: "policy_denied", Message: d.Reason},
		}
	}

	plan, err := tool.Impl.Plan(ctx, PlanInput{
		Call: call, Route: route, Prepared: prep, Caps: in.Caps, Facts: in.Facts,
	})
	if err != nil {
		return failed(call, route, "plan_error", err)
	}
	// 原语可以"正常返回、但结果里带着结构化错误"——比如新建时发现目标已存在。
	// 这类不是执行失败，而是明确告诉模型该换哪个做法，所以原样回传。
	if plan.Result.Err != nil {
		plan.Result.CallID = call.ID
		plan.Result.Route = route
		return plan.Result
	}

	if len(plan.Edits) > 0 {
		ops, err := in.Facts.Commit(plan.Edits)
		if err != nil {
			var te *ToolError
			if errors.As(err, &te) {
				return Result{CallID: call.ID, Route: route, Err: te}
			}
			return failed(call, route, "commit_error", err)
		}
		for i := range ops {
			ops[i].Turn = in.Turn
			ops[i].Primitive = call.Primitive
		}
		plan.Result.Ops = ops
	}

	plan.Result.CallID = call.ID
	plan.Result.Route = route
	plan.Result.OK = true
	return plan.Result
}

// available 列出清单里的名字，供"名字不认识"的错误提示使用。
func (rt *Runtime) available() []string {
	out := make([]string, 0, len(rt.order))
	for _, name := range rt.order {
		out = append(out, string(name))
	}
	return out
}

// executeCheckpoint 处理控制原语：登记检查点意图，返回确认。
//
// 意图记在任务状态上（CheckpointRequested 置位 + summary 留档），真正是否提交、
// 何时提交、信息怎么合成由引擎的检查点环节决定——模型表达的是"这里值得"，不是"现在提交"。
// 本轮已表达过就返回确认：重复表达不是错误，是模型在长轮次里正常会做的事。
// 该原语不产生 WriteOp：它不改变工作区，检查点提交的是**之前**累积的改动。
func (rt *Runtime) executeCheckpoint(in ExecInput) Result {
	call := in.Call
	summary := call.Summary
	if already := in.Facts.CheckpointRequested(); already {
		return Result{
			CallID: call.ID,
			Route:  Route{Path: PathControl, Reason: "控制原语"},
			OK:     true,
			Summary: "本轮已请求过检查点，无需重复；系统会在合适的时机提交" +
				quoteSummary(summary),
		}
	}
	in.Facts.RequestCheckpoint(summary)
	msg := "已记录检查点意图"
	if summary != "" {
		msg += "：" + summary
	}
	msg += "；系统会在合适的时机提交"
	return Result{
		CallID:  call.ID,
		Route:   Route{Path: PathControl, Reason: "控制原语"},
		OK:      true,
		Summary: msg,
	}
}

func quoteSummary(s string) string {
	if s == "" {
		return ""
	}
	return "（" + s + "）"
}

// inspect 采集分发所需的文件事实。
//
// 拿不准的时候宁可保守：把"语法可解析"当作 false，结果是降级到文本路径——
// 这是安全的错，最多损失精度；反过来猜错就是让定位建立在错误的假设上。
func (rt *Runtime) inspect(call Call, caps ExtCaps) (FileState, error) {
	lang := langOf(call.Target)
	return FileState{
		Lang:       lang,
		Registered: caps.Registered(lang),
		ParseOK:    caps.Available,
	}, nil
}

// langOf 由文件扩展名推断语言标识。实现可替换，这里只覆盖常见几类；
// 认不出来就返回空，分发会安全地落到文本路径。
func langOf(path string) string {
	switch {
	case hasSuffix(path, ".go"):
		return "go"
	case hasSuffix(path, ".ts"), hasSuffix(path, ".tsx"):
		return "typescript"
	case hasSuffix(path, ".py"):
		return "python"
	case hasSuffix(path, ".rs"):
		return "rust"
	default:
		return ""
	}
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

func failed(call Call, route Route, kind string, err error) Result {
	return Result{
		CallID: call.ID,
		Route:  route,
		Err:    &ToolError{Kind: kind, Message: err.Error()},
	}
}
