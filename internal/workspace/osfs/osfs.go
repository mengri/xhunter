// Package osfs 是「文件操作」这件事的本地实现：一个以目录为根、只认相对路径的工作区。
//
// 它只实现 xhunter/workspace 定义的工作区契约（只读视图 + 唯一写入原语），不含任何业务
// 判断：路径怎么解析、指纹怎么算、目录里什么该被枚举，都是「本地文件系统」这个事实的
// 性质。换成远端快照或内存镜像，就是换一个实现，上层一行不动。
package osfs

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"xhunter/llm"
	"xhunter/workspace"
)

// controlDir 是本系统在工作区里的控制目录：会话材料、门禁清单、技能都在它下面。
// 枚举时要给它留门——跳过隐藏目录是为了躲开版本控制、编辑器与构建产物的噪音，
// 而这个目录是我们的配置，不是噪音。
const controlDir = ".xhunter"

// Opener 按本地文件系统打开工作区。它是装配层注入的那个工厂。
type Opener struct{}

// Open 以给定根目录打开工作区。根为空、不可访问或不是目录，都在这里（初始化阶段）
// 显式失败——工作区的构造与检查前移到打开这一步，检查不过就不会进入后续任何动作。
func (Opener) Open(root string) (workspace.Storage, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("工作区根为空")
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("工作区根不可访问：%w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("工作区根不是目录：%s", root)
	}
	return &storage{root: root}, nil
}

// storage 的 root 是私有字段：工作区根路径因此只存在于这一个结构里，
// 不会经任何接口被读走（接口上的路径一律是工作区相对路径）。
type storage struct {
	root string
}

var _ workspace.Storage = (*storage)(nil)

// resolve 是路径解析与边界校验的唯一实现，三道都拦：绝对路径、".." 字面逃逸、符号链接逃逸。
func (s *storage) resolve(rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return "", &llm.Fault{Kind: "invalid_path", Message: "空路径"}
	}
	if filepath.IsAbs(rel) {
		return "", &llm.Fault{Kind: "invalid_path", Message: "拒绝绝对路径，只接受工作区内相对路径"}
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", &llm.Fault{Kind: "path_escape", Message: fmt.Sprintf("路径逃逸：%q 超出工作区根", rel)}
	}
	abs := filepath.Join(s.root, clean)

	if real, err := filepath.EvalSymlinks(abs); err == nil {
		rootReal, err2 := filepath.EvalSymlinks(s.root)
		if err2 == nil && real != rootReal && !strings.HasPrefix(real, rootReal+string(filepath.Separator)) {
			return "", &llm.Fault{Kind: "path_escape", Message: fmt.Sprintf("符号链接逃逸：%q 指向工作区之外", rel)}
		}
	}
	return abs, nil
}

func (s *storage) Read(rel string, r workspace.LineRange) (workspace.FileContent, error) {
	abs, err := s.resolve(rel)
	if err != nil {
		return workspace.FileContent{}, err
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return workspace.FileContent{}, &llm.Fault{Kind: "not_found", Message: "文件不存在：" + rel, Retryable: true}
		}
		return workspace.FileContent{}, err
	}
	raw := string(b)
	fc := workspace.FileContent{
		Path:        rel,
		Raw:         raw,
		Fingerprint: fingerprint(raw),
		TotalLines:  countLines(raw),
	}
	if r.From > 0 || r.To > 0 {
		fc.Raw, fc.Truncated = sliceLines(raw, r)
	}
	return fc, nil
}

// Stat 用于判断「新建还是改写」。
func (s *storage) Stat(rel string) (workspace.FileInfo, error) {
	abs, err := s.resolve(rel)
	if err != nil {
		return workspace.FileInfo{}, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return workspace.FileInfo{Path: rel, Exists: false}, nil
		}
		return workspace.FileInfo{}, err
	}
	return workspace.FileInfo{Path: rel, Size: st.Size(), Exists: true}, nil
}

// List 返回相对路径列表，按字典序排序。
func (s *storage) List(pattern string) ([]string, error) {
	if filepath.IsAbs(pattern) || strings.Contains(pattern, "..") {
		return nil, &llm.Fault{Kind: "invalid_path", Message: "模式必须是工作区内相对模式"}
	}
	var out []string
	err := filepath.WalkDir(s.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != s.root && strings.HasPrefix(d.Name(), ".") && d.Name() != controlDir {
				return filepath.SkipDir
			}
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
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// WriteRange 把 [br.Start, br.End) 替换为 content，是唯一的写入原语。
// 它不做任何语义判断（改前必读、指纹过期都由上层统一把关），只管把区间换掉。
func (s *storage) WriteRange(rel string, br workspace.ByteRange, content string) (string, error) {
	abs, err := s.resolve(rel)
	if err != nil {
		return "", err
	}
	var cur []byte
	if b, rerr := os.ReadFile(abs); rerr == nil {
		cur = b
	} else if !errors.Is(rerr, fs.ErrNotExist) {
		return "", rerr
	}
	if br.Start < 0 || br.End > len(cur) || br.Start > br.End {
		return "", &llm.Fault{Kind: "bad_range",
			Message: fmt.Sprintf("字节区间 [%d,%d) 超出文件长度 %d", br.Start, br.End, len(cur))}
	}
	next := make([]byte, 0, len(cur)-(br.End-br.Start)+len(content))
	next = append(next, cur[:br.Start]...)
	next = append(next, content...)
	next = append(next, cur[br.End:]...)

	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(abs, next, 0o644); err != nil {
		return "", err
	}
	return fingerprint(string(next)), nil
}

// fingerprint 取内容指纹的前 8 字节十六进制，只用于判断「内容是否变过」。
func fingerprint(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// countLines 按行计数，与通用编辑器口径一致。
func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// sliceLines 取 1-based 闭区间行；越界时收敛到有效范围，并报告是否发生了截断。
func sliceLines(s string, r workspace.LineRange) (string, bool) {
	lines := strings.Split(s, "\n")
	from, to := r.From, r.To
	if from <= 0 {
		from = 1
	}
	if to <= 0 || to > len(lines) {
		to = len(lines)
	}
	if from > len(lines) {
		return "", true
	}
	return strings.Join(lines[from-1:to], "\n"), from > 1 || to < len(lines)
}
