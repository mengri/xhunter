// Package harness 实现 Xhunter 的环节执行器。
//
// 结构上分三层：
//
//	Engine    环节链的驱动者：pre（任务级一次）/ turn（每轮）/ post（任务级一次）
//	Context   一轮的上下文；任务级状态放在 Hunt 上，跨轮存活
//	Runtime   工具调用的流水线：Inspect → Dispatch → Locate → Decide → Plan → Commit
//
// 有两处可注入的插件面，性质不同：
//
//	map[StageID]HandlerFunc  环节。由构造参数传入，可以重排、增减、替换，
//	                         但必须通过 Pipeline.Validate 的位置约束。
//	[]Tool                   工具集。**整张清单由装配层给**：有哪些原语、叫什么、
//	                         什么顺序，框架一概不知道（见 Tool 与 NewRuntime）。
//
// 两层的差别来自它们面对的对象：环节面对"编排者"（流程会演进，所以要可配置），
// 原语面对"模型"（工具面是模型的接口，变了就等于换了契约）。
package harness

import (
	"context"
	"time"

	"xhunter/llm"
)

// ============================================================ 标识与终态

type (
	BountyID   string
	TurnNo     int
	ToolCallID string
)

type Status string

const (
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// ExitCode 是进程退出码。
//
// 分成四档而不是"成功/失败"两档，是为了让调用方能区分"该重试"和"不该重试"：
// 任务本身失败重跑还是失败，而环境问题修好后可以接着跑。
type ExitCode int

const (
	ExitOK        ExitCode = 0 // 成功，产物可进入评审
	ExitFailed    ExitCode = 1 // 任务失败，重跑同样会失败
	ExitEnv       ExitCode = 2 // 环境问题，修好后可重试
	ExitCancelled ExitCode = 3 // 被取消
)

// Terminal 是一次任务收敛后的终态，相当于状态机里的吸收态。
//
// 首个终态生效、后续不覆盖：否则一次任务里"先超预算、后踩策略拒绝"这类叠加情形，
// 最终退出码会取决于环节的执行顺序，而不是事实本身。
type Terminal struct {
	Status Status
	Reason string
	Code   ExitCode
}

// ============================================================ 任务输入

// RepoRef 指向远端仓库、任务分支与基线提交。
// 任务分支由引擎创建并推送——模型既看不到也改不了它，工具面上也没有任何 git 操作。
type RepoRef struct {
	Remote     string
	Branch     string
	BaseCommit string
}

type Budget struct {
	MaxTurns     int
	MaxTokens    int
	MaxWallClock time.Duration
}

// CheckpointMode 是检查点密度的取值。
//
// 默认是"结构完整点"而不是时间点：连续编辑半成品时不该产生噪声提交，
// 检查点应当对应一个自洽的状态。
type CheckpointMode string

const (
	CheckpointOnStructure CheckpointMode = "on_structure"
	CheckpointEveryTurn   CheckpointMode = "every_turn"
	CheckpointEveryWrite  CheckpointMode = "every_write"
)

type CheckpointPolicy struct {
	Mode CheckpointMode
}

// SessionRef 指向同一任务的历次执行记录；为 nil 表示新任务。
type SessionRef struct {
	ID  string
	Ref string
}

type Bounty struct {
	ID         BountyID
	Task       string
	Repo       RepoRef
	Session    *SessionRef
	Budget     Budget
	Checkpoint CheckpointPolicy
}

// ============================================================ 原语与调用

// Tool 是一个操作原语的完整描述：模型看到的名字与形状、我们执行它的实现，
// 以及分发所需的寻址性质。
//
// 它是框架与业务的分界：**框架只认识 Tool，不认识任何具体原语**——有哪些原语、
// 叫什么名字、按什么顺序暴露，全部由装配层给。框架因此可以原样复用到一个完全
// 不同的工具集上，而"这套工具集就是这 7 个"这类业务约束留在装配层。
type Tool struct {
	Name PrimitiveName
	Decl ToolDecl
	Impl Primitive
	// Address 说明这个原语怎么被寻址——分发据此决定内部路径，
	// 而不是在框架里按名字分支（那等于把业务知识写进框架）。
	Address Addressing
}

// Addressing 是原语的寻址性质：它决定分发走哪条内部路径。
type Addressing string

const (
	// AddressedBySelector：由选择器表达式决定——写字面量走文本寻址，写限定名走符号寻址。
	// 模型用**表达方式**选路径，不需要知道目标环境支持哪条。
	AddressedBySelector Addressing = "selector"
	// AddressedAsText：没有符号语义（如按文件名模式匹配、按条目名执行）。
	AddressedAsText Addressing = "text"
	// AddressedAsSymbol：只有符号路径有意义，且**没有可用的降级形态**——
	// 环境不支持时返回结构化错误，而不是悄悄退回文本路径（那会产出一个
	// 改了声明、没改调用点的半完成结果）。
	AddressedAsSymbol Addressing = "symbol"
)

// PrimitiveName 是一个操作原语的名字。
//
// 它是**标识**，不是行为——实现这一行为的东西叫 Primitive（见 primitives.go）。
// 两者分开命名，是因为它们在代码里出现的场合完全不同：名字是契约（对模型、
// 对配置、对事件流），实现是行为（对我们）。
//
// 名字由装配层决定，框架不做白名单：模型可见面是"装配层交给运行时的清单"，
// 约束它是业务的事，不是框架的事。
//
// 因此框架**只声明自己拥有的那一个名字**（控制原语），业务原语的名字由各自的实现
// 包自己声明（如 `internal/primitives/read` 的 `Name`）——框架里出现业务原语名，
// 就意味着它又在认识业务了。
type PrimitiveName string

const (
	// PrimCheckpoint 是唯一的控制原语：模型用它**表达**"这里值得留检查点"的意图。
	//
	// 它刻意不参与文件原语的分发体系（不进 Dispatch、不产生 FileEdit、不碰工作区），
	// 语义只有一件事：把意图转交给引擎。引擎收到后做什么（立即提交还是等结构完整点、
	// 提交信息怎么写、推送不推送）全部由引擎决定——提交信息由引擎合成、分支与推送目标
	// 从未进入模型可见面，每轮最多生效一次，多余调用得到"本轮已处理"的确认。
	//
	// 它与业务原语的区别在于**归属**：控制原语是循环自身的语义（表达意图、由引擎裁决），
	// 因此由框架自带；面向文件的那批是业务工具集，由装配层提供。
	PrimCheckpoint PrimitiveName = "checkpoint"
)

// PathKind 是分发结果：同一个原语内部的执行路径。
// 控制原语不读写文件，走的是第三种"路径"——它不经分发体系，见 PrimCheckpoint。
type PathKind string

const (
	PathText   PathKind = "text"
	PathSymbol PathKind = "symbol"
	PathControl PathKind = "control"
)

type LineRange struct{ From, To int }

// Selector 是原语的定位参数。
//
// 模型用**表达方式**选择内部路径：写字面量就是内容寻址，写限定名就是符号寻址。
// 它不需要知道（也无从知道）目标环境支持哪条路径。
type Selector struct {
	Literal  string     // 文本寻址：字面量片段
	Symbol   string     // 符号寻址：限定名
	InSymbol string     // 符号内相对定位
	Range    *LineRange // 行范围（读取）
	FileView bool       // 读取符号视图（大纲）
	Scope    string     // 限定范围：文件 / 目录 / 包
}

// Call 是模型发出的一次原语调用。
//
// Target 是工作区内的**相对路径**。模型这一侧不存在绝对路径、字节区间、
// 分支名、凭据这些"资源层"概念——所有翻译都在引擎内部完成。
//
// 三个具名参数各有自己的槽位，因为它们回答的是不同的问题：NewName 是重命名的
// **目标名**（与 Target/Selector 指出的"改哪里"是两件事）、Gate 是要运行的
// **具名条目**（来自工具面的白名单词汇，不是文件内容）、Summary 是检查点的
// **意图说明**（自由文本，引擎合成提交信息时会用到）。
//
// 它们此前借用过 selector / content 的槽位，那是"参数形状未定"时的权宜。
// 借用会让模型可见的参数名跟着内部字段走，所以形状一定格就各自归位。
type Call struct {
	ID        ToolCallID
	Primitive PrimitiveName
	Target    string
	Selector  Selector
	Content   string
	NewName   string
	Gate      string
	Summary   string
}

// FileState 是分发所需的文件事实。
type FileState struct {
	Lang       string
	Registered bool // 该语言是否在扩展的注册表中
	ParseOK    bool // 语法是否可解析
}

// Route 是分发结果。Reason 说明为何走这条路、或为何降级，随结果一并上报，
// 使降级对模型与使用者同时可见。
type Route struct {
	Path   PathKind
	Reason string
}

type ByteRange struct{ Start, End int }

// FileEdit 是唯一的写入原语输入：目标文件 + 字节区间 + 新内容。
//
// 形状刻意单一——只有"替换某个区间"这一种表达。若允许多种写入形态
// （整文件覆盖、追加、删除……），写入的语义就会分散到各个原语实现里去。
type FileEdit struct {
	File       string
	ByteRange  ByteRange
	NewContent string
}

// WriteOp 是一次写操作记录：交付物、检查点与恢复都建立在它之上。
type WriteOp struct {
	Primitive PrimitiveName
	File      string
	ByteRange ByteRange
	Before    string
	After     string
	Turn      TurnNo
}

// ToolError 是结构化错误，形状来自模型侧的中立契约——工具失败与流中断用的是同一套
// 三要素，分开定义就会漂移。别名而非新类型：两边是同一个东西，不存在两个真相。
type ToolError = llm.Fault

type Result struct {
	CallID  ToolCallID
	OK      bool
	Route   Route
	Summary string
	Ops     []WriteOp
	Err     *ToolError
}

// Usage 从模型侧契约借来：token 用量由协议实现上报，轮数与耗时由引擎统计。
type Usage = llm.Usage

// Outcome 是对外的结果文件内容。
//
// 与 Terminal 的分工：Terminal 只回答"结论是什么"，Outcome 是"结论 + 交付物"。
// 判死可能发生在很早就（比如初始化失败），而交付物要到收尾阶段才齐备。
type Outcome struct {
	Status       Status
	Reason       string
	ExitCode     ExitCode
	Branch       string
	CommitSHA    string
	FilesChanged []string
	Usage        Usage
}

// Gate 是一个具名校验条目。
//
// 模型只知道这个名字，命令、参数、判据都在引擎侧——所以模型无法通过改命令
// 来让自己更容易通过，也不可能借它把 shell 请回来。
type Gate struct {
	Name     string
	Argv     []string
	Required bool
}

// Phase 是心跳携带的阶段。取值只增不改：使用方按已知取值处理，未知取值忽略即可。
type Phase string

const (
	PhaseBootstrap Phase = "bootstrap"
	PhaseAssemble  Phase = "assemble"
	PhaseInfer     Phase = "infer"
	PhaseTools     Phase = "tools"
	PhaseFinalize  Phase = "finalize"
)

// ============================================================ 首轮消息构造（扩展点）

// PromptInput 是构造首轮消息时能看到的任务事实。
//
// 只给事实、不给权限：提供方能读到工作区、能知道任务与工具面是什么，但拿不到
// 写入口、拿不到策略裁决、也拿不到凭据——它交付的是文本，不是行为。Workspace
// 上的路径一律是工作区相对路径，因此"读工作区之外的文件"没有表达方式。
type PromptInput struct {
	Bounty    Bounty     // 任务描述、仓库、分支、基线、会话标识
	Workspace Workspace  // 只读工作区视图，读的是基线内容
	Tools     []ToolDecl // 已定格的工具面，供提供方按需陈述
}

// PromptPart 是首轮一段正文：由单个插件产出，是拼接前的最小单位。
type PromptPart struct {
	Body string
	// Sources 是这段正文用到的素材来源（基线提交、扩展标识…）。
	// 它进生效配置快照供审计：事后要能回答"这次用的是哪份约定、哪个版本的提示词"，
	// 而不必回溯当时的工作区状态。
	Sources []string
	// Notices 是构造过程中的降级记录。它与正文分开，因为两者的去向不同：
	// 正文进上下文，降级进事件流。跳过一份格式非法的素材必须留下痕迹——
	// 静默跳过会让产出与仓库约定不符，而这类偏差要到评审时才看得出来。
	Notices []Notice
}

// Notice 是一次降级记录：发生了什么、在哪个对象上、为什么。
//
// 结构化而不是一句日志文本：消费方要能按范围与对象筛选（"这次跑跳过了哪些技能"），
// 也要能进事件流成为机器可读的契约。
type Notice struct {
	Scope   string // 降级发生的范围，如 "prompt.skills"
	Subject string // 具体对象：被跳过的文件、被截断的清单……
	Reason  string // 为什么
}

// PromptPlugin 构造首轮的一段正文——**这是扩展点**。
//
// 首轮固定为 system 与 user 两段，每段可以由**多个**插件共同拼成，按装配期排定的
// 顺序拼接；同一个插件也可以同时挂在两段上（比如"项目约定"进 system、"任务背景"
// 进 user）。于是"系统提示词写什么、项目约定怎么发现、技能清单从哪来、任务怎么向
// 模型陈述"全部由插件决定，内核不再知道约定文件在哪、技能目录叫什么。
//
// 插件只产出正文，不产出消息序列：它插不进第三段、调换不了两段的顺序，也无从越过
// 内核在 system 段末尾追加的条款。内核保留四条职责，因为每一条都不能交出去：
//
//  1. **两段的位置与顺序由内核拼**：插件给的是正文，不是消息序列——顺序即装配顺序，
//     由装配层排定，因此"哪段在前"是装配期的事实，不是运行期的偶然；
//  2. **只取一次并冻结**：初始化阶段各调用一次，整任务内不再刷新。运行中重取会同时
//     毁掉两件事——模型可以借修改约定文件来改自己的指令，prompt 前缀也会失去缓存
//     价值。代价是运行中的改动只进交付物、不影响本次运行（书写与生效分离）；
//  3. **内核条款在 system 段之后追加**：工具纪律、安全边界、无人类条款、止损规则由
//     内核自己拼在末尾，插件删不掉也改不动。插件处于不可信边界，它给的是内容，
//     不是权限；环境事实同理由内核注入 user 段——那是内核知道而插件无从知道的事实，
//     由插件陈述就会在换机器、换平台之后继续断言旧事实，而模型察觉不到；
//  4. **不得越界**：正文改变不了工具面、策略裁决、路径边界与凭据边界；
//     这些由内核与策略持有。
//
// 两条附带义务由插件自己履行（内核不代劳，因为要履行它们就得知道素材格式）：
//   - **容错**：单条素材非法（比如一份写坏的技能说明）应当跳过而非整体失败，
//     否则一份坏文件就能让整个任务起不来；
//   - **可观测降级**：跳过或截断了什么，经装配期注入的事件出口上报。静默跳过会让产出
//     与仓库约定不符，而这类偏差要到评审时才看得出来。
//
// 真构造不出来（基线读不了、扩展不可用）才返回错误，此时任务按环境错误收敛：
// 少了系统提示词与项目约定，任务继续跑没有意义，只会生产一堆看起来正常的东西。
type PromptPlugin interface {
	Build(ctx context.Context, in PromptInput) (PromptPart, error)
}

// PromptParts 是首轮两段正文的拼接结果，与协议的首轮消息一一对应。
type PromptParts struct {
	System PromptPart
	User   PromptPart
}

// ============================================================ 协作者接口

// Message 是上下文里的一条消息。Role 取 Role* 常量。
//
// 角色是中立词汇而不是原样透传的字符串：适配器要按角色决定翻译成上游的哪种消息，
// 而一个拼错的角色名在透传时会变成对端的静默行为差异（例如被当成 user 处理），
// 这类错误在无人值守场景下几乎不可察觉。
type Message struct {
	Role    Role
	Content string
	Calls   []Call
	Results []Result
}

// 消息角色与事件族的取值都来自模型侧契约：两端用的是同一套词汇，
// 各自定义一份就会出现"这边发 text、那边等 delta"这类只在运行时才暴露的错位。
const (
	RoleSystem    = llm.RoleSystem
	RoleUser      = llm.RoleUser
	RoleAssistant = llm.RoleAssistant
	RoleTool      = llm.RoleTool
)

type Role = llm.Role

type EventKind = llm.EventKind

const (
	EvText    = llm.EvText
	EvToolUse = llm.EvToolUse
	EvUsage   = llm.EvUsage
	EvError   = llm.EvError
	EvEnd     = llm.EvEnd
)

// Event 直接用模型侧契约的类型：它描述的是一次推理里发生了什么，与编排无关。
type Event = llm.Event

// Caps 同样来自模型侧契约：能力声明是"这个模型能做什么"，不是编排的选择。
type Caps = llm.Caps

// ToolDecl 是工具对模型可见的声明，协议中立。
//
// 声明与原语实现是两件事：原语决定"能做哪些事"，声明决定"模型看到的形状"。
// 它必须只有一份——上下文里的工具说明层与发给供应商的注册面读同一份，
// 各自推导就会出现"说明了一层、实际注册了另一层"的漂移。
type ToolDecl = llm.ToolDecl

// Session 与 Provider 是模型侧契约，别名引用使引擎侧代码不必到处写 llm. 前缀。
type Session = llm.Session

type Provider = llm.Provider

type ExternalEvent struct {
	Type    string
	Payload map[string]any
}

// EventSink 是外部事件与日志的出口。两条通道必须分开：
// 事件流是给程序消费的结构化契约，日志是给人看的，混在一起两边都用不好。
type EventSink interface {
	Emit(ev ExternalEvent) error
	Log(level, msg string, kv ...any)
	Heartbeat(p Phase) error
}

type Verdict string

const (
	VerdictAllow Verdict = "allow"
	VerdictDeny  Verdict = "deny"
)

// Decision 必须携带原因：拒绝时原因会回灌给模型，
// 让它换个做法，而不是对着同一堵墙反复尝试。
type Decision struct {
	Verdict Verdict
	Reason  string
}

// Policy 是无人类场景下唯一顶替人的位置：路径边界、破坏性操作、预算与止损
// 都由它裁决。默认应当是拒绝——放行需要一条明确的理由，而不是反过来。
type Policy interface {
	Decide(ctx context.Context, call Call, route Route) (Decision, error)
	Charge(u Usage)
	Exhausted(turn TurnNo) (bool, string)
}

type Commit struct {
	SHA    string
	Branch string
}

// GitWorktree 负责基线获取、任务分支与提交。
//
// PrepareBaseline 返回**工作区根路径**——这是它唯一一次向外交出这个信息，
// 拿到它的人是引擎内部，不是模型侧的任何组件。
type GitWorktree interface {
	PrepareBaseline(ctx context.Context, repo RepoRef) (workRoot string, err error)
	Commit(ctx context.Context, repo RepoRef, msg string) (Commit, error)
	Diff(ctx context.Context, baseCommit string) ([]string, error)
	Clean(ctx context.Context) error
}

// TurnRecord 是一轮的完整记录：模型说了什么、要求了哪些调用、结果如何。
//
// 三者必须一起记：只记结果的话，重建上下文时"模型的助手消息"就没了着落，
// 而工具结果在上游协议里是**依附于某次调用**的——调用丢失会让结果无法配对。
type TurnRecord struct {
	Turn    TurnNo
	Text    string
	Calls   []Call
	Results []Result
}

type ContextBuilder interface {
	Assemble(ctx context.Context, h *Hunt, turn TurnNo) ([]Message, error)
	Append(rec TurnRecord)
	Restore(ctx context.Context, msgs []Message) error
	TokenCount() int
}

type SessionRecorder interface {
	RecordTurn(rec TurnRecord)
	RecordOp(op WriteOp)
	Ops() []WriteOp
	Snapshot() error
}

type Precision string

const (
	PrecisionSyntactic Precision = "syntactic"
	PrecisionSemantic  Precision = "semantic"
)

// ExtCaps 是扩展上报的能力描述符。核心只消费它，不感知后端是哪种解析器。
type ExtCaps struct {
	Available  bool
	Languages  []string
	CanResolve bool
	Precision  Precision
}

func (c ExtCaps) Registered(lang string) bool {
	if !c.Available || lang == "" {
		return false
	}
	for _, l := range c.Languages {
		if l == lang {
			return true
		}
	}
	return false
}

// Impact 是改动影响面。
//
// 它必须在裁决之前算出来，否则"这次 rename 会动 200 个文件"这类事实
// 在策略面前是盲的——策略只能看到"要改一个文件"。
type Impact struct {
	FilesChanged int
	Occurrences  int
	Unknown      bool // 语法级后端无法穷尽引用时置位
}

// Prepared 是符号路径的只读定位结果：由扩展算出，核心据此落盘。
type Prepared struct {
	File      string
	ByteRange ByteRange
	Impact    Impact
	Precision Precision
}

// ExtHost 刻意没有任何写方法——扩展因此无法直接写工作区，
// 它只能返回"改哪里、改成什么"，由核心执行落盘。
type ExtHost interface {
	Capabilities(ctx context.Context) ExtCaps
	Locate(ctx context.Context, call Call) (Prepared, error)
	Close() error
}
