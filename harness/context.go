package harness

import (
	"context"
	"math"
	"time"
)

// abortIndex 是恒大于任何链长的哨兵值。把推进位置设成它，循环条件自然不成立，
// 链就停在原地——用数值表达"中止"，不需要额外的标志位。
const abortIndex = math.MaxInt8 >> 1

// Hunt 是任务级状态，跨轮存活。
//
// 与 Context 的分工很简单：Hunt 装"整个任务都知道的事"，Context 装"这一轮的事"。
// 这条分工的收益是：轮级边界由构造保证——新的一轮拿到的是全新的 Context，
// 不可能带上上一轮的待执行调用（那会导致同一批调用被执行两次、写盘两次）。
type Hunt struct {
	Bounty   Bounty
	Usage    Usage
	ExtCaps  ExtCaps
	WriteErr error

	// ToolDecls 是模型可见的工具声明，在 ext.caps 环节定格一次。
	// 定格得早是必要的：构造请求的说明层与发起请求的注册面必须是同一份，
	// 各自推导就会出现"说明了工具 A、实际注册了工具 B"的漂移。
	ToolDecls []ToolDecl

	// Prompt 是首轮两段正文的拼接结果，在 prompt.build 环节构造一次后冻结。
	// 冻结不是优化：允许中途重取，模型就能借修改约定文件来改自己的指令，
	// prompt 前缀也会失去缓存价值。它存在任务级状态上，因为收尾与恢复都要读得到。
	Prompt PromptParts

	FailStreak int // 连续失败轮数，用于止损

	// Terminal 是任务级结论。放在 Hunt 而不是 Context，因为终态要跨轮，
	// 并且要贯穿到收尾链——收尾阶段并不认识"是哪一轮判的死"。
	Terminal *Terminal

	// gates 与 commit 不导出：对外读取统一走 TaskFacts 的方法，
	// 状态也不会被包外直接改写。
	gates   []Gate
	commit  *Commit
	ledger  *Ledger
	storage Storage
	started time.Time

	// 检查点意图（控制原语写入，检查点环节消费）：本轮是否已表达 + 模型给出的说明。
	// 轮边界（engine.settle 之后的下一轮 round）会把 requested 复位；summary 保留最近
	// 一次的值，供提交信息合成参考。
	checkpointRequested bool
	checkpointSummary   string
}

// TaskFacts 是原语实现能看到的任务级事实，刻意收窄到几件事：
// 只读的文件视图、门禁清单、读台账、一个形状唯一的写入口、
// 以及检查点意图的两个动作（表达与查询）。
//
// 特别地，它**不包含工作区根路径**——绝对路径不出现在任何接口签名上，
// 于是"写工作区之外的文件"不是被拦下来的，而是没有表达方式。
type TaskFacts interface {
	Workspace() Workspace
	Gates() []Gate
	Ledger() *Ledger
	Commit(edits []FileEdit) ([]WriteOp, error)
	// RequestCheckpoint 记录控制原语表达的检查点意图（含模型给出的说明）。
	RequestCheckpoint(summary string)
	// CheckpointRequested 报告本轮是否已表达过检查点意图。
	CheckpointRequested() bool
}

func (h *Hunt) Workspace() Workspace { return h.storage }
func (h *Hunt) Gates() []Gate        { return h.gates }
func (h *Hunt) Ledger() *Ledger      { return h.ledger }

// RequestCheckpoint 记录检查点意图。
//
// 状态放 Hunt（任务级）而不是 Context（轮级）：检查点环节在轮内跑，
// 读取的却是"本轮是否已表达"——这个旗标由轮边界重置，但持有者必须是
// 生命周期更长的对象，否则轮级状态没法跨过环节链传递。
// summary 原样留档：引擎合成提交信息时会把它作为素材，评审的人也能看到
// 模型认为"这里值得"的理由。
func (h *Hunt) RequestCheckpoint(summary string) {
	h.checkpointRequested = true
	h.checkpointSummary = summary
}

func (h *Hunt) CheckpointRequested() bool { return h.checkpointRequested }

// consumeCheckpoint 取走本轮的检查点意图，读过即清。
//
// "一次意图只兑现一次"由这里保证：提交环节调用它之后，无论最终是否真的提交，
// 后续环节都不会再看到这次请求。理由（summary）一并清掉而不是留给下一轮——
// 提交是累积的，若这一轮没提交成，下一轮该带的是**下一轮**的理由，
// 把旧理由挂在一次很久之后的提交上会让 git 历史说谎。
func (h *Hunt) consumeCheckpoint() (intent string, requested bool) {
	intent, requested = h.checkpointSummary, h.checkpointRequested
	h.checkpointRequested, h.checkpointSummary = false, ""
	return intent, requested
}

func (h *Hunt) Commit(edits []FileEdit) ([]WriteOp, error) {
	return NewCommitter(h.storage, h.ledger).Commit(edits)
}

// Context 是一轮的上下文，每轮新建。
type Context struct {
	Ctx  context.Context
	Hunt *Hunt
	Deps Deps
	Cfg  Config

	TurnNo   TurnNo
	Messages []Message
	Stream   Session
	// AssistantText 是本轮模型输出的正文，由流式增量合并而成。
	// 合并而不是逐块上报，是因为一次回答对调用方是一条记录；
	// 而合并本身要在这里做——适配器只负责把增量原样交出来。
	AssistantText string
	Pending       []Call   // 本轮收到但尚未执行的调用
	Results       []Result // 本轮的执行结果
	Failed        bool

	handlers []Stage
	index    int
	Stage    StageID // 当前环节，供心跳与日志标注
}

// Terminate 设置终态并停止当前链——所有失败与取消都汇到这里，是唯一的收敛入口。
//
// 两个后果值得说明：
//   - 链停下来之后，后续环节根本不会进入，所以每个环节入口都去复查"上一段是否已判死"
//     是多余的；
//   - 收尾链是另一条链，重置推进位置后照常执行，因此"失败也照常交付"是结构保证的，
//     不依赖每个 return 处小心处理。
func (c *Context) Terminate(status Status, reason string, code ExitCode) {
	if c.Hunt.Terminal == nil {
		c.Hunt.Terminal = &Terminal{Status: status, Reason: reason, Code: code}
	}
	c.Abort()
}

// Abort 只停止当前链，不设置终态。
func (c *Context) Abort() { c.index = abortIndex }

func (c *Context) Aborted() bool { return c.index >= abortIndex }

// next 推进链，刻意不导出。
//
// 环节一旦能在内部调用它（先把剩余链跑完、再回来执行后置逻辑），实际执行序就不再
// 等于清单里的位置，三件事会同时失效：位置校验的意义、环节之间的字段读写约定、
// 以及"中止之后不再有动作"这条保证。
//
// 需要包裹整条链的需求（计时、日志、panic 收敛）放在引擎的驱动层，
// 那里没有位置约束要保护。
func (c *Context) next() {
	c.index++
	for ; c.index < len(c.handlers); c.index++ {
		st := c.handlers[c.index]
		c.Stage = st.ID
		st.Do(c)
	}
}

// emit 上报外部事件。写失败只记录第一个。
//
// 只留第一个是因为后续失败多半源于同一原因（对端已关闭），首个错误信息量最大；
// 而事件通道的断裂要由轮边界统一收敛，不适合在每次写入时各自处理。
func (c *Context) emit(ev ExternalEvent) {
	if err := c.Deps.Sink.Emit(ev); err != nil && c.Hunt.WriteErr == nil {
		c.Hunt.WriteErr = err
	}
}

func (c *Context) beat(p Phase) {
	if err := c.Deps.Sink.Heartbeat(p); err != nil && c.Hunt.WriteErr == nil {
		c.Hunt.WriteErr = err
	}
}

// outcome 组装对外结果：终态定结论，其余由任务级状态补齐。
func (c *Context) outcome() Outcome {
	t := c.Hunt.Terminal
	if t == nil {
		// 走到这里说明收尾链没能给出结论——这是编排缺陷，不是任务失败，
		// 因此归到环境问题，重跑有机会修好。
		t = &Terminal{StatusFailed, "no_terminal", ExitEnv}
	}
	return Outcome{
		Status:    t.Status,
		Reason:    t.Reason,
		ExitCode:  t.Code,
		Branch:    c.Hunt.Bounty.Repo.Branch,
		CommitSHA: commitSHA(c.Hunt.commit),
		Usage:     c.Hunt.Usage,
	}
}

func commitSHA(c *Commit) string {
	if c == nil {
		return ""
	}
	return c.SHA
}

// Ledger 记录"模型看过哪些文件、当时的内容指纹是什么"。
//
// 它同时解决两个问题：改一个没读过的文件应当被拒绝（避免凭想象编辑）；
// 读完之后文件又被改动过，落笔也应当被拒绝（避免基于过期内容覆盖别人的改动）。
//
// 它是任务级的，并且随会话材料一起持久化——恢复之后工作区内容与记录一致，
// 因此不必让模型把文件全部重读一遍。
type Ledger struct {
	entries map[string]string
}

func NewLedger() *Ledger { return &Ledger{entries: map[string]string{}} }

// Mark 登记一次读取，或一次写入后的新指纹。
func (l *Ledger) Mark(file, fingerprint string) { l.entries[file] = fingerprint }

func (l *Ledger) Fingerprint(file string) (string, bool) {
	fp, ok := l.entries[file]
	return fp, ok
}

var _ TaskFacts = (*Hunt)(nil)
