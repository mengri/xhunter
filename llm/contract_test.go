package llm

import (
	"reflect"
	"sort"
	"testing"
)

// 契约方法集 / 字段集写死一份期望：一旦有人往这些中立契约上加通道或字段（例如给 Session 加一个
// "权限应答"入口，或给 Provider 加一个执行方法），这里立刻变红——那等于把控制面塞进数据面、或把
// 工具执行委托给供应商，都是本包刻意排除的。

// Session 只有数据面 Events 与控制面 Cancel，不含任何权限应答通道：控制面必须能从消费之外触达。
func TestSession_HasNoPermissionChannel(t *testing.T) {
	want := []string{"Cancel", "Events"}
	got := methodNames(reflect.TypeOf((*Session)(nil)).Elem())
	if !reflect.DeepEqual(got, want) {
		t.Errorf("llm.Session 方法集 = %v，期望字面量 %v（只有 Events/Cancel，不含权限应答通道）", got, want)
	}
}

// Provider 只有 Infer 与 Capabilities：工具执行不委托给 Provider，否则"什么时候真正动手"会变成
// 供应商行为，换一家模型就换掉语义。
func TestProvider_MethodSetIsInferAndCapabilities(t *testing.T) {
	want := []string{"Capabilities", "Infer"}
	got := methodNames(reflect.TypeOf((*Provider)(nil)).Elem())
	if !reflect.DeepEqual(got, want) {
		t.Errorf("llm.Provider 方法集 = %v，期望字面量 %v（工具执行不委托 Provider）", got, want)
	}
}

// Caps 只声明当前真正被用到的能力，且上限类字段来自投递、不得估算。
func TestCaps_DeclaresBehaviourFields(t *testing.T) {
	want := []string{"MaxContextTokens"}
	got := fieldNames(reflect.TypeOf(Caps{}))
	if !reflect.DeepEqual(got, want) {
		t.Errorf("llm.Caps 字段集 = %v，期望字面量 %v（只声明用到的能力）", got, want)
	}
}

// methodNames 给出接口类型的方法名（升序）。
func methodNames(t reflect.Type) []string {
	out := make([]string, 0, t.NumMethod())
	for i := 0; i < t.NumMethod(); i++ {
		out = append(out, t.Method(i).Name)
	}
	sort.Strings(out)
	return out
}

// fieldNames 给出结构体的字段名（升序）。
func fieldNames(t reflect.Type) []string {
	out := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		out = append(out, t.Field(i).Name)
	}
	sort.Strings(out)
	return out
}
