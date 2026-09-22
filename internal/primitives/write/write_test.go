package write

import (
	"xhunter/internal/workspace/osfs"
	"context"
	"testing"

	"xhunter/harness"
)

// facts 只实现本原语真正用到的东西：工作区。
// 其余方法靠嵌入的接口占位——真被调用会 panic，这恰好证明 write 不碰它们
// （不提交、不读门禁、不碰检查点意图）：一件只产出"编辑计划"的原语，落盘由引擎统一做。
type facts struct {
	harness.TaskFacts
	ws harness.Workspace
}

func (f *facts) Workspace() harness.Workspace { return f.ws }

func input(t *testing.T, target, content string) harness.PlanInput {
	t.Helper()
	return harness.PlanInput{
		Call:  harness.Call{ID: "c1", Primitive: Name, Target: target, Content: content},
		Route: harness.Route{Path: harness.PathText, Reason: "该原语只有文本路径"},
		Facts: &facts{ws: newStore(t)},
	}
}

func TestPlan_NewFileProducesInsertAtHead(t *testing.T) {
	got, err := impl{}.Plan(context.Background(), input(t, "note.txt", "hello"))
	if err != nil {
		t.Fatalf("新建不该报错：%v", err)
	}
	if got.Result.Err != nil {
		t.Fatalf("新建不该带结构化错误：%+v", got.Result.Err)
	}
	// 形状单一：新建就是"往空文件的开头插入"，写入语义因此收敛在一处。
	if len(got.Edits) != 1 {
		t.Fatalf("编辑数 = %d，期望 1", len(got.Edits))
	}
	e := got.Edits[0]
	if e.File != "note.txt" || e.NewContent != "hello" {
		t.Errorf("编辑 = %+v", e)
	}
	if e.ByteRange != (harness.ByteRange{Start: 0, End: 0}) {
		t.Errorf("区间应为 [0,0)，实际 %+v", e.ByteRange)
	}
}

// 已有文件一律拒绝，并指向正确的做法：整文件重写会顺手抹掉没注意到的内容，
// 而"精确替换"要求先找到目标——这个约束本身就是保护。
func TestPlan_ExistingFileIsRejectedWithGuidance(t *testing.T) {
	in := input(t, "note.txt", "hello")
	st := in.Facts.Workspace().(harness.Storage)
	if _, err := st.WriteRange("note.txt", harness.ByteRange{Start: 0, End: 0}, "已有内容"); err != nil {
		t.Fatal(err)
	}

	got, err := impl{}.Plan(context.Background(), in)
	if err != nil {
		t.Fatalf("撞上已有文件是可自愈的拒绝，不是执行失败：%v", err)
	}
	if got.Result.Err == nil || got.Result.Err.Kind != "file_exists" {
		t.Fatalf("应返回结构化错误：%+v", got.Result)
	}
	if !got.Result.Err.Retryable {
		t.Error("换成 edit 就能继续，所以是可重试的")
	}
	if len(got.Edits) != 0 {
		t.Errorf("被拒绝时不得产出编辑计划：%+v", got.Edits)
	}
}

// 空内容合法：建一个占位文件是正当动作，不该被拦。
func TestPlan_EmptyContentIsAllowed(t *testing.T) {
	got, err := impl{}.Plan(context.Background(), input(t, "placeholder", ""))
	if err != nil {
		t.Fatalf("空内容不该报错：%v", err)
	}
	if len(got.Edits) != 1 || got.Edits[0].NewContent != "" {
		t.Errorf("应产出建空文件的编辑计划：%+v", got.Edits)
	}
}

// 路径不合法由工作区拒绝：原语不自己判断路径，边界只有一处实现。
func TestPlan_InvalidPathPropagates(t *testing.T) {
	if _, err := (impl{}).Plan(context.Background(), input(t, "../escape.txt", "x")); err == nil {
		t.Fatal("越界路径必须被拒绝")
	}
}

func TestTool_Shape(t *testing.T) {
	tool := Tool()
	if tool.Name != Name || tool.Decl.Name != string(Name) {
		t.Errorf("名字与声明必须一致：%+v", tool)
	}
	// 新建没有符号语义：指名要写的路径就是全部定位信息。
	if tool.Address != harness.AddressedAsText {
		t.Errorf("寻址性质 = %q，期望只有文本路径", tool.Address)
	}
	if tool.Impl == nil || len(tool.Decl.Schema) == 0 {
		t.Error("实现与参数形状都必须齐备")
	}
}

// newStore 造一个挂在临时目录上的本地工作区（实现来自 internal/workspace/osfs）。
func newStore(t *testing.T) harness.Storage {
	t.Helper()
	st, err := osfs.Opener{}.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开工作区失败：%v", err)
	}
	return st
}
