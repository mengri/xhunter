package hunt

import "encoding/json"

// 生效配置快照：本次 Hunt 实际用了哪套规则。
//
// 它是**只读事实**——在 Prepare 的装配完成后冻结一次，此后起飞事件与结果文件读的是同一份，
// 不各算一遍（两份迟早会漂）。它不由模型影响：模型看不到它，也没有能改它的通道。它同时服务
// 两件事：远程诊断（"这次跑在哪套装配上"）与 MR 评审（"这次验收用的是什么规则"）。

// AssemblyFacts 是「本次实际生效的规则」里只有装配层知道的那部分：策略配置、检查点行为、
// 扩展能力指纹与目标平台。这些事实在装配时就定死、不在运行期产生，因此由装配层**值注入**，
// Session 不去猜——猜出来的快照只会与真实装配漂开。
type AssemblyFacts struct {
	// Policy 是策略配置（默认拒绝、路径边界的口径）。具体的键由策略实现自述。
	Policy map[string]any
	// Checkpoint 是检查点行为口径（当前只有一档：只在结构完整点上自动产生）。
	Checkpoint string
	// Ext 是扩展能力指纹。空数组 = **没有扩展**（一期就是这种情况：扩展未接入）；不要留 nil——
	// nil 会序列化成 `null`，被读成"不知道有没有扩展"（那是另一句话）。冻结快照处会把 nil 归成空切片。
	Ext []string
	// Platform 是目标平台（GOOS/GOARCH）。
	Platform string
	// MaxFailStreak / MaxTurnsHard 是**机制侧**的硬顶（harness 连续失败阈值 / 轮数硬顶），只用于
	// `config_snapshot` 事件如实报出"这次运行实际生效的机制阈值"，**不进 EffectiveConfig**——
	// 机制硬顶是 harness 的运行参数，不属于"本次生效的规则装配清单"。
	//
	// 这条事实只能由装配层注入：它构造 harness.Config 并交给循环，Session 拿不到（harness 不认识
	// Session，也没有反向查询的通道）。0 表示未注入，如实表达"不知道"，不编一个看着像默认值的数。
	MaxFailStreak int
	MaxTurnsHard  int
}

// EffectiveConfig 是生效配置快照的形状。
//
// 门禁清单**不在其中**：一期门禁为空（`Session.gates = nil`），把空清单塞进来会被读成
// "没有门禁"，那是另一句话；门禁清单随门禁能力一起接入。
type EffectiveConfig struct {
	// Primitives 是定格后的工具面顺序（含殿后追加的 checkpoint）：顺序即模型看到的顺序。
	Primitives []string `json:"primitives"`
	// SystemPlugins / UserPlugins 是两段提示词插件名，顺序即正文拼接顺序。
	SystemPlugins []string `json:"system_plugins"`
	UserPlugins   []string `json:"user_plugins"`
	// Filters 是结果过滤器链的名字，顺序即生效顺序。
	Filters []string `json:"filters"`
	// Policy / Checkpoint / Platform 原样来自装配层注入的 AssemblyFacts。
	Policy     map[string]any `json:"policy"`
	Budget     Budget         `json:"budget"`
	Checkpoint string         `json:"checkpoint"`
	// Ext 是扩展能力指纹。**空数组 = 没有扩展**（已知事实），不是 `null`（未提供）——同一条纪律
	// 见 `files_changed` / `needs` 对空值的处理。
	Ext      []string `json:"ext"`
	Platform string   `json:"platform"`
}

// MarshalJSON 让预算在快照里以可读形状出现：0 表示该维度不限；墙钟换算成毫秒——
// 直接把 time.Duration 序列化会写成一串纳秒数字，读的人看不出那是什么，也就读不出
// "0 = 不限"这条口径。
func (b Budget) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		MaxTurns       int   `json:"max_turns"`
		MaxTokens      int   `json:"max_tokens"`
		MaxWallClockMS int64 `json:"max_wall_clock_ms"`
	}{b.MaxTurns, b.MaxTokens, b.MaxWallClock.Milliseconds()})
}
