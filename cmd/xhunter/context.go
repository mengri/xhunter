package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"xhunter/harness"
	"xhunter/hunt"
	"xhunter/llm"
)

// 压缩的默认口径（FR-14.1）：预警 70% 触发、目标 50% 为压后水位、硬上限 90%，冷却 3 轮。
//
// 三个比例与冷却只在这里写一遍：`hunt.CompactionConfig` 收的是**绝对阈值**（三档水位），
// 由装配层用 `providerconfig.Resolved.Watermarks` 从可用输入预算算出后注入——比例在装配层、
// 阈值在契约里，各写各的不会漂。
const (
	compactionWarnRatio   = 0.70
	compactionTargetRatio = 0.50
	compactionHardRatio   = 0.90
	compactionCooldown    = 3
)

// charsPerToken 是水位这把尺子的刻度：4 字符 ≈ 1 token。
//
// 它是**估算**，不是真值：真值只有上游回报（usage 事件），两者不得混用——这里只回答
// "离水位还差多少"，不进任何对外口径（"用量不可得时不估算"那条纪律管的是对外口径）。
// 用字符数而不是字节数：中文按字符更接近直觉，且同一段文本在任何一次运行里都得出同一个数
// ——可复现是压缩的验收项之一（AC-18）。
const charsPerToken = 4

// resultKeepChars 是 L0 截断后**两头各留**的字符数（中间丢掉的部分在标注里给出数量）。
const resultKeepChars = 512

// 分层下压的层名（FR-14.2）：事件载荷的 `level` 与它们逐字一致。
//
// L4（模型摘要）**不在本期实现**：它默认就不启用（确定性优先，FR-14.4/14.5），而"生成一次即
// 落盘、恢复时读回"要动材料与恢复两条链路，缺了它就不许声称做了 L4——宁可留缺口，也不做一个
// 会在恢复路径上重新生成的版本。
const (
	levelL0 = "L0"
	levelL1 = "L1"
	levelL2 = "L2"
	levelL3 = "L3"
)

// compactionLevels 是下压的**顺序**：按序下压、够用即停（FR-14.2）。
var compactionLevels = []string{levelL0, levelL1, levelL2, levelL3}

// contextBuilder 持有首轮提示词与历史，并按需组装「提示词 + 历史」的完整消息。
//
// 「历史只住在这里」是刻意的——轮级状态（harness.Turn）只装「本轮那段」，handler
// 因此物理上碰不到历史；发给模型的东西一律由 Assemble 现拼，不靠某处维护的一份可变副本。
//
// 压缩也住在这里（§7.2：触发点在 `Assemble`）。它是**低频、一次压到位**的：两段消息里前置
// 的稳定内容正是为了吃前缀缓存，任何对已缓存前缀的改写都会让它后面全部 token 的缓存失效，
// 所以命中水位即一次把该层的**全部老轮**压掉，然后进冷却。
type contextBuilder struct {
	prompt []llm.Message
	turns  []harness.Turn
	// states 与 turns **一一对应**：每轮当前被压到哪一层（零值 = 未压）。
	states []turnState
	// cfg 是压缩的装配参数（三档水位 ＋ 冷却）。零值 = 不压缩（没有水位就无从谈压缩）。
	cfg hunt.CompactionConfig
	// cooldown 是"再压一次之前还要经过几轮"：压完置为冷却值，每轮 Append 递减。
	cooldown int
	// opsSrc 给出本次运行的写操作序列（L3 工作日志的「已完成改动」一节）。nil = 不写这一节。
	opsSrc func() []hunt.WriteOp
	// pending 是自上次取走以来的压缩结论：`context_compacted` 事件载荷的**唯一来源**。
	pending []hunt.Compaction
	// overHard 是“压完仍不低于硬上限”（FR-14.1“硬上限视为不可继续”）。每次组装重算，
	// 与压缩结论不是一回事：结论回答“压了几层、放了多少”，这条回答“还跑得动吗”。
	overHard bool
	// log 是 L3 折叠出来的结构化工作日志（空 = 还没有折叠）。它是**投影**，不是总结：
	// 由会话记录按固定字段算出，零模型调用、可复现。
	log string
}

// turnState 是一轮历史当前被压到哪一层。各层可叠加（L0 之后再 L2 是"截断后再摘要"，
// 渲染时以更深的那一层为准）。
type turnState struct {
	truncated   bool // L0：工具输出已截断
	droppedText bool // L1：推理过程（模型正文）已丢，工具调用与结果留着
	summarized  bool // L2：工具输出已整体换成可寻址摘要
	folded      bool // L3：本轮已折叠进结构化工作日志
}

// SetPrompt 设定首轮提示词（Prepare 调一次，此后不变）。
func (c *contextBuilder) SetPrompt(msgs []llm.Message) { c.prompt = msgs }

// Assemble 给出「提示词 + 历史」的完整消息。压缩的触发点就在这里：
// 先按水位下压，再按每轮的当前层级渲染。
func (c *contextBuilder) Assemble() []llm.Message {
	c.compact()
	msgs := make([]llm.Message, 0, len(c.prompt)+2*len(c.turns)+1)
	msgs = append(msgs, c.prompt...)
	// 折叠日志排在历史之前：它是"更早的那些轮次"的替身，位置与它们原本的位置一致。
	if c.log != "" {
		msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: c.log})
	}
	for i, t := range c.turns {
		if c.states[i].folded {
			continue
		}
		msgs = append(msgs, c.produce(t, c.states[i])...)
	}
	return msgs
}

// Append 累积本轮产生的 message。冷却在这里递减——它是**轮**级的量。
func (c *contextBuilder) Append(rec harness.Turn) {
	c.turns = append(c.turns, rec)
	c.states = append(c.states, turnState{})
	if c.cooldown > 0 {
		c.cooldown--
	}
}

// TakeCompactions 取走（并清空）自上次取走以来的压缩结论。取走即清空：同一条结论
// 发两次会让平台把一次压缩记成两次。
func (c *contextBuilder) TakeCompactions() []hunt.Compaction {
	out := c.pending
	c.pending = nil
	return out
}

// OverHardLimit 报告“压完仍不低于硬上限”（FR-14.1）：下一轮装不进窗口，继续发只会
// 换来上游的报错。它是**每次组装重算**的事实，不是一次性结论。
func (c *contextBuilder) OverHardLimit() bool { return c.overHard }

// compact 是每次组装都要走的一道：先按水位下压，再回答“压完还装得进窗口吗”。
func (c *contextBuilder) compact() {
	if c.cfg.WarnAt > 0 && c.cooldown <= 0 {
		c.press()
	}
	// 硬上限是**每次组装都要回答**的问题，与冷却无关：冷却管的是“别反复改写已缓存前缀”，
	// 而“下一轮装不进窗口”是还能不能发的问题——压完仍不低于它就没有余量跑下一轮，
	// 继续发只会换来上游的报错（usage§7“上下文达硬上限 → 退出 2”，按预算耗尽处理）。
	c.overHard = c.cfg.HardAt > 0 && c.measure() >= c.cfg.HardAt
}

// press 命中水位即下压：按序压到目标水位以下为止，压完进冷却。
//
// 三档水位的分工：**预警**是“该动手了”，**目标**是“压到这里就够”，**硬上限**是“再不压就跑不完
// 下一轮”。因此硬上限触发时同样一路压到目标，只是事件里的 `watermark` 记 "hard"——平台据此
// 看出这次是“差点撞墙”而不是常规保养（**不是判错**：终止与否由压完之后的水位决定，
// 见 `compact`）。
//
// **当前轮永不压**（FR-14.3）：它正在被模型用来接着说，压它等于把模型脚下的地板抽掉。
func (c *contextBuilder) press() {
	before := c.measure()
	if before < c.cfg.WarnAt {
		return
	}
	watermark := "warn"
	if c.cfg.HardAt > 0 && before >= c.cfg.HardAt {
		watermark = "hard"
	}
	// 目标档没配就退到预警线：总得有个"压到哪算够"，不能压一轮算一轮。
	target := c.cfg.TargetAt
	if target <= 0 {
		target = c.cfg.WarnAt
	}
	applied := 0
	for _, level := range compactionLevels {
		if c.measure() < target {
			break // 够用即停
		}
		if !c.apply(level) {
			continue // 这一层没有可压的东西（老轮已经压过、或压不动了）
		}
		after := c.measure()
		// 释放量按**同一把尺子**的前后差算：不另估一次，两个数字因此不可能对不上。
		// 可以为负——折叠工作日志本身就是一层新内容，压掉的那一轮若已经很薄，净效果就是
		// 「反而占了一点」。如实报，不夹到 0：夹掉之后事件与前后差就不再对得上。
		c.pending = append(c.pending, hunt.Compaction{
			Level:          level,
			ReleasedTokens: before - after,
			Watermark:      watermark,
		})
		before = after
		applied++
	}
	if applied == 0 {
		// 一层都没压动（可压的老轮还不存在——当前轮永不压）→ **不上冷却**。
		// 冷却若在「压不动」时就上膛，等下一轮真的带来第一个可压的老轮，反而被自己的
		// 冷却挡住：该压的时候压不动，与冷却「别反复改写已缓存前缀」的初衷正好相反。
		return
	}
	c.cooldown = c.cfg.Cooldown
	if c.cooldown <= 0 {
		c.cooldown = compactionCooldown
	}
}

// apply 把某一层压到**全部可压的老轮**上，返回是否真的动了东西。
//
// 一次压全部而不是"每轮压一点"：改写已缓存前缀的代价与改多少关系不大，与改几次关系很大。
func (c *contextBuilder) apply(level string) bool {
	n := len(c.turns) - 1 // 当前轮永不压
	if n <= 0 {
		return false
	}
	applied := false
	for i := 0; i < n; i++ {
		st := &c.states[i]
		if st.folded {
			continue // 已折叠的轮次不再逐层加工：它们已经只剩工作日志了
		}
		switch level {
		case levelL0:
			if !st.truncated {
				st.truncated = true
				applied = true
			}
		case levelL1:
			if !st.droppedText {
				st.droppedText = true
				applied = true
			}
		case levelL2:
			if !st.summarized {
				st.summarized = true
				applied = true
			}
		case levelL3:
			st.folded = true
			applied = true
		}
	}
	if level == levelL3 && applied {
		c.rebuildLog()
	}
	return applied
}

// measure 用同一把尺子量出当前组装结果的 token 量（估算，见 charsPerToken）。
func (c *contextBuilder) measure() int {
	total := 0
	for _, m := range c.prompt {
		total += estimateMessage(m)
	}
	if c.log != "" {
		total += estimateTokens(c.log)
	}
	for i, t := range c.turns {
		if c.states[i].folded {
			continue
		}
		for _, m := range c.produce(t, c.states[i]) {
			total += estimateMessage(m)
		}
	}
	return total
}

// produce 把一轮按它当前的层级渲染成留在上下文里的消息：先是模型这一轮说了什么（含它要调的
// 工具），再是这些调用的结果。下一轮因此能看到「自己刚做过什么、得到了什么」。
//
// 空轮不占位置：既没正文也没调用，就没有什么可带进历史的。
func (c *contextBuilder) produce(t harness.Turn, st turnState) []llm.Message {
	text := t.Text
	if st.droppedText {
		// L1：推理过程可丢弃（它是"模型怎么想的"，工具调用与结果才是可复现的事实）。
		// 最终答复不在这里丢——当前轮永不压，且 `Session` 另存了最后一轮正文。
		text = ""
	}
	if text == "" && len(t.Calls) == 0 {
		return nil
	}
	msgs := make([]llm.Message, 0, 2)
	if text != "" || len(t.Calls) > 0 {
		msgs = append(msgs, llm.Message{Role: llm.RoleAssistant, Content: text, Calls: t.Calls})
	}
	results := t.Results
	switch {
	case st.summarized:
		results = addressableSummaries(t)
	case st.truncated:
		results = truncatedResults(t.Results)
	}
	if len(results) > 0 {
		msgs = append(msgs, llm.Message{Role: llm.RoleTool, Results: results})
	}
	return msgs
}

// rebuildLog 把已折叠的轮次投影成结构化工作日志（FR-14.4：投影，不是总结）。
//
// 字段固定、顺序固定、零模型调用——同一份记录投影两次必须逐字相同（AC-18）。
// 任务与验收标准**不在**这里：它在首轮提示词里，而提示词永不裁剪。
func (c *contextBuilder) rebuildLog() {
	folded := make([]harness.Turn, 0, len(c.turns))
	from, to := -1, -1
	for i, t := range c.turns {
		if c.states[i].folded {
			folded = append(folded, t)
			if from < 0 {
				from = i + 1
			}
			to = i + 1
		}
	}
	if len(folded) == 0 {
		c.log = ""
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[结构化工作日志] 第 %d–%d 轮已折叠；以下由会话记录投影生成（零模型调用，可复现）\n",
		from, to)

	calls, results, failures := 0, 0, 0
	for _, t := range folded {
		calls += len(t.Calls)
		results += len(t.Results)
		for _, r := range t.Results {
			if r.IsError {
				failures++
			}
		}
	}
	fmt.Fprintf(&b, "- 折叠范围内：%d 次工具调用、%d 条结果（其中 %d 条失败）\n", calls, results, failures)

	b.WriteString("\n## 已完成改动\n")
	changed := c.changedFiles()
	if len(changed) == 0 {
		b.WriteString("-（无）\n")
	} else {
		for _, line := range changed {
			fmt.Fprintf(&b, "- %s\n", line)
		}
	}

	b.WriteString("\n## 失败尝试与原因\n")
	tries := make([]string, 0)
	for i, t := range folded {
		for _, r := range t.Results {
			if !r.IsError {
				continue
			}
			tries = append(tries, fmt.Sprintf("- 第 %d 轮：%s", i+1, firstLine(r.Output)))
		}
	}
	if len(tries) == 0 {
		b.WriteString("-（无）\n")
	} else {
		for _, line := range tries {
			b.WriteString(line + "\n")
		}
	}

	// 假设与未决问题是**模型的判断**，只能自陈——折叠时把它们从正文里捞回来，
	// 否则一次压缩就能把"我说过缺什么"删掉（FR-14.3 的永不丢清单）。
	b.WriteString("\n## 未决问题与假设\n")
	kept := make([]string, 0)
	for _, t := range folded {
		d := hunt.ParseDeclared(t.Text)
		for _, n := range d.Needs {
			kept = append(kept, "- 需要补全："+n)
		}
		for _, a := range d.Assumptions {
			kept = append(kept, "- 假设："+a)
		}
	}
	if len(kept) == 0 {
		b.WriteString("-（无）\n")
	} else {
		for _, line := range kept {
			b.WriteString(line + "\n")
		}
	}

	c.log = b.String()
}

// changedFiles 给出「哪个文件被哪个工具改了几次」（按文件名排序——顺序必须可复现）。
func (c *contextBuilder) changedFiles() []string {
	if c.opsSrc == nil {
		return nil
	}
	byFile := map[string]map[string]int{}
	for _, op := range c.opsSrc() {
		tools, ok := byFile[op.File]
		if !ok {
			tools = map[string]int{}
			byFile[op.File] = tools
		}
		tools[string(op.Primitive)]++
	}
	files := make([]string, 0, len(byFile))
	for f := range byFile {
		files = append(files, f)
	}
	sort.Strings(files)
	out := make([]string, 0, len(files))
	for _, f := range files {
		tools := byFile[f]
		names := make([]string, 0, len(tools))
		for n := range tools {
			names = append(names, n)
		}
		sort.Strings(names)
		parts := make([]string, 0, len(names))
		for _, n := range names {
			parts = append(parts, n+" ×"+strconv.Itoa(tools[n]))
		}
		out = append(out, f+"："+strings.Join(parts, "、"))
	}
	return out
}

// truncatedResults 是 L0：工具输出两头各留一段，中间丢掉并标注丢弃量。
//
// 留两头是因为"开头是结果主体、结尾是结论/报错"——只留开头会把最关键的那句丢掉。
func truncatedResults(in []llm.ToolResult) []llm.ToolResult {
	out := make([]llm.ToolResult, 0, len(in))
	for _, r := range in {
		if len(r.Output) <= 2*resultKeepChars {
			out = append(out, r)
			continue
		}
		dropped := len(r.Output) - 2*resultKeepChars
		out = append(out, llm.ToolResult{
			CallID: r.CallID, IsError: r.IsError,
			Output: r.Output[:resultKeepChars] +
				"\n…（已截断 " + strconv.Itoa(dropped) + " 字符）…\n" +
				r.Output[len(r.Output)-resultKeepChars:],
		})
	}
	return out
}

// addressableSummaries 是 L2：工具输出整体换成**可寻址摘要**——说清"当时动的是谁"，
// 模型据此能重新读回来。丢的是内容，不是地址。
func addressableSummaries(t harness.Turn) []llm.ToolResult {
	byID := map[string]llm.ToolCall{}
	for _, call := range t.Calls {
		byID[string(call.ID)] = call
	}
	out := make([]llm.ToolResult, 0, len(t.Results))
	for _, r := range t.Results {
		target := ""
		if call, ok := byID[r.CallID]; ok {
			target = callTarget(call)
		}
		if target == "" {
			target = "（无寻址信息）"
		}
		out = append(out, llm.ToolResult{
			CallID: r.CallID, IsError: r.IsError,
			Output: fmt.Sprintf("%s → %d 字符（已折叠为可寻址摘要，可重新读取）", target, len(r.Output)),
		})
	}
	return out
}

// callTarget 从调用参数里挑出**能重新定位**的那个字段：路径优先、其次符号/字面量/模式。
func callTarget(call llm.ToolCall) string {
	var args map[string]any
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return call.Name
	}
	for _, key := range []string{"path", "file", "symbol", "literal", "pattern"} {
		if v, ok := args[key]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return call.Name + " " + s
			}
		}
	}
	return call.Name
}

// estimateMessage 量一条消息：正文 + 调用参数 + 结果。
func estimateMessage(m llm.Message) int {
	total := estimateTokens(m.Content)
	for _, c := range m.Calls {
		total += estimateTokens(c.Name) + estimateTokens(string(c.Arguments))
	}
	for _, r := range m.Results {
		total += estimateTokens(r.Output)
	}
	return total
}

// estimateTokens 把一段文本折成 token 量（估算，见 charsPerToken）。
func estimateTokens(text string) int {
	if text == "" {
		return 0
	}
	return (len([]rune(text)) + charsPerToken - 1) / charsPerToken
}
