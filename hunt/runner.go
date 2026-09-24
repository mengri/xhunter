package hunt

import "context"

// ============================================================ 门禁执行

// GateResult 是一次门禁执行的结果：**判定已经在门禁内部做完**，交给模型的是结论。
//
// Evidence 是截断后的输出（保留头尾）——判据在全量输出上算，但喂给模型的那一份必须截断，
// 否则一条失败的构建日志就能吃掉整轮上下文。
type GateResult struct {
	// CallID 是这次结论对应的工具调用标识：事件流用它把结论配回某一次调用（使用手册 §5 的
	// check_result 与 tool_call 同 call_id）。
	CallID     string
	Name       string
	Passed     bool
	Cached     bool
	ExitCode   int
	DurationMS int64
	Summary    string
	Evidence   string
	Source     string
}

// GateRunner 执行一条门禁，并按**它自己声明的判据**判定。
//
// 契约里只有一条要紧的边界：error **只表示执行失败**（命令不存在、起不来、超时、判据不可
// 判定）——那是环境问题，修好配置就能重跑；而"判定不通过"是**质量结论**，体现在 Passed=false，
// 不是错误。把两者混同的后果很具体：门禁命令名写错会被读成"代码质量差"，任务反复失败却
// 永远修不好。
type GateRunner interface {
	// Run 在 root（工作区根）里执行 g；fingerprint 是待提交改动的内容指纹，用于结果缓存。
	Run(ctx context.Context, root string, g Gate, fingerprint string) (GateResult, error)
}
