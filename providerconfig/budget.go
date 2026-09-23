package providerconfig

import (
	"fmt"
	"math"
)

// InputBudget 计算可用输入预算（FR-9.6）：
//
//	可用输入预算 = 窗口上限 − 固定开销（系统段 + 工具 schema） − 输出预留
//
// 结果必须为正：非正说明接入事实本身不成立（窗口太小或输出预留过大），
// 按配置错误显式失败，而不是在运行时靠"压到很小"苟活。
func (r Resolved) InputBudget(fixedOverhead int) (int, error) {
	budget := r.MaxContextTokens - fixedOverhead - r.OutputReserve
	if budget <= 0 {
		return 0, fmt.Errorf(
			"可用输入预算非正：context=%d − 固定开销=%d − 输出预留=%d = %d（FR-9.6）",
			r.MaxContextTokens, fixedOverhead, r.OutputReserve, budget)
	}
	return budget, nil
}

// Watermarks 按比例给出三档水位（FR-14.1）。基数是可用输入预算，而不是裸窗口。
// 四舍五入而非截断：截断会让 0.7×90000 算成 62999，与人工核算结果对不上。
func (r Resolved) Watermarks(fixedOverhead int, warn, target, hard float64) (warnAt, targetAt, hardAt int, err error) {
	budget, err := r.InputBudget(fixedOverhead)
	if err != nil {
		return 0, 0, 0, err
	}
	return int(math.Round(float64(budget) * warn)),
		int(math.Round(float64(budget) * target)),
		int(math.Round(float64(budget) * hard)),
		nil
}
