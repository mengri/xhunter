package harness

import "context"

// 框架测试用的最小工具集：名字与参数形状与产品一致，实现是桩。
//
// 框架不该依赖具体业务实现（那些在 internal/primitives 下），所以框架侧的测试
// 自带一套工具——这本身就验证了"框架原样装上一套不同工具集"这件事成立。

const (
	testWrite  = PrimitiveName("write")
	testRead   = PrimitiveName("read")
	testEdit   = PrimitiveName("edit")
	testFind   = PrimitiveName("find")
	testGlob   = PrimitiveName("glob")
	testRename = PrimitiveName("rename")
	testCheck  = PrimitiveName("check")
)

func testTools() []Tool {
	return []Tool{
		{
			Name:    testWrite,
			Decl:    ToolDecl{Name: string(testWrite), Description: "新建文件（测试桩）", Schema: ObjectSchema(`{"path":{"type":"string"},"content":{"type":"string"}}`, "path", "content")},
			Impl:    writeStub{},
			Address: AddressedAsText,
		},
	}
}

// writeStub 直接产出"整文件写入"的编辑计划——够用来跑通流水线，不含真实原语的判断。
type writeStub struct{}

func (writeStub) Plan(_ context.Context, in PlanInput) (Plan, error) {
	return Plan{Edits: []FileEdit{{
		File:       in.Call.Target,
		ByteRange:  ByteRange{},
		NewContent: in.Call.Content,
	}}}, nil
}

// stubFacts 是实现 TaskFacts 的最小桩：够用来驱动单个原语与运行时。
type stubFacts struct {
	storage     Storage
	ledger      *Ledger
	checkpoint  bool
	cpSummary   string
}

func (f *stubFacts) Workspace() Workspace    { return f.storage }
func (f *stubFacts) Gates() []Gate           { return nil }
func (f *stubFacts) Ledger() *Ledger         { return f.ledger }
func (f *stubFacts) Commit([]FileEdit) ([]WriteOp, error) { return nil, nil }
func (f *stubFacts) RequestCheckpoint(summary string) {
	f.checkpoint = true
	f.cpSummary = summary
}
func (f *stubFacts) CheckpointRequested() bool { return f.checkpoint }

var _ TaskFacts = (*stubFacts)(nil)
