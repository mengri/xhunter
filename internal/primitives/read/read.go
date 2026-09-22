// Package read 实现 read 原语：按路径与行范围读取内容，越界处显式标注截断。
//
// 它是"改前必读"的落点：读的同时登记台账，写入时校验的因此不是"模型做过什么动作"，
// 而是"这个文件的当前内容模型是否见过"。
package read

import (
	"context"
	"fmt"
	"strings"

	"xhunter/harness"
)

// Name 是本原语对模型可见的名字。
const Name = harness.PrimitiveName("read")

// Tool 给出本原语的完整描述：名字、声明、实现、寻址性质。
//
// 寻址由选择器表达式决定——写行范围是文本寻址，写限定名或要符号大纲是符号寻址。
func Tool() harness.Tool {
	return harness.Tool{
		Name:    Name,
		Decl:    decl,
		Impl:    impl{},
		Address: harness.AddressedBySelector,
	}
}

var decl = harness.ToolDecl{
	Name:        string(Name),
	Description: "读文件：按路径与行范围读取内容，越界处标注截断。",
	Schema: harness.ObjectSchema(`{
			"path": {"type": "string", "description": "工作区内相对路径"},
			"range": {"type": "object", "description": "按行读取的区间（1 起，闭区间），省略则读全文",
				"properties": {"from": {"type": "integer"}, "to": {"type": "integer"}}},
			"symbol": {"type": "string", "description": "限定名：按符号读取"},
			"in_symbol": {"type": "string", "description": "符号内相对定位"},
			"file_view": {"type": "boolean", "description": "改为列出文件内的符号（大纲）"}
		}`, "path"),
}

type impl struct{}

func (impl) Plan(_ context.Context, in harness.PlanInput) (harness.Plan, error) {
	// 符号路径：定位由扩展完成，核心只消费结果。
	if in.Route.Path == harness.PathSymbol {
		return harness.Plan{Result: harness.Result{
			Summary: fmt.Sprintf("符号视图 %s", in.Call.Target),
		}}, nil
	}

	fc, err := in.Facts.Workspace().Read(in.Call.Target, rangeOf(in.Call))
	if err != nil {
		return harness.Plan{}, err
	}
	in.Facts.Ledger().Mark(in.Call.Target, fc.Fingerprint)

	return harness.Plan{Result: harness.Result{Summary: render(fc)}}, nil
}

func rangeOf(call harness.Call) harness.LineRange {
	if call.Selector.Range != nil {
		return *call.Selector.Range
	}
	return harness.LineRange{}
}

// readFullByteLimit 是单次全文读取进入上下文的字节预算。
//
// 它不是能力上限——带行范围的读取可以取到文件的任何部分——而是单次调用的
// 护栏，防止一次失控的读取把上下文灌满。取值刻意宽松（64KB，约合上万 token）：
// 正常的源码文件与技能正文（规范建议全文 5000 token 内）都远够不到。
const readFullByteLimit = 64 << 10

// render 把读取结果渲染成回灌给模型的文本。
//
// 正文**逐行带行号前缀**，一石三鸟：
//
//   - 换行符归一：CRLF / CR / LF 在字节层是三种东西，逐行切分后模型看到的是
//     行序列——分隔符差异不再进入模型的视野，行号也就不会因平台而错位；
//   - 行定位免费：引用某一行从"计数行为"变成"抄写行为"——模型构造续读范围
//     （Range.From/To）时直接抄回显里的行号，不必自己数换行；
//   - 截断可感知：回显的行号到哪，模型就知道读到了哪，静默截断无处藏身。
//
// 截断必须显式标注位置与总量，这条纪律对两种读取方式同样生效：
//
//   - 行范围读取：workspace 层负责切片并置位 Truncated/TotalLines，这里只转述；
//   - 全文读取：这里负责——超过单次预算时在行边界收刀，标注显示了多少行、
//     共多少行、续读起点是第几行。
//
// 静默截断是最坏的一种失败：模型以为看到了全文，基于缺失的内容做判断，
// 而这类偏差在后续任何一步都不会再暴露出来。
func render(fc harness.FileContent) string {
	if fc.Truncated {
		return fmt.Sprintf("%s（已按行范围截断，共 %d 行）\n%s", fc.Path, fc.TotalLines, numberLines(fc.Raw, 1))
	}
	if len(fc.Raw) <= readFullByteLimit {
		return fmt.Sprintf("%s（%d 行）\n%s", fc.Path, fc.TotalLines, numberLines(fc.Raw, 1))
	}
	// 优先在行边界收刀，别把一行劈成两半；整段没有一个换行符时只好按字节切。
	// 收刀点落在换行符上（含它本身），因此显示段以完整行收尾。
	cut := strings.LastIndexByte(fc.Raw[:readFullByteLimit], '\n')
	if cut <= 0 {
		cut = readFullByteLimit
	} else {
		cut++
	}
	shown := fc.Raw[:cut]
	shownLines := strings.Count(shown, "\n")
	// 续读起点直接给出行号：1-based 闭区间下，已显示第 1..shownLines 行，
	// 下一行就是 shownLines+1。把数字算好放进标注，模型照抄即可——
	// 让它自己从"显示了 3855 行"推出 3856 是一次不必要的算术，也是一次出错机会。
	return fmt.Sprintf("%s（已截断：显示第 1–%d 行，共 %d 行；请从第 %d 行续读）\n%s",
		fc.Path, shownLines, fc.TotalLines, shownLines+1, numberLines(shown, 1))
}

// numberLines 给每行加上右对齐的行号前缀（"    7→原文"）。
//
// 行号按 \n 切分计数；行尾若带 \r（CRLF 文件）一并剥掉——它属于分隔符，
// 不属于内容，剥掉后模型看到的行序列与平台无关。
// 末尾的换行符只表示"最后一行到此结束"，不产生第 N+1 行——空尾段不编号。
// 注意：行号前缀只属于回显，不属于文件内容——edit 的内容寻址匹配的是原文，
// 模型引用时不得把前缀抄进去（这条由工具声明与约定文档说明）。
func numberLines(raw string, first int) string {
	lines := strings.Split(raw, "\n")
	// 以 \n 收尾时 Split 会多出一个空尾段，它不是一行，跳过。
	if n := len(lines); n > 1 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	width := len(fmt.Sprintf("%d", first+len(lines)-1))
	var b strings.Builder
	for i, line := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		num := fmt.Sprintf("%d", first+i)
		for pad := len(num); pad < width; pad++ {
			b.WriteByte(' ')
		}
		b.WriteString(num)
		b.WriteString("→")
		b.WriteString(strings.TrimSuffix(line, "\r"))
	}
	return b.String()
}
