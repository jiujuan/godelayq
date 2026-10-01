package executor

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"godelayq/core"
)

// HandlerSync 是把档位表同步到调度器注册表所需的调度器能力：
// 在 Registrar 之上多了"枚举现有键"与"摘除一个键"。
//
// 枚举是必需的：同步的语义是"让 exec. 前缀的处理函数等于登记表里生效的那批"，
// 没有读回全部注册键的口子就看不出哪几个该摘掉。
// *core.Scheduler 满足它（后两个方法是 TASK-W04 加的）。
type HandlerSync interface {
	Registrar
	// UnregisterHandler 摘除一个注册键（成对删 handlers 与 handlerClasses）。
	UnregisterHandler(jobType string) bool
	// HandlerNames 返回全部已注册的键，按字典序。
	HandlerNames() []string
}

// ApplyResult 是一次"把档位文件同步进登记表与调度器"的结果，供调用方直接回给用户。
type ApplyResult struct {
	// Stored 是同步之后登记表中生效的 store 侧档位数（不含降级条目）
	Stored int
	// Config 是同步之后登记表中生效的 config 侧档位数
	Config int
	// Degraded 是因与 executors.commands 撞名而没生效的 store 条目数
	Degraded int
	// Added 是这次新登记进调度器的键，Removed 是这次被摘掉的键，
	// 两者都只涉及 exec. 前缀，代码注册的普通任务一律不在其中。
	Added   []string
	Removed []string
	// Warnings 是档位文件里没能进表的记录（与启动路径同一口径：单条不连坐）
	Warnings []ProfileWarning
}

// Applier 把"档位文件 → 登记表 → 调度器注册表"这条链收在一个对象里。
//
// 为什么要有它而不是让调用方自己串：这三步的**顺序与差异计算**是规则而不是接线——
// 谁先谁后错了会留下"接口看得见档位、调度器没有处理函数"的半状态，
// 而这段判据（只动 store 来源、只摘 exec. 前缀、探测与校验用同一份配置）
// 放在装配方就等于每个装配方各写一遍。
//
// 三条口径：
//   - 配置侧的处理函数一次都不重建：启动时 executor.Register 注册的那批保持原样，
//     本类型只负责 store 那一侧（design §5.2 的来源划分）。
//   - 摘除只看 core.ExecPrefix 前缀的键：代码注册的 payment_check 之类永不受影响。
//   - Apply 全程持一把写锁：两次并发的页面写入不能交叉着读同一份文件再各自生效，
//     后一次必须看到前一次的落盘结果。
type Applier struct {
	store    core.ExecutorProfileStore
	syncer   HandlerSync
	registry *Registry
	// artifacts 为 nil 时处理函数照样能建出来，只是每次执行都会因为没地方写输出而失败；
	// 装配方（cmd/server）在 executors.enabled=true 时一定给一个可用的进来。
	artifacts *ArtifactStore
	logger    *slog.Logger

	writeMu sync.Mutex
}

// NewApplier 建一个档位同步器。registry 与 store 都不可为 nil；
// registry 提供 executors 一节的归一化取值与生效表，store 是档位文件的读写口。
func NewApplier(store core.ExecutorProfileStore, syncer HandlerSync,
	registry *Registry, artifacts *ArtifactStore, logger *slog.Logger) (*Applier, error) {
	if store == nil {
		return nil, fmt.Errorf("executor applier: a profile store is required")
	}
	if syncer == nil {
		return nil, fmt.Errorf("executor applier: a handler registry is required")
	}
	if registry == nil {
		return nil, fmt.Errorf("executor applier: a profile registry is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Applier{
		store:     store,
		syncer:    syncer,
		registry:  registry,
		artifacts: artifacts,
		logger:    logger,
	}, nil
}

// Validate 按与启动完全同一条规则校验并探测一条档位定义（设计文档 §4 的 I1）。
//
// 校验用宽松路径模式（D5：页面建的档位可以指向本机其它路径），探测不启动任何进程。
// 返回的 Profile 已经带上解析后的绝对路径，调用方拿它去填响应即可。
// 探测结论为"不可用"时不返回错误——"这台机器现在跑不了"是可以保存的状态（§5.3）。
func (a *Applier) Validate(cmd core.ExecutorCommand) (*Profile, ProbeResult, error) {
	profile, err := BuildProfile(cmd, a.registry.executors, PathAnywhere)
	if err != nil {
		return nil, ProbeResult{}, err
	}
	return profile, Probe(profile), nil
}

// Apply 重读档位文件，把它整表同步进登记表与调度器。
//
// 顺序固定：读文件 → 校验/探测 → ApplyStore（整表替换）→ 同步处理函数。
// ApplyStore 失败时直接返回错误，此刻登记表与调度器都还没动过，
// 而调用方的写入（Save/Delete）已经落盘——那是 I2 的另一面：
// 落盘先于生效，所以生效失败要由调用方回滚文件并说明"重启可对齐"。
//
// 处理函数的同步是声明式的（"应当是这批"），因此可重入：同一份文件连 Apply 两次，
// 第二次既不新增也不摘除。
func (a *Applier) Apply() (ApplyResult, error) {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()

	records, err := a.store.List()
	if err != nil {
		return ApplyResult{}, fmt.Errorf("load executor profiles failed: %w", err)
	}

	entries, warnings := mergeStoreProfiles(a.registry.executors, records)
	result := ApplyResult{Warnings: warnings}
	for _, warning := range warnings {
		a.logger.Warn("stored executor profile not registered",
			"profile", warning.Name, "reason", warning.Reason)
	}

	if err := a.registry.ApplyStore(entries); err != nil {
		return result, err
	}

	// 生效表是刚才那一批的投影：Keys() 与 SourceOf() 各自只读一次快照，
	// 这里先取一次存下来，后面的差集都对着这一份算。
	live := a.registry.Keys()
	storeKeys := make([]string, 0, len(live))
	configKeys := make([]string, 0, len(live))
	for _, key := range live {
		source, ok := a.registry.SourceOf(key)
		if !ok {
			continue
		}
		if source == SourceStore {
			storeKeys = append(storeKeys, key)
		} else {
			configKeys = append(configKeys, key)
		}
	}
	result.Stored = len(storeKeys)
	result.Config = len(configKeys)
	result.Degraded = len(a.registry.Degraded())

	want := make(map[string]bool, len(storeKeys))
	for _, key := range storeKeys {
		want[key] = true
	}
	// 配置侧的键位单独留一份：摘除判定要放过它们，
	// 因为崩溃恢复守卫与 executor.Register 都在启动时就认得这批。
	keepConfig := make(map[string]bool, len(configKeys))
	for _, key := range configKeys {
		keepConfig[key] = true
	}
	have := make(map[string]bool)
	for _, name := range a.syncer.HandlerNames() {
		if strings.HasPrefix(name, core.ExecPrefix) {
			have[name] = true
		}
	}

	// 该注册的：store 来源的全部键。PUT 改的是同一条档位的闭包内容，
	// 靠键位看不出差别，所以整批重登记——闭包构造只是把档位与产物存储绑在一起，
	// 不碰任何进程，重登记的代价与它的正确性相比不用考虑。
	executors := a.registry.executors
	for _, key := range storeKeys {
		profile, ok := a.registry.Lookup(key)
		if !ok {
			continue
		}
		replaced := have[key]
		a.syncer.RegisterHandlerClass(key, Handler(profile, a.artifacts, executors, a.logger),
			core.JobClassExec)
		if !replaced {
			result.Added = append(result.Added, key)
		}
	}

	// 该摘除的：调度器里有、生效表里没有、也不属于配置侧的 exec. 键。
	// 降级条目不会在这里被摘掉——它的键位由 config 那条占着，仍在生效表里（§5.2）。
	for key := range have {
		if want[key] || keepConfig[key] {
			continue
		}
		if a.syncer.UnregisterHandler(key) {
			result.Removed = append(result.Removed, key)
		}
	}
	sort.Strings(result.Added)
	sort.Strings(result.Removed)

	return result, nil
}
