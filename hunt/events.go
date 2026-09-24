package hunt

// 本文件定义外部事件的载荷形状（以使用手册 §5 为准）；哪种已发出见各自注释。

// configSnapshotPayload 是 `config_snapshot` 事件的载荷（使用手册 §5）：把"这次运行实际生效的
// 若干阈值"一次性报给平台，供其解释后续的机制性终止。
//
// 各阈值从哪来：
//   - MaxFailStreak：**机制侧**连续失败阈值（`harness.Config.MaxFailStreak`，装配层注入）；
//   - MaxTurnsHard：轮数硬顶（0 = 不限，装配层注入）；
//   - StreamIdleTimeoutMS：**机制侧**接收段不活动超时（`harness.Config.StreamIdleTimeout` 的
//     生效值，毫秒，装配层注入）。**它的 0 表示"不限/关闭"**——与其它键"0 = 不知道"的口径
//     **不同**（默认 120s 时是 120000，不是 0）；
//   - MaxDeniedStreak：**策略侧**连续拒绝阈值（`policy.Facts()`，策略自述）；
//   - MaxSameKindStreak：**策略侧**连续同类失败上限（`policy.Facts()`，策略自述）。
//
// **发出点**：`Session.Prepare` 与 `hunt_start` 同时点（见 `hooks.go` 的 `emitHuntStart`）——
// 两个止损阈值来自策略、三条机制阈值来自装配层注入的 harness 生效配置，都在起飞时已定。
type configSnapshotPayload struct {
	MaxDeniedStreak     int `json:"max_denied_streak"`
	MaxSameKindStreak   int `json:"max_same_kind_streak"`
	MaxFailStreak       int `json:"max_fail_streak"`
	MaxTurnsHard        int `json:"max_turns_hard"`
	StreamIdleTimeoutMS int `json:"stream_idle_timeout_ms"`
}

// payload 把载荷摊成事件用的 map：事件载荷是 `map[string]any`，结构体不能直接塞进
// `ExternalEvent.Payload`（会被序列化成嵌套对象、对不上契约的平铺形状）。
func (p configSnapshotPayload) payload() map[string]any {
	return map[string]any{
		"max_denied_streak":      p.MaxDeniedStreak,
		"max_same_kind_streak":   p.MaxSameKindStreak,
		"max_fail_streak":        p.MaxFailStreak,
		"max_turns_hard":         p.MaxTurnsHard,
		"stream_idle_timeout_ms": p.StreamIdleTimeoutMS,
	}
}

// contextCompactedPayload 是 `context_compacted` 事件的载荷（使用手册 §5）：每次压缩产出一条，
// 携带命中的层级与释放的 token 量（FR-14.7、AC-18）。
//
// `level` 是命中层级（`L0`~`L4`，见架构 §7.2 的分层表）；`watermark` 是命中哪一档水位
// （`warn` / `target` / `hard`）。
//
// **发出点不接**：分层下压与发出点属 MS-11——见 `ContextBuilder` 上的压缩位注释。
type contextCompactedPayload struct {
	Level          string `json:"level"`
	ReleasedTokens int    `json:"released_tokens"`
	Watermark      string `json:"watermark"`
}

// CompactionConfig 是压缩的**装配参数**：`ContextBuilder` 触发压缩时用到的三档水位与冷却。
//
// **进入路径**：水位不是裸比例，而是**可用输入预算**（`providerconfig.Resolved.InputBudget`，FR-9.6）
// 乘三档比例、四舍五入得到——`providerconfig.Resolved.Watermarks` 已落地这套口径（FR-14.1）。装配层
// 用同一个 `Resolved` 算出三档阈值，连冷却（默认 3 轮，见 §7.1 决策 15）一起交给 `ContextBuilder`。
// **触发点在 `ContextBuilder.Assemble` 内**（§7.2：压缩落在 ContextBuilder 内）——命中 Watermark
// 即下压一级并产出 `context_compacted`。**分层下压的实现属 MS-11**，本次只定义入口。
type CompactionConfig struct {
	WarnAt   int // 命中即预警（事件 watermark: "warn"）
	TargetAt int // 命中即下压到目标（"target"）
	HardAt   int // 命中即强制下压（"hard"）
	Cooldown int // 两次压缩之间的最小轮间隔（0 取默认 3）
}
