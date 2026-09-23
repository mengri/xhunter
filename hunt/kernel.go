package hunt

import (
	"runtime"
	"strings"
)

// 内核条款与环境事实：首轮两段正文里**不由插件贡献**的那部分。
//
// 位置是硬的（FR-7.8）：条款追加在 system 段的**末尾**，环境事实摆在对 user 段的
// **最前面**。插件只交正文，插不进第三段，也删不掉这两块——`firstPrompt` 是唯一的
// 拼装点，条款与事实在插件的正文之外由代码生成，插件没有表达"删除"的途径。
//
// 为什么必须由内核而非插件提供：这两块说的是"系统怎么工作"（无人可问、止损、
// 工具纪律、安全边界）与"这次跑在哪儿"（平台、基线、能力边界），是内核知道而插件
// 无从知道的事实。交给插件陈述，换一次装配就会换一套说法，而它们恰好是无人值守
// 场景下最不能漂的部分（FR-7.2、FR-7.3、FR-7.5、AC-29）。

// kernelClauses 是 system 段末尾的内核条款。
func kernelClauses() string {
	return strings.Join([]string{
		"[内核条款]（内核追加，不可增删）",
		"- 无人可问：不得提问、不得请求确认、不得等待标准输入、不得请求权限。信息不足时**由你判断继续还是停下**：依据是任务描述与项目约定——不同的用户、不同的任务对严谨度的要求不同，任务描述里写明了就按它办；一般地，缺的是关键前提（继续做只会建立在错误的默认上）就停下，缺的只是枝节就先做完再列。停下时在正文里用固定小节「## 需要补全」逐条列出**任务描述与项目约定都没有给出**的必要事实（缺什么、为什么不能取默认）——**写这个小节即表示停止**，系统读到它就收敛、不再往下走；另外用「## 假设」列出你实际采取了哪些默认。",
		"- 止损：同一类失败反复出现时换一种做法；预算耗尽或工具连续失败到上限即停下并如实报告，不得绕过限制。",
		"- 工具纪律：只通过工具读写工作区；修改已存在的文件前先读它；一次调用只做一件事；工具报错就按错误种类换做法，不要重复同一次失败调用。",
		"- 安全边界：策略拒绝是硬边界——它给出原因是为了让你换做法，而不是让你绕过；不尝试获取 shell、网络或凭据。",
	}, "\n")
}

// environmentFacts 是 user 段最前面的环境事实。
//
// 刻意不给绝对路径：工具面只接受工作区内相对路径，说一个绝对目录只会诱导模型
// 去用它；这里陈述的是"路径怎么算"与"哪些能力根本不存在"。
func environmentFacts(b Bounty) string {
	facts := []string{
		"[环境事实]（内核注入，不可增删）",
		"- 平台：" + runtime.GOOS + "/" + runtime.GOARCH,
	}
	if commit := ref(b.Repo.BaseCommit); commit != "" {
		facts = append(facts, "- 基线 commit："+commit)
	}
	if b.Repo.Branch != "" {
		facts = append(facts, "- 任务分支："+b.Repo.Branch+"（由系统创建与推送；你没有任何能改变分支或已推送历史的操作）")
	}
	facts = append(facts,
		"- 路径：所有路径都是工作区内的相对路径",
		"- 能力边界：无 shell、无网络、无 git 操作；工作区之外的任何位置都不可达",
	)
	return strings.Join(facts, "\n")
}

// ref 取来源标识用的短哈希：够定位，又不至于把整串贴进提示词。
func ref(commit string) string {
	commit = strings.TrimSpace(commit)
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

// kernelClauseMarker / environmentFactMarker 是两块内核正文的起始标记。
// 测试与后续的生效配置快照据此识别它们，不必比对全文。
const (
	kernelClauseMarker    = "[内核条款]"
	environmentFactMarker = "[环境事实]"
)
