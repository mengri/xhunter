package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// 符号能力指纹进会话材料的诊断位（FR-13.7 / IA-6.5）：它回答的是"续跑后的精度为什么与上次
// 不同"——那是诊断时最难查的一类事。**没有**符号能力时是空数组：那是已知事实，null 会被读成
// "不知道有没有"。
func TestMaterial_MetaCarriesTheExtFingerprint(t *testing.T) {
	root := t.TempDir()
	rec := &sessionRecorder{bounty: hunt.Bounty{
		ID: "b-1", Repo: git.RepoRef{Branch: "xhunter/s-1", BaseCommit: "base1"},
		Session: &hunt.SessionRef{ID: "s-1"},
	}, ext: []string{"ext:gopls", "lang:go", "precision:semantic"}}
	if err := rec.Open(root); err != nil {
		t.Fatalf("Open 失败：%v", err)
	}
	got, err := rec.Load(root)
	if err != nil {
		t.Fatalf("Load 失败：%v", err)
	}
	if strings.Join(got.Ext, " ") != "ext:gopls lang:go precision:semantic" {
		t.Fatalf("读回的能力指纹 = %v，期望写进去的那一份", got.Ext)
	}

	// 没有符号能力 = 已知事实，落盘为 [] 而不是 null。
	bare := &sessionRecorder{bounty: rec.bounty}
	root2 := t.TempDir()
	if err := bare.Open(root2); err != nil {
		t.Fatalf("Open 失败：%v", err)
	}
	raw, err := os.ReadFile(filepath.Join(root2, materialDirFor("s-1"), materialFile))
	if err != nil {
		t.Fatalf("材料不可读：%v", err)
	}
	if !strings.Contains(string(raw), `"ext":[]`) {
		t.Errorf("没有符号能力时 meta.ext 应是空数组，实际：%s", raw)
	}
}

// 材料版本不认识 → 报错（不自动迁移、不猜）。
func TestMaterial_LoadRejectsUnknownSchemaVersion(t *testing.T) {
	dir := t.TempDir()

	good := filepath.Join(dir, "good.jsonl")
	if err := os.WriteFile(good, []byte(`{"type":"meta","schema_version":1}`+"\n"+`{"type":"turn","no":1}`+"\n"), 0o644); err != nil {
		t.Fatalf("写夹具失败：%v", err)
	}
	if head, _, err := loadMaterial(good); err != nil {
		t.Errorf("认得的版本不该报错：%v", err)
	} else if head.Version != 1 {
		t.Errorf("loadMaterial 应交出 meta 的 schema_version，实得 %d", head.Version)
	}

	bad := filepath.Join(dir, "bad.jsonl")
	if err := os.WriteFile(bad, []byte(`{"type":"meta","schema_version":999}`+"\n"), 0o644); err != nil {
		t.Fatalf("写夹具失败：%v", err)
	}
	if _, _, err := loadMaterial(bad); err == nil {
		t.Error("不认识的 schema_version 必须报错（不自动迁移）")
	}
}

// 材料不存在 → 零值 + nil 错误（"不存在"与"损坏"分开）。**不得**顺手把材料建出来——
// 读材料是纯读，建目录/写 meta 是 `Open` 的活；先建出来就再也分不清"上次留下的"与"刚建的"。
func TestLoad_MissingMaterialIsZeroValueNotAnError(t *testing.T) {
	root := t.TempDir()
	rec := &sessionRecorder{bounty: hunt.Bounty{Session: &hunt.SessionRef{ID: "s-miss"}}}

	got, err := rec.Load(root)
	if err != nil {
		t.Fatalf("材料不存在不是错误，实得：%v", err)
	}
	if got.SchemaVersion != 0 {
		t.Errorf("SchemaVersion = %d，期望 0（零值）", got.SchemaVersion)
	}
	if len(got.Turns) != 0 || len(got.Ops) != 0 {
		t.Errorf("零值 Restored 不该带轮次/写操作：%+v", got)
	}
	if got.Usage != (llm.Usage{}) {
		t.Errorf("零值 Restored 不该带用量：%+v", got.Usage)
	}
	// 纯读：Load 之后材料路径仍不存在。
	p := filepath.Join(root, materialDirFor("s-miss"), materialFile)
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("Load 不该创建材料文件（纯读），却见到：%v", err)
	}
}

// 读回按行序解析三类记录：turn / op / usage；usage 按记录累加。
func TestLoad_ReadsTurnsOpsAndUsageInOrder(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, materialDirFor("s-read"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	body := strings.Join([]string{
		`{"type":"meta","schema_version":1,"session_id":"s-read"}`,
		`{"type":"turn","no":1,"text":"第一轮"}`,
		`{"type":"op","tool":"write","file":"a.txt","range":{"start":0,"end":0},"before":"","after":"x","turn":1}`,
		`{"type":"usage","input_tokens":10,"output_tokens":4,"cached_input_tokens":2}`,
		`{"type":"turn","no":2,"text":"第二轮"}`,
		`{"type":"usage","input_tokens":5,"output_tokens":2,"cached_input_tokens":1}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, materialFile), []byte(body), 0o644); err != nil {
		t.Fatalf("写材料失败：%v", err)
	}

	got, err := (&sessionRecorder{bounty: hunt.Bounty{Session: &hunt.SessionRef{ID: "s-read"}}}).Load(root)
	if err != nil {
		t.Fatalf("Load 失败：%v", err)
	}
	if got.SchemaVersion != 1 {
		t.Errorf("SchemaVersion = %d，期望 1", got.SchemaVersion)
	}
	if len(got.Turns) != 2 || got.Turns[0].Text != "第一轮" || got.Turns[1].Text != "第二轮" {
		t.Errorf("轮次应按行序读回两条：%+v", got.Turns)
	}
	if len(got.Ops) != 1 || got.Ops[0].File != "a.txt" || got.Ops[0].Primitive != "write" {
		t.Errorf("写操作序列不对：%+v", got.Ops)
	}
	// 用量按记录累加：10+5 / 4+2 / 2+1（字面量）。
	if got.Usage != (llm.Usage{InputTokens: 15, OutputTokens: 6, CachedInputTokens: 3}) {
		t.Errorf("usage 累加值 = %+v，期望 {15 6 3}", got.Usage)
	}
}

// 未知记录类型是错误（不猜、不静默跳过）：模型看不到它，但读回端不该把一份"未来格式"当成
// 已知材料混过去。
func TestLoad_UnknownRecordTypeIsAnError(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, materialDirFor("s-unknown"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	body := `{"type":"meta","schema_version":1}` + "\n" + `{"type":"future_record","x":1}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, materialFile), []byte(body), 0o644); err != nil {
		t.Fatalf("写材料失败：%v", err)
	}
	if _, err := (&sessionRecorder{bounty: hunt.Bounty{Session: &hunt.SessionRef{ID: "s-unknown"}}}).Load(root); err == nil {
		t.Error("未知记录类型必须报错（不猜、不静默跳过）")
	}
}

// 材料只有 meta 行（零记录）也算"存在且合法"：Load 返回 SchemaVersion == 1、err == nil
// （"存在即恢复"这条判据的退化情形，在正常路径上不可达）。
func TestLoad_MetaOnlyMaterialIsNotAnError(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, materialDirFor("s-meta"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, materialFile), []byte(`{"type":"meta","schema_version":1}`+"\n"), 0o644); err != nil {
		t.Fatalf("写材料失败：%v", err)
	}
	got, err := (&sessionRecorder{bounty: hunt.Bounty{Session: &hunt.SessionRef{ID: "s-meta"}}}).Load(root)
	if err != nil {
		t.Fatalf("只有 meta 的材料是合法材料：%v", err)
	}
	if got.SchemaVersion != 1 {
		t.Errorf("SchemaVersion = %d，期望 1", got.SchemaVersion)
	}
	if len(got.Turns) != 0 || len(got.Ops) != 0 {
		t.Errorf("零记录材料不该带轮次/写操作：%+v", got)
	}
}
