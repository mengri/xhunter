package gates

import (
	"context"
	"reflect"
	"testing"

	"xhunter/git"
	"xhunter/hunt"
)

// stubGit 只交出 ReadFileAtCommit：加载侧的唯一输入是"基线里那一份文本"。
type stubGit struct {
	content []byte
	exists  bool
	err     error
	asked   string
}

func (s *stubGit) PrepareBaseline(context.Context, git.RepoRef) (string, error) { return "root", nil }
func (s *stubGit) Commit(context.Context, git.RepoRef, string) (git.Commit, error) {
	return git.Commit{}, nil
}
func (s *stubGit) Diff(context.Context, git.RepoRef) ([]string, error) { return nil, nil }
func (s *stubGit) Patch(context.Context, git.RepoRef) (string, error)  { return "", nil }
func (s *stubGit) Clean(context.Context) error                         { return nil }
func (s *stubGit) ReadFileAtCommit(_ context.Context, _ git.RepoRef, path string) ([]byte, bool, error) {
	s.asked = path
	return s.content, s.exists, s.err
}

// 通过条件**写在清单里**：同一条命令配不同判据就是不同的门禁，这是这一批要做的事。
func TestParse_ExpectComesFromTheConfig(t *testing.T) {
	content := []byte("gates:\n" +
		"  - name: fmt\n" +
		"    argv: [true]\n" +
		"    timeout: 30s\n" +
		"    required: true\n" +
		"    expect: {kind: empty_output}\n" +
		"  - name: lint\n" +
		"    argv: [true, run]\n" +
		"    expect: {kind: max_count, pattern: \"warning\", max: 5, stream: stderr}\n")
	gates, err := Parse(content)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if len(gates) != 2 {
		t.Fatalf("期望 2 条门禁，实际 %d", len(gates))
	}
	if gates[0].Expect != (hunt.Expect{Kind: hunt.ExpectEmptyOutput}) {
		t.Errorf("第一条判据应原样来自配置：%+v", gates[0].Expect)
	}
	if gates[0].Timeout.String() != "30s" {
		t.Errorf("timeout 应解析为 30s，实际 %s", gates[0].Timeout)
	}
	if !gates[0].Required {
		t.Error("第一条应是 required")
	}
	want := hunt.Expect{Kind: hunt.ExpectMaxCount, Pattern: "warning", Max: 5, Stream: hunt.ExpectStderr}
	if gates[1].Expect != want {
		t.Errorf("第二条判据：期望 %+v，实际 %+v", want, gates[1].Expect)
	}
}

// 未知字段一律报错：静默忽略一个拼错的键，会把"我明明配了"变成"没有门禁"。
func TestParse_RejectsUnknownFields(t *testing.T) {
	if _, err := Parse([]byte("gates:\n  - name: fmt\n    argv: [true]\n    requierd: true\n")); err == nil {
		t.Fatal("拼错的字段应当报错")
	}
	if _, err := Parse([]byte("gates:\n  - name: fmt\n    argv: [true]\n    timeout: 三十秒\n")); err == nil {
		t.Fatal("非法时长应当报错")
	}
}

// 仓库没声明门禁是正常事实，不是错误。
func TestSource_MissingDeclarationIsNone(t *testing.T) {
	st := &stubGit{exists: false}
	src := New(st, "")
	gates, source, err := src.Load(context.Background(), git.RepoRef{BaseCommit: "abc"})
	if err != nil {
		t.Fatalf("没有声明不该报错：%v", err)
	}
	if source != hunt.GateSourceNone || gates != nil {
		t.Errorf("期望 none 档位，实际 %q / %v", source, gates)
	}
	if st.asked != "gates.yml" {
		t.Errorf("缺省路径应是 gates.yml，实际 %q", st.asked)
	}
}

func TestSource_LoadsAndValidatesTheDeclaredList(t *testing.T) {
	st := &stubGit{exists: true, content: []byte("gates:\n  - name: fmt\n    argv: [true]\n    expect: {kind: exit_zero}\n")}
	src := New(st, "ci/gates.yml")
	gates, source, err := src.Load(context.Background(), git.RepoRef{BaseCommit: "abc"})
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if source != hunt.GateSourceRepo {
		t.Errorf("期望 repo 档位，实际 %q", source)
	}
	if len(gates) != 1 || gates[0].Name != "fmt" {
		t.Errorf("期望读到 1 条 fmt，实际 %+v", gates)
	}
	if st.asked != "ci/gates.yml" {
		t.Errorf("应按声明的路径读，实际 %q", st.asked)
	}
}

// 声明了空清单 = 明确不设门禁，与"没有声明"同档。
func TestSource_EmptyListIsNone(t *testing.T) {
	st := &stubGit{exists: true, content: []byte("gates: []\n")}
	_, source, err := New(st, "").Load(context.Background(), git.RepoRef{BaseCommit: "abc"})
	if err != nil {
		t.Fatalf("空清单不该报错：%v", err)
	}
	if source != hunt.GateSourceNone {
		t.Errorf("期望 none 档位，实际 %q", source)
	}
}

func TestSource_ReadFailureIsAnError(t *testing.T) {
	st := &stubGit{exists: true, err: context.DeadlineExceeded}
	if _, _, err := New(st, "").Load(context.Background(), git.RepoRef{BaseCommit: "abc"}); err == nil {
		t.Fatal("读不出来应当报错")
	}
}

// Source 同时是 `hunt.GateSource`：工作区豁免档要用**同一份解析规则**，
// 否则"同一份清单两种读法"会成为最难发现的一类错。
func TestSource_ParseIsTheSameRuleTheLoaderUses(t *testing.T) {
	var _ hunt.GateSource = New(&stubGit{}, "")
	content := []byte("gates:\n  - name: fmt\n    argv: [true]\n    expect: {kind: exit_zero}\n")
	viaFunc, err := Parse(content)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	viaSource, err := New(&stubGit{}, "").Parse(content)
	if err != nil {
		t.Fatalf("按来源解析失败：%v", err)
	}
	if len(viaFunc) != len(viaSource) || !reflect.DeepEqual(viaFunc, viaSource) {
		t.Errorf("两条解析路径应给出同一份清单：%+v vs %+v", viaFunc, viaSource)
	}
}
