package openaichat

import (
	"strings"
	"testing"

	"xhunter/llm"
)

func TestToWireMessages_UnknownRoleIsRejected(t *testing.T) {
	_, err := toWireMessages([]llm.Message{{Role: llm.Role("human"), Content: "x"}})
	if err == nil {
		t.Fatal("未知角色必须显式报错：角色决定消息被当成什么，透传会让拼错的角色名变成对端的静默行为")
	}
	if !strings.Contains(err.Error(), "human") {
		t.Errorf("错误信息 = %q，应带上原始角色名，否则无从定位是哪里拼错了", err.Error())
	}
}

func TestToWireMessages_PairsResultsOneByOne(t *testing.T) {
	msgs, err := toWireMessages([]llm.Message{{Role: llm.RoleTool, Results: []llm.ToolResult{
		{CallID: "c1", Output: "已读取 10 行"},
		{CallID: "c2", Output: "工具执行失败[not_found，不可重试]：匹配不到唯一位置", IsError: true},
	}}})
	if err != nil {
		t.Fatalf("翻译失败：%v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("消息数 = %d，期望一次调用一条结果消息（上游按调用标识配对）", len(msgs))
	}
	if msgs[0].ToolCallID != "c1" || msgs[0].Content != "已读取 10 行" {
		t.Errorf("成功结果 = %+v", msgs[0])
	}
	if msgs[1].ToolCallID != "c2" || !strings.Contains(msgs[1].Content, "not_found") {
		t.Errorf("失败结果 = %+v，类型与可重试性必须带上", msgs[1])
	}
}

func TestToWireTools_PassesSchemaThrough(t *testing.T) {
	tools := toWireTools([]llm.ToolDecl{
		{Name: "read", Description: "读文件", Schema: []byte(`{"type":"object","required":["path"]}`)},
		{Name: "glob", Description: "匹配文件名"},
	})
	if len(tools) != 2 {
		t.Fatalf("工具数 = %d", len(tools))
	}
	if tools[0].Type != "function" || tools[0].Function.Name != "read" {
		t.Errorf("工具声明 = %+v", tools[0])
	}
	if string(tools[0].Function.Parameters) != `{"type":"object","required":["path"]}` {
		t.Errorf("参数形状应原样搬运，实得 %s", tools[0].Function.Parameters)
	}
	if len(tools[1].Function.Parameters) != 0 {
		t.Errorf("没有形状的声明不得补空壳，实得 %s", tools[1].Function.Parameters)
	}
}
