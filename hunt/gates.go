package hunt

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"xhunter/git"
	"xhunter/workspace"
)

// 门禁清单来源（FR-5.2b/5.2c）：来源优先级是「Bounty 下发 > 仓库声明 > 无」。
//
// 下面三个常量是**清单来源档位**的取值——它同时是结果文件与上报里的口径。注：门禁「配置变更」
// 事件 `gate_config_changed` 的 `source`（如 `working_tree`）是**另一根轴**——它说的是"清单从工作区
// 还是基线读到"；这里的 source 说的是"清单来自哪一档"。两者不重叠，故不复用。
const (
	// GateSourceBounty：清单来自任务下发（Bounty 显式配置，可覆盖或禁用仓库声明）。
	GateSourceBounty = "bounty"
	// GateSourceRepo：清单来自仓库声明（基线 commit 的 `gates.yml`）。
	GateSourceRepo = "repo"
	// GateSourceNone：没有清单（不设门禁）。
	GateSourceNone = "none"
	// GateSourceWorkingTree：豁免档——让**工作区**里新写的清单本次就生效。它只能由 Bounty
	// 授予（模型拿不到这个开关），且受三条护栏约束（元门禁 / 强度不得降低 / 显式上报）。
	GateSourceWorkingTree = "working_tree"
)

// GateManifestPath 是仓库根的清单文件名。它刻意不在 `.xhunter/` 下：门禁清单是仓库级配置，
// 别的工具（CI、编辑器）也要能读同一份。
//
// 名字只有这一处定义：基线档与工作区豁免档读的是**同一个文件名的两份内容**（不同提交），
// 两处各写一个常量迟早会漂成"两处读的不是同一个文件"。
const GateManifestPath = "gates.yml"

// GateSource 提供仓库声明的门禁清单。
//
// 来源优先级「Bounty 下发 > 仓库声明 > 无」里的**后两档由它承担**——Bounty 下发的那档不走它
// （任务侧直接给清单，不读仓库）。返回的 source 说明这份清单从哪一档来（见 GateSource* 常量），
// 它进生效配置快照与上报，供评审回答"这次用的是哪套规则"。
//
// **仓库声明从基线 commit 读**（`git show <base_commit>:gates.yml`），**不从工作区读**（FR-5.2g）：
// 门禁是这次交付的验收标准，让它运行中途变，结论就不可复现。因此入参是 `git.RepoRef`。
type GateSource interface {
	Load(ctx context.Context, repo git.RepoRef) (gates []Gate, source string, err error)
	// Parse 把清单文本变成门禁条目。工作区豁免档读的是工作区里的文本，解析规则必须与
	// 基线档**同一份**——解析分叉等于两套判据，而"同一份清单两种读法"是最难发现的一类错。
	Parse(content []byte) ([]Gate, error)
}

// ValidateGates 校验一份清单**是否可判定**：每条都通过 `Gate.Validate`，一次报出全部问题。
//
// 判据不可判定的清单一旦生效，门禁要么永真要么永假——那比没有门禁更坏，因为它看起来在工作。
// 一次报全而不是只报第一条：这些是同一次配置里就该一起改完的事，只报一条会让平台反复重派。
func ValidateGates(gates []Gate) error {
	var errs []string
	for _, g := range gates {
		if err := g.Validate(); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("门禁清单不可用：\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// MetaGate 校验一份清单**能不能真的用来判定**：每条都通过 `hunt.ValidateGates`（名字、命令、
// 判据齐备），并且命令能启动（PATH 上找得到）。
//
// 一次报出**全部**问题：只报第一条会让平台反复重派才看得全，而这些都是同一次配置里就该改完的事。
func MetaGate(gates []Gate) error {
	// 两件事**一次报全**：先短路返回可判定性问题，会让"命令起不来"这类问题要等下一轮重派
	// 才看得见——而它们同属一份配置，本就该一起改完。
	var errs []string
	for _, g := range gates {
		if err := g.Validate(); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		if _, err := exec.LookPath(g.Argv[0]); err != nil {
			errs = append(errs, fmt.Sprintf("门禁 %q 的命令无法启动：%s", g.Name, g.Argv[0]))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("门禁清单不可用：\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// CheckStrength 校验候选清单相对基线**没有降低强度**：基线里 `required` 的门禁既不能被移除，
// 也不能改命令或判据，只允许新增。
//
// 这条只作用于"让新清单本次生效"的豁免路径——默认值是从基线读，那时没有"降低"可言。
func CheckStrength(baseline, candidate []Gate) error {
	byName := make(map[string]Gate, len(candidate))
	for _, g := range candidate {
		byName[g.Name] = g
	}
	var errs []string
	for _, b := range baseline {
		if !b.Required {
			continue
		}
		c, ok := byName[b.Name]
		if !ok {
			errs = append(errs, fmt.Sprintf("必需的门禁 %q 被移除", b.Name))
			continue
		}
		if !sameStrings(b.Argv, c.Argv) {
			errs = append(errs, fmt.Sprintf("必需的门禁 %q 改了命令：%v → %v", b.Name, b.Argv, c.Argv))
		}
		if b.Expect != c.Expect {
			errs = append(errs, fmt.Sprintf("必需的门禁 %q 改了判据", b.Name))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("门禁强度不得降低：\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// resolveGates 裁决本次用哪份清单：**Bounty 下发 > 仓库声明（基线 commit）> 无**。
//
// 仓库声明那一档由装配层注入的 `GateSource` 承担（它读的是基线，不是工作区）。下发清单同样要
// 过"判据可判定"这一关——下发只是换了来源，没有换标准。
//
// 唯一的例外是 Bounty 授予的 `working_tree` 豁免档：那时读**工作区**里新写的清单，让它本次就
// 生效（改门禁本身就是任务时，否则要等下一次成为基线）。豁免不绕过任何护栏，见
// resolveWorkingTreeGates。
func (s *Session) resolveGates(ctx context.Context) ([]Gate, string, error) {
	if strings.TrimSpace(s.cfg.Bounty.GatesSource) == GateSourceWorkingTree {
		return s.resolveWorkingTreeGates(ctx)
	}
	if len(s.cfg.Bounty.Gates) > 0 {
		if err := ValidateGates(s.cfg.Bounty.Gates); err != nil {
			return nil, "", err
		}
		return s.cfg.Bounty.Gates, GateSourceBounty, nil
	}
	if s.cfg.Gates == nil {
		return nil, GateSourceNone, nil
	}
	gates, source, err := s.cfg.Gates.Load(ctx, s.cfg.Bounty.Repo)
	if err != nil {
		return nil, "", fmt.Errorf("加载门禁清单失败：%w", err)
	}
	return gates, source, nil
}

// resolveWorkingTreeGates 处理「新清单本次就生效」这条豁免路径。
//
// 清单文件**不限制模型写**（改动照样进交付 diff 由人 review），所以这里扛的是唯一一道防线。
// 三条护栏**全部在清单生效之前**起作用，且按固定次序：
//  1. **元门禁**：条目可判定 ＋ 命令真的能起来（判不了的清单比没有门禁更坏）；
//  2. **强度不得降低**：基线里 required 的门禁不能被移除、改命令或改判据，只允许新增；
//  3. **显式上报**：`gate_config_changed`——评审必须能看出"这次用的不是基线那套规则"。
//
// 任一条不成立即**启动期失败**：带着一份靠不住的清单跑完，只会得到一个"看起来通过了"的结论。
func (s *Session) resolveWorkingTreeGates(ctx context.Context) ([]Gate, string, error) {
	if s.cfg.Gates == nil {
		return nil, "", errors.New("授予了工作区清单豁免，但没有装配门禁来源（无法读基线做强度比对）")
	}
	if s.storage == nil {
		return nil, "", errors.New("授予了工作区清单豁免，但工作区尚未就绪")
	}
	// 基线那一份是护栏的**参照物**：没有它就无从判断"强度是否降低"。
	baseline, _, err := s.cfg.Gates.Load(ctx, s.cfg.Bounty.Repo)
	if err != nil {
		return nil, "", fmt.Errorf("读取基线门禁清单失败：%w", err)
	}
	content, exists, err := s.readManifest()
	if err != nil {
		return nil, "", err
	}
	var candidate []Gate
	if exists {
		candidate, err = s.cfg.Gates.Parse(content)
		if err != nil {
			return nil, "", err
		}
	}
	if err := MetaGate(candidate); err != nil {
		return nil, "", err
	}
	if err := CheckStrength(baseline, candidate); err != nil {
		return nil, "", err
	}
	s.emitGateConfigChanged(candidate)
	return candidate, GateSourceWorkingTree, nil
}

// readManifest 读工作区里的清单文件。文件不存在是**正常事实**（返回 exists=false），
// 读不出来才是错误——两者在护栏里的后果完全不同（前者是"没有清单"，后者是启动期失败）。
func (s *Session) readManifest() ([]byte, bool, error) {
	// 先问存在、再取内容：「没有清单」与「读不出来」在护栏里的后果完全不同（前者是空清单、
	// 后者是启动期失败），两步分开才不会把前者读成后者。
	info, err := s.storage.Stat(GateManifestPath)
	if err != nil {
		return nil, false, fmt.Errorf("探测工作区门禁清单失败：%w", err)
	}
	if !info.Exists {
		return nil, false, nil
	}
	fc, err := s.storage.Read(GateManifestPath, workspace.LineRange{})
	if err != nil {
		return nil, false, fmt.Errorf("读取工作区门禁清单失败：%w", err)
	}
	if fc.Truncated {
		return nil, false, fmt.Errorf("门禁清单 %s 过长被截断，无法据此判定", GateManifestPath)
	}
	return []byte(fc.Raw), true, nil
}

// emitGateConfigChanged 上报"这次用的不是基线那套门禁规则"。
//
// 它只在豁免真正生效后发：评审要能一眼看出"清单来源变了"，否则一份被改松的门禁会与被改紧
// 的门禁看起来一模一样。
func (s *Session) emitGateConfigChanged(gates []Gate) {
	if s.cfg.Sink == nil {
		return
	}
	names := make([]string, 0, len(gates))
	for _, g := range gates {
		names = append(names, g.Name)
	}
	_ = s.cfg.Sink.Emit(ExternalEvent{Type: "gate_config_changed", Payload: map[string]any{
		"source": GateSourceWorkingTree,
		"gates":  names,
	}})
}
