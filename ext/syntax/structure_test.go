package syntax

import (
	"context"
	"strings"
	"testing"

	"xhunter/ext"
	"xhunter/workspace"
)

// 本文件钉住**结构判据**这条能力：语法完整（判据①）与"改动封闭在哪个符号里"（判据②）。
// 两条都要能说"判不了"——未注册的语言既谈不上完整，也谈不上封闭。

func TestHost_ParseOKOnCompleteSyntax(t *testing.T) {
	if v := host(map[string]string{"a.go": sample}).Parse(context.Background(), "a.go"); v != ext.ParseOK {
		t.Errorf("完整语法应判 OK，实际 %d", v)
	}
}

// 语法不完整是**这份文件的事实**（三态里唯一的确定否定），不是"判不了"。
func TestHost_ParseBrokenOnIncompleteSyntax(t *testing.T) {
	broken := "package demo\n\nfunc Alpha() string {\n\treturn \"alpha\"\n"
	if v := host(map[string]string{"a.go": broken}).Parse(context.Background(), "a.go"); v != ext.ParseBroken {
		t.Errorf("缺少右括号应判 Broken，实际 %d", v)
	}
}

// 未注册的语言：没判过。读成 Broken 会让未注册语言的仓库每轮都被判"结构不完整"。
func TestHost_ParseUnknownOnUnregisteredLanguage(t *testing.T) {
	if v := host(map[string]string{"a.txt": "hello"}).Parse(context.Background(), "a.txt"); v != ext.ParseUnknown {
		t.Errorf("未注册语言应判 Unknown，实际 %d", v)
	}
}

// 读不到（文件不在工作区里）同样是"没判过"，而不是"不完整"。
func TestHost_ParseUnknownWhenFileIsUnreadable(t *testing.T) {
	if v := host(nil).Parse(context.Background(), "a.go"); v != ext.ParseUnknown {
		t.Errorf("读不到应判 Unknown，实际 %d", v)
	}
}

// 判据②取**最小**的那个包含者：函数体里的一处改动落在函数里，不是"落在文件里"。
func TestHost_EncloseFindsTheSmallestEnclosingDeclaration(t *testing.T) {
	h := host(map[string]string{"a.go": sample})
	at := strings.Index(sample, "return \"alpha\"")
	prep, found, err := h.Enclose(context.Background(), ext.EncloseRequest{
		File:      "a.go",
		ByteRange: workspace.ByteRange{Start: at, End: at + 6},
	})
	if err != nil || !found {
		t.Fatalf("函数体内的一处改动应判为封闭：found=%v err=%v", found, err)
	}
	wantStart := strings.Index(sample, "func Alpha")
	wantEnd := strings.Index(sample, "}\n\nfunc Beta") + 1
	if prep.ByteRange.Start != wantStart || prep.ByteRange.End != wantEnd {
		t.Errorf("封闭者应是 Alpha 的声明 [%d,%d)，实际 [%d,%d)",
			wantStart, wantEnd, prep.ByteRange.Start, prep.ByteRange.End)
	}
	if !prep.Impact.Unknown {
		t.Error("语法级后端无法穷尽引用：规模必须带 Unknown")
	}
}

// 落在声明之间（包声明那一段）→ 判过、**确实**没有声明包含它：这是结论，不是"判不了"。
func TestHost_EncloseReportsNotEnclosedOutsideDeclarations(t *testing.T) {
	h := host(map[string]string{"a.go": sample})
	prep, found, err := h.Enclose(context.Background(), ext.EncloseRequest{
		File:      "a.go",
		ByteRange: workspace.ByteRange{Start: 0, End: 6}, // "package"
	})
	if err != nil {
		t.Fatalf("判得了就不该报错：%v", err)
	}
	if found {
		t.Errorf("包声明不属于任何符号：应判为不封闭，实际 %+v", prep)
	}
}

// 改动横跨两个声明 → 同样是不封闭（这是"结构完整点"的核心：一次改动属于一个符号）。
func TestHost_EncloseReportsNotEnclosedAcrossDeclarations(t *testing.T) {
	h := host(map[string]string{"a.go": sample})
	prep, found, err := h.Enclose(context.Background(), ext.EncloseRequest{
		File:      "a.go",
		ByteRange: workspace.ByteRange{Start: strings.Index(sample, "func Alpha"), End: strings.Index(sample, "func Beta") + 5},
	})
	if err != nil {
		t.Fatalf("判得了就不该报错：%v", err)
	}
	if found {
		t.Errorf("横跨两个声明的区间不该判为封闭：%+v", prep)
	}
}

// 未注册语言：Enclose 必须报"判不了"（error），不能报"没有声明包含它"——
// 后者是结论，会让未注册语言的仓库每轮都被判"改动不封闭"。
func TestHost_EncloseOnUnregisteredLanguageIsNotAVerdict(t *testing.T) {
	h := host(map[string]string{"a.txt": "hello"})
	_, found, err := h.Enclose(context.Background(), ext.EncloseRequest{
		File: "a.txt", ByteRange: workspace.ByteRange{Start: 0, End: 3},
	})
	if err == nil {
		t.Fatal("未注册语言应报判不了，而不是给出是否封闭的结论")
	}
	if found {
		t.Error("判不了时不得同时给出'封闭'的结论")
	}
}

// 语法残缺时判不了封闭：残缺的文件本就无从谈边界。
func TestHost_EncloseOnBrokenSyntaxIsNotAVerdict(t *testing.T) {
	h := host(map[string]string{"a.go": "package demo\n\nfunc Alpha() {"})
	if _, _, err := h.Enclose(context.Background(), ext.EncloseRequest{
		File: "a.go", ByteRange: workspace.ByteRange{Start: 20, End: 24},
	}); err == nil {
		t.Fatal("语法残缺时应报判不了")
	}
}

// 空区间（纯插入）按其位置算：在声明体内插入仍然属于这个声明。
func TestHost_EncloseCountsEmptyRangeByPosition(t *testing.T) {
	h := host(map[string]string{"a.go": sample})
	at := strings.Index(sample, "return \"alpha\"")
	_, found, err := h.Enclose(context.Background(), ext.EncloseRequest{
		File: "a.go", ByteRange: workspace.ByteRange{Start: at, End: at},
	})
	if err != nil || !found {
		t.Errorf("声明体内的插入点应判为封闭：found=%v err=%v", found, err)
	}
}
