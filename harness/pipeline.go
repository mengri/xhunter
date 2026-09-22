package harness

import "fmt"

// 本文件实现流程组装：控制流程由一份环节清单拼装而成，而不是写死在控制流里。
//
// 这里有个看似矛盾的地方：流程可以重排、增减，但不变量又必须成立。解法是把不变量
// 从"代码里的固定顺序"改成"配置的合法性规则"——Validate 在启动期拒绝非法编排，
// 于是不合法的流程根本跑不起来。可组装不必以放弃安全性为代价。

// StageID 是一个具名环节。环节是流程的最小可组装单位。
type StageID string

const (
	// pre：初始化，顺序执行一次。
	StagePrepareBaseline StageID = "git.prepare_baseline"
	StageGatesLoad       StageID = "gates.load"
	StageExtCaps         StageID = "ext.caps"
	StagePromptBuild     StageID = "prompt.build"
	StageSessionRestore  StageID = "session.restore"
	StageConfigSnapshot  StageID = "config.snapshot"

	// turn.guards：每轮开始前的守卫，命中即收敛到终态。
	StageGuardCancel  StageID = "guard.cancel"
	StageGuardChannel StageID = "guard.channel"
	StageGuardBudget  StageID = "guard.budget"

	// turn.steps：每轮顺序执行。
	StageContextCompact   StageID = "context.compact"
	StageContextAssemble  StageID = "context.assemble"
	StageProviderInfer    StageID = "provider.infer"
	StageStreamReceive    StageID = "stream.receive"
	StageToolsExecute     StageID = "tools.execute"
	StageCheckpointCommit StageID = "checkpoint.commit"

	// post：收尾，顺序执行一次。
	StageSessionSnapshot  StageID = "session.snapshot"
	StageGatesRequired    StageID = "gates.required"
	StageDeliveryCommit   StageID = "delivery.commit"
	StageDeliverableDiff  StageID = "deliverable.diff"
	StageWorkspaceCleanup StageID = "workspace.cleanup"
	StageExtClose         StageID = "ext.close"
	StageEventHuntEnd     StageID = "event.hunt_end"
)

// HandlerFunc 是一个环节的执行体。
//
// 刻意不返回 error：失败在这里不是异常，而是一种终态。用 Terminate 显式收敛，
// 比把错误层层上抛更贴近事实——很多"失败"（预算耗尽、被取消）根本不是错误，
// 而是流程正常走到了某个终点。这也让 Run 的契约保持干净：结果一定有效。
type HandlerFunc func(*Context)

// Stage 是链上的一节：具名环节加执行体。带 ID 是为了心跳与日志能标明当前进度。
type Stage struct {
	ID StageID
	Do HandlerFunc
}

// build 把环节清单编译成可执行链。未注册的环节在启动期就失败——
// 这是"非法编排不得运行"的另一半：一半靠 Validate 检查位置，一半靠这里检查实现是否齐备。
func build(ids []StageID, reg map[StageID]HandlerFunc) ([]Stage, error) {
	chain := make([]Stage, 0, len(ids))
	for _, id := range ids {
		do, ok := reg[id]
		if !ok {
			return nil, fmt.Errorf("流程组装非法：环节 %q 未注册", id)
		}
		chain = append(chain, Stage{ID: id, Do: do})
	}
	return chain, nil
}

type section string

const (
	secPre   section = "pre"
	secGuard section = "guard"
	secStep  section = "step"
	secPost  section = "post"
)

// knownStages 是全部合法环节，以及各自允许出现的区段。
// 区段限制的意义在于：守卫放在步骤之后，就失去了"守卫"的意义。
var knownStages = map[StageID]section{
	StagePrepareBaseline: secPre, StageGatesLoad: secPre, StageExtCaps: secPre,
	StagePromptBuild: secPre, StageSessionRestore: secPre, StageConfigSnapshot: secPre,

	StageGuardCancel: secGuard, StageGuardChannel: secGuard, StageGuardBudget: secGuard,

	StageContextCompact: secStep, StageContextAssemble: secStep, StageProviderInfer: secStep,
	StageStreamReceive: secStep, StageToolsExecute: secStep, StageCheckpointCommit: secStep,

	StageSessionSnapshot: secPost, StageGatesRequired: secPost, StageDeliveryCommit: secPost,
	StageDeliverableDiff: secPost, StageWorkspaceCleanup: secPost, StageExtClose: secPost,
	StageEventHuntEnd: secPost,
}

type TurnSpec struct {
	Guards []StageID
	Steps  []StageID
}

// Pipeline 是一次任务的完整流程编排。零值经 Normalize 后即为默认编排，
// 于是"引入组装能力"本身不改变任何既有行为。
type Pipeline struct {
	Pre  []StageID
	Turn TurnSpec
	Post []StageID
}

func DefaultPipeline() Pipeline {
	return Pipeline{
		Pre: []StageID{
			StagePrepareBaseline, StageGatesLoad, StageExtCaps,
			StagePromptBuild, StageSessionRestore, StageConfigSnapshot,
		},
		Turn: TurnSpec{
			Guards: []StageID{StageGuardCancel, StageGuardChannel, StageGuardBudget},
			Steps: []StageID{
				StageContextCompact, StageContextAssemble, StageProviderInfer,
				StageStreamReceive, StageToolsExecute, StageCheckpointCommit,
			},
		},
		Post: []StageID{
			StageSessionSnapshot, StageGatesRequired, StageDeliveryCommit,
			StageDeliverableDiff, StageWorkspaceCleanup, StageExtClose,
			StageEventHuntEnd,
		},
	}
}

func (p Pipeline) Normalize() Pipeline {
	if len(p.Pre) == 0 && len(p.Turn.Steps) == 0 && len(p.Post) == 0 {
		return DefaultPipeline()
	}
	return p
}

// Validate 校验组装结果。每条规则都对应一个真实的失效后果，而不是抽象的"规范要求"。
//
// 规则分两类，按后果区分：
//   - **约束存在**：少了它任务就不该开始（基线、守卫、写盘入口、首轮消息构造、交付提交、
//     终态上报）——缺了这些，失败要么不可见、要么不可交付；
//   - **约束位置**：环节本身可以不要，配了就得在正确的位置（接收先于执行、检查点在执行
//     之后、终态上报在最后）。选择权留在配置手里，安全边界留在校验手里。
func (p Pipeline) Validate() error {
	pl := p.Normalize()

	// R0：环节必须已定义，且出现在允许的区段。
	for _, g := range []struct {
		sec section
		ids []StageID
	}{
		{secPre, pl.Pre}, {secGuard, pl.Turn.Guards}, {secStep, pl.Turn.Steps}, {secPost, pl.Post},
	} {
		for _, id := range g.ids {
			want, ok := knownStages[id]
			if !ok {
				return fmt.Errorf("流程组装非法：未知环节 %q", id)
			}
			if want != g.sec {
				return fmt.Errorf("流程组装非法：环节 %q 属于 %s 区段，不能放在 %s", id, want, g.sec)
			}
		}
	}

	// R1：同一区段内不得重复。重复执行会破坏"唯一入口"与幂等假设——
	// 比如两次 tools.execute 意味着两个写盘窗口。
	for _, g := range []struct {
		name string
		ids  []StageID
	}{
		{"pre", pl.Pre}, {"guard", pl.Turn.Guards}, {"step", pl.Turn.Steps}, {"post", pl.Post},
	} {
		seen := map[StageID]bool{}
		for _, id := range g.ids {
			if seen[id] {
				return fmt.Errorf("流程组装非法：%s 区段重复环节 %q", g.name, id)
			}
			seen[id] = true
		}
	}

	// R2：基线准备必须存在且位于 pre 最前。
	// 它之后的所有初始化动作都可能写工作区，基线没就绪就写，等于写在错误的树上；
	// 而且任务分支的创建与推送也在这里完成，晚一点就可能有人以为可以跳过。
	if err := requirePresent("pre", pl.Pre, StagePrepareBaseline, "基线未就绪就执行其他初始化动作，会写进错误的树"); err != nil {
		return err
	}
	if pl.Pre[0] != StagePrepareBaseline {
		return fmt.Errorf("流程组装非法：%s 必须是 pre 的第一个环节", StagePrepareBaseline)
	}

	// R2b：首轮消息的构造必须存在。
	//
	// 缺了它，模型在没有任何系统提示词与项目约定的情况下开跑——工具面与策略都还在，
	// 唯独"它是谁、这个项目有什么约定"没有了。这类偏差不会让任何一步报错，只会让产出
	// 慢慢偏出仓库约定，到评审时才看得出来。
	//
	// 位置只约束一条：ext.caps 在场时，构造必须排在它之后——否则正文陈述的工具面与
	// 定格的工具面不一致，正是定格要消除的那种漂移。"晚于基线"不必另立规则：
	// R2 已经要求基线占据 pre 首位，而同一区段内不允许重复，构造自然只能排在它后面。
	if err := requirePresent("pre", pl.Pre, StagePromptBuild, "首轮无人构造，模型将看不到任何系统提示词与项目约定"); err != nil {
		return err
	}
	if eci := indexOf(pl.Pre, StageExtCaps); eci >= 0 && indexOf(pl.Pre, StagePromptBuild) < eci {
		return fmt.Errorf("流程组装非法：%s 必须在 %s 之后，否则正文陈述的工具面与定格的工具面会不一致", StagePromptBuild, StageExtCaps)
	}

	// R3：取消与预算守卫必须存在。它们是两条"不可能被静默跳过"的终止路径——
	// 少了取消，进程收到信号也没人管；少了预算，任务可以无限烧下去。
	if err := requirePresent("guard", pl.Turn.Guards, StageGuardCancel, "取消信号无法收敛，流程可能静默挂起"); err != nil {
		return err
	}
	if err := requirePresent("guard", pl.Turn.Guards, StageGuardBudget, "预算上限无法生效"); err != nil {
		return err
	}

	// R4：写盘唯一入口必须存在且唯一。
	if err := requirePresent("step", pl.Turn.Steps, StageToolsExecute, "它是唯一写盘入口，不可省略"); err != nil {
		return err
	}

	// R5：必须有推理环节，否则不可能产生任何工具调用。
	if indexOf(pl.Turn.Steps, StageProviderInfer) < 0 {
		return fmt.Errorf("流程组装非法：step 区段缺少 %s", StageProviderInfer)
	}

	// R6：接收必须先于执行。
	// 工具在流结束后统一执行，接收段只做入队、不带副作用——这样流内挂起或取消时，
	// 不会留下写了一半的工作区。
	ri, ei := indexOf(pl.Turn.Steps, StageStreamReceive), indexOf(pl.Turn.Steps, StageToolsExecute)
	if ri >= 0 && ri > ei {
		return fmt.Errorf("流程组装非法：%s 必须在 %s 之前，接收段不得有副作用", StageStreamReceive, StageToolsExecute)
	}

	// R6b：检查点提交的是**本轮**已应用的改动，因此必须排在执行之后。
	// 反过来放就变成提交上一轮的内容，与"钉住一个自洽状态"的语义相反。
	ci := indexOf(pl.Turn.Steps, StageCheckpointCommit)
	if ci >= 0 && ci < ei {
		return fmt.Errorf("流程组装非法：%s 必须在 %s 之后，它提交的是本轮已应用的改动", StageCheckpointCommit, StageToolsExecute)
	}

	// R7：收尾必须提交，且必须上报终态。交付物就是任务分支的 tip，
	// 没有提交这一步，前面所有工作都没有交付物；没有终态上报，调用方无从判断结果。
	if err := requirePresent("post", pl.Post, StageDeliveryCommit, "交付物不会产生"); err != nil {
		return err
	}
	if err := requirePresent("post", pl.Post, StageEventHuntEnd, "终态未上报，调用方无从判断结果"); err != nil {
		return err
	}

	// R8：终态上报必须是收尾的最后一个环节——上报之后不应该再有动作，
	// 否则"已结束"这个信号发出去之后事情还在变。
	if last := pl.Post[len(pl.Post)-1]; last != StageEventHuntEnd {
		return fmt.Errorf("流程组装非法：%s 必须是 post 的最后一个环节，实际是 %s", StageEventHuntEnd, last)
	}

	return nil
}

// String 便于日志与"本次生效的编排"快照输出。
func (p Pipeline) String() string {
	pl := p.Normalize()
	return fmt.Sprintf("pre[%v] turn{guards[%v] steps[%v]} post[%v]",
		pl.Pre, pl.Turn.Guards, pl.Turn.Steps, pl.Post)
}

func requirePresent(sec string, ids []StageID, want StageID, why string) error {
	if indexOf(ids, want) < 0 {
		return fmt.Errorf("流程组装非法：%s 区段缺少 %s（%s）", sec, want, why)
	}
	return nil
}

func indexOf(ids []StageID, want StageID) int {
	for i, id := range ids {
		if id == want {
			return i
		}
	}
	return -1
}
