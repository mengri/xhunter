// Package ext 提供符号解析的抽象能力：符号定位（限定名 → 字节区间）、能力上报。
//
// 它是「基础能力包」：只定义抽象接口与结果类型，不认识任何业务形状——「符号化」
// 怎么和具体的读写操作组合，是消费方（符号读写领域）的事，不是本包的事。底层用
// 哪种解析器（LSP、tree-sitter、别的引擎）由装配层注入。
package ext

import (
	"context"

	"xhunter/workspace"
)

// Precision 是定位的精度档位：语法级只知道「结构上在哪」，语义级知道「类型/引用上是什么」。
type Precision string

const (
	PrecisionSyntactic Precision = "syntactic"
	PrecisionSemantic  Precision = "semantic"
)

// ExtCaps 是符号后端上报的能力描述。核心只消费它，不感知后端是哪种解析器。
type ExtCaps struct {
	Available  bool
	Languages  []string
	CanResolve bool
	Precision  Precision
}

// Registered 报告某语言是否在后端的注册表里。
func (c ExtCaps) Registered(lang string) bool {
	if !c.Available || lang == "" {
		return false
	}
	for _, l := range c.Languages {
		if l == lang {
			return true
		}
	}
	return false
}

// Impact 是符号定位的改动规模。**它只是事实，不是闸门**：上报给变更说明与诊断用，
// 不参与策略裁决（终局是无人 review，判据来自证据，而不是"改得多就保守拒绝"）。
type Impact struct {
	FilesChanged int
	Occurrences  int
	Unknown      bool // 语法级后端无法穷尽引用时置位
}

// Prepared 是符号定位的结果：由后端算出，消费方据此落盘。
type Prepared struct {
	File      string
	ByteRange workspace.ByteRange
	Impact    Impact
	Precision Precision
}

// LocateRequest 是一次符号定位的请求。它只表达「定位哪个符号」，不携带业务形状。
type LocateRequest struct {
	Symbol string // 符号限定名
	File   string // 限定到某个文件（可空，为空则全工作区）
}

// ExtHost 刻意没有任何写方法——后端因此无法直接写工作区，
// 它只能返回「改哪里、改成什么」，由消费方执行落盘。
type ExtHost interface {
	Capabilities(ctx context.Context) ExtCaps
	Locate(ctx context.Context, req LocateRequest) (Prepared, error)
	// Fingerprint 给出**扩展能力指纹**：扩展标识 ＋ 版本 ＋ 语言清单（每个元素一个 token）。
	// 它服务两件事——会话材料（`meta.ext`）与生效配置快照；两侧**不一致只记录、不阻断**
	// （IA-6.5 / FR-13.7）。返回 `[]string` 是为了直接落进材料里的能力指纹字段位。
	Fingerprint() []string
	Close() error
}

// Unimplemented 是 ExtHost 的 **panic 哨兵**：未冻结期用它把"扩展未接入"显式化——走到即炸、
// 绝不静默，比传一个 nil 参数在运行期悄悄 nil 解引用要好。由 MS-8 替换为真实实现。
//
// 它是 **A 类入口**（不在正常路径上）：只有符号原语会用 `ExtHost`，而符号原语现在"声明不实现"，
// 所以装上它不会让任何一次运行走到这里。
type Unimplemented struct{}

var _ ExtHost = Unimplemented{}

func (Unimplemented) Capabilities(context.Context) ExtCaps {
	panic("ext.ExtHost 未实现：扩展未接入，由 MS-8 实现")
}

func (Unimplemented) Locate(context.Context, LocateRequest) (Prepared, error) {
	panic("ext.ExtHost 未实现：扩展未接入，由 MS-8 实现")
}

func (Unimplemented) Fingerprint() []string {
	panic("ext.ExtHost 未实现：扩展未接入，由 MS-8 实现")
}

func (Unimplemented) Close() error {
	panic("ext.ExtHost 未实现：扩展未接入，由 MS-8 实现")
}
