package main

import (
	"fmt"
	"runtime"
	"strings"
	"time"

	"xhunter/ext"
	"xhunter/ext/mcp"
	"xhunter/ext/syntax"
	"xhunter/git"
	"xhunter/harness"
	"xhunter/hunt"
	"xhunter/internal/git/cli"
	"xhunter/internal/policy"
	"xhunter/internal/workspace/osfs"
	"xhunter/workspace"
)

// checkpointBehaviour 是本期的检查点行为口径：只在结构完整点上自动产生（判据不可判定时
// 不提交）。它进生效配置快照——评审者据此知道本次运行的检查点策略，而不是靠记忆去猜。
const checkpointBehaviour = "on_structure"

// defaultWorkspaces 给出文件操作的实现：本地文件系统。
func defaultWorkspaces() workspace.WorkspaceOpener {
	return osfs.Opener{}
}

// defaultGit 给出 git 操作的实现：命令行。
func defaultGit() git.GitWorktree {
	return cli.New(cli.Config{})
}

// defaultExt 给出符号后端：内置**语法级**后端（进程内，零外部依赖、零子进程）。
//
// 它是首发后端也是兜底：外挂进程（MCP）是给第三方后端的通道，核心只认 `ext.ExtHost` 接口，
// 换后端不改任何业务代码。构造需要工作区（定位要读源码），因此是工厂形态。
func defaultExt(ws workspace.Workspace) ext.ExtHost { return syntax.New(ws) }

// extChoice 是本次装配选出的符号后端：**宿主工厂与能力指纹同源**。
//
// 生效配置快照要报"这次真用的是哪个后端"（`effective_config.ext`），指纹若另判一次，
// 装的与报的迟早会漂——所以"选后端"这一件事同时给出两端。
type extChoice struct {
	// new 给出宿主。工作区就绪后才造（内置后端要读工作区）；外挂后端不需要它，忽略即可。
	new func(workspace.Workspace) ext.ExtHost
	// fp 是这次装配的能力指纹。
	fp []string
}

// chooseExt 按部署事实选符号后端：配了外挂命令就走外挂通道（MCP over stdio 子进程），
// 否则用内置语法级后端（FR-13.9）。**它不启动进程**——选择只是记下事实，进程在首次
// 符号调用时才拉起（懒启动，FR-13.5）；为一个还没用上的能力去开子进程是不必要的开销。
//
// 部署事实写错即启动期失败（退出 1），与预算、心跳同一条纪律：一个拼错的变量名不该
// 在运行期才变成"符号能力莫名不可用"。
func chooseExt(lookup lookupEnv) (extChoice, error) {
	if readEnv(lookup, envExtCommand) == "" {
		return extChoice{new: defaultExt, fp: syntax.FingerprintTokens()}, nil
	}
	cfg, err := parseExtConfig(lookup)
	if err != nil {
		return extChoice{}, err
	}
	host := mcp.New(cfg)
	return extChoice{
		new: func(workspace.Workspace) ext.ExtHost { return host },
		fp:  host.Fingerprint(),
	}, nil
}

// parseExtConfig 读外挂后端的部署事实。命令本身由 chooseExt 判过（非空才走到这里）。
//
// 四个可选项的口径一致：**不配**取默认，**配错**即失败——「不配」与「配错」在这里
// 必须分开，否则一个拼错的部署事实会静默退化成"符号能力不可用"。
func parseExtConfig(lookup lookupEnv) (mcp.Config, error) {
	cfg := mcp.Config{Command: readEnv(lookup, envExtCommand)}

	if v := readEnv(lookup, envExtArgs); v != "" {
		cfg.Args = strings.Fields(v)
	}
	if v := readEnv(lookup, envExtTimeout); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return mcp.Config{}, fmt.Errorf("%s 必须是正的时间长度（如 15s、1m；当前 %q）", envExtTimeout, v)
		}
		cfg.Timeout = d
	}
	if v := readEnv(lookup, envExtLanguages); v != "" {
		for _, lang := range strings.Split(v, ",") {
			lang = strings.TrimSpace(lang)
			if lang == "" {
				return mcp.Config{}, fmt.Errorf("%s 必须是逗号分隔的语言名（当前 %q）", envExtLanguages, v)
			}
			cfg.Languages = append(cfg.Languages, lang)
		}
	}
	// 环境是**点名授予**：外挂进程默认一个变量都不继承，只拿这里点名且确实存在的那几项。
	// 沿用当前环境里的值（而不是让部署侧重写一遍）——值只有一份来源，抄一遍迟早会不一样。
	if v := readEnv(lookup, envExtEnv); v != "" {
		for _, name := range strings.Split(v, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				return mcp.Config{}, fmt.Errorf("%s 必须是逗号分隔的环境变量名（当前 %q）", envExtEnv, v)
			}
			value, ok := lookup(name)
			if !ok {
				return mcp.Config{}, fmt.Errorf("%s 点名的 %q 在当前环境里不存在", envExtEnv, name)
			}
			cfg.Env = append(cfg.Env, name+"="+value)
		}
	}
	return cfg, nil
}

// defaultPolicy 给出策略引擎的实现：默认拒绝 + 路径边界 + 预算。
func defaultPolicy(budget hunt.Budget) hunt.Policy {
	return policy.New(policy.Config{Budget: budget})
}

// assemblyFacts 汇总「本次实际生效的规则」里只有装配层知道的那部分，值注入 Session。
//
// 策略口径来自策略实现自述（边界常量是它的知识，装配层不另抄一份）；检查点行为是本期固定
// 档；扩展指纹来自实际装配的符号后端（由 `chooseExt` 选出、与宿主同源，因此"没有后端"
// 只可能是后端自己报空数组，装配层不替它说假话）；目标平台与内核环境事实
// 同源（GOOS/GOARCH）；机制硬顶取自实际生效的 harness 配置，供 config_snapshot 如实报出。
func assemblyFacts(hcfg harness.Config, backend extChoice) hunt.AssemblyFacts {
	return hunt.AssemblyFacts{
		Policy:     policy.Facts(),
		Checkpoint: checkpointBehaviour,
		// 符号能力是本次装配的真实事实：指纹与装上去的那个宿主**同源**（同一个
		// extChoice 给出的两端，不另抄一份字符串）。换后端（内置 ↔ 外挂）因此
		// 一定会体现在快照里——精度档位变了却报不出来，是诊断时最难查的一类事。
		Ext:      backend.fp,
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
		// 机制硬顶从实际生效的 harness 配置里取（与 harness.New 用的是同一份），这里不另抄
		// 3/1000——否则改了 withDefaults、快照报的还是旧数。
		MaxFailStreak: hcfg.MaxFailStreak,
		MaxTurnsHard:  hcfg.MaxTurns,
		// 接收段不活动超时的生效值（毫秒；0 = 不限/关闭）。与上面两条硬顶**同源同通道**上报，
		// 只进 config_snapshot、不进 EffectiveConfig。
		StreamIdleTimeoutMS: int(hcfg.StreamIdleTimeout.Milliseconds()),
	}
}
