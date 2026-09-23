package providerconfig

import "testing"

// FR-9.6：可用输入预算 = 窗口上限 − 固定开销 − 输出预留。
func TestInputBudget(t *testing.T) {
	r := Resolved{MaxContextTokens: 200000, OutputReserve: 8192}
	got, err := r.InputBudget(5000)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if want := 200000 - 5000 - 8192; got != want {
		t.Fatalf("want %d, got %d", want, got)
	}

	tight := Resolved{MaxContextTokens: 1000, OutputReserve: 900}
	if _, err := tight.InputBudget(200); err == nil {
		t.Fatal("预算非正必须显式失败")
	}
}

// 水位线以可用输入预算为基数，而不是裸窗口（FR-9.6、FR-14.1）。
func TestWatermarks(t *testing.T) {
	r := Resolved{MaxContextTokens: 100000, OutputReserve: 10000}
	warn, target, hard, err := r.Watermarks(0, 0.70, 0.50, 0.90)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if warn != 63000 || target != 45000 || hard != 81000 {
		t.Fatalf("水位计算异常：warn=%d target=%d hard=%d", warn, target, hard)
	}
}
