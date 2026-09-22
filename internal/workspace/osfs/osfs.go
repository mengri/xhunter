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
//
// 符号链接这一道必须对**尚不存在的目标**同样成立：写新文件时叶子不存在，EvalSymlinks
// 会失败；若因此跳过检查，路径中间的软链（例如仓库里提交过的 link → 工作区之外）就会被
// 顺着写出去。因此这里逐级向上找到第一个"能被解析"的祖先，对它做解析与边界比较——
// 越界与否由该祖先的真实位置决定，与叶子是否存在无关。
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

	// 根在 Open 时已验证存在；解析失败时退回字面值，后续比较仍按"严格在根之下"判定。
	rootReal, err := filepath.EvalSymlinks(s.root)
	if err != nil {
		rootReal = filepath.Clean(s.root)
	}

	for probe := abs; ; {
		real, err := filepath.EvalSymlinks(probe)
		switch {
		case err == nil:
			if real != rootReal && !strings.HasPrefix(real, rootReal+string(filepath.Separator)) {
				return "", &llm.Fault{Kind: "path_escape", Message: fmt.Sprintf("符号链接逃逸：%q 指向工作区之外", rel)}
			}
			return abs, nil
		case errors.Is(err, fs.ErrNotExist):
			// 这一级还不存在：越界判定交给最近的、真实存在的那一级。
			parent := filepath.Dir(probe)
			if parent == probe {
				return abs, nil
			}
			probe = parent
		default:
			// 解析不了（权限、软链成环、中间级不是目录……）一律不放行：
			// 这是边界，宁可拒绝也不要"因为看不懂所以跳过检查"。
			return "", &llm.Fault{Kind: "invalid_path",
				Message: fmt.Sprintf("路径无法解析：%q（%v）", rel, err)}
		}
	}
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
		fc.Raw, fc.Truncated, fc.FirstLine = sliceLines(raw, r)
	} else {
		fc.FirstLine = 1
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
//
// 模式语义（模型惯用的写法都要能用，而不是静默返回空）：
//   - 不含 "/" 的模式按**文件名**匹配（`*.go` 命中任意深度）；
//   - 含 "/" 的模式按**工作区相对路径**匹配（`internal/*.go`）；
//   - `**/` 前缀表示"任意深度"（`**/*.go` 等价于 `*.go` 按文件名匹配）。
//
// 空模式是显式错误：它一个文件都匹配不到，静默返回空列表会让"调用写错了"看起来像
// "仓库里没有这类文件"。
func (s *storage) List(pattern string) ([]string, error) {
	if strings.TrimSpace(pattern) == "" {
		return nil, &llm.Fault{Kind: "invalid_path", Message: "模式不能为空"}
	}
	if filepath.IsAbs(pattern) || strings.Contains(pattern, "..") {
		return nil, &llm.Fault{Kind: "invalid_path", Message: "模式必须是工作区内相对模式"}
	}

	pat := filepath.ToSlash(pattern)

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
		rel = filepath.ToSlash(rel)
		if matchPattern(pat, rel) {
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// matchPattern 是枚举面的模式匹配：
//
//   - 不含 "/" 的模式按**文件名**匹配，命中任意深度（`*.go`）；
//   - 含 "/" 的模式按**路径分段**匹配，其中整段为 `**` 表示"零到多层目录"
//     （`internal/**/*.go`）；
//   - 每段仍走 filepath.Match 的通配语义（`*` / `?` / `[...]`，不跨 "/"）。
//
// 标准库的 Match 不认 `**`，但它是模型最惯用的写法之一；不认就会静默返回空列表，
// 把"模式写错了"伪装成"仓库里没有这类文件"。
func matchPattern(pattern, rel string) bool {
	if !strings.Contains(pattern, "/") {
		ok, _ := filepath.Match(pattern, filepath.Base(rel))
		return ok
	}
	return matchSegments(strings.Split(pattern, "/"), strings.Split(rel, "/"))
}

func matchSegments(pattern, path []string) bool {
	if len(pattern) == 0 {
		return len(path) == 0
	}
	if pattern[0] == "**" {
		// `**` 吃掉零到任意多层，剩下的模式在每一层后缀上再试。
		for i := 0; i <= len(path); i++ {
			if matchSegments(pattern[1:], path[i:]) {
				return true
			}
		}
		return false
	}
	if len(path) == 0 {
		return false
	}
	ok, _ := filepath.Match(pattern[0], path[0])
	if !ok {
		return false
	}
	return matchSegments(pattern[1:], path[1:])
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
// 同时交出**实际起点行号**：回显给模型的行号必须与文件真实行号一致，否则模型据此
// 写的"第 20 行"会指向别处。
func sliceLines(s string, r workspace.LineRange) (raw string, truncated bool, first int) {
	lines := strings.Split(s, "\n")
	from, to := r.From, r.To
	if from <= 0 {
		from = 1
	}
	if to <= 0 || to > len(lines) {
		to = len(lines)
	}
	if from > len(lines) {
		return "", true, from
	}
	return strings.Join(lines[from-1:to], "\n"), from > 1 || to < len(lines), from
}
