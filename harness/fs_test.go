package harness

import (
	"os"
	"path/filepath"
	"strings"
)

// 框架测试用的最小工作区与 opener。
//
// 真实实现（含路径边界、符号链接校验、隐藏目录规则）在 internal/workspace/osfs，
// 那里有自己的测试；框架侧的测试只需要"能读能写"这一件事，不必重复实现边界语义
// ——而且不该依赖它：框架不允许 import internal（会成环），这本身就说明
// "工作区是注入的"这件事是真的。

type testStorage struct{ root string }

var _ Storage = (*testStorage)(nil)

func (s *testStorage) resolve(rel string) string { return filepath.Join(s.root, rel) }

func (s *testStorage) Read(rel string, r LineRange) (FileContent, error) {
	b, err := os.ReadFile(s.resolve(rel))
	if err != nil {
		return FileContent{}, &ToolError{Kind: "not_found", Message: "文件不存在：" + rel, Retryable: true}
	}
	raw := string(b)
	fc := FileContent{Path: rel, Raw: raw, Fingerprint: "fp:" + rel + ":" + raw, TotalLines: strings.Count(raw, "\n") + 1}
	if r.From > 0 || r.To > 0 {
		lines := strings.Split(raw, "\n")
		from, to := r.From, r.To
		if from <= 0 {
			from = 1
		}
		if to <= 0 || to > len(lines) {
			to = len(lines)
		}
		fc.Raw = strings.Join(lines[from-1:to], "\n")
		fc.Truncated = from > 1 || to < len(lines)
	}
	return fc, nil
}

func (s *testStorage) Stat(rel string) (FileInfo, error) {
	st, err := os.Stat(s.resolve(rel))
	if err != nil {
		return FileInfo{Path: rel, Exists: false}, nil
	}
	return FileInfo{Path: rel, Size: st.Size(), Exists: true}, nil
}

func (s *testStorage) List(pattern string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(s.root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(s.root, p)
		if rerr != nil {
			return nil
		}
		if ok, _ := filepath.Match(pattern, filepath.Base(rel)); ok {
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out, err
}

func (s *testStorage) WriteRange(rel string, br ByteRange, content string) (string, error) {
	abs := s.resolve(rel)
	cur, _ := os.ReadFile(abs)
	if br.Start < 0 || br.End > len(cur) || br.Start > br.End {
		return "", &ToolError{Kind: "bad_range", Message: "区间越界"}
	}
	next := append(append(append([]byte{}, cur[:br.Start]...), content...), cur[br.End:]...)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(abs, next, 0o644); err != nil {
		return "", err
	}
	return "fp:" + rel + ":" + string(next), nil
}

// testWorkspaces 是测试用的 opener：把根直接变成一个测试工作区。
type testWorkspaces struct{}

func (testWorkspaces) Open(root string) (Storage, error) { return &testStorage{root: root}, nil }

// testFS 造一个挂在临时目录上的测试工作区，供需要真实读写的用例使用。
func testFS(t interface{ TempDir() string }) Storage {
	return &testStorage{root: t.TempDir()}
}
