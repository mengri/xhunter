package hunt

import (
	"strings"
	"testing"
	"time"
)

// 判据的期望值一律写字面量：拿被测常量当期望值，会把"常量被改掉"这件事变成绿灯。

func TestExpect_ExitZeroJudgesByExitCodeAlone(t *testing.T) {
	e := Expect{Kind: ExpectExitZero}
	if v, _ := e.Judge("有输出也不影响", "", 0); v != ExpectPass {
		t.Errorf("exit_zero 且退出码 0：期望通过，实际 %v", v)
	}
	if v, _ := e.Judge("", "", 3); v != ExpectFail {
		t.Errorf("exit_zero 且退出码 3：期望不通过，实际 %v", v)
	}
}

// 输出为空且退出码非 0，是命令崩溃或参数写错的形态——它不是"质量达标"，判通过会让一次
// 坏掉的执行变成绿灯。这一条是判据里最容易踩反的地方，单独钉住。
func TestExpect_EmptyOutputWithNonZeroExitIsUndecidable(t *testing.T) {
	e := Expect{Kind: ExpectEmptyOutput}
	v, reason := e.Judge("", "", 2)
	if v != ExpectUndecidable {
		t.Errorf("空输出 + 退出码 2：期望不可判定，实际 %v（%s）", v, reason)
	}
	if !strings.Contains(reason, "无法据此下质量结论") {
		t.Errorf("理由要说清为什么判不了：%q", reason)
	}
}

func TestExpect_EmptyOutputTrimsTrailingNewline(t *testing.T) {
	e := Expect{Kind: ExpectEmptyOutput}
	// `gofmt -l` 这类命令的正常输出常以换行结尾：把它当成"有输出"会让格式门禁永远不通过。
	if v, _ := e.Judge("\n", "", 0); v != ExpectPass {
		t.Errorf("只有换行符：期望通过，实际 %v", v)
	}
	if v, reason := e.Judge("a.go\nb.go\n", "", 0); v != ExpectFail || !strings.Contains(reason, "实际 2 行") {
		t.Errorf("两行输出：期望不通过且理由报 2 行，实际 %v / %q", v, reason)
	}
}

// max_count 数的是**含匹配的行数**，不是关键字出现的次数：同一行压缩打印两条告警算一条。
func TestExpect_MaxCountCountsLinesNotOccurrences(t *testing.T) {
	e := Expect{Kind: ExpectMaxCount, Pattern: "warning", Max: 1}
	out := "a.go:1: warning: unused x; warning: unused y\nb.go:9: ok\n"
	if v, _ := e.Judge(out, "", 0); v != ExpectPass {
		t.Errorf("同一行两处匹配按 1 行计，未超过上限 1：期望通过，实际 %v", v)
	}
	if v, reason := e.Judge(out, "", 0); v != ExpectPass {
		t.Errorf("期望通过，实际 %v", v)
	} else if !strings.Contains(reason, "未超过上限 1") {
		t.Errorf("理由要带上实际与上限：%q", reason)
	}

	e2 := Expect{Kind: ExpectMaxCount, Pattern: "warning", Max: 0}
	v, reason := e2.Judge(out, "", 0)
	if v != ExpectFail {
		t.Errorf("有 1 行匹配、上限 0：期望不通过，实际 %v", v)
	}
	if !strings.Contains(reason, "匹配行数 1 超过上限 0") {
		t.Errorf("不通过的理由要给出实际行数与上限：%q", reason)
	}
}

func TestExpect_RegexNeedsAtLeastOneMatchingLine(t *testing.T) {
	e := Expect{Kind: ExpectRegex, Pattern: "PASS"}
	if v, _ := e.Judge("--- PASS: TestFoo\nok\n", "", 0); v != ExpectPass {
		t.Errorf("有匹配行：期望通过，实际 %v", v)
	}
	if v, _ := e.Judge("--- FAIL: TestFoo\n", "", 0); v != ExpectFail {
		t.Errorf("无匹配行：期望不通过，实际 %v", v)
	}
}

// stream 决定判据算在哪条流上：诊断打在 stderr 的门禁若按 stdout 判，会永远"通过"。
func TestExpect_StreamSelectsWhereTheVerdictIsComputed(t *testing.T) {
	e := Expect{Kind: ExpectEmptyOutput, Stream: ExpectStderr}
	if v, _ := e.Judge("stdout 有内容", "", 1); v != ExpectPass {
		t.Errorf("判据只看 stderr：stderr 为空即通过，实际 %v", v)
	}
	both := Expect{Kind: ExpectRegex, Pattern: "boom", Stream: ExpectBoth}
	if v, _ := both.Judge("stdout", "boom", 1); v != ExpectPass {
		t.Errorf("both 要同时看到两条流：期望通过，实际 %v", v)
	}
}

func TestExpect_UnknownKindAndStreamAreUndecidable(t *testing.T) {
	if v, _ := (Expect{Kind: ExpectKind("随便写的")}).Judge("", "", 0); v != ExpectUndecidable {
		t.Errorf("未知判据种类：期望不可判定，实际 %v", v)
	}
	if v, _ := (Expect{Kind: ExpectEmptyOutput, Stream: ExpectStream("tty")}).Judge("", "", 0); v != ExpectUndecidable {
		t.Errorf("未知输出流：期望不可判定，实际 %v", v)
	}
}

// 判据在全量输出上算；输出超过上界时**不截断后硬判**——那会给出与全量不同的结论。
func TestExpect_OutputBeyondCapIsUndecidable(t *testing.T) {
	e := Expect{Kind: ExpectEmptyOutput}
	big := strings.Repeat("x\n", (1<<20)/2+2)
	if len(big) <= 1<<20 {
		t.Fatalf("夹具不足以越过上界：%d 字节", len(big))
	}
	v, reason := e.Judge(big, "", 0)
	if v != ExpectUndecidable {
		t.Errorf("输出越过上界：期望不可判定，实际 %v", v)
	}
	if !strings.Contains(reason, "无法在全量输出上计算") {
		t.Errorf("理由要说清是判不了而不是不通过：%q", reason)
	}
}

func TestExpect_ValidateRejectsUndecidableSpec(t *testing.T) {
	cases := []struct {
		name string
		e    Expect
		want string
	}{
		{"缺 pattern", Expect{Kind: ExpectRegex}, "必须给出 pattern"},
		{"pattern 编译不过", Expect{Kind: ExpectMaxCount, Pattern: "([", Max: 1}, "无法编译"},
		{"max 为负", Expect{Kind: ExpectMaxCount, Pattern: "x", Max: -1}, "不得为负"},
		{"未知种类", Expect{Kind: ExpectKind("nope")}, "未知的判据种类"},
		{"未知流", Expect{Kind: ExpectExitZero, Stream: ExpectStream("tty")}, "未知的判据输出流"},
	}
	for _, c := range cases {
		err := c.e.Validate()
		if err == nil {
			t.Errorf("%s：期望报错，实际通过校验", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：错误信息应含 %q，实际 %q", c.name, c.want, err.Error())
		}
	}
	if err := (Expect{Kind: ExpectExitZero}).Validate(); err != nil {
		t.Errorf("exit_zero 不需要额外字段：%v", err)
	}
}

// 元门禁：把 shell 请回来的清单条目必须在生效前被拒——否则"数组直启"这条约束形同虚设。
func TestGate_ValidateRejectsShellAndMalformedEntries(t *testing.T) {
	cases := []struct {
		name string
		g    Gate
		want string
	}{
		{"缺名字", Gate{Argv: []string{"go", "build"}}, "缺少 name"},
		{"缺命令", Gate{Name: "build"}, "缺少 argv"},
		{"直接给 shell", Gate{Name: "sh", Argv: []string{"sh", "-c", "true"}}, "不得把 shell 请回来"},
		{"绝对路径的 shell 同样拒", Gate{Name: "sh", Argv: []string{"/bin/bash", "-c", "true"}}, "不得把 shell 请回来"},
		{"负超时", Gate{Name: "build", Argv: []string{"go", "build"}, Timeout: -time.Second}, "不得为负"},
		{"判据不可用", Gate{Name: "lint", Argv: []string{"lint"}, Expect: Expect{Kind: ExpectRegex}}, "判据不可用"},
		{"没有判据", Gate{Name: "lint", Argv: []string{"lint"}}, "判据不可用"},
	}
	for _, c := range cases {
		err := c.g.Validate()
		if err == nil {
			t.Errorf("%s：期望报错，实际通过校验", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：错误信息应含 %q，实际 %q", c.name, c.want, err.Error())
		}
	}
	// expect 是必填：没有判据的门禁要么永真要么永假，比没有门禁更坏。
	if err := (Gate{Name: "fmt", Argv: []string{"gofmt", "-l", "."}, Expect: Expect{Kind: ExpectEmptyOutput}}).Validate(); err != nil {
		t.Errorf("合法的条目不该被拒：%v", err)
	}
}

func TestIsShellCommand(t *testing.T) {
	for _, argv0 := range []string{"sh", "/bin/sh", "/usr/bin/bash", "zsh", "dash"} {
		if !IsShellCommand(argv0) {
			t.Errorf("%q 是 shell，应被识别", argv0)
		}
	}
	for _, argv0 := range []string{"go", "gofmt", "npm", "/usr/bin/golangci-lint", ""} {
		if IsShellCommand(argv0) {
			t.Errorf("%q 不是 shell，不该被识别", argv0)
		}
	}
}
