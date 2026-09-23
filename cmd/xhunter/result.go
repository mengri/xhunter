package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"xhunter/harness"
	"xhunter/hunt"
)

// 结果文件与补丁是"交付记录"这一侧的产物：主交付是远端那条任务分支（FR-6.1），
// 这里是附带的机器可读结论，供平台记账与评审（FR-1.5、使用手册 §6）。
//
// 只写当前真能给出的事实：`gates` / `session_delta` 尚未落地，就不在这里
// 摆空壳——空数组会被读成"没有门禁、没有假设"，那是另一句话。字段状态以使用手册 §6 的
// 标注为准。

// resultFile 是结果文件的形状（使用手册 §6）。
type resultFile struct {
	BountyID     string   `json:"bounty_id"`
	SessionID    string   `json:"session_id,omitempty"`
	Status       string   `json:"status"`
	Reason       string   `json:"reason,omitempty"`
	ExitCode     int      `json:"exit_code"`
	BaseCommit   string   `json:"base_commit"`
	Branch       string   `json:"branch"`
	CommitSHA    string   `json:"commit_sha,omitempty"`
	PatchPath    string   `json:"patch_path,omitempty"`
	FilesChanged []string `json:"files_changed"`

	// Summary 是模型最后一轮的答复正文——它也是交付物的一部分（任务可能就是要产出一份
	// 小结）。没有答复就没有这个键（omitempty），不摆空壳。
	Summary string `json:"summary,omitempty"`

	// Gates 是门禁结果（FR-5.2、使用手册 §6）。**未接线**：写入端（MS-5）接上之前不写该键
	// （omitempty），不摆空壳。形状见 gateFile（`passed` 三态：true / false / null=未运行）。
	Gates []gateFile `json:"gates,omitempty"`

	// Needs / Assumptions / Unverified 是模型在正文固定小节里的自陈（FR-6.3/6.4）。用指针不加 omitempty
	// 是三态要求：「没提供」要写成 null，而不是缺字段、更不是 []——空数组会被读成
	// "没有需要补全的条件"，那是另一句话（使用手册 §6）。
	Needs       *[]string        `json:"needs"`
	Assumptions *[]string        `json:"assumptions"`
	Unverified  *[]string        `json:"unverified"`
	Usage       hunt.UsageReport `json:"usage"`
	Error       *errorFile       `json:"error,omitempty"`

	// EffectiveConfig 是本次 Hunt 的生效配置快照（FR-11.6）：只读事实，回答"这次用的是
	// 哪套规则"，服务远程诊断与 MR 评审。Prepare 未成功时没有快照，指针为 nil、字段
	// 省略——不摆空壳（空快照会被读成"没有原语、没有插件"）。
	EffectiveConfig *hunt.EffectiveConfig `json:"effective_config,omitempty"`
}

// errorFile 只描述"任务为什么没成"：kind 供程序分支，message 给人看，
// retryable 与退出码同源（环境问题才可重试）。
type errorFile struct {
	Kind      string `json:"kind"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// gateFile 是结果文件里一条门禁的形状（FR-5.2、使用手册 §6）。
//
// `passed` 用**三态**（`*bool`）：`true` 通过、`false` 未通过、`null` **未运行**。§6 要求结果文件
// **必须列出未运行的门禁**（`passed: null`）——用一个 `bool` 会把"没跑"混成"没通过"，那是两句话。
// 字段语义与 §7.8 一致；**本期只定义形状**，写入端在 MS-5 接上。
type gateFile struct {
	Name     string `json:"name"`
	Passed   *bool  `json:"passed"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Source   string `json:"source,omitempty"`
	Summary  string `json:"summary,omitempty"`
}

// writeRunOutputs 写出补丁与结果文件。任一写失败都返回错误——交不出交付记录
// 属环境问题（退出码 1），不得静默继续（对齐 FR-10.4 的"通道断裂即终止"精神）。
//
// usage 与 summary 都取自 `Delivery`（收尾定型的交付事实），与终态事件读**同一份**：
// 结果文件与 `hunt_end` 因此不可能对同一件事给出两个数。effective 是生效配置快照
// （Prepare 成功才有内容，否则零值、字段省略）。
func writeRunOutputs(resultPath, patchPath string, bounty hunt.Bounty, out harness.Outcome, d hunt.Delivery, declared hunt.Declared, effective hunt.EffectiveConfig) error {
	if patchPath != "" {
		if err := writeFile(patchPath, []byte(d.Patch)); err != nil {
			return fmt.Errorf("写补丁文件失败：%w", err)
		}
	}
	if resultPath == "" {
		return nil
	}

	r := resultFile{
		BountyID:     string(bounty.ID),
		SessionID:    SessionID(bounty),
		Status:       string(out.Status),
		Reason:       out.Reason,
		ExitCode:     int(out.ExitCode),
		BaseCommit:   bounty.Repo.BaseCommit,
		Branch:       bounty.Repo.Branch,
		FilesChanged: d.Files,
		Summary:      d.Summary,
		Needs:        optionalList(declared.Needs),
		Assumptions:  optionalList(declared.Assumptions),
		Unverified:   optionalList(declared.Unverified),
		Usage:        d.Usage,
	}
	if r.FilesChanged == nil {
		r.FilesChanged = []string{}
	}
	if d.Commit != nil {
		r.CommitSHA = d.Commit.SHA
	}
	if patchPath != "" {
		r.PatchPath = patchPath
	}
	// 有快照才写：Primitives 非空即表示"进了对话、装配已完成"；否则交给 omitempty 省略。
	if len(effective.Primitives) > 0 {
		r.EffectiveConfig = &effective
	}
	if out.Status == harness.StatusFailed {
		r.Error = &errorFile{
			Kind:      hunt.ErrorKind(out.Reason),
			Message:   out.Reason,
			Retryable: hunt.RetryableForExitCode(out.ExitCode),
		}
	}

	buf, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化结果文件失败：%w", err)
	}
	buf = append(buf, '\n')
	if err := writeFile(resultPath, buf); err != nil {
		return fmt.Errorf("写结果文件失败：%w", err)
	}
	return nil
}

// optionalList 把空切片收成 nil：三态里 nil 才是「没提供」。
//
// 解析器已经保证「小节不存在 → nil」，这里再把「小节在、但一条都没有」也归到没提供——
// 一个空的「## 需要补全」不构成"缺条件"，不该让平台看到一份空清单。
func optionalList(items []string) *[]string {
	if len(items) == 0 {
		return nil
	}
	return &items
}

// writeFile 写出文件；父目录不存在时创建（平台给的路径可能指向尚未建好的目录）。
func writeFile(path string, data []byte) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, data, 0o644)
}
