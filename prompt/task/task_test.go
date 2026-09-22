package task

import (
	"context"
	"strings"
	"testing"

	"xhunter/hunt"
)

// 任务正文必须进正文，且只进一份：它是模型唯一能知道"要做什么"的地方。
func TestBuild_RendersTaskBody(t *testing.T) {
	task := "把 internal/policy 下的绝对路径检查提到公共函数里\n验收：scripts/check.py 全绿"
	part, err := New().Build(context.Background(), hunt.PromptInput{
		Bounty: hunt.Bounty{ID: "b1", Task: task},
	})
	if err != nil {
		t.Fatalf("构造任务陈述失败：%v", err)
	}
	if !strings.Contains(part.Body, task) {
		t.Errorf("正文必须完整包含任务原文：%q", part.Body)
	}
	if strings.Count(part.Body, task) != 1 {
		t.Errorf("任务原文只应出现一次：%q", part.Body)
	}
	// 来源要可审计：任务陈述的素材是这次投递。
	if len(part.Sources) != 1 || !strings.Contains(part.Sources[0], "b1") {
		t.Errorf("Sources 必须标注投递来源：%v", part.Sources)
	}
	if len(part.Notices) != 0 {
		t.Errorf("正常渲染不该产生降级记录：%v", part.Notices)
	}
}

// 空白任务不贡献正文（空段不占位置），也不报错——"没有任务"由投递侧在启动期拦下。
func TestBuild_BlankTaskContributesNothing(t *testing.T) {
	for _, body := range []string{"", "   ", "\n\t\n"} {
		part, err := New().Build(context.Background(), hunt.PromptInput{
			Bounty: hunt.Bounty{Task: body},
		})
		if err != nil {
			t.Fatalf("空白任务不该报错：%v", err)
		}
		if part.Body != "" || len(part.Sources) != 0 {
			t.Errorf("空白任务不该贡献正文或来源：%+v", part)
		}
	}
}

// 首尾空白不进入正文：投递侧已 TrimSpace，库使用者未必——两处都不该把空白当内容。
func TestBuild_TrimsSurroundingWhitespace(t *testing.T) {
	part, err := New().Build(context.Background(), hunt.PromptInput{
		Bounty: hunt.Bounty{Task: "\n\n  修一个 bug  \n\n"},
	})
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}
	if strings.Contains(part.Body, "  \n") || strings.HasSuffix(part.Body, "\n") {
		t.Errorf("正文不得带任务原文的首尾空白：%q", part.Body)
	}
	if !strings.HasSuffix(part.Body, "修一个 bug") {
		t.Errorf("正文应以任务原文收尾：%q", part.Body)
	}
}
