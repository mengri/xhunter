package main

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// 守 FR-1.9「任何形态都走同一份装配」：cmd/xhunter 的**生产代码**里对装配入口
// `xhunter/harness` 的**一次字面调用** `harness.New(`，且必须落在 executeHunt 内。
//
// 守得住：新增一条**字面**装配路径——递归覆盖 cmd/xhunter 全部子目录（含 internal 子包）、
// 把入口写进注释里（先剥注释）、把入口写进字符串字面量里（字符串**内容**按长度掩成空格）、
// 用非默认导入别名（`h "xhunter/harness"`）调用，都能咬住。
// **守不住**（如实记，别当它守得住）：
//   - 把入口存成变量再多路调用（`newEngine := harness.New`）、或经别的包的包装函数间接装配
//     ——需要静态调用图分析，本用例不做；
//   - **跨行调用** `harness.` 换行 `New(`（Go 合法，但 gofmt 会并成一行）；
//   - **名字与括号间带空格** `harness.New (`（needle 不匹配）。
//     后两条都由 `gofmt -l .`（DoD 要求无输出）兜底，不是完全没防。
func TestWiring_SingleAssemblyPath(t *testing.T) {
	var hits []string // "相对路径:行号:别名"
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err // 目录/文件读不了就报错，不静默跳过
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil // 测试文件里出现这个字样不算装配路径
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		alias, ok := harnessAlias(path, src)
		if !ok {
			return nil // 该文件没导入 harness（或不可匹配的 _ / . 形式），不会在这里装配
		}
		code := stripComments(string(src))
		needle := alias + ".New("
		for i, line := range strings.Split(code, "\n") {
			for n := strings.Count(line, needle); n > 0; n-- {
				hits = append(hits, filepath.ToSlash(path)+":"+strconv.Itoa(i+1)+":"+alias)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("扫描 cmd/xhunter 失败：%v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("cmd/xhunter 生产代码里对装配入口的字面调用应恰好一次，实得 %d 次：%v", len(hits), hits)
	}

	// 那一次必须落在 executeHunt 的函数体内——否则就是又冒出一条装配路径。
	parts := strings.Split(hits[0], ":")
	if len(parts) != 3 || parts[0] != "main.go" {
		t.Fatalf("唯一一次装配入口应落在 main.go，实得 %v", hits)
	}
	alias := parts[2]
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("读取 main.go 失败：%v", err)
	}
	code := stripComments(string(src))
	needle := alias + ".New("
	call := strings.Index(code, needle)
	start := strings.Index(code, "func executeHunt(")
	if call < 0 || start < 0 {
		t.Fatalf("定位失败：call=%d start=%d", call, start)
	}
	// executeHunt 之后可能没有别的顶层函数（它是文件里最后一个）——那时函数体一直延伸到文件尾，
	// 区间末端取 len(code)；若在这里 Fatalf 就会在完全合法的装配上报假红。
	end := len(code)
	if next := strings.Index(code[start:], "\nfunc "); next >= 0 {
		end = start + next
	}
	if call < start || call > end {
		t.Errorf("装配入口不在 executeHunt 内（call@%d，executeHunt 区间 [%d,%d]）", call, start, end)
	}
}

// harnessAlias 返回该文件里 `xhunter/harness` 的导入别名（默认即包名 `harness`）。
// 空白导入 `_` 与点导入 `.` 无法用限定名匹配，返回 ok=false 如实跳过（后者理论上可在本文件里
// 裸调 `New`，但本仓不用这种写法，且裸 `New(` 过于泛化，不作匹配）。
func harnessAlias(path string, src []byte) (string, bool) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ImportsOnly)
	if err != nil {
		return "", false
	}
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || p != "xhunter/harness" {
			continue
		}
		if imp.Name == nil {
			return "harness", true
		}
		name := imp.Name.Name
		if name == "_" || name == "." {
			return "", false
		}
		return name, true
	}
	return "", false
}

// stripComments 去掉 Go 源码里的行注释与块注释，并把字符串字面量的**内容**按长度掩成空格
// （保留引号与换行），供"找调用"用：注释与字符串内容都不是代码，真正的装配调用**不可能**
// 出现在字符串字面量里，所以掩掉内容对"找调用"严格更优——既灭掉字符串误报，又不漏真调用。
//
// 三态字符串（"…" / '…' / `…`）都处理；转义（`\"`）不会提前结束字符串；按 rune 等长替换，
// 换行原样保留，因此行号与 `strings.Index` 定位都不漂。
func stripComments(src string) string {
	const (
		normal = iota
		lineComment
		blockComment
		dquote
		squote
		backtick
	)
	var b strings.Builder
	b.Grow(len(src))
	runes := []rune(src)
	state := normal
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch state {
		case normal:
			switch {
			case c == '/' && i+1 < len(runes) && runes[i+1] == '/':
				state = lineComment
				b.WriteRune(' ')
				i++
			case c == '/' && i+1 < len(runes) && runes[i+1] == '*':
				state = blockComment
				b.WriteRune(' ')
				i++
			case c == '"':
				state = dquote
				b.WriteRune(c)
			case c == '\'':
				state = squote
				b.WriteRune(c)
			case c == '`':
				state = backtick
				b.WriteRune(c)
			default:
				b.WriteRune(c)
			}
		case lineComment:
			if c == '\n' {
				state = normal
				b.WriteRune(c)
			}
		case blockComment:
			if c == '*' && i+1 < len(runes) && runes[i+1] == '/' {
				state = normal
				i++
			} else if c == '\n' {
				b.WriteRune(c)
			}
		case dquote:
			switch {
			case c == '\\' && i+1 < len(runes):
				b.WriteRune(' ')
				i++
				b.WriteRune(blank(runes[i]))
			case c == '"':
				state = normal
				b.WriteRune(c)
			default:
				b.WriteRune(blank(c))
			}
		case squote:
			switch {
			case c == '\\' && i+1 < len(runes):
				b.WriteRune(' ')
				i++
				b.WriteRune(blank(runes[i]))
			case c == '\'':
				state = normal
				b.WriteRune(c)
			default:
				b.WriteRune(blank(c))
			}
		case backtick:
			if c == '`' {
				state = normal
				b.WriteRune(c)
			} else {
				b.WriteRune(blank(c))
			}
		}
	}
	return b.String()
}

// blank 把被掩字符换成空格，但**换行原样保留**——行号定位依赖它。
func blank(c rune) rune {
	if c == '\n' {
		return '\n'
	}
	return ' '
}
