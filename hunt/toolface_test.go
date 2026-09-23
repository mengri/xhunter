package hunt

import (
	"context"
	"strings"
	"testing"

	"xhunter/harness"
	"xhunter/workspace"
)

// 本文件补的是「工具面自身的成立性」：装配层交出来的原语清单必须自洽。
//
// 为什么由框架兜这一道而不是全押给装配层：一份重复或匿名的声明会让供应商直接拒收整个
// 请求，而错误现场在工具面——不在这里挡，调用方只会看到一条无从追起的上游错误。
// 校验只认「工具面自洽」，不认业务名字：有哪些原语、叫什么，是装配层的知识。

func TestBuildTools_RejectsDuplicateName(t *testing.T) {
	s := NewSession(Config{Tools: func(workspace.Workspace) []Primitive {
		return []Primitive{stubPrim{name: "read"}, stubPrim{name: "read"}}
	}})

	err := s.buildTools(&memStorage{})
	if err == nil {
		t.Fatal("重复名字必须被拒——否则会向供应商发出两条同名声明")
	}
	if !strings.Contains(err.Error(), `"read"`) || !strings.Contains(err.Error(), "两次") {
		t.Errorf("错误信息应指明是哪个名字、出了什么事：%v", err)
	}
}

// 匿名原语必须被拒：模型无法调用一个没有名字的工具，而它在工具面上占了一格。
func TestBuildTools_RejectsEmptyName(t *testing.T) {
	s := NewSession(Config{Tools: func(workspace.Workspace) []Primitive {
		return []Primitive{stubPrim{name: ""}}
	}})

	if err := s.buildTools(&memStorage{}); err == nil {
		t.Fatal("声明里 Name 为空必须被拒")
	}
}

// checkpoint 由执行体自带、由 decls 殿后追加：工厂再给一个就会发出两条同名声明。
func TestBuildTools_RejectsCheckpointFromFactory(t *testing.T) {
	s := NewSession(Config{Tools: func(workspace.Workspace) []Primitive {
		return []Primitive{stubPrim{name: PrimCheckpoint}}
	}})

	err := s.buildTools(&memStorage{})
	if err == nil {
		t.Fatal("工厂不得自带 checkpoint：它由 decls 殿后追加")
	}
	if !strings.Contains(err.Error(), string(PrimCheckpoint)) {
		t.Errorf("错误信息应点名 checkpoint：%v", err)
	}
}

// 顺序即工具面顺序：合法清单要原样定格，一个不增一个不减。
func TestBuildTools_KeepsOrderAndAcceptsAFullFace(t *testing.T) {
	s := NewSession(Config{Tools: func(workspace.Workspace) []Primitive {
		return []Primitive{
			stubPrim{name: "read"}, stubPrim{name: "write"}, stubPrim{name: "edit"},
			stubPrim{name: "find"}, stubPrim{name: "glob"},
		}
	}})

	if err := s.buildTools(&memStorage{}); err != nil {
		t.Fatalf("合法清单不该被拒：%v", err)
	}
	want := []PrimitiveName{"read", "write", "edit", "find", "glob"}
	if len(s.order) != len(want) {
		t.Fatalf("顺序长度 = %d，期望 %d：%v", len(s.order), len(want), s.order)
	}
	for i := range want {
		if s.order[i] != want[i] {
			t.Errorf("第 %d 项 = %q，期望 %q（顺序即模型可见顺序）", i, s.order[i], want[i])
		}
	}
}

// 不装工具面不是错误（降级为「无工具」）；但工具面一旦交出来，就必须成立。
func TestBuildTools_NoFactoryIsNotAnError(t *testing.T) {
	if err := NewSession(Config{}).buildTools(&memStorage{}); err != nil {
		t.Errorf("没有工具面不该报错：%v", err)
	}
}

// checkpoint 是模型可见的最后一个工具：它由执行体追加，工厂给不出来。
func TestDecls_AppendsCheckpointLast(t *testing.T) {
	s := NewSession(Config{Tools: func(workspace.Workspace) []Primitive {
		return []Primitive{stubPrim{name: "read"}}
	}})
	if err := s.buildTools(&memStorage{}); err != nil {
		t.Fatalf("构造工具面失败：%v", err)
	}

	decls := s.decls()
	if len(decls) != 2 {
		t.Fatalf("声明数 = %d，期望 2（工厂 1 ＋ checkpoint）：%+v", len(decls), decls)
	}
	if last := decls[len(decls)-1].Name; last != string(PrimCheckpoint) {
		t.Errorf("checkpoint 必须殿后，实得末项 %q", last)
	}
}

// 工具面不成立 = 装配期缺件：在首轮推理之前失败，与缺 git / 缺策略同一出口（退出码 1）。
// 这条守的是「装不起来就起不来」——一个重复声明的工具面不该被带到对话里才暴露。
func TestPrepare_FailsOnBrokenToolFace(t *testing.T) {
	s := NewSession(Config{
		Bounty: Bounty{ID: "b1", Task: "t", Repo: gitRepoRef()},
		Git:    &stubBaselineGit{}, Opener: stubOpener{}, Policy: allowAll{}, Sink: &captureSink{},
		Tools: func(workspace.Workspace) []Primitive {
			return []Primitive{stubPrim{name: "read"}, stubPrim{name: "read"}}
		},
	})

	err := s.Prepare(context.Background(), &harness.Run{})
	if err == nil {
		t.Fatal("工具面不成立必须显式失败")
	}
	if !strings.Contains(err.Error(), "工具面不成立") {
		t.Errorf("错误信息应指明是工具面：%v", err)
	}
}
