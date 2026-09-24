// Package symbolic 提供符号读写原语：按限定名定位代码符号后读写。
//
// 它依赖基本读写（hunt/basic）复用区间读写，依赖符号解析（xhunter/ext）做定位。
// 定位 → 字节区间 → 复用基本读写，是这里唯一的新增职责；「符号化」与「降级」因此
// 收在本包内，harness 与 basic 都不感知符号。
//
// **工具名不随能力增减**：符号能力不可用时，原语返回的是**结构化错误**，
// 不是"悄悄换一条文本路径"——静默降级会让模型以为自己改的是符号，实际改的是字符串。
package symbolic

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"xhunter/ext"
	"xhunter/hunt"
	"xhunter/llm"
	"xhunter/workspace"
)

// 符号原语对模型可见的名字。
const (
	SymbolRead   = hunt.PrimitiveName("symbol_read")
	SymbolEdit   = hunt.PrimitiveName("symbol_edit")
	SymbolRename = hunt.PrimitiveName("symbol_rename")
)

// SymbolReadTool 构造符号读原语：定位符号后读其所在区间。
func SymbolReadTool(ws workspace.Workspace, ex ext.ExtHost) hunt.Primitive {
	return symbolicPrim{name: SymbolRead, ws: ws, ex: ex, decl: llm.ToolDecl{
		Name:        string(SymbolRead),
		Description: "按限定名定位符号并读取其定义。",
		Schema: llm.ObjectSchema(`{
			"symbol": {"type": "string", "description": "符号限定名"},
			"path": {"type": "string", "description": "限定到某个文件（可选）"}
		}`, "symbol"),
	}}
}

// SymbolEditTool 构造符号编辑原语：定位符号后替换其定义区间。
func SymbolEditTool(ws workspace.Workspace, ex ext.ExtHost) hunt.Primitive {
	return symbolicPrim{name: SymbolEdit, ws: ws, ex: ex, decl: llm.ToolDecl{
		Name:        string(SymbolEdit),
		Description: "按限定名定位符号并替换其定义。",
		Schema: llm.ObjectSchema(`{
			"symbol": {"type": "string", "description": "符号限定名"},
			"content": {"type": "string", "description": "替换成的新内容"},
			"path": {"type": "string", "description": "限定到某个文件（可选）"}
		}`, "symbol", "content"),
	}}
}

// SymbolRenameTool 构造符号重命名原语：跨文件重命名，一次完成全部改动。
func SymbolRenameTool(ws workspace.Workspace, ex ext.ExtHost) hunt.Primitive {
	return symbolicPrim{name: SymbolRename, ws: ws, ex: ex, decl: llm.ToolDecl{
		Name:        string(SymbolRename),
		Description: "跨文件重命名符号，一次调用完成全部改动。当前环境不支持时返回明确错误。",
		Schema: llm.ObjectSchema(`{
			"symbol": {"type": "string", "description": "要重命名的符号限定名"},
			"new_name": {"type": "string", "description": "重命名后的名称"},
			"path": {"type": "string", "description": "限定到某个文件（可选）"}
		}`, "symbol", "new_name"),
	}}
}

type symbolicPrim struct {
	name hunt.PrimitiveName
	ws   workspace.Workspace
	ex   ext.ExtHost
	decl llm.ToolDecl
}

func (p symbolicPrim) Decl() llm.ToolDecl { return p.decl }

// Writes 报告本原语是否写盘：符号读不写，符号编辑与重命名写。
// 按名字分而不是按某次调用的参数分——写不写盘是原语的性质，不是这一次调用的性质。
func (p symbolicPrim) Writes() bool { return p.name != SymbolRead }

// Execute 执行一次符号调用：先问能力、再定位、最后按原语各自的语义行动。
//
// 顺序是刻意的：**能力检查先于定位**。后端不可用时返回的是"这次没有符号能力"，
// 而定位不到是"这个符号不在这"——两句话混成一句，模型就分不清该换工具还是换名字。
func (p symbolicPrim) Execute(ctx context.Context, call hunt.Call, facts hunt.Facts) (hunt.Result, []workspace.FileEdit, error) {
	if p.ex == nil {
		return hunt.Result{Err: &llm.Fault{
			Kind: "ext_unavailable", Message: "本次没有装配符号后端：符号能力不可用", Retryable: false}}, nil, nil
	}
	caps := p.ex.Capabilities(ctx)
	if !caps.Available {
		return hunt.Result{Err: &llm.Fault{
			Kind:      "ext_unavailable",
			Message:   "符号后端不可用（改用 read / edit 按文本寻址）：本次没有可用的符号解析能力",
			Retryable: false,
		}}, nil, nil
	}
	symbol := strings.TrimSpace(call.Selector.Symbol)
	if symbol == "" {
		return hunt.Result{Err: &llm.Fault{Kind: "bad_args", Message: "缺少 symbol（符号限定名）", Retryable: false}}, nil, nil
	}
	// 语言未注册 → 结构化错误。**绝不降级成文本替换**：那会让模型以为改的是符号，
	// 实际改的是同名字符串，而这类错误在无人 review 的场景下发现不了。
	if lang := languageOf(call.Target); lang != "" && !caps.Registered(lang) {
		return hunt.Result{Err: &llm.Fault{
			Kind:      "language_unregistered",
			Message:   "符号后端没有注册语言 " + lang + "：" + describeTarget(call.Target),
			Retryable: false,
		}}, nil, nil
	}

	prep, err := p.ex.Locate(ctx, ext.LocateRequest{
		Symbol: symbol, File: call.Target, All: p.name == SymbolRename,
	})
	if err != nil {
		// 定位不到是**结论**：这个符号不在这（或后端没扫到）。不可重试——
		// 换个限定名才有意义，重跑同一个请求不会改变结果。
		return hunt.Result{Err: &llm.Fault{
			Kind: "symbol_not_found", Message: err.Error(), Retryable: false}}, nil, nil
	}

	switch p.name {
	case SymbolRead:
		return p.read(ctx, facts, prep, symbol)
	case SymbolEdit:
		return p.edit(call, prep)
	case SymbolRename:
		return p.rename(call, prep)
	}
	return hunt.Result{Err: &llm.Fault{Kind: "bad_args", Message: "未知原语", Retryable: false}}, nil, nil
}

// read 读取定位到的区间全文。
//
// 读的同时登记台账：符号读与文本读一样是"模型看到了这份内容"，
// 「改前必读」的校验因此同样成立。
func (p symbolicPrim) read(_ context.Context, facts hunt.Facts, prep ext.Prepared, symbol string) (hunt.Result, []workspace.FileEdit, error) {
	fc, err := p.ws.Read(prep.File, workspace.LineRange{})
	if err != nil {
		return hunt.Result{}, nil, err
	}
	if fc.Truncated {
		// 读不全就不给内容：在截断的内容上"读到一半的符号"比报错更坏——
		// 模型会据此改一个它没看全的定义。
		return hunt.Result{Err: &llm.Fault{
			Kind:      "read_truncated",
			Message:   "符号所在文件过长，无法一次读全：" + prep.File,
			Retryable: false,
		}}, nil, nil
	}
	if facts != nil {
		facts.Ledger().Mark(prep.File, fc.Fingerprint)
	}
	if prep.ByteRange.Start > len(fc.Raw) || prep.ByteRange.End > len(fc.Raw) {
		// 后端给的区间与当前内容不符（多半是文件在定位之后又变了）：如实报错，不切半个符号出来。
		return hunt.Result{Err: &llm.Fault{
			Kind:      "stale_locate",
			Message:   "定位结果已过期（文件在定位之后被改动）：" + prep.File,
			Retryable: true,
		}}, nil, nil
	}
	return hunt.Result{Summary: renderSymbol(symbol, prep, fc.Raw[prep.ByteRange.Start:prep.ByteRange.End])}, nil, nil
}

// edit 把定位到的区间整体替换掉：原语只产出编辑计划，落盘由执行体统一做。
func (p symbolicPrim) edit(call hunt.Call, prep ext.Prepared) (hunt.Result, []workspace.FileEdit, error) {
	if strings.TrimSpace(call.Content) == "" {
		return hunt.Result{Err: &llm.Fault{Kind: "bad_args", Message: "缺少 content（替换成的新内容）", Retryable: false}}, nil, nil
	}
	return hunt.Result{Summary: fmt.Sprintf("已定位 %s：%s 字节 [%d,%d)（精度 %s），将替换为新内容",
			call.Selector.Symbol, prep.File, prep.ByteRange.Start, prep.ByteRange.End, prep.Precision)},
		[]workspace.FileEdit{{File: prep.File, ByteRange: prep.ByteRange, NewContent: call.Content}}, nil
}

// rename 一次改完声明与全部引用点。
//
// **不降级为文本替换**：改的是后端定位出来的每一个标识符区间，不是"把同名字符串换掉"。
// 后端列不出出现点时返回结构化错误——半完成的重命名比不重命名更坏：声明改了、引用没改，
// 代码立刻变成不能编译的状态，而这种状态在无人 review 的场景下会被直接交出去。
func (p symbolicPrim) rename(call hunt.Call, prep ext.Prepared) (hunt.Result, []workspace.FileEdit, error) {
	newName := strings.TrimSpace(call.NewName)
	if newName == "" {
		return hunt.Result{Err: &llm.Fault{Kind: "bad_args", Message: "缺少 new_name（重命名后的名称）", Retryable: false}}, nil, nil
	}
	if len(prep.Sites) == 0 {
		return hunt.Result{Err: &llm.Fault{
			Kind:      "cannot_resolve",
			Message:   "当前符号后端列不出 " + strconv.Quote(call.Selector.Symbol) + " 的全部引用点：不做半套重命名（改用 symbol_edit 只改声明，或换用能解析引用的后端）",
			Retryable: false,
		}}, nil, nil
	}
	edits := make([]workspace.FileEdit, 0, len(prep.Sites))
	for _, s := range prep.Sites {
		edits = append(edits, workspace.FileEdit{File: s.File, ByteRange: s.ByteRange, NewContent: newName})
	}
	return hunt.Result{Summary: renderRename(call.Selector.Symbol, newName, prep)}, edits, nil
}

// renderSymbol 把一次符号读渲染成回灌给模型的文本。
//
// 精度与"未穷尽"如实写进结果（FR-4.13）：模型据此知道这条结论有多可信，
// 而不是拿到一段看起来确定无疑的文本。
func renderSymbol(symbol string, prep ext.Prepared, body string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "符号 %s：%s 字节 [%d,%d)", symbol, prep.File, prep.ByteRange.Start, prep.ByteRange.End)
	fmt.Fprintf(&sb, "\n精度：%s", prep.Precision)
	if prep.Impact.Unknown {
		sb.WriteString("（引用未穷尽：跨包引用不在本次视野内）")
	}
	sb.WriteString("\n")
	sb.WriteString(body)
	return sb.String()
}

// renderRename 把一次重命名渲染成结论：改了哪些文件、多少处，以及"未穷尽"这件事。
func renderRename(symbol, newName string, prep ext.Prepared) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "重命名 %s → %s：%d 个文件、%d 处", symbol, newName, prep.Impact.FilesChanged, prep.Impact.Occurrences)
	if prep.Impact.Unknown {
		sb.WriteString("；引用未穷尽（语法级后端看不到跨包引用），请自行确认包外调用点")
	}
	if len(prep.Sites) > 0 {
		sb.WriteString("\n改动：")
		for _, s := range prep.Sites {
			sb.WriteString("\n  - " + s.File + " 字节 [" + strconv.Itoa(s.ByteRange.Start) + "," + strconv.Itoa(s.ByteRange.End) + ")")
		}
	}
	return sb.String()
}

// languageOf 按扩展名推断语言。
//
// 认不出的扩展名也按"一种语言"返回（`.py` → `py`），而不是返回空串：空串会被读成
// "无从判断"，于是绕过注册检查一路走到定位失败——而真话是"这个文件是 py，后端没注册 py"，
// 那正是要如实告诉模型的事。没有扩展名才真的无从判断。
func languageOf(file string) string {
	ext := strings.ToLower(filepath.Ext(strings.TrimSpace(file)))
	if len(ext) <= 1 {
		return ""
	}
	return ext[1:]
}

func describeTarget(file string) string {
	if strings.TrimSpace(file) == "" {
		return "（未限定文件）"
	}
	return file
}
