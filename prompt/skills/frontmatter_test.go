package skills

import (
	"strings"
	"testing"
)

// 只取 name 与 description 两个平面标量；其余形态一律按"不认"处理——
// 猜错了会注入一段含义不明的说明，跳过并留痕才是安全的那一侧。
func TestParseFrontmatter(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		wantName  string
		wantDesc  string
		wantError string
	}{
		{
			name:     "基本形态",
			raw:      "---\nname: release\ndescription: 发布流程\n---\n\n# 标题\n",
			wantName: "release", wantDesc: "发布流程",
		},
		{
			name:     "CRLF 与 BOM 不该判非法",
			raw:      "\ufeff---\r\nname: release\r\ndescription: 发布流程\r\n---\r\n正文\r\n",
			wantName: "release", wantDesc: "发布流程",
		},
		{
			name:     "未知字段一律忽略",
			raw:      "---\nname: lint\ndescription: 检查约定\nallowed-tools: bash\nlicense: MIT\n---\n",
			wantName: "lint", wantDesc: "检查约定",
		},
		{
			name:     "值的引号被剥掉",
			raw:      "---\nname: \"lint\"\ndescription: '检查约定'\n---\n",
			wantName: "lint", wantDesc: "检查约定",
		},
		{
			name:      "缺少结束标记",
			raw:       "---\nname: x\ndescription: y\n",
			wantError: "结束标记",
		},
		{
			name:      "缺少起始标记",
			raw:       "# 直接是正文\n",
			wantError: "起始标记",
		},
		{
			name:      "缺少 name",
			raw:       "---\ndescription: y\n---\n",
			wantError: "缺少 name",
		},
		{
			name:      "缺少 description",
			raw:       "---\nname: x\n---\n",
			wantError: "缺少 description",
		},
		{
			name:      "name 含大写",
			raw:       "---\nname: Release\ndescription: y\n---\n",
			wantError: "小写字母",
		},
		{
			name:      "name 含非法字符",
			raw:       "---\nname: re_lease\ndescription: y\n---\n",
			wantError: "小写字母",
		},
		{
			name:      "嵌套块不是平面标量",
			raw:       "---\nname: x\ndescription:\n  nested: value\n---\n",
			wantError: "缺少 description",
		},
		{
			name:      "行内映射不是平面标量",
			raw:       "---\nname: x\ndescription: {a: b}\n---\n",
			wantError: "缺少 description",
		},
		{
			name:      "name 超长",
			raw:       "---\nname: " + strings.Repeat("a", maxNameLen+1) + "\ndescription: y\n---\n",
			wantError: "超过",
		},
		{
			name:      "description 超长",
			raw:       "---\nname: x\ndescription: " + strings.Repeat("字", maxDescLen+1) + "\n---\n",
			wantError: "超过",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseFrontmatter(tc.raw)
			if tc.wantError != "" {
				if err == nil {
					t.Fatalf("期望报错（含 %q），实际通过：%+v", tc.wantError, got)
				}
				if !strings.Contains(err.Error(), tc.wantError) {
					t.Errorf("错误信息 = %q，期望包含 %q", err.Error(), tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("不该报错：%v", err)
			}
			if got.name != tc.wantName || got.description != tc.wantDesc {
				t.Errorf("解析结果 = %+v，期望 name=%q description=%q", got, tc.wantName, tc.wantDesc)
			}
		})
	}
}

// 参数上限来自规范：超长不是"截断一下照样用"——一句话说不清的说明，
// 说明它写得不对，跳过比塞进上下文更安全。
func TestParseFrontmatter_LengthLimitsComeFromSpec(t *testing.T) {
	if maxNameLen != 64 || maxDescLen != 1024 {
		t.Errorf("长度上限应与规范一致，实际 %d / %d", maxNameLen, maxDescLen)
	}
	// 恰好达上限应当通过（边界在"超过"这一侧）。
	raw := "---\nname: " + strings.Repeat("a", maxNameLen) +
		"\ndescription: " + strings.Repeat("字", maxDescLen) + "\n---\n"
	if _, err := parseFrontmatter(raw); err != nil {
		t.Errorf("恰好达到上限不该判非法：%v", err)
	}
}
