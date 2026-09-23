package hunt

// 本文件定义**尚未接线的外部事件载荷形状**（以使用手册 §5 为准）；只定义形状与发出点注释，
// 发出点随各自里程碑接入——现在发出去会是假数字（来源还不存在）。

// configSnapshotPayload 是 `config_snapshot` 事件的载荷（使用手册 §5）：把"这次运行实际生效的
// 三个阈值"一次性报给平台，供其解释后续的机制性终止。
//
// 三个阈值各自从哪来：
//   - MaxFailStreak：**机制侧**连续失败阈值（`harness.Config.MaxFailStreak`）；
//   - MaxDeniedStreak：**策略侧**连续拒绝阈值（MS-3 的止损阈值，**现在还不存在**）；
//   - MaxTurnsHard：轮数硬顶（0 = 不限）。
//
// **发出点不接**：`MaxDeniedStreak` 的来源（MS-3）还不存在，现在发出去就是假数字；等 MS-3 把
// 阈值定死后，在 `Session.Prepare` 与 `hunt_start` 同时点接入（见 `hooks.go` 的 `emitHuntStart`）。
type configSnapshotPayload struct {
	MaxDeniedStreak int `json:"max_denied_streak"`
	MaxFailStreak   int `json:"max_fail_streak"`
	MaxTurnsHard    int `json:"max_turns_hard"`
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
