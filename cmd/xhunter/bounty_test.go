package main

import (
	"errors"
	"strings"
	"testing"

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

func TestSelectionFromEnv_Validate(t *testing.T) {
	if err := selectionFromEnv(fakeEnv(nil)).validate(); err == nil {
		t.Fatal("三要素全缺时必须报错")
	}
	sel := selectionFromEnv(fakeEnv(map[string]string{
		envProvider:     "anthropic",
		envModel:        "claude-test",
		envProviderFile: "/etc/xhunter/providers.json",
	}))
	if err := sel.validate(); err != nil {
		t.Fatalf("三要素齐备时不该报错：%v", err)
	}
	if sel.ProviderID != "anthropic" || sel.ModelID != "claude-test" || sel.ConfigPath != "/etc/xhunter/providers.json" {
		t.Errorf("选择 = %+v", sel)
	}

	// 缺一项也要报出缺的是哪一项（诊断价值就在这里）。
	partial := selectionFromEnv(fakeEnv(map[string]string{envProvider: "anthropic"}))
	err := partial.validate()
	if err == nil {
		t.Fatal("缺项必须报错")
	}
	for _, want := range []string{envModel, envProviderFile} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息 = %q，应指出缺少 %s", err, want)
		}
	}
}
