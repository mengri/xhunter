package hunt

import (
	"strings"
	"unicode/utf8"
)

// Declared 是模型在正文里自陈的三类清单（FR-6.3/6.4）：缺什么条件、采取了哪些默认、
// 没验证到什么。
//
// 为什么解析落在业务层而不是 harness：小节名是**业务约定**——它们由内核条款陈述（见
// kernelClauses），harness 不认识它们。解析只做「切行去前缀」，不解释语义：它是模型的
// 自陈，不是判据。执行体据它决定终态（只有 `needs` 非空才收敛为停下），不据它推断对错。
type Declared struct {
	Needs       []string // 「## 需要补全」的条目；非空即表示模型选择停下
	Assumptions []string // 「## 假设」的条目：模型采取过的默认值
	Unverified  []string // 「## 未验证」的条目：没验证到的不确定项与未覆盖风险（只陈述，不停）
}

// 三个固定小节名。改这里就等于改内核条款的措辞——两处必须一起改。
const (
	needsSection       = "## 需要补全"
	assumptionsSection = "## 假设"
	unverifiedSection  = "## 未验证"
)

// ParseDeclared 从一段正文里切出三个固定小节的条目。
//
// 三态与结果文件一致：小节不存在 → 对应切片为 nil（**不是**空切片）——「没提供」与
// 「提供了但一条都没有」是两句话，序列化时前者才是 null。
//
// 两个取舍：
//   - 小节**以名字开头**即算命中（允许模型写 `## 需要补全（缺两项）`）；标题后面跟什么
//     不影响归属。过宽的匹配由内核条款的措辞约束——那是给模型看的话，不是给解析器的。
//   - 小节内每条**非空行**算一条，只去掉列表符号与有序编号，**不做跨行合并**：这份清单
//     本来就是逐条写的，合并等于替模型改写它的话。
func ParseDeclared(text string) Declared {
	var d Declared
	var cur *[]string
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if isHeading(trimmed) {
			cur = sectionTarget(&d, trimmed)
			continue
		}
		if cur == nil {
			continue
		}
		if item := trimItemPrefix(trimmed); item != "" {
			*cur = append(*cur, item)
		}
	}
	return d
}

// isHeading 报告这是一行 Markdown 标题。
func isHeading(line string) bool { return strings.HasPrefix(line, "#") }

// sectionTarget 判断一个标题属于哪个固定小节；不属于则返回 nil——它同时表示「后面的
// 内容不归这三类」，因此解析器遇到别的小节会自动收手。
//
// 两侧都必须过 normalizeHeading：光去 `#` 会留下一个前导空格，标题与小节名就再也对不上
// （这类不对称不会报错，只会静默地什么都切不出来）。
func sectionTarget(d *Declared, heading string) *[]string {
	title := normalizeHeading(heading)
	switch {
	case strings.HasPrefix(title, normalizeHeading(needsSection)):
		return &d.Needs
	case strings.HasPrefix(title, normalizeHeading(assumptionsSection)):
		return &d.Assumptions
	case strings.HasPrefix(title, normalizeHeading(unverifiedSection)):
		return &d.Unverified
	}
	return nil
}

// normalizeHeading 去掉标题标记与首尾空白，得到可比对的标题文本。
func normalizeHeading(s string) string {
	return strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(s), "#"))
}

// trimItemPrefix 去掉列表符号与有序编号，返回条目正文。只去前缀，不改写正文。
func trimItemPrefix(line string) string {
	s := strings.TrimSpace(line)
	if s == "" {
		return ""
	}
	// 无序列表：`- x` / `* x` / `+ x`
	if r, size := utf8.DecodeRuneInString(s); r == '-' || r == '*' || r == '+' {
		return strings.TrimSpace(s[size:])
	}
	// 有序编号：`1、x` / `1）x` / `1)x` / `1. x`。
	// 半角句点必须后跟空白才算编号，否则 `2.5 倍` 这类正文会被吃掉开头。
	if n := digitRun(s); n > 0 {
		if rest := s[n:]; rest != "" {
			r, size := utf8.DecodeRuneInString(rest)
			if strings.ContainsRune("、）．。：)", r) || (r == '.' && hasSpaceAfter(rest, size)) {
				return strings.TrimSpace(rest[size:])
			}
		}
	}
	// 括号编号：`（1）x` / `(1) x`
	if r, size := utf8.DecodeRuneInString(s); r == '（' || r == '(' {
		if n := digitRun(s[size:]); n > 0 {
			if rest := s[size+n:]; rest != "" {
				if r2, size2 := utf8.DecodeRuneInString(rest); r2 == '）' || r2 == ')' {
					return strings.TrimSpace(rest[size2:])
				}
			}
		}
	}
	return s
}

// digitRun 返回开头连续数字的字节数。
func digitRun(s string) int {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return i
}

// hasSpaceAfter 报告第 size 个字节之后是否紧跟空白。
func hasSpaceAfter(s string, size int) bool {
	return len(s) > size && (s[size] == ' ' || s[size] == '\t')
}
