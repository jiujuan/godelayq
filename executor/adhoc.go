package executor

import (
	"fmt"

	"godelayq/core"
)

// 内置自由执行档位（adhoc）：四条"跑哪个文件、打到哪里由任务自己说"的档位。
//
// 它们与 executors.commands 里的档位是同一个东西——同一个构造函数、同一套探测、
// 同一个处理函数、同一个执行池——区别只有一处：位置字段不在定义里，而在任务的 payload 里。
// 走这条路而不是新造一种执行通路，是为了让并发许可、超时上限、产物落盘、失败分类、
// 重试与崩溃恢复那一大套既有能力一条都不必重写（设计文档 §D5）。
//
// 整节由 executors.adhoc.enabled 控制，默认关闭（§D11）。

// 四条内置档位的注册键，去掉 exec. 前缀之后的档位名。
const (
	AdhocPHPName     = "php"
	AdhocPythonName  = "python"
	AdhocShellName   = "shell"
	AdhocHTTPName    = "http"
	adhocLocationKey = "script"
	adhocLocationURL = "url"
)

// adhocSpec 是一条内置档位的静态定义。
type adhocSpec struct {
	// name 是档位名；注册键是 exec.<name>。
	// 档位名字符集不含点（core/executor_profile_store.go 的 profileNamePattern），
	// 所以 "exec.adhoc.php" 那种写法不合法，这里用四条短名字。
	name string
	kind Kind
	// runtime 是解释器名。空是一个占位记号：只有 shell 那一条用空串，
	// 实际取值来自 executors.adhoc.shell_runtime（见 adhocSpecs）。
	runtime string
	// extensions 是脚本位置允许的扩展名（小写、含点）；nil 表示不要求。
	extensions []string
	// locationKey 是任务写位置时用的 payload 顶层键。
	locationKey string
	// locationKind 是位置的种类：path | url，接口与界面据此决定输入框形态。
	locationKind string
	// httpMethod / httpBody 只对 http 那条有意义。
	// 默认 POST + json：健康检查与回调类需求都要能带体；
	// 只想要 GET 的部署可以照 executors.commands 另建一条普通档位（设计文档 §12 P5）。
	httpMethod string
	httpBody   string
}

// adhocSpecs 给出四条定义，顺序固定（接口与启动日志的输出因此稳定）。
func adhocSpecs(ec core.ExecutorsConfig) []adhocSpec {
	extensions := func(names ...string) []string {
		if !ec.Adhoc.WantExtensionCheck() {
			// 关掉要求时这里给空：判据只有一处，N05 那边看见空列表就是"不要求"，
			// 不必再各自读一遍配置。
			return nil
		}
		return names
	}

	return []adhocSpec{
		{
			name: AdhocPHPName, kind: KindScript, runtime: "php",
			extensions: extensions(".php"), locationKey: adhocLocationKey, locationKind: "path",
		},
		{
			name: AdhocPythonName, kind: KindScript, runtime: "python",
			extensions: extensions(".py"), locationKey: adhocLocationKey, locationKind: "path",
		},
		{
			// shell 那条的解释器由配置给（默认 bash），所以这里留空占位，
			// 由 AdhocProfiles 换成 executors.adhoc.shell_runtime 的取值。
			name: AdhocShellName, kind: KindScript,
			extensions: extensions(".sh", ".bash"), locationKey: adhocLocationKey, locationKind: "path",
		},
		{
			name: AdhocHTTPName, kind: KindHTTP,
			locationKey: adhocLocationURL, locationKind: "url",
			httpMethod: "POST", httpBody: "json",
		},
	}
}

// AdhocSkip 是一条没被构造出来的内置档位与原因。
type AdhocSkip struct {
	// HandlerKey 是那条内置档位本来要占用的注册键。
	HandlerKey string
	// Reason 是可以直接进日志与接口的说明。
	Reason string
}

// AdhocProfiles 按配置构造那四条内置档位。
//
// 返回的第二个值是"因为配置或本机环境而没构造出来的条目"，调用方负责把它说出去
// （executor.Register 的计数与启动日志，见 TASK-N04 §3.3）。
// 没构造出来不是错误：把 runtime_allow 收成只有 bash 的部署，
// 应当安静地只剩 Shell 与 HTTP 两条，而不是启动失败。
//
// 构造仍然走 buildProfile（profile.go 那一份），所以字段组合、解释器白名单、
// 超时上限、主机写法这些判据与配置文件里的档位完全相同——
// 唯一多出来的东西是 Profile.Adhoc 这个标记，它让档位定义里的位置字段可以留空
// （判据在 checkFieldsMatchKind 与 fillHTTPProfile 的 adhoc 分支，各只放宽两处）。
//
// executors.adhoc.enabled 为 false 时返回空切片：那时这四条键根本不存在，
// 接口、页面与 /job-types 都看不出任何变化（设计文档 §D11）。
func AdhocProfiles(cfg core.Config) ([]*Profile, []AdhocSkip, error) {
	normalized := cfg.Normalized()
	ec := normalized.Executors
	if !ec.Adhoc.Enabled {
		return nil, nil, nil
	}

	workspace, err := resolveWorkspace(ec.Workspace)
	if err != nil {
		return nil, nil, fmt.Errorf("executors.workspace %q is unusable as a profile root: %w", ec.Workspace, err)
	}
	runtimes := runtimeSet(ec.RuntimeAllow)

	profiles := make([]*Profile, 0, 4)
	var skipped []AdhocSkip
	for _, spec := range adhocSpecs(ec) {
		runtime := spec.runtime
		if spec.kind == KindScript && runtime == "" {
			runtime = ec.Adhoc.ShellRuntime
		}
		if spec.kind == KindScript && !runtimes[runtime] {
			skipped = append(skipped, AdhocSkip{
				HandlerKey: HandlerKeyPrefix + spec.name,
				Reason: fmt.Sprintf("runtime %q is not in executors.runtime_allow, "+
					"the built-in profile cannot use it (add it to the allow list or use a declared profile instead)",
					runtime),
			})
			continue
		}

		command, err := adhocCommand(spec, ec, runtime)
		if err != nil {
			return nil, nil, fmt.Errorf("built-in profile %q is unusable: %w", spec.name, err)
		}
		profile, err := buildProfile(&command, singleProfileIndex, workspace, runtimes, ec, PathAnywhere, true)
		if err != nil {
			// 走到这里属于定义写错：内置条目是自己拼出来的，没有任何用户输入参与。
			// 报错而不是跳过，否则一份看起来"打开了 adhoc"的配置会少一条而无人知晓。
			return nil, nil, fmt.Errorf("built-in profile %q is unusable: %w", spec.name, err)
		}
		profile.AdhocLocationKey = spec.locationKey
		profile.AdhocLocationKind = spec.locationKind
		profile.AdhocExtensions = append([]string(nil), spec.extensions...)
		profiles = append(profiles, profile)
	}
	return profiles, skipped, nil
}

// adhocCommand 把一条内置定义拼成 `core.ExecutorCommand`，交给既有的构造函数去判。
//
// 拼而不是直接构造 Profile 是为了守住"一份规则"（web-profile-design.md 的 D3）：
// 配置文件里的档位、页面上建的档位、以及这四条内置档位，
// 全部经过 buildProfile 的同一套字段组合、解释器白名单、超时上限与主机写法判据。
// 位置字段一律留空——那是这四条与其余档位的唯一差别，由 buildProfile 的 adhoc 分支放过。
func adhocCommand(spec adhocSpec, ec core.ExecutorsConfig, runtime string) (core.ExecutorCommand, error) {
	switch spec.kind {
	case KindScript:
		if runtime == "" {
			return core.ExecutorCommand{}, fmt.Errorf("no runtime is available for it")
		}
		return core.ExecutorCommand{
			Name:    spec.name,
			Kind:    string(spec.kind),
			Runtime: runtime,
			// Timeout 留空：走 executors.default_timeout，与配置文件里没写 timeout 的档位同一条。
		}, nil

	case KindHTTP:
		// 地址范围守卫是这次放宽里唯一没动的那一层（设计文档 §D9 第③层）：
		// DenyPrivate 取 executors.adhoc.url_allow_private 的反面，默认禁止回环与私网。
		denyPrivate := !ec.Adhoc.URLAllowPrivate
		return core.ExecutorCommand{
			Name:            spec.name,
			Kind:            string(spec.kind),
			Method:          spec.httpMethod,
			Body:            spec.httpBody,
			AllowedHosts:    append([]string(nil), ec.Adhoc.URLHosts...),
			DenyPrivate:     &denyPrivate,
			Timeout:         ec.Adhoc.HTTPTimeout,
			CaptureResponse: false,
		}, nil

	default:
		return core.ExecutorCommand{}, fmt.Errorf("unknown kind %q", spec.kind)
	}
}
