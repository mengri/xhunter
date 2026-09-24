// Package gates 是「仓库声明的门禁清单」的加载与校验：把仓库根 `gates.yml` 里写的
// 命令与**通过条件**变成一份可判定的 `hunt.Gate` 清单。
//
// 它只读**基线 commit** 那一份：门禁是这次交付的验收标准，让它运行中途变，这次的结论就
// 不可复现。文件本身不禁改——改了照样进交付 diff 给人 review，只是本次不生效。
//
// 清单的**护栏**（元门禁、强度不得降低）在 `hunt` 包：那是业务规则，且工作区豁免档也要用
// 同一份（见 `hunt.MetaGate` / `hunt.CheckStrength`）——本包只负责"把文本变成条目"。
package gates

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"xhunter/git"
	"xhunter/hunt"
)

// DefaultPath 是仓库根的清单文件名。它与工作区豁免档读的是**同一个文件名**（不同提交的内容），
// 因此直接取 `hunt.GateManifestPath`——不在这里另写一份常量。
const DefaultPath = hunt.GateManifestPath

// Source 从基线 commit 读仓库声明的门禁清单。它承担「Bounty 下发 > 仓库声明 > 无」里的
// 仓库声明那一档——Bounty 下发的那档不走它（任务侧直接给清单，不读仓库）。
type Source struct {
	Git  git.GitWorktree
	Path string
}

// New 给出一份来源；path 为空时用 `gates.yml`。
func New(g git.GitWorktree, path string) *Source {
	if strings.TrimSpace(path) == "" {
		path = DefaultPath
	}
	return &Source{Git: g, Path: path}
}

// Load 读基线 commit 里的清单。
//
// 仓库没有声明门禁是**正常事实**：返回 `none` 档位与 nil 错误，而不是错误——把它报成错误，
// "没有门禁"就和"读不出来"混为一谈了。
func (s *Source) Load(ctx context.Context, repo git.RepoRef) ([]hunt.Gate, string, error) {
	content, exists, err := s.Git.ReadFileAtCommit(ctx, repo, s.Path)
	if err != nil {
		return nil, "", err
	}
	if !exists {
		return nil, hunt.GateSourceNone, nil
	}
	gates, err := Parse(content)
	if err != nil {
		return nil, "", err
	}
	if len(gates) == 0 {
		// 声明了空清单 = 明确不设门禁，与"没有声明"同档。
		return nil, hunt.GateSourceNone, nil
	}
	// 元门禁：清单要生效，先得**可判定**——包括命令是否真的能起来。
	if err := hunt.MetaGate(gates); err != nil {
		return nil, "", err
	}
	return gates, hunt.GateSourceRepo, nil
}

// Parse 解析清单文本（`GateSource.Parse`）。
//
// **未知字段一律报错**：静默忽略一个拼错的键（`requierd`、`expecct`），会把"我明明配了"
// 变成"没有门禁"——那是比报错坏得多的结果，因为它看起来一切正常。
func (s *Source) Parse(content []byte) ([]hunt.Gate, error) { return Parse(content) }

// entry 是清单里一条门禁的文本形状。
type entry struct {
	Name     string   `yaml:"name"`
	Argv     []string `yaml:"argv"`
	Required bool     `yaml:"required"`
	Timeout  string   `yaml:"timeout"`
	Dir      string   `yaml:"dir"`
	Env      []string `yaml:"env"`
	Expect   struct {
		Kind    string `yaml:"kind"`
		Pattern string `yaml:"pattern"`
		Max     *int   `yaml:"max"`
		Stream  string `yaml:"stream"`
	} `yaml:"expect"`
}

// Parse 解析清单文本。
//
// **未知字段一律报错**：静默忽略一个拼错的键（`requierd`、`expecct`），会把"我明明配了"
// 变成"没有门禁"——那是比报错坏得多的结果，因为它看起来一切正常。
func Parse(content []byte) ([]hunt.Gate, error) {
	dec := yaml.NewDecoder(bytes.NewReader(content))
	dec.KnownFields(true)
	var doc struct {
		Gates []entry `yaml:"gates"`
	}
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("解析门禁清单失败：%w", err)
	}
	out := make([]hunt.Gate, 0, len(doc.Gates))
	for i, e := range doc.Gates {
		g, err := e.gate()
		if err != nil {
			return nil, fmt.Errorf("第 %d 条门禁不可用：%w", i+1, err)
		}
		out = append(out, g)
	}
	return out, nil
}

func (e entry) gate() (hunt.Gate, error) {
	g := hunt.Gate{
		Name:     strings.TrimSpace(e.Name),
		Argv:     e.Argv,
		Required: e.Required,
		Dir:      strings.TrimSpace(e.Dir),
		Env:      e.Env,
		Expect: hunt.Expect{
			Kind:    hunt.ExpectKind(e.Expect.Kind),
			Pattern: e.Expect.Pattern,
			Stream:  hunt.ExpectStream(e.Expect.Stream),
		},
	}
	if e.Timeout != "" {
		d, err := time.ParseDuration(e.Timeout)
		if err != nil {
			return hunt.Gate{}, fmt.Errorf("timeout %q 不是合法时长：%w", e.Timeout, err)
		}
		g.Timeout = d
	}
	if e.Expect.Max != nil {
		g.Expect.Max = *e.Expect.Max
	}
	return g, nil
}
