// Package ext 提供符号解析的抽象能力：符号定位（限定名 → 字节区间）、能力上报。
//
// 它是「基础能力包」：只定义抽象接口与结果类型，不认识任何业务形状——「符号化」
// 怎么和具体的读写操作组合，是消费方（符号读写领域）的事，不是本包的事。底层用
// 哪种解析器（进程内语法后端、MCP 外挂进程、LSP）由装配层注入。
package ext

import (
	"context"
	"errors"

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

// Site 是一个符号的一处出现（声明或引用）。重命名要一次改完**全部**出现点，
// 因此定位必须能一次给齐——"改了声明再去找引用"会让两次定位之间出现半完成状态。
type Site struct {
	File      string
	ByteRange workspace.ByteRange
}

// Prepared 是符号定位的结果：由后端算出，消费方据此落盘。
//
// `File` + `ByteRange` 是**声明**所在（读与整块替换用）；`Sites` 是这个符号的**全部出现点**
// （重命名用），只有请求 `All` 时才填——多数定位只要声明，列出全部引用是额外的扫描。
type Prepared struct {
	File      string
	ByteRange workspace.ByteRange
	// Sites 是全部出现点（含声明）。与 Impact **同源**：都来自同一次定位，
	// 因此"上报的规模"与"落盘的编辑计划"不可能对不上。
	Sites     []Site
	Impact    Impact
	Precision Precision
}

// LocateRequest 是一次符号定位的请求。它只表达「定位哪个符号」，不携带业务形状。
type LocateRequest struct {
	Symbol string // 符号限定名
	File   string // 限定到某个文件（可空，为空则全工作区）
	// All 要求一并返回全部出现点（重命名用）。默认只定位声明——多数调用只要声明，
	// 而列全部引用要扫遍所有候选文件。
	All bool
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

// ErrUnavailable 表示"符号后端不可用"。它是一个**可判定**的错误：消费方据此把
// 「没有后端」（环境问题，换个装配就有）与「定位不到」（结论，这个符号不在这）分开。
var ErrUnavailable = errors.New("符号后端不可用：没有装配符号解析能力")

// Unimplemented 是 ExtHost 的**空实现**：不装配符号后端时用它是"如实上报不可用"，
// 而不是"走到即炸"——符号能力不可用是一条可以带着跑的事实（符号原语会给出结构化错误），
// 不是装配缺陷。
type Unimplemented struct{}

var _ ExtHost = Unimplemented{}

// Capabilities 报告不可用：让消费方有机会走"结构化错误"而不是让符号能力假装在线。
func (Unimplemented) Capabilities(context.Context) ExtCaps {
	return ExtCaps{}
}

func (Unimplemented) Locate(context.Context, LocateRequest) (Prepared, error) {
	return Prepared{}, ErrUnavailable
}

// Fingerprint 给出空指纹（不是 nil）：装了什么就报什么，"没有"是已知事实。
func (Unimplemented) Fingerprint() []string { return []string{} }

func (Unimplemented) Close() error { return nil }
