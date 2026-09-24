package hunt

import (
	"context"
	"testing"

	"xhunter/harness"
)

// config_snapshot 把接收段不活动超时的**生效值**也报出来（毫秒）。它走的是与机制硬顶同一条
// 通道（装配层注入 AssemblyFacts），只进 config_snapshot、不进 EffectiveConfig。
func TestConfigSnapshot_CarriesStreamIdleTimeout(t *testing.T) {
	sink := &captureSink{}
	cfg := prepareConfig(sink)
	cfg.Assembly.StreamIdleTimeoutMS = 120000
	s := NewSession(cfg)
	if err := s.Prepare(context.Background(), &harness.Run{}); err != nil {
		t.Fatalf("Prepare 失败：%v", err)
	}

	snaps := sink.ofType("config_snapshot")
	if len(snaps) != 1 {
		t.Fatalf("应恰好一条 config_snapshot，实际 %d：%v", len(snaps), sink.events)
	}
	got, ok := snaps[0]["stream_idle_timeout_ms"].(int)
	if !ok || got != 120000 {
		t.Errorf("stream_idle_timeout_ms = %v（%T），期望字面量 120000",
			snaps[0]["stream_idle_timeout_ms"], snaps[0]["stream_idle_timeout_ms"])
	}
}
