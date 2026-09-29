package executor

import (
	"log/slog"
	"sort"

	"godelayq/core"
)

// entry 是登记表里的一条：校验过的档位，加上这台机器的可用性结论。
type entry struct {
	profile *Profile
	probe   ProbeResult
}

// Registry 是"档位 + 可用性"的只读登记表。
//
// 它与调度器的处理函数注册表（core.Scheduler.LookupHandler）是两份东西：
// 那份回答"这个任务类型有没有处理函数"，这份回答"这个档位是谁配的、配的是什么、现在能不能跑"。
// api 层用后者做展示与提交期拒绝（TASK-E07、E16），前者照旧承担实际执行。
//
// 构造之后不再变化，因此读方法不需要加锁。
type Registry struct {
	enabled       bool
	requiredRole  string
	loaderAllowed bool
	entries       map[string]*entry
	keys          []string // 已按注册键字典序排好，接口输出要稳定
}

// NewRegistry 加载并探测全部档位。
//
// 探测失败的档位照常留在表里，只记一条 warn：让运维在接口上看得见"已声明但不可用"，
// 比让它静默消失更容易发现问题，所以这里不因不可用而返回错误。
// 档位配置本身非法（越界路径、引用未声明的参数等）仍然返回错误，由装配方决定是否终止启动。
//
// executors.enabled 为 false 时返回空登记表（不是 nil），调用方不需要判空指针分支。
func NewRegistry(cfg core.Config, logger *slog.Logger) (*Registry, error) {
	normalized := cfg.Normalized()

	registry := &Registry{
		enabled:       normalized.Executors.Enabled,
		requiredRole:  normalized.Executors.RequiredRole,
		loaderAllowed: normalized.Executors.LoaderAllow,
		entries:       make(map[string]*entry),
	}
	if !registry.enabled {
		// 关闭即完全惰性：不加载档位也不探测。配置里的错误要到打开执行器时才暴露，
		// 这与"enabled=false 时本节取值全部不生效"的口径一致。
		return registry, nil
	}
	if logger == nil {
		logger = slog.Default()
	}

	profiles, err := LoadProfiles(normalized)
	if err != nil {
		return nil, err
	}

	for _, profile := range profiles {
		result := Probe(profile)
		if !result.Available {
			logger.Warn("executor profile unavailable",
				"profile", profile.Name,
				"handler_key", profile.HandlerKey(),
				"kind", string(profile.Kind),
				"reason", result.Reason)
		}
		registry.entries[profile.HandlerKey()] = &entry{profile: profile, probe: result}
	}
	registry.keys = sortedKeys(registry.entries)

	return registry, nil
}

// Enabled 返回执行器总开关，供接口区分"没开"与"开了但没有档位"。
func (r *Registry) Enabled() bool { return r.enabled }

// RequiredRole 返回提交执行器任务所需的最低档位名（executors.required_role）。
// 它属于登记表而不是调度器：只有这里知道执行能力存在。
func (r *Registry) RequiredRole() string { return r.requiredRole }

// LoaderAllowed 返回是否允许目录任务加载器接受 exec. 前缀的任务文件。
// 自行接入 core.DirectoryLoader 的程序用它读取这个决定，避免配置项没有实际读取方（见 TASK-E17）。
func (r *Registry) LoaderAllowed() bool { return r.loaderAllowed }

// Keys 返回全部注册键，按字典序。返回的是副本，调用方改动不影响登记表。
func (r *Registry) Keys() []string {
	keys := make([]string, len(r.keys))
	copy(keys, r.keys)
	return keys
}

// Profiles 返回全部档位，按注册键字典序。返回的切片是副本，元素本身仍是登记表持有的对象。
func (r *Registry) Profiles() []*Profile {
	profiles := make([]*Profile, 0, len(r.keys))
	for _, key := range r.keys {
		profiles = append(profiles, r.entries[key].profile)
	}
	return profiles
}

// Lookup 按注册键取档位。
func (r *Registry) Lookup(handlerKey string) (*Profile, bool) {
	item, ok := r.entries[handlerKey]
	if !ok {
		return nil, false
	}
	return item.profile, true
}

// ProbeOf 返回某个档位的探测结论。
func (r *Registry) ProbeOf(handlerKey string) (ProbeResult, bool) {
	item, ok := r.entries[handlerKey]
	if !ok {
		return ProbeResult{}, false
	}
	return item.probe, true
}

// Available 判断档位当前能否执行；不可用时第二个返回值是可直接显示给用户的原因。
func (r *Registry) Available(handlerKey string) (string, bool) {
	item, ok := r.entries[handlerKey]
	if !ok {
		return "", false
	}
	if item.probe.Available {
		return "", true
	}
	return item.probe.Reason, false
}

func sortedKeys(entries map[string]*entry) []string {
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
