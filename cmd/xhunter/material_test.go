package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"xhunter/git"
	"xhunter/harness"
	"xhunter/hunt"
	"xhunter/llm"
)

// 材料真的落在 `.xhunter/<session_id>/session.jsonl`，首行 meta 带 schema_version；不同 session 互不覆盖。
func TestSave_MaterialLandsUnderSessionDirWithSchemaVersion(t *testing.T) {
	root := t.TempDir()
	rec := &sessionRecorder{bounty: hunt.Bounty{
		ID: "b-1", Repo: git.RepoRef{Branch: "xhunter/s-1", BaseCommit: "base1"},
		Session: &hunt.SessionRef{ID: "s-1"},
	}}
	if err := rec.Open(root); err != nil {
		t.Fatalf("Open 失败：%v", err)
	}
	rec.RecordOp(hunt.WriteOp{Primitive: "write", File: "a.txt", Turn: 1})
	rec.RecordUsage(llm.Usage{InputTokens: 10, OutputTokens: 4, CachedInputTokens: 2})
	rec.RecordTurn(harness.Turn{No: 1, Text: "ok"})
	if err := rec.Snapshot(); err != nil {
		t.Fatalf("Snapshot 失败：%v", err)
	}

	path1 := filepath.Join(root, ".xhunter", "s-1", "session.jsonl")
	raw1, err := os.ReadFile(path1)
	if err != nil {
		t.Fatalf("材料未落在预期路径 %s：%v", path1, err)
	}
	lines := nonEmptyLines(string(raw1))
	if len(lines) < 4 {
		t.Fatalf("材料应含 meta ＋ turn/op/usage：\n%s", raw1)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &meta); err != nil {
		t.Fatalf("首行不是合法 JSON：%v", err)
	}
	if meta["type"] != "meta" {
		t.Errorf("首行 type = %v，期望 meta", meta["type"])
	}
	if meta["schema_version"] != float64(1) {
		t.Errorf("schema_version = %v，期望 1", meta["schema_version"])
	}
	if ext, ok := meta["ext"].([]any); !ok || len(ext) != 0 {
		t.Errorf("ext 应是空数组而非 null：%v", meta["ext"])
	}

	// 同一工作区里的另一个 session 各落各的目录，互不覆盖。
	rec2 := &sessionRecorder{bounty: hunt.Bounty{
		ID: "b-2", Repo: git.RepoRef{Branch: "xhunter/s-2", BaseCommit: "base2"},
		Session: &hunt.SessionRef{ID: "s-2"},
	}}
	if err := rec2.Open(root); err != nil {
		t.Fatalf("Open 失败：%v", err)
	}
	rec2.RecordTurn(harness.Turn{No: 1, Text: "b2"})
	if err := rec2.Snapshot(); err != nil {
		t.Fatalf("Snapshot 失败：%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".xhunter", "s-2", "session.jsonl")); err != nil {
		t.Errorf("第二个 session 的材料未落到自己的目录：%v", err)
	}
	raw1b, err := os.ReadFile(path1)
	if err != nil || string(raw1b) != string(raw1) {
		t.Error("第二个 session 的写入影响了第一个 session 的材料")
	}
}

// 材料版本不认识 → 报错（不自动迁移、不猜）。
func TestMaterial_LoadRejectsUnknownSchemaVersion(t *testing.T) {
	dir := t.TempDir()

	good := filepath.Join(dir, "good.jsonl")
	if err := os.WriteFile(good, []byte(`{"type":"meta","schema_version":1}`+"\n"+`{"type":"turn","no":1}`+"\n"), 0o644); err != nil {
		t.Fatalf("写夹具失败：%v", err)
	}
	if _, err := loadMaterial(good); err != nil {
		t.Errorf("认得的版本不该报错：%v", err)
	}

	bad := filepath.Join(dir, "bad.jsonl")
	if err := os.WriteFile(bad, []byte(`{"type":"meta","schema_version":999}`+"\n"), 0o644); err != nil {
		t.Fatalf("写夹具失败：%v", err)
	}
	if _, err := loadMaterial(bad); err == nil {
		t.Error("不认识的 schema_version 必须报错（不自动迁移）")
	}
}
