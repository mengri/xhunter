package modelcatalog

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xhunter/providerconfig"
)

// fixture 是上游目录的缩影，覆盖：sample_spec、无关模式、缺上限、上限自相矛盾、
// 带前缀与不带前缀的键名、同名冲突、含价格与不含价格。
const fixture = `{
  "sample_spec": {"max_input_tokens": "max input tokens", "litellm_provider": "one of ..."},
  "claude-sonnet-4-20250514": {
    "litellm_provider": "anthropic", "mode": "chat",
    "max_input_tokens": 1000000, "max_output_tokens": 64000,
    "input_cost_per_token": 3e-06, "output_cost_per_token": 1.5e-05
  },
  "claude-haiku-4-5": {
    "litellm_provider": "anthropic", "mode": "chat",
    "max_input_tokens": 200000, "max_output_tokens": 8192
  },
  "anthropic/claude-haiku-4-5": {
    "litellm_provider": "anthropic", "mode": "chat",
    "max_input_tokens": 200000, "max_output_tokens": 8192,
    "input_cost_per_token": 1e-06
  },
  "gemini/gemini-2.0-flash": {
    "litellm_provider": "gemini", "mode": "chat",
    "max_input_tokens": 1048576, "max_output_tokens": 8192
  },
  "text-embedding-3-large": {
    "litellm_provider": "openai", "mode": "embedding",
    "max_input_tokens": 8191, "max_output_tokens": 0
  },
  "dall-e-3": {
    "litellm_provider": "openai", "mode": "image_generation"
  },
  "no-limits-model": {
    "litellm_provider": "openai", "mode": "chat"
  },
  "incoherent-model": {
    "litellm_provider": "openai", "mode": "chat",
    "max_input_tokens": 1000, "max_output_tokens": 1000
  },
  "ambiguous-large-window": {
    "litellm_provider": "xai", "mode": "chat",
    "max_input_tokens": 256000, "max_output_tokens": 256000, "max_tokens": 256000
  },
  "legacy-max-tokens-only": {
    "litellm_provider": "openai", "mode": "chat",
    "max_tokens": 128000, "max_output_tokens": 4096
  },
  "unknown-provider-model": {
    "mode": "chat", "max_input_tokens": 1000, "max_output_tokens": 100
  }
}`

func parseFixture(t *testing.T, body string) map[string]Entry {
	t.Helper()
	var raw map[string]Entry
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("fixture 解析失败：%v", err)
	}
	return raw
}

func TestConvert_FiltersAndNormalizes(t *testing.T) {
	cfg, rep, err := Convert(parseFixture(t, fixture), DefaultOutputReserve)
	if err != nil {
		t.Fatalf("转换失败：%v", err)
	}

	if rep.SkippedSampleSpec != 1 {
		t.Fatalf("sample_spec 必须跳过，实际 %d", rep.SkippedSampleSpec)
	}
	if rep.SkippedMode < 3 { // embedding / image_generation / 无 provider
		t.Fatalf("无关模式应被跳过，实际 %d", rep.SkippedMode)
	}
	if rep.SkippedNoLimits != 1 {
		t.Fatalf("缺上限应被跳过，实际 %d", rep.SkippedNoLimits)
	}
	if rep.SkippedIncoherent != 1 {
		t.Fatalf("窗口小于策略默认值的条目应被跳过，实际 %d", rep.SkippedIncoherent)
	}
	if rep.UsedPolicyDefault != 1 {
		t.Fatalf("未区分输入输出的条目应使用策略默认值，实际 %d", rep.UsedPolicyDefault)
	}

	// 带前缀的键名统一去前缀。
	anthropic := cfg.Provider["anthropic"]
	if _, ok := anthropic.Models["claude-sonnet-4-20250514"]; !ok {
		t.Fatalf("anthropic 模型缺失：%v", keysOf(anthropic.Models))
	}
	if _, ok := anthropic.Models["claude-haiku-4-5"]; !ok {
		t.Fatal("去前缀后应得到 claude-haiku-4-5")
	}
	if rep.Collisions != 1 {
		t.Fatalf("同名冲突应被计数，实际 %d", rep.Collisions)
	}
	// 冲突取舍：信息更全者胜（带价格的那条）。
	if anthropic.Models["claude-haiku-4-5"].Cost == nil {
		t.Fatal("冲突时应保留带价格的条目")
	}

	// gemini 的前缀也被去掉。
	if _, ok := cfg.Provider["gemini"].Models["gemini-2.0-flash"]; !ok {
		t.Fatalf("gemini 去前缀失败：%v", keysOf(cfg.Provider["gemini"].Models))
	}

	// 上游只给"总窗口"一个数时：上下文取事实值，输出预留取策略默认值并打标。
	amb := cfg.Provider["xai"].Models["ambiguous-large-window"]
	if amb.Limit.Context != 256000 {
		t.Fatalf("上下文应取上游事实值：%+v", amb.Limit)
	}
	if amb.Limit.Output != DefaultOutputReserve {
		t.Fatalf("输出预留应取策略默认值 %d：%+v", DefaultOutputReserve, amb.Limit)
	}
	if !amb.Limit.OutputIsPolicyDefault {
		t.Fatal("取自策略默认值的条目必须打标，便于审计")
	}

	// max_tokens 作为输入上限的兜底（老条目只有 max_tokens）。
	legacy := cfg.Provider["openai"].Models["legacy-max-tokens-only"]
	if legacy.Limit.Context != 128000 {
		t.Fatalf("max_tokens 应作为 context 兜底：%+v", legacy.Limit)
	}
	if legacy.Limit.Output != 4096 {
		t.Fatalf("显式的 max_output_tokens 应优先于兜底：%+v", legacy.Limit)
	}
}

// 价格单位：美元/token → 纳美元/token。
func TestConvert_CostUnits(t *testing.T) {
	cfg, _, err := Convert(parseFixture(t, fixture), DefaultOutputReserve)
	if err != nil {
		t.Fatalf("转换失败：%v", err)
	}
	c := cfg.Provider["anthropic"].Models["claude-sonnet-4-20250514"].Cost
	if c == nil {
		t.Fatal("价格未解析")
	}
	if c.InputNanoUSDPerToken != 3000 {
		t.Fatalf("3e-06 USD/token 应为 3000 纳美元，实际 %d", c.InputNanoUSDPerToken)
	}
	if c.OutputNanoUSDPerToken != 15000 {
		t.Fatalf("1.5e-05 USD/token 应为 15000 纳美元，实际 %d", c.OutputNanoUSDPerToken)
	}
}

// 转换产物必须能通过生效校验（上限齐备），否则它就是不可用的目录。
func TestConvert_OutputPassesResolvedValidation(t *testing.T) {
	cfg, _, err := Convert(parseFixture(t, fixture), DefaultOutputReserve)
	if err != nil {
		t.Fatalf("转换失败：%v", err)
	}
	if err := cfg.ValidateResolved(); err != nil {
		t.Fatalf("转换结果应可通过生效校验：%v", err)
	}
}

func TestConvert_EmptyResultIsExplicitError(t *testing.T) {
	_, _, err := Convert(map[string]Entry{}, DefaultOutputReserve)
	if err == nil {
		t.Fatal("空目录必须显式报错，而不是产出一个空目录")
	}
}

func TestUpdate_FetchesConvertsAndSaves(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			t.Error("缺少 User-Agent")
		}
		_, _ = w.Write([]byte(fixture))
	}))
	defer srv.Close()

	dir := t.TempDir()
	snap, err := Update(context.Background(), UpdateOptions{Source: srv.URL, Dir: dir, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("更新失败：%v", err)
	}
	if snap.Meta.Source != srv.URL {
		t.Fatalf("来源未记录：%q", snap.Meta.Source)
	}
	if snap.Meta.UpstreamBytes == 0 || snap.Meta.UpstreamSHA256 == "" {
		t.Fatal("元数据不完整")
	}
	if snap.Meta.Report.Converted == 0 {
		t.Fatal("转换统计为空")
	}

	// 落盘内容可被 Load 读回，且通过生效校验。
	back, err := Load(dir)
	if err != nil {
		t.Fatalf("读回失败：%v", err)
	}
	if back.Meta.UpstreamSHA256 != snap.Meta.UpstreamSHA256 {
		t.Fatal("读回的元数据不一致")
	}
	// 目录不存在时给出可操作的提示。
	if _, err := Load(t.TempDir()); !errors.Is(err, ErrNoSnapshot) {
		t.Fatalf("want ErrNoSnapshot, got %v", err)
	}
}

// 原子写：失败或成功后都不应留下临时文件。
func TestSave_LeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	cfg, _, err := Convert(parseFixture(t, fixture), DefaultOutputReserve)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, Snapshot{Meta: Meta{Source: "test"}, Catalog: cfg}); err != nil {
		t.Fatalf("保存失败：%v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("残留临时文件：%s", e.Name())
		}
	}
	if len(entries) != 1 || entries[0].Name() != SnapshotName {
		t.Fatalf("目录内容异常：%v", entries)
	}
}

// 上游返回非 200、超限、坏 JSON 都必须显式失败，且不破坏已有快照。
func TestUpdate_FailureModes(t *testing.T) {
	dir := t.TempDir()
	// 先放一份可用快照。
	cfg, _, err := Convert(parseFixture(t, fixture), DefaultOutputReserve)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, Snapshot{Meta: Meta{Source: "seed"}, Catalog: cfg}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(SnapshotPath(dir))
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string]http.HandlerFunc{
		"HTTP 500": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) },
		"坏 JSON":   func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{not json")) },
		"空目录":      func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) },
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			defer srv.Close()
			if _, err := Update(context.Background(), UpdateOptions{Source: srv.URL, Dir: dir}); err == nil {
				t.Fatal("应当失败")
			}
			after, err := os.ReadFile(SnapshotPath(dir))
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("失败时不得破坏已有快照")
			}
		})
	}
}

// 覆盖片段与目录合并：目录提供上限，用户只给连接参数（FR-9.5 的来源①+②）。
func TestSnapshot_MergesWithUserConfig(t *testing.T) {
	cfg, _, err := Convert(parseFixture(t, fixture), DefaultOutputReserve)
	if err != nil {
		t.Fatal(err)
	}
	user, err := providerconfig.Parse(strings.NewReader(
		`{"provider":{"anthropic":{"options":{"apiKey":"{env:ANTHROPIC_API_KEY}"}}}}`))
	if err != nil {
		t.Fatalf("用户覆盖片段应可解析：%v", err)
	}

	r, err := user.Resolve(cfg, "anthropic", "claude-sonnet-4-20250514")
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if r.MaxContextTokens != 1000000 {
		t.Fatalf("目录上限未生效：%d", r.MaxContextTokens)
	}
	if r.APIKeyEnv != "ANTHROPIC_API_KEY" {
		t.Fatalf("用户连接参数未生效：%q", r.APIKeyEnv)
	}
	// 可用输入预算按 FR-9.6 计算。
	budget, err := r.InputBudget(2000)
	if err != nil {
		t.Fatal(err)
	}
	if want := 1000000 - 2000 - 64000; budget != want {
		t.Fatalf("want %d, got %d", want, budget)
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

var _ = filepath.Join
