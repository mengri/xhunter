// Package osfs 是"文件操作"这件事的本地实现：一个以目录为根、只认相对路径的工作区。
//
// 它只实现 harness 定义的工作区契约（只读视图 + 唯一写入原语），不含任何业务判断：
// 路径怎么解析、指纹怎么算、目录里什么该被枚举，都是"本地文件系统"这个事实的性质。
// 换成远端快照或内存镜像，就是换一个实现，上层一行不动。
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

	"xhunter/harness"
)

// controlDir 是本系统在工作区里的控制目录：会话材料、门禁清单、技能都在它下面。
//
// 枚举时要给它留门——跳过隐藏目录是为了躲开版本控制、编辑器与构建产物的噪音，
// 而这个目录是我们的**配置**，不是噪音：跳过它就等于技能清单这类东西根本看不见。
const controlDir = ".xhunter"

// Opener 按本地文件系统打开工作区。它是装配层注入的那个工厂。
type Opener struct{}

// Open 以给定根目录打开工作区。根为空即装配错误：没有根就没有工作区可言。
func (Opener) Open(root string) (harness.Storage, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("工作区根为空")
	}
	return &storage{root: root}, nil
}

// storage 的 root 是私有字段：工作区根路径因此只存在于这一个结构里，
// 不会经任何接口被读走（接口上的路径一律是工作区相对路径）。
type storage struct {
	root string
}

var _ harness.Storage = (*storage)(nil)

// resolve 是路径解析与边界校验的唯一实现，三道都拦：
//
//	绝对路径          —— 调用方无从表达工作区之外的位置
//	".." 的字面逃逸   —— 挡住 ../ 与嵌套的 ../
//	符号链接逃逸      —— 路径本身在工作区内，但指向了外面
//
// 第三道容易被漏掉：工作区里可能存在一个指向外部的软链，
// 前两道都拦不住它。
func (s *storage) resolve(rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return "", &harness.ToolError{Kind: "invalid_path", Message: "空路径"}
	}
	if filepath.IsAbs(rel) {
		return "", &harness.ToolError{Kind: "invalid_path", Message: "拒绝绝对路径，只接受工作区内相对路径"}
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", &harness.ToolError{Kind: "path_escape", Message: fmt.Sprintf("路径逃逸：%q 超出工作区根", rel)}
	}
	abs := filepath.Join(s.root, clean)

	// 只对已存在的路径做符号链接校验：不存在的路径没有真实目标可查。
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		rootReal, err2 := filepath.EvalSymlinks(s.root)
		if err2 == nil && real != rootReal && !strings.HasPrefix(real, rootReal+string(filepath.Separator)) {
			return "", &harness.ToolError{Kind: "path_escape", Message: fmt.Sprintf("符号链接逃逸：%q 指向工作区之外", rel)}
		}
	}
	return abs, nil
}

func (s *storage) Read(rel string, r harness.LineRange) (harness.FileContent, error) {
	abs, err := s.resolve(rel)
	if err != nil {
		return harness.FileContent{}, err
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return harness.FileContent{}, &harness.ToolError{Kind: "not_found", Message: "文件不存在：" + rel, Retryable: true}
		}
		return harness.FileContent{}, err
	}
	raw := string(b)
	fc := harness.FileContent{
		Path:        rel,
		Raw:         raw,
		Fingerprint: fingerprint(raw),
		TotalLines:  countLines(raw),
	}
	// 按行范围读取时，截断必须显式标记：静默截断会让模型以为看到了全文，
	// 从而基于缺失的内容做判断。
	if r.From > 0 || r.To > 0 {
		fc.Raw, fc.Truncated = sliceLines(raw, r)
	}
	return fc, nil
}

// Stat 用于判断"新建还是改写"——两个语义完全不同的动作，靠它区分。
func (s *storage) Stat(rel string) (harness.FileInfo, error) {
	abs, err := s.resolve(rel)
	if err != nil {
		return harness.FileInfo{}, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return harness.FileInfo{Path: rel, Exists: false}, nil
		}
		return harness.FileInfo{}, err
	}
	return harness.FileInfo{Path: rel, Size: st.Size(), Exists: true}, nil
}

// List 返回相对路径列表，按字典序排序。
// 稳定排序是刻意的：同样的工作区状态每次应当得到同样的顺序，
// 否则同一份上下文在不同次运行里会不一样。
func (s *storage) List(pattern string) ([]string, error) {
	if filepath.IsAbs(pattern) || strings.Contains(pattern, "..") {
		return nil, &harness.ToolError{Kind: "invalid_path", Message: "模式必须是工作区内相对模式"}
	}
	var out []string
	err := filepath.WalkDir(s.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 单个条目读不了不该让整体列举失败
		}
		if d.IsDir() {
			if p != s.root && strings.HasPrefix(d.Name(), ".") && d.Name() != controlDir {
				return filepath.SkipDir // 跳过隐藏目录，主要是 .git
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
//
// 形状单一是有意的：只有"替换区间"这一种表达。有了它，"新建"是往 [0,0) 插入、
// "重命名"是多组区间替换，写入语义因此收敛到一处，不会散到各个原语实现里。
//
// 它**不做任何语义判断**（改前必读、指纹过期都由上层统一把关），只管把区间换掉。
func (s *storage) WriteRange(rel string, br harness.ByteRange, content string) (string, error) {
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
		return "", &harness.ToolError{Kind: "bad_range",
			Message: fmt.Sprintf("字节区间 [%d,%d) 超出文件长度 %d", br.Start, br.End, len(cur))}
	}
	// 逐段拼接：区间之外的字节原样保留。
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

// fingerprint 取内容指纹的前 8 字节十六进制。
// 它只用于判断"内容是否变过"，不用于防篡改，因此不需要全长。
func fingerprint(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// countLines 按行计数，与通用编辑器口径一致：末尾的换行符只表示
// "最后一行到此结束"，不产生一个额外空行——否则同一个文件在"有没有
// 末尾换行"两种写法之间会差出一行，行号回显与续读起点都会跟着漂。
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
func sliceLines(s string, r harness.LineRange) (string, bool) {
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
