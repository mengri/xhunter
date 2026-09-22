package llm

import (
	"encoding/json"
	"strings"
)

// ObjectSchema 把「属性名 → 属性定义」的 JSON 片段包成标准形状，用于构造 ToolDecl.Schema。
//
// required 与 additionalProperties 都必须写死：绑定侧拒绝未知字段，schema 若允许额外
// 字段，模型照 schema 多传一个就会吃到参数错误——描述与实现必须是同一句话。
//
// 片段写成多行更好维护，但发往上游的字节必须紧凑（它逐字进入每一轮请求），
// 所以这里顺手压掉空白。片段本身必须是合法 JSON。
func ObjectSchema(properties string, required ...string) json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":` + compact(properties) +
		`,"required":` + jsonArray(required) + `,"additionalProperties":false}`)
}

// ObjectSchemaAnyOf 用于「给其一即可」的参数组（如按内容检索还是按符号定位）：
// required 不写在顶层，而由 anyOf 的每个分支各自给出。
func ObjectSchemaAnyOf(properties string, anyOf ...[]string) json.RawMessage {
	branches := make([]string, 0, len(anyOf))
	for _, req := range anyOf {
		branches = append(branches, `{"required":`+jsonArray(req)+`}`)
	}
	return json.RawMessage(`{"type":"object","properties":` + compact(properties) +
		`,"anyOf":[` + strings.Join(branches, ",") + `],"additionalProperties":false}`)
}

// compact 去掉片段里的换行与缩进：schema 会被逐字发给上游，注释式的排版只浪费字节。
// 它只做去空白，不做任何 JSON 修补——片段本身就是合法 JSON。
func compact(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '\n', '\t', '\r':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func jsonArray(items []string) string {
	if len(items) == 0 {
		return "[]"
	}
	quoted := make([]string, 0, len(items))
	for _, s := range items {
		quoted = append(quoted, `"`+s+`"`)
	}
	return "[" + strings.Join(quoted, ",") + "]"
}
