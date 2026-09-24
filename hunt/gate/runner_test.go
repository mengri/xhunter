package gate

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"xhunter/hunt"
)

// 门禁跑的是仓库自己的命令，但它**不该拿到 Xhunter 的运行环境**——那里面有模型密钥与
// git 凭据。白名单而不是黑名单：黑名单永远追不上"下一个会泄密的变量名"。
func TestCheck_EnvIsMinimalAndCredentialFree(t *testing.T) {
	const secret = "sk-not-for-gates"
	if err := os.Setenv("XHUNTER_API_KEY", secret); err != nil {
		t.Fatalf("设置运行环境失败：%v", err)
	}
	defer os.Unsetenv("XHUNTER_API_KEY")
	if err := os.Setenv("GIT_ASKPASS", secret); err != nil {
		t.Fatalf("设置运行环境失败：%v", err)
	}
	defer os.Unsetenv("GIT_ASKPASS")

	res, err := NewRunner().Run(context.Background(), t.TempDir(), hunt.Gate{
		Name:   "env",
		Argv:   []string{"env"},
		Expect: hunt.Expect{Kind: hunt.ExpectRegex, Pattern: `^PATH=`},
	}, "fp")
	if err != nil {
		t.Fatalf("门禁执行失败：%v", err)
	}
	if !res.Passed {
		t.Fatalf("最小环境里必须有 PATH：%+v", res)
	}
	// 白名单之外的东西一律不下传：凭据泄进子进程是"换个地方泄"，不是没泄。
	if strings.Contains(res.Evidence, secret) {
		t.Errorf("子进程拿到了 Xhunter 的运行凭据：%s", res.Evidence)
	}
	if strings.Contains(res.Evidence, "GIT_ASKPASS") {
		t.Errorf("子进程拿到了 git 凭据通道：%s", res.Evidence)
	}
	// 固定项：非交互、无颜色、不求凭证提示——门禁必须能自己跑完。
	for _, want := range []string{"CI=1", "NO_COLOR=1", "GIT_TERMINAL_PROMPT=0"} {
		if !strings.Contains(res.Evidence, want) {
			t.Errorf("子进程环境缺固定项 %q：%s", want, res.Evidence)
		}
	}
}

// 数组直启：`argv[0]` 是 shell 一律拒绝。显式把 shell 请回来等于给了一条绕过
// "模型只给门禁名"的路——那正是门禁要堵死的东西。
func TestCheck_ArgvIsDirectAndRejectsShell(t *testing.T) {
	for _, argv0 := range []string{"sh", "bash", "/bin/sh", "/usr/bin/zsh"} {
		_, err := NewRunner().Run(context.Background(), t.TempDir(), hunt.Gate{
			Name:   "shell",
			Argv:   []string{argv0, "-c", "true"},
			Expect: hunt.Expect{Kind: hunt.ExpectExitZero},
		}, "fp")
		if err == nil {
			t.Errorf("%q 应当被拒：门禁不得把 shell 请回来", argv0)
			continue
		}
		if !strings.Contains(err.Error(), "shell") {
			t.Errorf("%q 的错误应说明是 shell 问题：%v", argv0, err)
		}
	}
}

// 超时是**环境问题**，不是质量结论：命令跑不完，我们并没有判过它。
// 杀的是整棵进程树，否则孤儿进程占着管道、Wait 永远不返回。
func TestCheck_TimeoutIsEnvError(t *testing.T) {
	start := time.Now()
	_, err := NewRunner().Run(context.Background(), t.TempDir(), hunt.Gate{
		Name:    "slow",
		Argv:    []string{"sleep", "30"},
		Timeout: 100 * time.Millisecond,
		Expect:  hunt.Expect{Kind: hunt.ExpectExitZero},
	}, "fp")
	if err == nil {
		t.Fatal("超时应报环境错误，不该给出结论")
	}
	if !strings.Contains(err.Error(), "超过") {
		t.Errorf("错误应说明是超时：%v", err)
	}
	// 超时必须真的生效：等到 30 秒才返回说明看门狗没起作用。
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("超时未生效：等待了 %s", elapsed)
	}
}

// 退出码非 0 是**质量结论**（判据按退出码算时不通过），不是环境错误——
// 混同的后果很具体：命令名写错会被读成"代码质量差"，任务反复失败却永远修不好。
func TestCheck_NonZeroExitIsQualityFailure(t *testing.T) {
	res, err := NewRunner().Run(context.Background(), t.TempDir(), hunt.Gate{
		Name:   "unit",
		Argv:   []string{"false"},
		Expect: hunt.Expect{Kind: hunt.ExpectExitZero},
	}, "fp")
	if err != nil {
		t.Fatalf("判定不通过不该上抛为错误：%v", err)
	}
	if res.Passed {
		t.Errorf("退出码非 0 且判据为 exit_zero，应判不通过：%+v", res)
	}
	if res.ExitCode == 0 {
		t.Errorf("应如实报出退出码：%+v", res)
	}
}

// 命令根本起不来是**环境错误**：那与代码质量无关，修好配置就能重跑。
func TestCheck_UnstartableCommandIsEnvError(t *testing.T) {
	_, err := NewRunner().Run(context.Background(), t.TempDir(), hunt.Gate{
		Name:   "ghost",
		Argv:   []string{"definitely-not-a-command-zzz"},
		Expect: hunt.Expect{Kind: hunt.ExpectExitZero},
	}, "fp")
	if err == nil {
		t.Fatal("起不来的命令应报环境错误")
	}
	if !strings.Contains(err.Error(), "无法启动") {
		t.Errorf("错误应说明命令起不来：%v", err)
	}
}

// 结果按「门禁名 + 改动指纹」缓存：改动没变，就没必要再跑一次几十秒的测试。
// 指纹变了必须重跑——否则改完代码拿到的是改动前的结论。
func TestCheck_CacheHitByFingerprint(t *testing.T) {
	runner := NewRunner()
	g := hunt.Gate{Name: "unit", Argv: []string{"true"}, Expect: hunt.Expect{Kind: hunt.ExpectExitZero}}

	first, err := runner.Run(context.Background(), t.TempDir(), g, "fp-a")
	if err != nil {
		t.Fatalf("首次执行失败：%v", err)
	}
	if first.Cached {
		t.Error("首次执行不该是缓存结果")
	}
	second, err := runner.Run(context.Background(), t.TempDir(), g, "fp-a")
	if err != nil {
		t.Fatalf("命中缓存的执行失败：%v", err)
	}
	if !second.Cached {
		t.Error("同一指纹应命中缓存")
	}
	if second.DurationMS != first.DurationMS || second.ExitCode != first.ExitCode {
		t.Errorf("缓存应给出同一份结论：%+v vs %+v", first, second)
	}
	third, err := runner.Run(context.Background(), t.TempDir(), g, "fp-b")
	if err != nil {
		t.Fatalf("指纹变化后的执行失败：%v", err)
	}
	if third.Cached {
		t.Error("改动指纹变了必须重跑：拿改动前的结论当改动后的结论是假绿灯")
	}
}

// 判据按清单声明的那一条算：同一份输出配不同判据就是不同的门禁。
func TestCheck_ExpectVariantsDecideTheVerdict(t *testing.T) {
	root := t.TempDir()
	// `echo` 留下一行输出：按"输出为空"判就是不通过，按"至少一行匹配"判就是通过。
	cases := []struct {
		name   string
		expect hunt.Expect
		passed bool
	}{
		{"空输出判据遇到非空输出", hunt.Expect{Kind: hunt.ExpectEmptyOutput}, false},
		{"匹配判据遇到匹配行", hunt.Expect{Kind: hunt.ExpectRegex, Pattern: `^boom$`}, true},
		{"匹配判据遇到不匹配输出", hunt.Expect{Kind: hunt.ExpectRegex, Pattern: `^never$`}, false},
		{"计数判据在上限内", hunt.Expect{Kind: hunt.ExpectMaxCount, Pattern: `^boom$`, Max: 1}, true},
	}
	for _, c := range cases {
		res, err := NewRunner().Run(context.Background(), root, hunt.Gate{
			Name: "echo", Argv: []string{"echo", "boom"}, Expect: c.expect,
		}, "fp")
		if err != nil {
			t.Errorf("%s：执行失败：%v", c.name, err)
			continue
		}
		if res.Passed != c.passed {
			t.Errorf("%s：期望 passed=%v，实际 %+v", c.name, c.passed, res)
		}
	}
}
