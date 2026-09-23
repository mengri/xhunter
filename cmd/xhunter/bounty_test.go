package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"xhunter/hunt"
)

// fakeEnv 让组装过程在测试里不依赖真实环境。
func fakeEnv(values map[string]string) lookupEnv {
	return func(name string) (string, bool) {
		v, ok := values[name]
		return v, ok
	}
}

func TestLoadTask_ReadsPlainText(t *testing.T) {
	task, err := loadTask("task.md", func(string) ([]byte, error) {
		return []byte("\n  给 README 加一段安装说明\n\n验收：命令能跑通。  \n"), nil
	})
	if err != nil {
		t.Fatalf("读取任务失败：%v", err)
	}
	if task != "给 README 加一段安装说明\n\n验收：命令能跑通。" {
		t.Errorf("任务正文 = %q，期望仅去掉首尾空白（正文是自然语言，不解析格式）", task)
	}
}

func TestLoadTask_RejectsMissingOrEmpty(t *testing.T) {
	if _, err := loadTask("nope.md", func(string) ([]byte, error) {
		return nil, errors.New("no such file")
	}); err == nil {
		t.Error("文件读不到必须报错")
	}
	if _, err := loadTask("empty.md", func(string) ([]byte, error) {
		return []byte("   \n\t"), nil
	}); err == nil {
		t.Error("空任务必须报错：没有任何要做的内容")
	}
}

func TestBountyFromEnv_RequiresRepoFacts(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"缺仓库地址", map[string]string{envRepoBase: "abc123"}},
		{"缺基线提交", map[string]string{envRepoURL: "git@example.com:x/y.git"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := bountyFromEnv("任务", fakeEnv(tc.env)); err == nil {
				t.Fatal("部署事实缺一项就必须在启动期失败")
			}
		})
	}
}

// 三重预算必须真的投递出去：没有它，策略的 Exhausted 永远为假，"预算耗尽立即终止"
// （FR-9、AC-5）就只是文档里的一句话。
func TestBountyFromEnv_DeliversBudget(t *testing.T) {
	base := map[string]string{
		envRepoURL: "git@example.com:x/y.git", envRepoBase: "0123456789abcdef",
	}
	t.Run("未设置即不限", func(t *testing.T) {
		b, err := bountyFromEnv("任务", fakeEnv(base))
		if err != nil {
			t.Fatalf("解析失败：%v", err)
		}
		if b.Budget != (hunt.Budget{}) {
			t.Errorf("未设置预算应保持零值（= 不限）：%+v", b.Budget)
		}
	})
	t.Run("三个维度都投递", func(t *testing.T) {
		env := map[string]string{envBudgetTurns: "7", envBudgetTokens: "123456", envBudgetWallClock: "90m"}
		for k, v := range base {
			env[k] = v
		}
		b, err := bountyFromEnv("任务", fakeEnv(env))
		if err != nil {
			t.Fatalf("解析失败：%v", err)
		}
		if b.Budget.MaxTurns != 7 || b.Budget.MaxTokens != 123456 || b.Budget.MaxWallClock != 90*time.Minute {
			t.Errorf("预算未正确投递：%+v", b.Budget)
		}
	})
	t.Run("写错即启动期失败", func(t *testing.T) {
		for name, bad := range map[string]string{
			envBudgetTurns:     "0",
			envBudgetTokens:    "-1",
			envBudgetWallClock: "半小时",
		} {
			env := map[string]string{name: bad}
			for k, v := range base {
				env[k] = v
			}
			if _, err := bountyFromEnv("任务", fakeEnv(env)); err == nil {
				t.Errorf("%s=%q 必须报错，不能静默当成不限", name, bad)
			} else if !strings.Contains(err.Error(), name) {
				t.Errorf("错误信息应指明是哪个变量：%v", err)
			}
		}
	})
}

func TestBountyFromEnv_DerivesBranchAndID(t *testing.T) {
	const base = "0123456789abcdef0123456789abcdef01234567"
	b, err := bountyFromEnv("任务正文", fakeEnv(map[string]string{
		envRepoURL:  "git@example.com:x/y.git",
		envRepoBase: base,
	}))
	if err != nil {
		t.Fatalf("组装失败：%v", err)
	}
	if b.Task != "任务正文" {
		t.Errorf("任务 = %q", b.Task)
	}
	if b.Repo.Remote != "git@example.com:x/y.git" || b.Repo.BaseCommit != base {
		t.Errorf("仓库事实 = %+v", b.Repo)
	}
	// 缺省标识取自基线前 12 位，分支随之生成：可复现，且任务与分支能对上。
	if string(b.ID) != "0123456789ab" {
		t.Errorf("缺省任务标识 = %q，期望基线前 12 位", b.ID)
	}
	if b.Repo.Branch != "xhunter/0123456789ab" {
		t.Errorf("缺省分支 = %q", b.Repo.Branch)
	}
	if b.Session != nil {
		t.Error("没有续跑标识时应当是新任务")
	}
}

func TestBountyFromEnv_HonoursExplicitFacts(t *testing.T) {
	b, err := bountyFromEnv("任务", fakeEnv(map[string]string{
		envRepoURL:    "git@example.com:x/y.git",
		envRepoBase:   "deadbeef",
		envBountyID:   "B-42",
		envRepoBranch: "feat/custom",
	}))
	if err != nil {
		t.Fatalf("组装失败：%v", err)
	}
	if b.ID != hunt.BountyID("B-42") || b.Repo.Branch != "feat/custom" {
		t.Errorf("显式给出的事实被忽略了：%+v", b)
	}
	if b.Session != nil {
		t.Error("没有给出会话标识时应当是全新会话")
	}
	if SessionID(b) != "B-42" {
		t.Errorf("会话标识 = %q，期望等于本任务的 id", SessionID(b))
	}
}

// 同一个会话标识下的多次投递共享记忆与分支：换一个 bounty_id，接的是同一份工作与历史。
func TestBountyFromEnv_SessionIsSharedAcrossBounties(t *testing.T) {
	b, err := bountyFromEnv("接着改", fakeEnv(map[string]string{
		envRepoURL:   "git@example.com:x/y.git",
		envRepoBase:  "deadbeef",
		envBountyID:  "B-43",
		envSessionID: "S-7",
	}))
	if err != nil {
		t.Fatalf("组装失败：%v", err)
	}
	// 本次投递有自己的标识：事件流与结果文件按它记账。
	if b.ID != hunt.BountyID("B-43") {
		t.Errorf("任务标识 = %q，期望本次投递自己的 id", b.ID)
	}
	// 会话标识取显式给出的值：分支与记忆都落在它那一份上。
	if SessionID(b) != "S-7" {
		t.Errorf("会话标识 = %q，期望取 XHUNTER_SESSION_ID", SessionID(b))
	}
	if b.Session == nil || b.Session.ID != "S-7" {
		t.Errorf("会话引用 = %+v", b.Session)
	}
	if b.Repo.Branch != "xhunter/S-7" {
		t.Errorf("分支 = %q，期望以会话标识为准（同一会话的多次投递接在同一条分支上）", b.Repo.Branch)
	}
}

// 不传会话标识时，本次投递自成一次新会话：会话标识即本任务的 id。
func TestBountyFromEnv_WithoutSessionStartsItsOwn(t *testing.T) {
	b, err := bountyFromEnv("新任务", fakeEnv(map[string]string{
		envRepoURL:  "git@example.com:x/y.git",
		envRepoBase: "deadbeef",
		envBountyID: "B-9",
	}))
	if err != nil {
		t.Fatalf("组装失败：%v", err)
	}
	if b.Session != nil {
		t.Error("没有会话标识时不该带会话引用：这是一次全新会话")
	}
	if SessionID(b) != "B-9" || b.Repo.Branch != "xhunter/B-9" {
		t.Errorf("会话标识/分支 = %q/%q，期望都取本任务的 id", SessionID(b), b.Repo.Branch)
	}
}

func TestBountyFromEnv_ExplicitBranchWins(t *testing.T) {
	b, err := bountyFromEnv("任务", fakeEnv(map[string]string{
		envRepoURL:    "git@example.com:x/y.git",
		envRepoBase:   "deadbeef",
		envBountyID:   "B-43",
		envSessionID:  "S-7",
		envRepoBranch: "feat/pinned",
	}))
	if err != nil {
		t.Fatalf("组装失败：%v", err)
	}
	if b.Repo.Branch != "feat/pinned" {
		t.Errorf("分支 = %q，显式给出的分支优先", b.Repo.Branch)
	}
	if SessionID(b) != "S-7" {
		t.Errorf("会话标识 = %q，仍以会话标识为准", SessionID(b))
	}
}

// 追踪标识（FR-11.3）：投递给了就用它，没给则回填 bounty id——
// 事件流靠它串回平台上的那一次投递，不能为空。
func TestBountyFromEnv_TraceID(t *testing.T) {
	base := map[string]string{envRepoURL: "git@example.com:x/y.git", envRepoBase: "0123456789abcdef"}
	t.Run("显式给出", func(t *testing.T) {
		env := map[string]string{envTraceID: "trace-abc"}
		for k, v := range base {
			env[k] = v
		}
		b, err := bountyFromEnv("任务", fakeEnv(env))
		if err != nil {
			t.Fatalf("解析失败：%v", err)
		}
		if b.TraceID != "trace-abc" {
			t.Errorf("TraceID = %q，期望 trace-abc", b.TraceID)
		}
	})
	t.Run("缺省回填 bounty id", func(t *testing.T) {
		env := map[string]string{envBountyID: "b-7"}
		for k, v := range base {
			env[k] = v
		}
		b, err := bountyFromEnv("任务", fakeEnv(env))
		if err != nil {
			t.Fatalf("解析失败：%v", err)
		}
		if b.TraceID != "b-7" {
			t.Errorf("TraceID 应回填为 bounty id，实际 %q", b.TraceID)
		}
	})
}
