package harness

import "context"

// 原语契约：框架与业务的分界就在这个接口上。
//
// **Primitive 就是"原语"本身**——能做这件事的东西；它的名字是 PrimitiveName
// （见 types.go）。两者分开：名字是契约（对模型、对配置、对事件流），行为是实现
// （对我们）。所以这里没有 Impl 之类的后缀——接口就是那个概念，加后缀只会让
// 每个使用点都多一次心算。
//
// 框架只定义"实现长什么样"和"什么时候调它"，不定义有哪些实现——
// 具体原语由装配层提供的清单带进来（见 Tool 与 NewRuntime）。
//
// 三条契约由签名强制，不靠注释约束：
//
//	读的职责：读类只读；
//	写的边界：写类只产出文件编辑计划，绝不自己落盘（落盘统一在一处）；
//	不做裁决：允许不允许由策略层决定，原语只负责"能不能做"和"怎么做"。
type Primitive interface {
	Plan(ctx context.Context, in PlanInput) (Plan, error)
}

type PlanInput struct {
	Call     Call
	Route    Route
	Prepared Prepared // 符号路径的只读定位结果；文本路径时为空
	Caps     ExtCaps
	Facts    TaskFacts
}

type Plan struct {
	Result Result
	Edits  []FileEdit
}
