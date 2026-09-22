package harness

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// 挽具的意义是"换马不换挽具"，所以这条断言必须是结构性的而不是靠评审：
// Provider 只应当被问到"能做什么"和"发起一次推理"。一旦有人在接口上加一个执行方法，
// 工具执行的时机就变成了供应商行为——换一家模型，任务语义就跟着变。
func TestProviderInterface_DeclaresNoExecution(t *testing.T) {
	want := map[string]bool{"Infer": true, "Capabilities": true}

	typ := reflect.TypeOf((*Provider)(nil)).Elem()
	if typ.NumMethod() != len(want) {
		t.Fatalf("Provider 接口方法数 = %d，期望 %d（Infer、Capabilities）", typ.NumMethod(), len(want))
	}
	for i := 0; i < typ.NumMethod(); i++ {
		if name := typ.Method(i).Name; !want[name] {
			t.Errorf("Provider 接口出现方法 %s：工具执行不委托给供应商", name)
		}
	}

	sess := reflect.TypeOf((*Session)(nil)).Elem()
	if sess.NumMethod() != 2 {
		t.Errorf("Session 接口方法数 = %d，期望 2（Events、Cancel）", sess.NumMethod())
	}
}

// 供应商名字不该出现在 Harness 里。静态断言比人工检查可靠：
// 名字一旦出现，就说明某个编排决策开始依赖"是哪一家"，而这类依赖会以
// "换模型后行为不同"的形式暴露，很难追。
func TestHarness_KnowsNoVendor(t *testing.T) {
	vendors := []string{
		"openai", "anthropic", "claude", "gemini", "deepseek", "qwen", "mistral", "llama",
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("读取包目录失败：%v", err)
	}

	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("读取 %s 失败：%v", name, err)
		}
		scanned++
		lower := strings.ToLower(string(raw))
		for _, v := range vendors {
			if strings.Contains(lower, v) {
				t.Errorf("%s 中出现供应商名字 %q：Harness 不应知道具体是哪一家", name, v)
			}
		}
	}
	if scanned == 0 {
		t.Fatal("没有扫描到任何源文件——断言本身失效了")
	}
}

// 工具声明是模型可见的契约，必须恒定：它只取决于装配层给的清单，
// **不受环境能力影响**。环境能力只改变执行路径（符号 / 文本 / 结构化错误）。
func TestVisibleToolDecls_AreIndependentOfEnvironment(t *testing.T) {
	tools := testTools()
	all := VisibleToolDecls(tools)
	if len(all) != 2 {
		t.Fatalf("声明数 = %d，期望 2（清单里的 1 个 + 控制原语）", len(all))
	}
	for _, d := range all {
		if d.Name == "" || d.Description == "" {
			t.Errorf("声明不完整：%+v", d)
		}
	}
	if last := all[len(all)-1].Name; last != string(PrimCheckpoint) {
		t.Errorf("控制原语应殿后，实际末位 %q", last)
	}

	// 声明是清单的纯函数：同一个清单算两次，结果必须逐字相同（顺序也一致），
	// 否则模型看到的前缀会不稳定，缓存与契约一起失效。
	again := VisibleToolDecls(tools)
	if !reflect.DeepEqual(declNames(all), declNames(again)) {
		t.Errorf("同一份清单两次推出的声明不一致：%v → %v", declNames(all), declNames(again))
	}
	// 顺序即清单顺序：换个顺序，声明顺序跟着换（框架不排序、不筛选）。
	reordered := VisibleToolDecls([]Tool{tools[0], tools[0]})
	if len(reordered) != 3 {
		t.Errorf("框架不得去重或筛选：%d", len(reordered))
	}
}

func declNames(decls []ToolDecl) []string {
	out := make([]string, 0, len(decls))
	for _, d := range decls {
		out = append(out, d.Name)
	}
	return out
}

// harness 是可被其他项目单独引用的核心库，因此它**只能依赖比它更低的中立层**
// （目前是 `llm` 的对话与模型契约），不能依赖 cmd 与 CLI 侧的 internal 包——
// 一旦依赖后者，复用方就得把整个 CLI 与部署细节一起带上。
func TestHarness_DependsOnlyOnNeutralLayers(t *testing.T) {
	forbidden := []string{`"xhunter/cmd/`, `"xhunter/internal/`}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("读取包目录失败：%v", err)
	}

	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("读取 %s 失败：%v", name, err)
		}
		scanned++
		for _, line := range strings.Split(string(raw), "\n") {
			for _, bad := range forbidden {
				if strings.Contains(line, bad) {
					t.Errorf("%s 依赖了 CLI 侧：%s（harness 必须能脱离 CLI 被单独引用）",
						name, strings.TrimSpace(line))
				}
			}
		}
	}
	if scanned == 0 {
		t.Fatal("没有扫描到任何源文件——断言本身失效了")
	}
}
