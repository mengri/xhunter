package harness

import "testing"

// 分发是纯函数，可以完全不碰文件系统、不依赖时钟地验证。
//
// 路径由**原语的寻址性质**决定，而寻址性质是装配层随原语给的数据——
// 所以这里用三个性质值当输入，而不是按名字分支（框架不认识具体原语）。
func TestDispatch(t *testing.T) {
	full := ExtCaps{Available: true, Languages: []string{"go"}, CanResolve: true}
	goReady := FileState{Lang: "go", Registered: true, ParseOK: true}
	bySelector, textOnly, symbolOnly := AddressedBySelector, AddressedAsText, AddressedAsSymbol

	cases := []struct {
		name  string
		call  Call
		addr  Addressing
		caps  ExtCaps
		state FileState
		want  PathKind
	}{
		{
			name: "内容寻址走文本路径",
			call: Call{Primitive: testEdit, Target: "a.go", Selector: Selector{Literal: "foo"}},
			addr: bySelector, caps: full, state: goReady, want: PathText,
		},
		{
			name: "符号寻址且语言可用时走符号路径",
			call: Call{Primitive: testRead, Target: "a.go", Selector: Selector{Symbol: "pkg.Fn"}},
			addr: bySelector, caps: full, state: goReady, want: PathSymbol,
		},
		{
			name: "扩展不可用时降级文本",
			call: Call{Primitive: testRead, Target: "a.go", Selector: Selector{Symbol: "pkg.Fn"}},
			addr: bySelector, caps: ExtCaps{Available: false}, state: FileState{Lang: "go"}, want: PathText,
		},
		{
			name: "语言未注册时降级文本",
			call: Call{Primitive: testRead, Target: "a.md", Selector: Selector{Symbol: "title"}},
			addr: bySelector, caps: full, state: FileState{Lang: "markdown"}, want: PathText,
		},
		{
			name: "语法不可解析时降级文本",
			call: Call{Primitive: testEdit, Target: "a.go", Selector: Selector{Symbol: "pkg.Fn"}},
			addr: bySelector, caps: full, state: FileState{Lang: "go", Registered: true, ParseOK: false}, want: PathText,
		},
		{
			name: "仅符号寻址的原语不做降级",
			call: Call{Primitive: testRename, Target: "a.go"},
			addr: symbolOnly, caps: full, state: goReady, want: PathSymbol,
		},
		{
			name: "仅符号寻址的原语在环境不可用时仍落在符号路径（由运行时给出结构化错误）",
			call: Call{Primitive: testRename, Target: "a.go"},
			addr: symbolOnly, caps: ExtCaps{Available: false}, state: FileState{Lang: "go"}, want: PathSymbol,
		},
		{
			name: "只有文本路径的原语：选择器写成什么都不改变路径",
			call: Call{Primitive: testGlob, Target: "*.go", Selector: Selector{Symbol: "ignored"}},
			addr: textOnly, caps: full, state: goReady, want: PathText,
		},
		{
			name: "读取符号视图属于符号寻址",
			call: Call{Primitive: testRead, Target: "a.go", Selector: Selector{FileView: true}},
			addr: bySelector, caps: full, state: goReady, want: PathSymbol,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Dispatch(tc.call, tc.addr, tc.caps, tc.state)
			if got.Path != tc.want {
				t.Fatalf("路径 = %s，期望 %s（原因：%s）", got.Path, tc.want, got.Reason)
			}
			if got.Reason == "" {
				t.Fatal("必须给出选择或降级的原因，否则模型无从判断发生了什么")
			}
		})
	}
}

// 校验的价值在于：不合法的编排在启动期就被拒绝，而不是跑到一半才暴露。
func TestPipelineValidate(t *testing.T) {
	if err := DefaultPipeline().Validate(); err != nil {
		t.Fatalf("默认编排必须合法，实际：%v", err)
	}
	if err := (Pipeline{}).Validate(); err != nil {
		t.Fatalf("零值应当归一化为默认编排并合法，实际：%v", err)
	}

	cases := []struct {
		name string
		mut  func(*Pipeline)
	}{
		{"基线准备不在最前", func(p *Pipeline) {
			p.Pre = []StageID{StageExtCaps, StagePrepareBaseline}
		}},
		{"缺少取消守卫", func(p *Pipeline) {
			p.Turn.Guards = []StageID{StageGuardBudget}
		}},
		{"缺少预算守卫", func(p *Pipeline) {
			p.Turn.Guards = []StageID{StageGuardCancel}
		}},
		{"缺少唯一写盘入口", func(p *Pipeline) {
			p.Turn.Steps = []StageID{StageProviderInfer}
		}},
		{"缺少推理环节", func(p *Pipeline) {
			p.Turn.Steps = []StageID{StageToolsExecute}
		}},
		{"接收排在执行之后", func(p *Pipeline) {
			p.Turn.Steps = []StageID{StageProviderInfer, StageToolsExecute, StageStreamReceive}
		}},
		{"检查点排在执行之前", func(p *Pipeline) {
			p.Turn.Steps = []StageID{StageProviderInfer, StageCheckpointCommit, StageToolsExecute}
		}},
		{"环节放错区段", func(p *Pipeline) {
			p.Pre = []StageID{StagePrepareBaseline, StageGuardCancel}
		}},
		{"同区段重复环节", func(p *Pipeline) {
			p.Post = append(p.Post, StageDeliveryCommit)
		}},
		{"缺少交付提交", func(p *Pipeline) {
			p.Post = []StageID{StageEventHuntEnd}
		}},
		{"终态上报不在最后", func(p *Pipeline) {
			p.Post = []StageID{StageDeliverableDiff, StageEventHuntEnd, StageDeliveryCommit}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := DefaultPipeline()
			tc.mut(&p)
			if err := p.Validate(); err == nil {
				t.Fatal("该编排应当被拒绝，但通过了校验")
			}
		})
	}
}

// 未注册的环节应当在编译链时就失败，而不是等运行到那一步。
func TestBuildRejectsUnregisteredStage(t *testing.T) {
	reg := map[StageID]HandlerFunc{}
	if _, err := build([]StageID{StageGuardCancel}, reg); err == nil {
		t.Fatal("环节未注册时应当编译失败")
	}
}
