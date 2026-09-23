package hunt

import (
	"context"

	"xhunter/git"
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
)

// GateSource 提供仓库声明的门禁清单。
//
// 来源优先级「Bounty 下发 > 仓库声明 > 无」里的**后两档由它承担**——Bounty 下发的那档不走它
// （任务侧直接给清单，不读仓库）。返回的 source 说明这份清单从哪一档来（见 GateSource* 常量），
// 它进生效配置快照与上报，供评审回答"这次用的是哪套规则"。
//
// **仓库声明从基线 commit 读**（`git show <base_commit>:gates.yml`），**不从工作区读**（FR-5.2g）：
// 门禁是这次交付的验收标准，让它运行中途变，结论就不可复现。因此入参是 `git.RepoRef`。
//
// 未冻结期：契约先定义。`hunt.Config.Gates` 是装配槽，**调用点（`Prepare` 的来源裁决）在 MS-5
// 接上**——本次不接：它是 **B 类入口**（每次运行都会走到），接上会让任何一次运行都跑不起来。
type GateSource interface {
	Load(ctx context.Context, repo git.RepoRef) (gates []Gate, source string, err error)
}
