package basic

import (
	"encoding/json"
	"sort"
	"testing"

	"xhunter/hunt"
	"xhunter/internal/workspace/osfs"
	"xhunter/llm"
	"xhunter/workspace"
)

// facts 只实现被测原语真正用到的那一部分，其余方法靠嵌入的接口占位——真被调用即 panic。
// 这恰好把「这个原语到底依赖什么」钉住：write / glob 完全不碰台账与门禁。
type facts struct {
	hunt.Facts
	ledger *hunt.Ledger
}

func newFacts() *facts { return &facts{ledger: &hunt.Ledger{}} }

func (f *facts) Ledger() *hunt.Ledger { return f.ledger }

// newStore 造一个挂在临时目录上的本地工作区（实现来自 internal/workspace/osfs）。
func newStore(t *testing.T) workspace.Storage {
	t.Helper()
	st, err := osfs.Opener{}.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开工作区失败：%v", err)
	}
	return st
}

// seed 往工作区里落一个文件（准备夹具）。
func seed(t *testing.T, st workspace.Storage, path, content string) {
	t.Helper()
	if _, err := st.WriteRange(path, workspace.ByteRange{}, content); err != nil {
		t.Fatalf("准备夹具失败：%v", err)
	}
}

// schemaShape 是声明里可被断言的部分。
type schemaShape struct {
	Properties           map[string]any `json:"properties"`
	Required             []string       `json:"required"`
	AdditionalProperties *bool          `json:"additionalProperties"`
}

// decodeSchema 解开一个声明，顺带守住两条通用纪律：合法 JSON、声明了说明与名字。
func decodeSchema(t *testing.T, d llm.ToolDecl) schemaShape {
	t.Helper()
	if d.Name == "" || d.Description == "" {
		t.Fatalf("声明必须自带名字与说明：%+v", d)
	}
	if !json.Valid(d.Schema) {
		t.Fatalf("%s 的参数形状不是合法 JSON：%s", d.Name, d.Schema)
	}
	var s schemaShape
	if err := json.Unmarshal(d.Schema, &s); err != nil {
		t.Fatalf("解不开 %s 的参数形状：%v", d.Name, err)
	}
	return s
}

// propNames 按名字排序给出参数面——断言"参数面精确等于这些"用，
// 避免用子串匹配（"description" 里就含 "script"）。
func propNames(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func head(s string) string {
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}

func tail(s string) string {
	if len(s) > 60 {
		return "…" + s[len(s)-60:]
	}
	return s
}
