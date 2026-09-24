package hunt

import (
	"fmt"
	"regexp"
	"strings"
)

// ============================================================ 门禁判据

// ExpectKind 是「算通过」的条件种类。判据**由清单声明**，引擎不内置默认：
// 门禁命令千差万别——格式检查看输出是否为空、lint 看告警条数、构建看退出码——
// 一律按退出码判会让格式类门禁永远"通过"，那等于没有门禁。
type ExpectKind string

const (
	// ExpectExitZero：退出码为 0 即通过。
	ExpectExitZero ExpectKind = "exit_zero"
	// ExpectEmptyOutput：选定输出流（去空白后）为空即通过，如 `gofmt -l .`。
	ExpectEmptyOutput ExpectKind = "empty_output"
	// ExpectRegex：选定输出流上找到至少一处匹配即通过。
	ExpectRegex ExpectKind = "regex"
	// ExpectMaxCount：选定输出流上**含匹配的行数**不超过 Max 即通过，如 lint 的"告警数 ≤ N"。
	ExpectMaxCount ExpectKind = "max_count"
)

// ExpectStream 指定判据在哪条输出流上计算；空值按 stdout 处理。
type ExpectStream string

const (
	ExpectStdout ExpectStream = "stdout"
	ExpectStderr ExpectStream = "stderr"
	ExpectBoth   ExpectStream = "both"
)

// maxJudgeOutputBytes 是判据能看到的输出上界。
//
// 判据必须在**全量输出**上算（交给模型的输出要截断，截断不得影响判定），但"全量"不可能真的
// 无界：超过这个量就判为**不可判定**，而不是在截断后的文本上硬判——后者会给出与全量不同的
// 结论，而门禁的全部价值正是"同一份改动跑两次，结论相同"。
const maxJudgeOutputBytes = 1 << 20

// Expect 是一条门禁的通过判据：按什么算通过，由它说了算。
type Expect struct {
	Kind ExpectKind
	// Pattern 是 RE2 语法的匹配式，regex / max_count 时必填。大小写不另设开关——由表达式
	// 自己写 `(?i)`：多一个开关就多一处"配置与行为不一致"的可能，而门禁要的是可复现。
	Pattern string
	// Max 是 max_count 的上限（含）。
	Max int
	// Stream 指定判据在哪条流上算；空值按 stdout 处理。
	Stream ExpectStream
}

// ExpectVerdict 是判据的三态结论。
//
// 三态是刻意的：**不可判定不等于没通过**。判据缺失、输出超上界、命令崩了却没留下输出——
// 这几种情形我们并没有判过，只是没法判。读成"不通过"，平台会去改代码；读成"通过"，门禁
// 就成了摆设。两者都错，所以单独一档并如实上报（由调用方按环境错误处理）。
type ExpectVerdict int

const (
	ExpectPass ExpectVerdict = iota
	ExpectFail
	ExpectUndecidable
)

// Validate 在清单生效前校验判据**是否可判定**（元门禁的一环）。
//
// 判据不可判定的清单一旦生效，门禁要么永真要么永假——那比没有门禁更坏，因为它看起来在工作。
func (e Expect) Validate() error {
	switch e.Kind {
	case ExpectExitZero, ExpectEmptyOutput:
		// 这两档不需要额外字段。
	case ExpectRegex, ExpectMaxCount:
		if strings.TrimSpace(e.Pattern) == "" {
			return fmt.Errorf("判据 %q 必须给出 pattern", e.Kind)
		}
		if _, err := regexp.Compile(e.Pattern); err != nil {
			return fmt.Errorf("判据 %q 的 pattern 无法编译：%w", e.Kind, err)
		}
		if e.Kind == ExpectMaxCount && e.Max < 0 {
			return fmt.Errorf("判据 max_count 的 max 不得为负：%d", e.Max)
		}
	default:
		return fmt.Errorf("未知的判据种类 %q", e.Kind)
	}
	switch e.Stream {
	case "", ExpectStdout, ExpectStderr, ExpectBoth:
	default:
		return fmt.Errorf("未知的判据输出流 %q", e.Stream)
	}
	return nil
}

// Judge 在**全量输出**上按声明的判据判定，给出三态结论与一句给人看的理由。
//
// 退出码只在 exit_zero 这一档参与判定：lint 常以退出码表达"有告警"，若把退出码叠加到每一档，
// "告警数 ≤ N"就会因为退出码非 0 而永远不通过，那一档判据等于没写。唯一例外是"输出为空且
// 退出码非 0"——那是命令崩溃或参数写错的形态，不是质量结论；判它"通过"会让一次坏掉的执行
// 变成绿灯，所以归为不可判定。
func (e Expect) Judge(stdout, stderr string, exitCode int) (ExpectVerdict, string) {
	switch e.Stream {
	case "", ExpectStdout, ExpectStderr, ExpectBoth:
	default:
		return ExpectUndecidable, fmt.Sprintf("判据输出流 %q 无法识别，无法判定", e.Stream)
	}

	if e.Kind == ExpectExitZero {
		if exitCode == 0 {
			return ExpectPass, "退出码为 0"
		}
		return ExpectFail, fmt.Sprintf("退出码为 %d", exitCode)
	}

	text := judgeText(stdout, stderr, e.Stream)
	if len(text) > maxJudgeOutputBytes {
		return ExpectUndecidable, fmt.Sprintf("输出超过 %d 字节，判据无法在全量输出上计算", maxJudgeOutputBytes)
	}
	// 两条流都空且退出码非 0 = 命令什么都没说就退了（崩溃、参数写错）。这时不能判"通过"，
	// 也不能判"不通过"——我们并没有判过，只是没有可判的东西。
	// 注意看的是**两条流**：只要命令留下了任何输出，它就说过了话，该按声明的流判。
	if strings.TrimSpace(stdout) == "" && strings.TrimSpace(stderr) == "" && exitCode != 0 {
		return ExpectUndecidable, fmt.Sprintf("命令退出码为 %d 且没有任何输出，无法据此下质量结论（多半是命令崩溃或参数写错）", exitCode)
	}

	switch e.Kind {
	case ExpectEmptyOutput:
		if strings.TrimSpace(text) == "" {
			return ExpectPass, "输出为空"
		}
		return ExpectFail, fmt.Sprintf("期望输出为空，实际 %d 行", countLines(text))

	case ExpectRegex, ExpectMaxCount:
		re, err := regexp.Compile(e.Pattern)
		if err != nil {
			return ExpectUndecidable, fmt.Sprintf("判据 %q 的 pattern 无法编译，无法判定", e.Kind)
		}
		lines := countMatchingLines(text, re)
		if e.Kind == ExpectRegex {
			if lines >= 1 {
				return ExpectPass, fmt.Sprintf("找到 %d 行匹配（要求至少 1 行）", lines)
			}
			return ExpectFail, "输出中没有任何匹配行"
		}
		if lines <= e.Max {
			return ExpectPass, fmt.Sprintf("匹配行数 %d 未超过上限 %d", lines, e.Max)
		}
		return ExpectFail, fmt.Sprintf("匹配行数 %d 超过上限 %d", lines, e.Max)

	default:
		return ExpectUndecidable, fmt.Sprintf("未知的判据种类 %q，无法判定", e.Kind)
	}
}

// judgeText 拼出判据要算的那份文本。
func judgeText(stdout, stderr string, stream ExpectStream) string {
	switch stream {
	case ExpectStderr:
		return stderr
	case ExpectBoth:
		if stdout == "" || stderr == "" {
			return stdout + stderr
		}
		return stdout + "\n" + stderr
	default:
		return stdout
	}
}

// countMatchingLines 数**含至少一处匹配的行数**（同一行多次匹配只算一次）。
//
// 与 `grep -c` 同口径：门禁说的是"告警不超过 N 条"，人眼数的也是条目（行）而不是关键字出现
// 的次数；把同一行压缩打印的两条告警算成两条，会让计数系统性偏高、把该过的交付判成失败。
func countMatchingLines(text string, re *regexp.Regexp) int {
	n := 0
	for _, line := range strings.Split(text, "\n") {
		if re.MatchString(line) {
			n++
		}
	}
	return n
}

// countLines 数文本行数：末尾的换行不算作额外一行。
func countLines(text string) int {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return 0
	}
	return len(strings.Split(text, "\n"))
}
