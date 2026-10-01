package executor

import (
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"godelayq/core"
)

// Source 说出一条档位来自哪一份来源。
//
// 两份来源在启动时合并成一张表（docs/design/web-profile-design.md §5.2），
// 而"这台机器现在能执行什么"从此必须靠这个字段才说得清。
type Source string

const (
	// SourceConfig 来自 config.yaml 的 executors.commands：页面上只读。
	SourceConfig Source = "config"
	// SourceStore 来自 executors.profiles_path 指向的档位文件：页面上可增删改。
	SourceStore Source = "store"
)

// entry 是登记表里的一条：校验过的档位，加上这台机器的可用性结论与来源。
type entry struct {
	profile *Profile
	probe   ProbeResult
	source  Source
}

// DegradedProfile 是一条"看得见但没生效"的档位：它来自档位文件，
// 注册键却与 config.yaml 里的同名档位撞上，于是留在展示面里、不注册处理函数。
//
// 为什么要留着它：静默丢掉的那一条会让"页面上明明建过、重启后不见了"变成无解之谜，
// 而把冲突写进接口与启动日志，运维一眼就能改档位名或改配置。
// 见设计文档 §5.2（那是待拍板 P1 取的答案：宁可降级，不让一次页面操作把进程逼停）。
type DegradedProfile struct {
	// Profile 是被降级的那条档位
	Profile *Profile
	// Probe 是它的探测结论：降级与"这台机器跑不了"是两件事，两个可以同时为真
	Probe ProbeResult
	// Reason 是降级原因，可直接展示给用户
	Reason string
}

// StoreEntry 是 ApplyStore 的入参：一条来自档位文件的档位。
type StoreEntry struct {
	Profile *Profile
	Probe   ProbeResult
	// Source 留空按 SourceStore 处理；这个入口只服务 store 侧，config 侧由 NewRegistry 填
	Source Source
	// Degraded 为真表示这条与已有的 config 档位撞名：进展示面、不进注册表
	Degraded bool
	// Reason 只在 Degraded 时有意义；留空由登记表给一句默认说明
	Reason string
}

// Registry 是"档位 + 可用性 + 来源"的登记表，可以在运行期整表替换。
//
// 它与调度器的处理函数注册表（core.Scheduler.LookupHandler）是两份东西：
// 那份回答"这个任务类型有没有处理函数"，这份回答"这个档位是谁配的、配的是什么、现在能不能跑"。
// api 层用后者做展示与提交期拒绝（TASK-E07、E16），前者照旧承担实际执行。
//
// 表本身不可变：entries 与 keys 装进同一份 snapshot，替换是"构造新的一整份、一次换掉指针"。
// 因此读侧永远看到一张自洽的表，读方法不需要加锁——这是 TASK-W03 改写的新口径，
// 取代原来那句"构造之后不再变化"。跟着来的约束是**每个方法只做一次 Load**：
// 同一个方法里读两次就可能拿到两张不同的表，"档位查得到、探测查不到"那种自相矛盾
// 就会真实发生（DoD 要求 grep 确认这一点）。
//
// 写侧只有 writeMu，它串行化"构造 + 替换"这一段，不参与任何读取。
type Registry struct {
	enabled       bool
	requiredRole  string
	loaderAllowed bool
	inlinePreview int
	// executors 是归一化之后的那一节配置，只用于算生效超时：
	// 接口在提交期要把"任务实际会按多长的超时落盘"算出来（TASK-E16 §3.2），
	// 而这条合成规则的执行侧版本在 Profile.timeoutWithin，两处必须同一个入口。
	executors core.ExecutorsConfig

	writeMu sync.Mutex
	data    atomic.Pointer[snapshot]
}

// NewRegistry 加载并探测 config.yaml 里声明的全部档位。
//
// 探测失败的档位照常留在表里，只记一条 warn：让运维在接口上看得见"已声明但不可用"，
// 比让它静默消失更容易发现问题，所以这里不因不可用而返回错误。
// 档位配置本身非法（越界路径、引用未声明的参数等）仍然返回错误，由装配方决定是否终止启动。
//
// executors.enabled 为 false 时返回空登记表（不是 nil），调用方不需要判空指针分支。
//
// 本函数只装 SourceConfig 那一侧；档位文件里的条目由装配方在构造之后调 ApplyStore 合进来
// （TASK-W05）。分开的原因是校验口径不同：配置侧一条写错即启动失败，
// 文件侧一条写错只跳过那一条。
func NewRegistry(cfg core.Config, logger *slog.Logger) (*Registry, error) {
	normalized := cfg.Normalized()

	registry := &Registry{
		enabled:       normalized.Executors.Enabled,
		requiredRole:  normalized.Executors.RequiredRole,
		loaderAllowed: normalized.Executors.LoaderAllow,
		// 预览上限在开关关闭时也有取值：接口用它裁剪一条已经存在快照里的输出预览，
		// 与"这台机器现在能不能执行"无关。
		inlinePreview: normalized.Executors.Output.InlinePreview,
		executors:     normalized.Executors,
	}
	if !registry.enabled {
		// 关闭即完全惰性：不加载档位也不探测。配置里的错误要到打开执行器时才暴露，
		// 这与"enabled=false 时本节取值全部不生效"的口径一致。
		registry.data.Store(newSnapshot(nil, nil))
		return registry, nil
	}
	if logger == nil {
		logger = slog.Default()
	}

	profiles, err := LoadProfiles(normalized)
	if err != nil {
		return nil, err
	}

	entries := make(map[string]*entry, len(profiles))
	for _, profile := range profiles {
		result := Probe(profile)
		if !result.Available {
			logger.Warn("executor profile unavailable",
				"profile", profile.Name,
				"handler_key", profile.HandlerKey(),
				"kind", string(profile.Kind),
				"reason", result.Reason)
		}
		entries[profile.HandlerKey()] = &entry{profile: profile, probe: result, source: SourceConfig}
	}
	registry.data.Store(newSnapshot(entries, nil))

	return registry, nil
}

// ApplyStore 用给进来的这一批档位重建登记表的 store 部分。
//
// 语义是**整批替换**：调用方给出此刻档位文件里的全部条目，不在这一批里的旧 store 条目消失，
// config 来源的条目一律保留。没有"只改一条"的增量入口——档位数量级用不着增量，
// 而增量改要自己维护 keys 的有序性，多一处会写错的地方。
//
// 失败时旧表原样不动（不会留下"删了一半"的中间态），所以调用方可以先落盘再生效。
// 两条拒绝规则：
//   - 同一批里注册键重复：直接拒，说明调用方给的那份文件自相矛盾。
//   - 与已有的 config 档位撞名：只有标了 Degraded 的条目允许（进展示面不进注册表，§5.2 的降级路径）；
//     没标的按编程错误拒掉——"配置里那条被页面上那条顶掉"是方向性错误，不能静默发生。
func (r *Registry) ApplyStore(items []StoreEntry) error {
	if !r.enabled {
		// 执行器关闭时一个 exec.* 类型都不注册，往里合 store 条目会造出
		// "接口看得见、调度器跑不了"的半状态。正常装配走不到这里（executors.web_enabled
		// 要求 executors.enabled 为真，core/config.go 的 Validate 已经挡住那种组合），
		// 所以这条是跨包写入口的边界，不是给运维的报错。
		return fmt.Errorf("executors are disabled, the profile store cannot be applied")
	}

	r.writeMu.Lock()
	defer r.writeMu.Unlock()

	current := r.data.Load()
	merged := make(map[string]*entry, len(current.entries)+len(items))
	for key, item := range current.entries {
		if item.source == SourceStore {
			continue
		}
		merged[key] = item
	}

	var degraded []DegradedProfile
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		if item.Profile == nil {
			return fmt.Errorf("executor registry: store entry without a profile")
		}
		key := item.Profile.HandlerKey()
		if seen[key] {
			return fmt.Errorf("executor registry: duplicate handler key %q in one store batch", key)
		}
		seen[key] = true

		source := item.Source
		if source == "" {
			source = SourceStore
		}

		if existing, clash := merged[key]; clash {
			if !item.Degraded {
				return fmt.Errorf("handler key %q is already used by a %s profile, store profile %q cannot take it",
					key, existing.source, item.Profile.Name)
			}
			reason := item.Reason
			if reason == "" {
				reason = fmt.Sprintf("profile %q is already declared in executors.commands, the stored one is not registered",
					item.Profile.Name)
			}
			degraded = append(degraded, DegradedProfile{Profile: item.Profile, Probe: item.Probe, Reason: reason})
			continue
		}
		merged[key] = &entry{profile: item.Profile, probe: item.Probe, source: source}
	}

	// keys 与 entries 必须出自同一次构造：这正是整表替换要保住的那件事。
	r.data.Store(newSnapshot(merged, degraded))
	return nil
}

// Enabled 返回执行器总开关，供接口区分"没开"与"开了但没有档位"。
func (r *Registry) Enabled() bool { return r.enabled }

// RequiredRole 返回提交执行器任务所需的最低档位名（executors.required_role）。
// 它属于登记表而不是调度器：只有这里知道执行能力存在。
func (r *Registry) RequiredRole() string { return r.requiredRole }

// LoaderAllowed 返回是否允许目录任务加载器接受 exec. 前缀的任务文件。
//
// 它是 executors.loader_allow 唯一的读取方（TASK-E17）：服务端不启用目录加载器，
// 自行接入 core.DirectoryLoader 的程序用这个取值去填 LoaderOptions.AllowExecJobs。
// 加载器本身不回读配置，所以把配置接到行为上的那一步在调用方。
func (r *Registry) LoaderAllowed() bool { return r.loaderAllowed }

// InlinePreview 返回输出预览的字节上限（executors.output.inline_preview）。
// 接口在把摘要透出去之前用它再裁一次：摘要一旦落盘，写它的那个配置值可能已经改小。
func (r *Registry) InlinePreview() int { return r.inlinePreview }

// EffectiveTimeout 返回这个档位在一次提交里实际生效的超时（TASK-E16 §3.2 第 1 条）。
//
// requested 是 payload 的 timeout 取值（没有填就是 0）。规则与执行侧完全同一条：
// 都走 Profile.timeoutWithin，入参是登记表里那份归一化过的配置。
// 接口之所以要在提交期算一次：任务详情显示的超时必须是真正会生效的那一个，
// 否则"详情写 5m、实际 30s 就断"这种落差会一直留在工单里。
//
// 这个方法只用配置、不读表，因此与整表替换的时序无关。
func (r *Registry) EffectiveTimeout(p *Profile, requested time.Duration) time.Duration {
	return p.timeoutWithin(r.executors, requested)
}

// MaxTimeout 返回单次执行超时的上限（executors.max_timeout，已归一化为取值）。
//
// 它是给提交表单算"可填区间"用的：档位默认值在每个档位的 timeout 字段里，
// 上限是这一个全局值，超过它的请求在提交期就被拒（TASK-E16 §3.2 第 2 条），
// 所以前端不该让人填出注定失败的取值。
func (r *Registry) MaxTimeout() time.Duration { return r.executors.MaxTimeout }

// Keys 返回全部注册键，按字典序。返回的是副本，调用方改动不影响登记表。
func (r *Registry) Keys() []string {
	snap := r.data.Load()
	keys := make([]string, len(snap.keys))
	copy(keys, snap.keys)
	return keys
}

// Profiles 返回全部档位，按注册键字典序。返回的切片是副本，元素本身仍是登记表持有的对象。
//
// 不含被降级的条目：那批档位没有注册处理函数，出现在这里等于让人去提交一个
// "类型名存在但跑不了"的任务。那批由 Degraded() 单独给——接口要能区分这两种状态。
func (r *Registry) Profiles() []*Profile {
	snap := r.data.Load()
	profiles := make([]*Profile, 0, len(snap.keys))
	for _, key := range snap.keys {
		profiles = append(profiles, snap.entries[key].profile)
	}
	return profiles
}

// Lookup 按注册键取档位。
func (r *Registry) Lookup(handlerKey string) (*Profile, bool) {
	snap := r.data.Load()
	item, ok := snap.entries[handlerKey]
	if !ok {
		return nil, false
	}
	return item.profile, true
}

// ProbeOf 返回某个档位的探测结论。
func (r *Registry) ProbeOf(handlerKey string) (ProbeResult, bool) {
	snap := r.data.Load()
	item, ok := snap.entries[handlerKey]
	if !ok {
		return ProbeResult{}, false
	}
	return item.probe, true
}

// Available 判断档位当前能否执行；不可用时第二个返回值是可直接显示给用户的原因。
//
// 查不到的键回 ("", false)：调用方分不清"没这个档位"与"有但不可用"时，
// 那是它自己的判据问题，本方法不替它编一个原因出来。
func (r *Registry) Available(handlerKey string) (string, bool) {
	snap := r.data.Load()
	item, ok := snap.entries[handlerKey]
	if !ok {
		return "", false
	}
	if item.probe.Available {
		return "", true
	}
	return item.probe.Reason, false
}

// SourceOf 返回某个注册键的来源，第二个返回值表示这个键在表里。
//
// 来源与降级状态都不放在 Profile 上：那是"登记表对这条档位的判断"，不是档位定义的一部分。
// 接口要两者齐看时按 HandlerKey() 反查这里。
func (r *Registry) SourceOf(handlerKey string) (Source, bool) {
	snap := r.data.Load()
	item, ok := snap.entries[handlerKey]
	if !ok {
		return "", false
	}
	return item.source, true
}

// Degraded 返回当前被降级的 store 档位（撞名那批），按注册键字典序。返回的是副本。
func (r *Registry) Degraded() []DegradedProfile {
	snap := r.data.Load()
	items := make([]DegradedProfile, len(snap.degraded))
	copy(items, snap.degraded)
	sort.Slice(items, func(i, j int) bool {
		return items[i].Profile.HandlerKey() < items[j].Profile.HandlerKey()
	})
	return items
}

// snapshot 是一张不可变的登记表。entries 与 keys 必须成对构造：
// 分两次换会让读侧在两个时刻之间拿到"键在、档位不在"的自相矛盾结果。
type snapshot struct {
	entries  map[string]*entry
	keys     []string // 已按注册键字典序排好，接口输出要稳定
	degraded []DegradedProfile
}

func newSnapshot(entries map[string]*entry, degraded []DegradedProfile) *snapshot {
	if entries == nil {
		entries = map[string]*entry{}
	}
	return &snapshot{entries: entries, keys: sortedKeys(entries), degraded: degraded}
}

func sortedKeys(entries map[string]*entry) []string {
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
