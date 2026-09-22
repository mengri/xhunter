package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// ObjectSchema 把「属性名 → 属性定义」的 JSON 片段包成标准形状，用于构造 ToolDecl.Schema。
//
// required 与 additionalProperties 都必须写死：绑定侧拒绝未知字段，schema 若允许额外
// 字段，模型照 schema 多传一个就会吃到参数错误——描述与实现必须是同一句话。
//
// 片段写成多行更好维护，但发往上游的字节必须紧凑（它逐字进入每一轮请求），
// 所以这里顺手压掉空白。片段本身必须是合法 JSON——不合法就当场炸（编程错误），
// 而不是把一段坏 schema 发给供应商，让错误在模型那一侧以奇怪的方式暴露。
func ObjectSchema(properties string, required ...string) json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":` + compactJSON(properties) +
		`,"required":` + jsonArray(required) + `,"additionalProperties":false}`)
}

// ObjectSchemaAnyOf 用于「给其一即可」的参数组（如按内容检索还是按符号定位）：
// required 不写在顶层，而由 anyOf 的每个分支各自给出。
func ObjectSchemaAnyOf(properties string, anyOf ...[]string) json.RawMessage {
	branches := make([]string, 0, len(anyOf))
	for _, req := range anyOf {
		branches = append(branches, `{"required":`+jsonArray(req)+`}`)
	}
	return json.RawMessage(`{"type":"object","properties":` + compactJSON(properties) +
		`,"anyOf":[` + strings.Join(branches, ",") + `],"additionalProperties":false}`)
}

// compactJSON 压掉片段里的排版空白。
//
// 用标准库的 Compact，而不是自己删空白字符：**字符串值里的空格是内容**，
// 按字符删会改掉 "description": "read a file" 这类默认值，而 schema 是发给模型看的。
func compactJSON(fragment string) string {
	var buf bytes.Buffer
	trimmed := strings.TrimSpace(fragment)
	if !json.Valid([]byte(trimmed)) {
		panic(fmt.Sprintf("llm：schema 属性片段不是合法 JSON：%s", trimmed))
	}
	if err := json.Compact(&buf, []byte(trimmed)); err != nil {
		panic(fmt.Sprintf("llm：schema 属性片段无法压缩：%v", err))
	}
	return buf.String()
}

// jsonArray 把名字列表编成 JSON 数组。空列表是 `[]` 而不是 `null`——
// `"required": null` 与 `"required": []` 在 schema 里是两种意思。
func jsonArray(items []string) string {
	if items == nil {
		items = []string{}
	}
	b, err := json.Marshal(items)
	if err != nil {
		panic(fmt.Sprintf("llm：required 列表无法编码：%v", err))
	}
	return string(b)
}
