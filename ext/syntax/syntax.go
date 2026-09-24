// Package syntax 提供内置的**语法级符号后端**：用标准库的语法树定位符号，不依赖任何外部进程。
//
// 它是首发后端、也是所有语言的兜底：不用装任何东西就能用，代价是只知道"结构上在哪"、
// 不知道"类型与引用上是什么"（`CanResolve=false`）——因此改动规模只能如实标注 Unknown，
// 绝不猜。
//
// 它跑在**核心进程内**，不是外挂扩展：外挂通道留给第三方后端，两者实现同一个
// `ext.ExtHost`，核心只认接口、不感知后端是哪种。
package syntax

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"

	"xhunter/ext"
	"xhunter/workspace"
)

// langs 是首发语言清单。自包含语法后端每加一门语言就要多一份解析器，
// 清单因此刻意小——它决定"哪些语言的符号能力可用"。
var langs = []string{"go"}

// extFilePattern 是源码文件的枚举模式（不含 "/" 的模式按文件名匹配、命中任意深度）。
const extFilePattern = "*.go"

// Host 是语法级后端。
//
// 它持有工作区视图：定位要读源码，而"能读什么"由工作区的边界给出（相对路径、不出工作区），
// 后端因此没有第二条文件访问路径——这既是不变量，也让它不可能读到工作区之外。
type Host struct {
	ws workspace.Workspace
}

// New 给出一份语法级后端。工作区是构造参数（运行期产物），与外挂扩展的"进程 + 根路径"
// 是同一种装配知识的两处落点。
func New(ws workspace.Workspace) *Host { return &Host{ws: ws} }

var _ ext.ExtHost = (*Host)(nil)

func (Host) Capabilities(context.Context) ext.ExtCaps {
	return ext.ExtCaps{
		Available:  true,
		Languages:  langs,
		CanResolve: false,
		Precision:  ext.PrecisionSyntactic,
	}
}

// FingerprintTokens 是这份后端的能力指纹（**不依赖实例**）。
//
// 装配层在拿到工作区之前就要把它写进生效配置快照，所以它必须能静态取到——
// 否则"这次用的是哪套符号能力"只能等运行期才知道，快照就得开一个例外。
func FingerprintTokens() []string {
	out := make([]string, 0, len(langs)+1)
	out = append(out, "syntax/1")
	for _, l := range langs {
		out = append(out, "lang:"+l)
	}
	return out
}

// Fingerprint 给出能力指纹（契约方法）：与装配层写进快照的是同一份。
func (Host) Fingerprint() []string { return FingerprintTokens() }

// Close 无资源可收：进程内后端没有子进程、没有连接。留着它是为了满足契约，
// 也让"换成外挂后端"这件事对调用方完全透明。
func (Host) Close() error { return nil }

// Locate 定位一个符号：先找声明，请求要求时再列出全部出现点（重命名用）。
//
// 找不到是**结论**（这个工作区里没有这个符号），不是环境问题——重试也不会有，
// 所以按错误上抛但标注不可重试由消费方决定。
func (h *Host) Locate(_ context.Context, req ext.LocateRequest) (ext.Prepared, error) {
	name, qualifier := splitSymbol(req.Symbol)
	if name == "" {
		return ext.Prepared{}, fmt.Errorf("符号限定名为空")
	}
	files, err := h.candidateFiles(req.File)
	if err != nil {
		return ext.Prepared{}, err
	}
	for _, file := range files {
		raw, ok := h.readAll(file)
		if !ok {
			continue
		}
		fset := token.NewFileSet()
		parsed, perr := parser.ParseFile(fset, file, raw, 0)
		if perr != nil {
			// 源码语法不完整 → 这个文件定位不了。那是**这个文件的事实**，不是运行错误：
			// 跳过继续找（一个文件写坏了不该让整个符号能力不可用）。
			continue
		}
		decl := findDecl(fset, parsed, name, qualifier)
		if decl == nil {
			continue
		}
		out := ext.Prepared{
			File:      file,
			ByteRange: *decl,
			Precision: ext.PrecisionSyntactic,
			Impact:    ext.Impact{FilesChanged: 1, Occurrences: 1, Unknown: true},
		}
		if req.All {
			sites := h.sites(files, name)
			out.Sites = sites
			// 规模与编辑计划**同源**：都来自这一次扫描（不是另算一遍）。
			out.Impact = impactOf(sites)
		}
		return out, nil
	}
	return ext.Prepared{}, fmt.Errorf("没有找到符号 %q", req.Symbol)
}

// candidateFiles 给出候选源码文件：指定了文件就只看它，否则枚举工作区里的源码。
func (h *Host) candidateFiles(file string) ([]string, error) {
	if strings.TrimSpace(file) != "" {
		return []string{file}, nil
	}
	files, err := h.ws.List(extFilePattern)
	if err != nil {
		return nil, fmt.Errorf("枚举源码文件失败：%w", err)
	}
	return files, nil
}

// readAll 读一份文件的全文；读不全（被截断）时返回 false——在截断的内容上做语法分析
// 会得出与全量不同的结论，那比"这份文件定位不了"更坏。
func (h *Host) readAll(file string) (string, bool) {
	fc, err := h.ws.Read(file, workspace.LineRange{})
	if err != nil || fc.Truncated {
		return "", false
	}
	return fc.Raw, true
}

// findDecl 在语法树里找名字匹配的声明，返回它的字节区间。
func findDecl(fset *token.FileSet, f *ast.File, name, qualifier string) *workspace.ByteRange {
	var found *workspace.ByteRange
	ast.Inspect(f, func(n ast.Node) bool {
		if found != nil {
			return false
		}
		switch node := n.(type) {
		case *ast.FuncDecl:
			if node.Name.Name != name {
				return true
			}
			if !qualifierMatches(qualifier, f.Name.Name, receiverType(node)) {
				return true
			}
			found = rangeOfNode(fset, node)
		case *ast.TypeSpec:
			if node.Name.Name == name && qualifierMatches(qualifier, f.Name.Name, "") {
				found = rangeOfNode(fset, node)
			}
		case *ast.ValueSpec:
			for _, ident := range node.Names {
				if ident.Name == name && qualifierMatches(qualifier, f.Name.Name, "") {
					found = rangeOfNode(fset, node)
					break
				}
			}
		}
		return true
	})
	return found
}

// qualifierMatches 判断限定名前缀是否匹配：为空即不筛选；否则按包名或接收者类型名匹配。
//
// 刻意宽松（包名或类型名任一命中即算）：语法级后端没有类型信息，把筛选做严会把"模型写对了
// 限定名"判成找不到；而错匹配只在**同名符号跨包**时才发生，那本来就在 Unknown 的覆盖范围内。
func qualifierMatches(qualifier, pkg, recv string) bool {
	if qualifier == "" {
		return true
	}
	return qualifier == pkg || qualifier == recv
}

func receiverType(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	return strings.TrimPrefix(typeName(fn.Recv.List[0].Type), "*")
}

// typeName 取类型表达式的名字（去掉指针与包限定）。
func typeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return "*" + typeName(t.X)
	case *ast.SelectorExpr:
		return typeName(t.X) + "." + t.Sel.Name
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return typeName(t.X)
	default:
		return ""
	}
}

// rangeOfNode 把一个语法节点换算成字节区间。用 Offset（不是行列）—
// 落盘唯一入口只认字节区间。
func rangeOfNode(fset *token.FileSet, n ast.Node) *workspace.ByteRange {
	start := fset.Position(n.Pos()).Offset
	end := fset.Position(n.End()).Offset
	if end < start {
		return nil
	}
	return &workspace.ByteRange{Start: start, End: end}
}

// sites 列出同包内一个标识符的全部出现点（声明与引用），供重命名使用。
//
// 语法级后端只能看到**本工作区里这些文件**：跨包引用（别的仓库、vendor 之外）不在视野内，
// 因此结果一律带 `Unknown`——那是事实，不是保守。
func (h *Host) sites(files []string, name string) []ext.Site {
	out := make([]ext.Site, 0)
	for _, file := range files {
		raw, ok := h.readAll(file)
		if !ok {
			continue
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, raw, 0)
		if err != nil {
			continue
		}
		skip := fieldNamesAndKeys(parsed)
		ast.Inspect(parsed, func(n ast.Node) bool {
			ident, ok := n.(*ast.Ident)
			if !ok || ident.Name != name {
				return true
			}
			if skip[ident] {
				return true
			}
			if br := rangeOfNode(fset, ident); br != nil {
				out = append(out, ext.Site{File: file, ByteRange: *br})
			}
			return true
		})
	}
	return out
}

// fieldNamesAndKeys 收集**不该被重命名**的同名标识符：结构体字段名与复合字面量的键。
//
// `Foo string` 里的 Foo 与要改的函数名 Foo 同名时，把它们一起改掉就是把无关的结构改坏——
// 语法级后端没有类型信息来区分，所以用结构位置把它们摘出去（比"全改"更安全，也比"不改"更准）。
func fieldNamesAndKeys(f *ast.File) map[*ast.Ident]bool {
	skip := map[*ast.Ident]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.StructType:
			for _, field := range node.Fields.List {
				for _, ident := range field.Names {
					skip[ident] = true
				}
			}
		case *ast.KeyValueExpr:
			// 复合字面量的键（`Foo: 1`）与字段名同理：同名的键不是"这个符号的引用"。
			if key, ok := node.Key.(*ast.Ident); ok {
				skip[key] = true
			}
		case *ast.SelectorExpr:
			// `x.Foo` 的 Sel 是**字段/方法选择**：语法级无从判断它是不是本次要改的那个符号，
			// 一律跳过（漏改会被 Unknown 如实覆盖，误改则直接改坏代码）。
			skip[node.Sel] = true
		}
		return true
	})
	return skip
}

// impactOf 把出现点折算成规模：文件数与处数都来自这一次扫描，与编辑计划同源。
func impactOf(sites []ext.Site) ext.Impact {
	files := map[string]bool{}
	for _, s := range sites {
		files[s.File] = true
	}
	return ext.Impact{
		FilesChanged: len(files),
		Occurrences:  len(sites),
		// 语法级后端永远无法穷尽跨包引用：未知就是未知，不猜一个"大概都改完了"。
		Unknown: true,
	}
}

// splitSymbol 拆开限定名：最后一段是标识符，前面是包/类型的筛选条件。
//
// `(*Type).Method` 这种形态按方法名处理，前面的部分只当筛选条件用——它本来就是人写给
// 模型看的，不是编译器接受的语法。
func splitSymbol(symbol string) (name, qualifier string) {
	symbol = strings.TrimSpace(symbol)
	if symbol == "" {
		return "", ""
	}
	if i := strings.LastIndex(symbol, "."); i >= 0 {
		return strings.TrimSpace(symbol[i+1:]), strings.TrimSpace(symbol[:i])
	}
	return symbol, ""
}
