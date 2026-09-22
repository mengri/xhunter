package main

import (
	"xhunter/harness"
	"xhunter/internal/primitives/check"
	"xhunter/internal/primitives/edit"
	"xhunter/internal/primitives/find"
	"xhunter/internal/primitives/glob"
	"xhunter/internal/primitives/read"
	"xhunter/internal/primitives/rename"
	"xhunter/internal/primitives/write"
)

// defaultTools 给出本产品的工具集：7 个文件原语，按模型看到的顺序排列。
//
// **这条清单就是业务边界**。框架（harness）只提供机制——怎么分发、怎么裁决、怎么落盘、
// 怎么把声明交给上游——它不认识任何具体原语；"有哪些原语、叫什么名字、以什么顺序暴露"
// 全在这里定。于是三件事同时成立：
//
//   - 换一套工具集（另一种产品的领域原语）不需要动框架一行；
//   - 工具面的业务约束（恒定 7 个、顺序固定、缺席即分期未交付）在这一层被看见和审查；
//   - 声明与执行来自同一份清单，不可能出现"说明了 A、实际执行了 B"。
//
// 顺序属于契约：它进入每一轮请求的前缀，因此一旦定下就不再随手调整。
func defaultTools() []harness.Tool {
	return []harness.Tool{
		read.Tool(),
		write.Tool(),
		edit.Tool(),
		find.Tool(),
		rename.Tool(),
		glob.Tool(),
		check.Tool(),
	}
}
