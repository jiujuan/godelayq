package executor

import (
	"fmt"
	"log/slog"

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
		// Runner 是真实的执行主体：校验 payload、拼 argv、起进程、把输出写进产物文件。
		// http 档位也走同一个入口，它在 Runner.Handler 里明确报告"执行分支归 TASK-E15"。
		registrar.RegisterHandlerClass(key, NewRunner(profile, artifacts, executors, logger).Handler(),
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

	return result, nil
}
