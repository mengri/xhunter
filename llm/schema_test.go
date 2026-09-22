package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

// schema 逐字进入每一轮请求：排版空白是纯浪费，但**字符串里的空格是内容**，
// 不能按字符删。
func TestObjectSchema_CompactsSyntaxButKeepsStringContent(t *testing.T) {
	schema := ObjectSchema(`{
		"path": {"type": "string", "description": "read a file"},
		"range": {"type": "object", "properties": {"from": {"type": "integer"}}}
	}`, "path")

	text := string(schema)
	if strings.Contains(text, "\n") || strings.Contains(text, "\t") {
		t.Errorf("排版空白必须压掉：%s", text)
	}
	if strings.Contains(text, `"path": {`) {
		t.Errorf("键与值之间的空白也应压掉：%s", text)
	}
	// 字符串值里的空格原样保留——按字符删空白会把它改成 "readafile"。
	var decoded struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schema, &decoded); err != nil {
		t.Fatalf("产物必须是合法 JSON：%v\n%s", err, text)
	}
	if got := decoded.Properties["path"].Description; got != "read a file" {
		t.Errorf("字符串内容被改动了：%q", got)
	}
}

// required 的名字要按 JSON 规则转义；空列表是 []，不是 null。
func TestObjectSchema_RequiredArrayIsWellFormed(t *testing.T) {
	empty := string(ObjectSchema(`{"a": {"type": "string"}}`))
	if !strings.Contains(empty, `"required":[]`) {
		t.Errorf("没有必填项时应是空数组：%s", empty)
	}
	quoted := string(ObjectSchema(`{"a": {"type": "string"}}`, `we"ird`))
	if !strings.Contains(quoted, `"required":["we\"ird"]`) {
		t.Errorf("required 名字必须转义：%s", quoted)
	}
}

// anyOf 形态：required 落在各分支里，顶层不写。
func TestObjectSchemaAnyOf_BranchesCarryRequired(t *testing.T) {
	schema := string(ObjectSchemaAnyOf(`{"literal": {"type": "string"}, "symbol": {"type": "string"}}`, []string{"literal"}, []string{"symbol"}))
	if !strings.Contains(schema, `"anyOf":[{"required":["literal"]},{"required":["symbol"]}]`) {
		t.Errorf("anyOf 分支不对：%s", schema)
	}
	if strings.Contains(schema, `"type":"object","properties":{"literal"`) == false {
		t.Errorf("属性片段丢失：%s", schema)
	}
	// 顶层不该出现 required。
	if strings.Count(schema, `"required"`) != 2 {
		t.Errorf("required 只应出现在两个分支里：%s", schema)
	}
}

// 片段不合法是编程错误：当场炸，而不是把坏 schema 发给供应商。
func TestObjectSchema_InvalidFragmentPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("非法片段必须当场失败")
		}
	}()
	_ = ObjectSchema(`{"path": }`)
}
