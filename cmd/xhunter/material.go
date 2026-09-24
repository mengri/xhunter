package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"

	"xhunter/harness"
	"xhunter/hunt"
	"xhunter/llm"
)

// 会话材料：随检查点进分支的文件，是崩溃后恢复的**唯一状态源**（使用手册 §3、架构 §7.6）。
// 形状是 JSONL：一行一条记录，**第一条是 meta**（版本与任务事实），其后是 turn / op / usage。

// materialSchemaVersion 是材料格式版本。读取端只认它：不认识就报错（不自动迁移、不猜）。
const materialSchemaVersion = 1

// 材料在工作区里的位置（相对工作区根）：`.xhunter/<session_id>/session.jsonl`。
const (
	materialDir  = ".xhunter"
	materialFile = "session.jsonl"
)

// materialDirFor 给出某个会话的材料目录（工作区相对路径）。它是这一路径的**唯一来源**：落盘与
// 交付排除（`RepoRef.MaterialDir`）都调它，不两处各拼一遍。
func materialDirFor(sessionID string) string {
	return path.Join(materialDir, sessionID)
}

// materialMeta 是材料第一行：版本与任务事实（恢复时据此对齐分支与基线）。
type materialMeta struct {
	Type          string   `json:"type"`
	SchemaVersion int      `json:"schema_version"`
	SessionID     string   `json:"session_id"`
	BountyID      string   `json:"bounty_id"`
	Branch        string   `json:"branch"`
	BaseCommit    string   `json:"base_commit"`
	Ext           []string `json:"ext"` // 扩展能力指纹位：本期空数组（不是 null），MS-8 填充
}

// materialTurn 是一轮的记录：直接复用 harness.Turn 的形状（不写平行 DTO）。
type materialTurn struct {
	Type string `json:"type"`
	harness.Turn
}

// materialOp 是一次写操作的记录：直接复用 hunt.WriteOp 的形状。
type materialOp struct {
	Type string `json:"type"`
	hunt.WriteOp
}

// materialUsage 是某轮的用量增量（三项）。
type materialUsage struct {
	Type   string `json:"type"`
	Input  int    `json:"input_tokens"`
	Output int    `json:"output_tokens"`
	Cached int    `json:"cached_input_tokens"`
}

// sessionRecorder 把会话材料落成 JSONL，供崩溃后按「tip + 材料」恢复。
//
// 落盘是**尽力而为**：写失败只由调用方记录、不阻断任务（材料丢了最多是崩溃后从头跑，IA-6.6）。
// 写入方式是追加：`Snapshot` 只把尚未落盘的记录 flush 出去，不每轮重写整个文件。
type sessionRecorder struct {
	bounty hunt.Bounty

	file    *os.File
	pending []any
	ops     []hunt.WriteOp
}

// Open 绑定材料位置并写好 meta 行：工作区根由 PrepareBaseline 在运行期给出，因此在工作区就绪后
// 调用一次。同一个会话的材料已存在时不重复写 meta（续跑，MS-7）。
//
// 注意顺序：本方法会为**不存在**的材料建目录并写 meta，因此它必须在 `Load`（纯读）之后调用——
// 否则先建出来一份空材料，就再也分不清"上次留下的材料"与"刚刚为本趟建的空材料"。
func (r *sessionRecorder) Open(root string) error {
	dir := filepath.Join(root, materialDirFor(r.bounty.SessionID()))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, materialFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	r.file = f

	// 只在空材料上写 meta：续跑时材料已存在，不该再插一条。
	if st, err := f.Stat(); err == nil && st.Size() == 0 {
		if err := r.writeNow(materialMeta{
			Type:          "meta",
			SchemaVersion: materialSchemaVersion,
			SessionID:     r.bounty.SessionID(),
			BountyID:      string(r.bounty.ID),
			Branch:        r.bounty.Repo.Branch,
			BaseCommit:    r.bounty.Repo.BaseCommit,
			Ext:           []string{}, // 本期没有扩展：空数组而非 null
		}); err != nil {
			return err
		}
	}
	return nil
}

func (r *sessionRecorder) RecordTurn(rec harness.Turn) {
	r.pending = append(r.pending, materialTurn{Type: "turn", Turn: rec})
}

func (r *sessionRecorder) RecordOp(op hunt.WriteOp) {
	r.ops = append(r.ops, op)
	r.pending = append(r.pending, materialOp{Type: "op", WriteOp: op})
}

func (r *sessionRecorder) RecordUsage(u llm.Usage) {
	r.pending = append(r.pending, materialUsage{
		Type: "usage", Input: u.InputTokens, Output: u.OutputTokens, Cached: u.CachedInputTokens,
	})
}

// Ops 给出**本次运行**累积的写操作序列（恢复的唯一刚需）。与 `Load` 的分工：本次 vs 上次。
func (r *sessionRecorder) Ops() []hunt.WriteOp { return r.ops }

// Load 读回**上次运行**的材料（resume 的唯一状态源）。
//
// 它是**纯读**：不建目录、不写 meta，因此必须在 `Open` 之前调用——`Open` 会为不存在的材料
// 写 meta，先 Open 就分不清"上次留下的"与"刚刚为本趟建的"。
//
// 材料不存在 → 零值 + nil（新任务，不是错误）；存在但读不出来才是错误：坏行、首行不是 meta、
// `schema_version` 不认识、未知记录类型——一律报错、**不自动迁移、不猜**。
// 三类记录按行序解析：turn → `Restored.Turns`，op → `Restored.Ops`，usage → 按记录**累加**
// 成 `Restored.Usage`（续算预算用）。
func (r *sessionRecorder) Load(root string) (hunt.Restored, error) {
	p := filepath.Join(root, materialDirFor(r.bounty.SessionID()), materialFile)
	if _, err := os.Stat(p); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return hunt.Restored{}, nil
		}
		return hunt.Restored{}, err
	}
	version, records, err := loadMaterial(p)
	if err != nil {
		return hunt.Restored{}, err
	}
	out := hunt.Restored{SchemaVersion: version}
	for _, raw := range records {
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			return hunt.Restored{}, fmt.Errorf("材料记录不是合法 JSON：%w", err)
		}
		switch probe.Type {
		case "turn":
			var rec materialTurn
			if err := json.Unmarshal(raw, &rec); err != nil {
				return hunt.Restored{}, fmt.Errorf("材料 turn 记录解析失败：%w", err)
			}
			out.Turns = append(out.Turns, rec.Turn)
		case "op":
			var rec materialOp
			if err := json.Unmarshal(raw, &rec); err != nil {
				return hunt.Restored{}, fmt.Errorf("材料 op 记录解析失败：%w", err)
			}
			out.Ops = append(out.Ops, rec.WriteOp)
		case "usage":
			var rec materialUsage
			if err := json.Unmarshal(raw, &rec); err != nil {
				return hunt.Restored{}, fmt.Errorf("材料 usage 记录解析失败：%w", err)
			}
			out.Usage.InputTokens += rec.Input
			out.Usage.OutputTokens += rec.Output
			out.Usage.CachedInputTokens += rec.Cached
		default:
			return hunt.Restored{}, fmt.Errorf("材料含未知记录类型 %q（不猜、不静默跳过）", probe.Type)
		}
	}
	return out, nil
}

// Snapshot 把尚未落盘的记录 flush 出去（每轮与收尾各一次，这就是"周期落盘"）。
func (r *sessionRecorder) Snapshot() error {
	if r.file == nil || len(r.pending) == 0 {
		return nil
	}
	var buf bytes.Buffer
	for _, rec := range r.pending {
		b, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if _, err := r.file.Write(buf.Bytes()); err != nil {
		return err
	}
	if err := r.file.Sync(); err != nil {
		return err
	}
	r.pending = r.pending[:0]
	return nil
}

// writeNow 立即写一条记录（用于 meta：材料一绑定就该有头）。
func (r *sessionRecorder) writeNow(rec any) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if _, err := r.file.Write(b); err != nil {
		return err
	}
	return r.file.Sync()
}

// loadMaterial 读回材料并按 meta 校验 schema_version：版本不认识就报错（不自动迁移、不猜）。
// 它把首行 meta 的 `schema_version` 连同其余记录行一并交出——调用方据此判定"材料是否存在且
// 合法"，这正是"本次是否恢复"的唯一判据。
func loadMaterial(path string) (int, []json.RawMessage, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // 单行可能很大（整轮消息）
	var out []json.RawMessage
	version := 0
	first := true
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if first {
			var meta struct {
				Type          string `json:"type"`
				SchemaVersion int    `json:"schema_version"`
			}
			if err := json.Unmarshal(line, &meta); err != nil {
				return 0, nil, fmt.Errorf("材料首行不是合法 meta：%w", err)
			}
			if meta.Type != "meta" {
				return 0, nil, fmt.Errorf("材料首行 type = %q，期望 meta", meta.Type)
			}
			if meta.SchemaVersion != materialSchemaVersion {
				return 0, nil, fmt.Errorf("材料 schema_version = %d，本程序只认 %d（不自动迁移）", meta.SchemaVersion, materialSchemaVersion)
			}
			version = meta.SchemaVersion
			first = false
			continue
		}
		out = append(out, append(json.RawMessage(nil), line...))
	}
	if err := sc.Err(); err != nil {
		return 0, nil, err
	}
	if first {
		return 0, nil, fmt.Errorf("材料 %s 为空：缺 meta 行", path)
	}
	return version, out, nil
}
