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
	Close() error
}
