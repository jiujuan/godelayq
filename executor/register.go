package executor

import (
	"fmt"
	"log/slog"
	"strings"

	"godelayq/core"
)

// Registrar 是注册档位所需的调度器能力：写入处理函数、查询某个键是否已被占用。
// *core.Scheduler 与 *api.Server 的组合都满足它，装配方传哪一个由它自己决定。
//
// RegisterHandlerClass 是执行器唯一需要的注册入口：档位任务属于 JobClassExec，
// 与普通任务分池执行（TASK-E13）。只有 RegisterHandler 的组合（例如把注册表换成
// 测试替身）仍然可用，只是所有档位会落进共享池。
type Registrar interface {
	RegisterHandler(jobType string, handler core.Handler)
	RegisterHandlerClass(jobType string, handler core.Handler, class core.JobClass)
	LookupHandler(jobType string) (core.Handler, bool)
}

// Registration 是一次注册的计数，直接进启动日志：
// 运维看一行就知道"声明了几个、注册了几个、其中几个当前跑不了"。
type Registration struct {
	// Total 是登记表里的档位数量
	Total int
	// Registered 是成功写进调度器的数量；键冲突时因为没有任何写入，这个值仍是 0
	Registered int
	// Unavailable 是已注册但探测失败的档位数量，它们照常可提交，执行期的拒绝由 E16 负责
	Unavailable int
}

// Register 把登记表里的每个档位注册成 exec.<name> 处理函数。
//
// cfg 是整份配置（内部归一化后取 executors 一节给执行器用），artifacts 是输出落盘的存储：
// 打开执行器时装配方一定要传一个可用的进来，传 nil 只会让每次执行在起进程之前失败——
// 那种失败在日志里看起来像"脚本坏了"，其实是装配漏了参数。
//
// 键冲突一律返回错误而不是覆盖：档位与代码里注册的处理函数同名，说明这台机器的
// 配置和代码对不上，静默覆盖会让其中一方悄悄失效，运维在现场查不到痕迹。
// 检查在写入之前全部做完，因此冲突时不会留下部分注册结果——注册表要么完整要么没动过。
//
// 探测失败的档位仍然注册：接口要能显示"已声明但不可用"（E03 的同一口径），
// 而静默消失会让 /job-types 里凭空少一个名字。
//
// 登记表为空时不写任何东西也不记注册日志；此时若开关是打开的，另记一条 warn。
func Register(registrar Registrar, reg *Registry, cfg core.Config, artifacts *ArtifactStore, logger *slog.Logger) (Registration, error) {
	if logger == nil {
		logger = slog.Default()
	}

	keys := reg.Keys()
	result := Registration{Total: len(keys)}
	executors := cfg.Normalized().Executors

	// 空表直接返回：开关关闭时这是正常状态，不该在每次启动留一行注册日志。
	if result.Total == 0 {
		if reg.Enabled() {
			// 开了开关却一个档位都没有，多半是 commands 写错了层级。
			logger.Warn("executors are enabled but no profile is declared",
				"hint", "declare entries as a list under executors.commands")
		}
		return result, nil
	}

	for _, key := range keys {
		if _, ok := registrar.LookupHandler(key); ok {
			profile, _ := reg.Lookup(key)
			return result, fmt.Errorf("handler key %q is already registered, executor profile %q cannot use it",
				key, profile.Name)
		}
	}

	for _, key := range keys {
		profile, ok := reg.Lookup(key)
		if !ok {
			continue
		}
		// 按 kind 分流：进程档位由 Runner 起进程，http 档位由 HTTPRunner 发请求（TASK-E15）。
		// 两条都是真实的执行主体，共用校验、许可、产物与失败分类那一套；
		// 分流只在这一处，Runner 自己不再解释 http 档位（它会按"档位类型不符"报永久失败）。
		registrar.RegisterHandlerClass(key, handlerFor(profile, artifacts, executors, logger).Handler(),
			core.JobClassExec)
		result.Registered++
		if probe, ok := reg.ProbeOf(key); ok && !probe.Available {
			result.Unavailable++
		}
	}

	logger.Info("executor handlers registered",
		"total", result.Total,
		"registered", result.Registered,
		"unavailable", result.Unavailable)

	warnRelaxedAddressPolicy(reg, keys, logger)

	return result, nil
}

// warnRelaxedAddressPolicy 给"关掉了地址防线"的 http 档位记一条启动期 warn（TASK-E15 §9）。
//
// 为什么这一条要在启动时说出来而不是等执行时报错：`deny_private_ranges: false` 是一次
// 明确的配置选择，E02 只能挡住它最危险的那半种写法（通配主机名），
// 写成具体内网主机名的档位会顺利通过校验并且从此每次都能连到那台机器。
// 让它在启动日志里留一行，部署方 review 配置时才有机会发现这条档位上了生产机。
func warnRelaxedAddressPolicy(reg *Registry, keys []string, logger *slog.Logger) {
	var relaxed []string
	for _, key := range keys {
		profile, ok := reg.Lookup(key)
		if !ok || profile.Kind != KindHTTP || profile.DenyPrivate {
			continue
		}
		relaxed = append(relaxed, key)
	}
	if len(relaxed) == 0 {
		return
	}

	logger.Warn("executor http profiles accept private and loopback addresses",
		"profiles", strings.Join(relaxed, ","),
		"reason", "deny_private_ranges is false",
		"hint", "intended for reaching a service on the same development machine; remove it from production configs")
}

// profileHandler 是两种执行主体共同的最小形状：交出一个能给调度器的处理函数。
// 用接口而不是 switch 里两份注册代码：分流只应该发生在一处（见 handlerFor）。
type profileHandler interface {
	Handler() core.Handler
}

// handlerFor 按档位的 kind 选执行主体：http 走 HTTPRunner，其余走进程执行器 Runner。
//
// 两条路共用 payload 校验、档位并发许可、产物存储与失败分类那一张表，
// 差别只在"起一个进程"还是"发一个请求"。
func handlerFor(p *Profile, artifacts *ArtifactStore, cfg core.ExecutorsConfig, logger *slog.Logger) profileHandler {
	if p.Kind == KindHTTP {
		runner, err := NewHTTPRunner(p, artifacts, cfg, logger)
		if err == nil {
			return runner
		}
		// NewHTTPRunner 只在 kind 不符时报错，而这一支刚判过它是 http：属于走不到的分支。
		// 真走到了就退回进程执行器，由它在执行时说明"档位类型与本执行器不符"——
		// 注册阶段 panic 会让整个进程起不来，而这条档位本身没写错。
	}
	return NewRunner(p, artifacts, cfg, logger)
}
