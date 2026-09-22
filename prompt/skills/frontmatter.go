package skills

// frontmatter 解析：只取 name 与 description 两个平面标量。
//
// 刻意不引 YAML 依赖——核心"零第三方依赖"这条不能被一份技能说明破掉，而规范要求的
// 字段本身就是平面标量，标准库足够。
//
// 代价是看不懂的形态一律按"格式非法"处理，而不是猜：嵌套结构、多行块、锚点、行内
// 映射全都不支持。宁可跳过一条并留下降级记录，也不要凭猜测注入一段含义不明的说明。

import (
	"errors"
	"fmt"
	"strings"
)

// metadata 是清单唯一需要的两个字段。
type metadata struct {
	name        string
	description string
}

// 规范给出的长度约束（FR-15.1）。
const (
	maxNameLen = 64
	maxDescLen = 1024
)

// parseFrontmatter 解析文件开头的 YAML frontmatter。
//
// 只认两种写法之外的第三件事：未知字段一律忽略（规范允许 license、allowed-tools 等，
// 而**素材不得自我授权**，所以那些字段连读都不必读）。
func parseFrontmatter(raw string) (metadata, error) {
	// 统一换行：CRLF 文件不该因为平台差异被判非法。
	text := strings.ReplaceAll(raw, "\r\n", "\n")
	text = strings.TrimPrefix(text, "\ufeff") // 编辑器可能留下 BOM，它是产物不是内容
	lines := strings.Split(text, "\n")

	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "" && start < 0 {
			continue // 开头的空行容忍，它不是内容错误
		}
		if strings.TrimSpace(line) != "---" {
			return metadata{}, errors.New("缺少 frontmatter 起始标记 ---")
		}
		start = i
		break
	}
	if start < 0 {
		return metadata{}, errors.New("缺少 frontmatter 起始标记 ---")
	}

	end := -1
	for i := start + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return metadata{}, errors.New("frontmatter 没有结束标记 ---")
	}

	var m metadata
	for _, line := range lines[start+1 : end] {
		key, value, ok := splitScalar(line)
		if !ok {
			continue // 空行、注释、非平面标量：与这两个字段无关
		}
		switch key {
		case "name":
			m.name = value
		case "description":
			m.description = value
		}
	}

	if m.name == "" {
		return metadata{}, errors.New("frontmatter 缺少 name")
	}
	if m.name != strings.ToLower(m.name) || !isNameChars(m.name) {
		return metadata{}, fmt.Errorf("name 只允许小写字母、数字与连字符：%q", m.name)
	}
	if len([]rune(m.name)) > maxNameLen {
		return metadata{}, fmt.Errorf("name 超过 %d 字符", maxNameLen)
	}
	if m.description == "" {
		return metadata{}, errors.New("frontmatter 缺少 description")
	}
	if len([]rune(m.description)) > maxDescLen {
		return metadata{}, fmt.Errorf("description 超过 %d 字符", maxDescLen)
	}
	return m, nil
}

// splitScalar 从一行里取"键: 平面标量值"。
//
// 认不出来就返回 false，由调用方忽略这一行——不猜，也不报错：frontmatter 里可能
// 有我们根本不关心的嵌套字段，那些行本来就不该影响判断。
func splitScalar(line string) (key, value string, ok bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", "", false
	}
	idx := strings.IndexByte(trimmed, ':')
	if idx <= 0 {
		return "", "", false
	}
	key = strings.TrimSpace(trimmed[:idx])
	value = strings.TrimSpace(trimmed[idx+1:])
	if key == "" || value == "" {
		return "", "", false
	}
	// 行内映射/序列不是平面标量：`description: {a: b}` 与 `description: [x]` 都拒收。
	if strings.HasPrefix(value, "{") || strings.HasPrefix(value, "[") {
		return "", "", false
	}
	// 块标量（`|` `>`）、锚点与别名（`&` `*`）同样不是平面标量。它们必须**拒收**
	// 而不是把 `|` 当成描述正文收下：那会把一段多行说明渲染成一行 "|"，
	// 属于"看不懂却假装看懂了"。
	switch value[0] {
	case '|', '>', '&', '*':
		return "", "", false
	}
	return key, unquote(value), true
}

// unquote 剥掉包裹在值两端的对称引号。
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func isNameChars(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}
