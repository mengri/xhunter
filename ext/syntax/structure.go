package syntax

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"

	"xhunter/ext"
	"xhunter/workspace"
)

// ErrUnsupported 表示"这份文件判不了"：类型未注册时既谈不上语法完整，也谈不上封闭在符号内。
//
// 它与"没有符号包含这段"（`found=false`）是两句话：后者是**结论**（判据可用、确实没封闭），
// 前者是**没判过**。混在一起会让未注册语言的仓库每轮都被判成"结构不完整"。
var ErrUnsupported = errors.New("文件类型未注册：语法后端判不了")

// Parse 报告文件语法是否完整（结构检查的判据①）。
//
// 只看**当前内容**：检查点要钉的正是此刻的内容，不是历史上某一版。语法不完整是这份文件的
// 事实（模型写到一半就是写到一半），因此是 ParseBroken；读不到或未注册则是 ParseUnknown——
// 那时我们并没有判过。
func (h *Host) Parse(_ context.Context, file string) ext.ParseVerdict {
	if langOf(file) == "" {
		return ext.ParseUnknown
	}
	raw, ok := h.readAll(file)
	if !ok {
		return ext.ParseUnknown
	}
	if _, err := parser.ParseFile(token.NewFileSet(), file, raw, 0); err != nil {
		return ext.ParseBroken
	}
	return ext.ParseOK
}

// Enclose 给出**包含**这处区间的最小声明（结构检查的判据②）。
//
// 判不了（类型未注册、读不到、语法不完整）返回 error：那与"没有声明包含它"是两句话，
// 后者才是 `found=false`。语法不完整时尤其要分开——残缺的文件本就无从谈"封闭"。
func (h *Host) Enclose(_ context.Context, req ext.EncloseRequest) (ext.Prepared, bool, error) {
	if langOf(req.File) == "" {
		return ext.Prepared{}, false, ErrUnsupported
	}
	raw, ok := h.readAll(req.File)
	if !ok {
		return ext.Prepared{}, false, ErrUnsupported
	}
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, req.File, raw, 0)
	if err != nil {
		return ext.Prepared{}, false, ErrUnsupported
	}
	decl := smallestEnclosing(fset, parsed, req.ByteRange)
	if decl == nil {
		// 判过、确实没有声明包含它——例如改动横跨两个函数、或落在包声明与 import 之间。
		return ext.Prepared{}, false, nil
	}
	return ext.Prepared{
		File:      req.File,
		ByteRange: *decl,
		Precision: ext.PrecisionSyntactic,
		Impact:    ext.Impact{FilesChanged: 1, Occurrences: 1, Unknown: true},
	}, true, nil
}

// langOf 给出文件所属的**已注册**语言；未注册返回空串。
func langOf(file string) string {
	for _, l := range langs {
		if strings.HasSuffix(file, "."+l) {
			return l
		}
	}
	return ""
}

// smallestEnclosing 找**最小**的那个包含该区间的声明：外层声明几乎总能包含它，
// 取最小才能说出"这次改动落在谁的身体里"。
func smallestEnclosing(fset *token.FileSet, f *ast.File, br workspace.ByteRange) *workspace.ByteRange {
	var best *workspace.ByteRange
	consider := func(n ast.Node) {
		r := rangeOfNode(fset, n)
		if r == nil || !contains(*r, br) {
			return
		}
		if best == nil || (r.End-r.Start) < (best.End-best.Start) {
			best = r
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.FuncDecl:
			consider(node)
		case *ast.TypeSpec:
			consider(node)
		case *ast.ValueSpec:
			consider(node)
		}
		return true
	})
	return best
}

// contains 判断字节区间是否被另一个区间包含。空区间（纯插入）按"位置落在其中"算——
// 在声明的起始位置插入内容，改动仍然属于这个声明。
func contains(outer, inner workspace.ByteRange) bool {
	if inner.Start < outer.Start || inner.End > outer.End {
		return false
	}
	return inner.Start <= inner.End
}
