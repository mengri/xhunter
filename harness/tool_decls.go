package harness

import (
	"encoding/json"
	"strings"
)

// 工具声明：模型可见的形状只此一份——由装配层给的清单推出来，框架不自己维护一张表。
//
// 说明文字与工具集规格一一对应，不重复描述参数能表达的东西（那属于参数形状）。
// 参数形状是 JSON Schema，与 MCP 的 inputSchema、两家 function calling 的参数格式同源，
// 一份声明可以原样交给任意一方——适配器只翻译信封，不碰形状。
//
// 两条纪律由测试守着（在装配层，因为只有那里知道完整清单）：
//   - schema 里声明的每个属性名，绑定层都必须收得下（否则模型照 schema 填的参数会被拒，
//     而错在它看不到的地方）；
//   - schema 只说绑定层实际接受的事：`additionalProperties: false` 与绑定的
//     "拒绝未知字段"是同一句话的两处表述。

// VisibleToolDecls 按给定顺序给出模型可见的工具声明，控制原语殿后。
//
// 顺序即模型看到的顺序，因此它属于前缀缓存的一部分：清单由装配层排定，
// 框架只负责保持——不排序、不筛选、不因环境增删（环境能力只影响执行路径）。
func VisibleToolDecls(tools []Tool) []ToolDecl {
	out := make([]ToolDecl, 0, len(tools)+1)
	for _, t := range tools {
		out = append(out, t.Decl)
	}
	return append(out, controlDecl)
}

// controlDecl 是控制原语的声明。它由框架自带——"请求检查点"是循环自身的词汇，
// 不属于业务工具集。
//
// summary 是它唯一的参数：一句话说明为什么这里值得留检查点，引擎会把它合入提交信息
// 供人评审。声明里写明"每轮至多一次"，模型不必试探。
var controlDecl = ToolDecl{
	Name: string(PrimCheckpoint),
	Description: "请求在此刻创建一个检查点。每轮至多生效一次；何时真正提交、提交信息如何组织由系统决定，" +
		"你只需给出 summary：一句话说明为什么这里值得留检查点（做了什么、为什么自洽）。",
	Schema: ObjectSchema(`{
			"summary": {"type": "string", "description": "为什么这里值得留检查点（一句话）"}
		}`, "summary"),
}

// ObjectSchema 把"属性名 → 属性定义"的 JSON 片段包成标准形状。
//
// required 与 additionalProperties 都必须写死：绑定层拒绝未知字段（DisallowUnknownFields），
// schema 若允许额外字段，模型照 schema 多传一个就会吃到参数错误——描述与实现必须是
// 同一句话，这是参数形状定格的全部意义。
//
// 片段写成多行更好维护，但发往上游的字节必须紧凑（它逐字进入每一轮请求），
// 所以这里顺手压掉空白。片段本身必须是合法 JSON，合法性由装配层的测试守住。
func ObjectSchema(properties string, required ...string) json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":` + compact(properties) +
		`,"required":` + jsonArray(required) + `,"additionalProperties":false}`)
}

// ObjectSchemaAnyOf 用于"给其一即可"的参数组（如按内容检索还是按符号定位）：
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
