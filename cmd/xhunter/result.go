package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"xhunter/harness"
	"xhunter/hunt"
)

// 结果文件与补丁是"交付记录"这一侧的产物：主交付是远端那条任务分支（FR-6.1），
// 这里是附带的机器可读结论，供平台记账与评审（FR-1.5、使用手册 §6）。
//
// 只写当前真能给出的事实：`assumptions` / `unverified` / `gates` / `effective_config`
// / `session_delta` 尚未落地，就不在这里摆空壳——空数组会被读成"没有门禁、没有假设"，
// 那是另一句话。字段状态以使用手册 §6 的标注为准。

// resultFile 是结果文件的形状（使用手册 §6）。
type resultFile struct {
	BountyID     string     `json:"bounty_id"`
	SessionID    string     `json:"session_id,omitempty"`
	Status       string     `json:"status"`
	Reason       string     `json:"reason,omitempty"`
	ExitCode     int        `json:"exit_code"`
	BaseCommit   string     `json:"base_commit"`
	Branch       string     `json:"branch"`
	CommitSHA    string     `json:"commit_sha,omitempty"`
	PatchPath    string     `json:"patch_path,omitempty"`
	FilesChanged []string   `json:"files_changed"`
	Usage        usageFile  `json:"usage"`
	Error        *errorFile `json:"error,omitempty"`
}

type usageFile struct {
	InputTokens       int   `json:"input_tokens"`
	OutputTokens      int   `json:"output_tokens"`
	CachedInputTokens int   `json:"cached_input_tokens"`
	Turns             int   `json:"turns"`
	ElapsedMS         int64 `json:"elapsed_ms"`
}

// errorFile 只描述"任务为什么没成"：kind 供程序分支，message 给人看，
// retryable 与退出码同源（环境问题才可重试）。
type errorFile struct {
	Kind      string `json:"kind"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// writeRunOutputs 写出补丁与结果文件。任一写失败都返回错误——交不出交付记录
// 属环境问题（退出码 2），不得静默继续（对齐 FR-10.4 的"通道断裂即终止"精神）。
func writeRunOutputs(resultPath, patchPath string, bounty hunt.Bounty, out harness.Outcome, d hunt.Delivery) error {
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
		Usage: usageFile{
			InputTokens:       out.Usage.InputTokens,
			OutputTokens:      out.Usage.OutputTokens,
			CachedInputTokens: out.Usage.CachedInputTokens,
			Turns:             out.Usage.Turns,
			ElapsedMS:         out.Usage.Elapsed.Milliseconds(),
		},
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
	if out.Status == harness.StatusFailed {
		r.Error = &errorFile{
			Kind:      errorKind(out.Reason),
			Message:   out.Reason,
			Retryable: out.ExitCode == harness.ExitEnv,
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

// errorKind 取原因的第一个冒号之前那段（`prepare_failed: …` → `prepare_failed`），
// 它稳定、可供程序分支；完整原因留在 message 里。
func errorKind(reason string) string {
	if i := strings.IndexByte(reason, ':'); i > 0 {
		return strings.TrimSpace(reason[:i])
	}
	if reason == "" {
		return "unknown"
	}
	return reason
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
