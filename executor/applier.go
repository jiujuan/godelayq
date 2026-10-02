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
	// configCommands 是"当前生效表里 config 那一批对应的原始命令列表"的记账，
	// 受上面这把 writeMu 保护，与 store 读写共用同一把锁（设计文档 §12"两条链共用一把写锁"）。
	//
	// 为什么记账放在 Applier 而不是把 Registry.executors 改成可变：那份冻结字段里的非
	// Commands 取值（workspace/runtime_allow/env_allow/两个 timeout）全是重启档或拒绝档，
	// 本系列没有任何合法路径去改它们；做成可变的等于给未来的误用留一个入口，
	// 而且 -race 会当场抓到 List() 那条无锁读（本卡 Registry 的字段集合零变化）。
	// 只在 writeMu 内读写，Apply 与 ApplyConfig 都在方法开头取锁。
	configCommands []core.ExecutorCommand
}

// executorsNow 返回"当前该用的 executors 取值"：非 Commands 的部分永远是启动期冻结的那一份，
// Commands 换成记账里的当前列表。合并 store 批次与建处理函数闭包都走这一份，
// 于是页面档位与 config 档位的撞名判定用的是同一张最新的名字表，而不是启动期那张过期的。
//
// 这个方法必须在 writeMu 内调用：它读 configCommands。Registry.executors 是构造后不再变化的
// 冻结字段，读它不需要锁。
func (a *Applier) executorsNow() core.ExecutorsConfig {
	current := a.registry.executors
	current.Commands = a.configCommands
	return current
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
		// 记账从登记表冻结的那一份起步：构造时 configCommands 就是 registry.executors.Commands，
		// 于是 executorsNow() 在第一次 ApplyConfig 之前与 registry.executors 完全相等。
		configCommands: registry.executors.Commands,
	}, nil
}

// NewConfigApplier 建一个只服务 config 批次、不带档位文件的同步器（设计文档 §13 的 P2）。
//
// 与 NewApplier 的差别的唯一来源是装配条件放宽：`Applier` 现在在 `web_enabled || (reload.enabled
// && enabled)` 时构造，而 `web_enabled=false` 的部署今天连 profiles_path 的父目录都不碰
// （W01 的冒烟证据）。为了热更档位去打开档位文件会破掉"关闭即惰性"这条口径，
// 所以这里不接 store：Apply（store 路径）返回明确错误而不是 nil panic，
// ApplyConfig（config 路径）照常可用。那种部署里登记表的 store 侧本来就没有条目，
// 撞名判定只看 config 批次，与今天的运行态一致。
func NewConfigApplier(syncer HandlerSync, registry *Registry,
	artifacts *ArtifactStore, logger *slog.Logger) (*Applier, error) {
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
		store:          nil,
		syncer:         syncer,
		registry:       registry,
		artifacts:      artifacts,
		logger:         logger,
		configCommands: registry.executors.Commands,
	}, nil
}

// Validate 按与启动完全同一条规则校验并探测一条档位定义（设计文档 §4 的 I1）。
//
// 校验用宽松路径模式（D5：页面建的档位可以指向本机其它路径），探测不启动任何进程。
// 返回的 Profile 已经带上解析后的绝对路径，调用方拿它去填响应即可。
// 探测结论为"不可用"时不返回错误——"这台机器现在跑不了"是可以保存的状态（§5.3）。
//
// 这里读的是登记表冻结的那份 a.registry.executors，而不是 a.executorsNow()：
// Validate 只看非 Commands 的许可字段（路径模式、timeout 上限），那些永不热更，
// 用冻结值就是正确的取值。而 writeMu 是写链的锁，不该被一次只读校验占住——
// 给它加锁会把页面提交挡在写档位后面，得不偿失。这个取舍是有意为之，不是漏了改。
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

	if a.store == nil {
		// NewConfigApplier 建出来的同步器没有档位文件（web_enabled=false 只热更 config 的部署）。
		// store 路径在这种部署里走不到：返回明确错误而不是 nil 指针 panic。
		return ApplyResult{}, fmt.Errorf(
			"executor applier: no profile store is configured, the store path (Apply) is unavailable")
	}

	records, err := a.store.List()
	if err != nil {
		return ApplyResult{}, fmt.Errorf("load executor profiles failed: %w", err)
	}

	entries, warnings := mergeStoreProfiles(a.executorsNow(), records)
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
	liveKeys := make(map[string]bool, len(live))
	configCount := 0
	for _, key := range live {
		source, ok := a.registry.SourceOf(key)
		if !ok {
			continue
		}
		liveKeys[key] = true
		if source == SourceStore {
			storeKeys = append(storeKeys, key)
		} else {
			configCount++
		}
	}
	result.Stored = len(storeKeys)
	result.Config = configCount
	result.Degraded = len(a.registry.Degraded())

	// 只重登记 store 那一批；config 那一批留在 liveKeys 里，摘除判定据此放过它们
	// （配置侧的处理函数归启动时的 executor.Register 管，一条都不重建）。
	result.Added, result.Removed = a.syncHandlers(storeKeys, liveKeys)

	return result, nil
}

// syncHandlers 把调度器里 exec. 前缀的处理函数对齐到登记表当前生效的那一批，
// 是 Apply 与 ApplyConfig 共享的"重登记 + 摘除"片段：差集逻辑只写这一份，
// 两条链的分别只落在入参上，不落在判据上。
//
//   - reRegister：本次要（重）建闭包的键集合。Apply 只交 store 那一批；
//     ApplyConfig 交整张生效表（config 那一批的闭包也要跟着新列表重建）。
//   - live：新表里全部生效键。摘除判定放过它们——只有"有前缀又不在表里"的键才被摘，
//     判据始终含 core.ExecPrefix，代码注册的普通任务键（不带前缀）永不触碰。
//
// 必须在 writeMu 内调用（它经 executorsNow 读 configCommands）。
// 先登记后摘除，与 Apply 既有顺序一致（闭包构造不碰进程，代价可忽略）。
func (a *Applier) syncHandlers(reRegister []string, live map[string]bool) (added, removed []string) {
	have := make(map[string]bool)
	for _, name := range a.syncer.HandlerNames() {
		if strings.HasPrefix(name, core.ExecPrefix) {
			have[name] = true
		}
	}

	executors := a.executorsNow()
	for _, key := range reRegister {
		profile, ok := a.registry.Lookup(key)
		if !ok {
			continue
		}
		replaced := have[key]
		// 走 Handler 这同一个构造口（与启动链 register.go 建的是同一种闭包），两条路径不会
		// 出现第二种处理函数。整批重登记：改的是同一条档位的闭包内容时靠键位看不出差别，
		// 而闭包构造只是把档位与产物存储绑在一起、不碰任何进程。
		a.syncer.RegisterHandlerClass(key, Handler(profile, a.artifacts, executors, a.logger),
			core.JobClassExec)
		if !replaced {
			added = append(added, key)
		}
	}

	for key := range have {
		if live[key] {
			continue
		}
		if a.syncer.UnregisterHandler(key) {
			removed = append(removed, key)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

// ApplyConfig 把 executors.commands 的新列表换进登记表与调度器（TASK-R04）。
//
// candidate 是整份新配置，本方法只用它的 Executors.Commands 一项：其余许可字段
// （workspace / runtime_allow / env_allow / 两个 timeout）是重启档，运行期一律用
// 启动期冻结在登记表里的那一份（设计文档 R8、§9）——但拦住它们的不是同一处：
//   - workspace / runtime_allow / 两个 timeout 由下面第 2 步的 LoadProfiles 按冻结值校验，
//     新档位越界即整次失败；
//   - env_allow 不参与那份校验（LoadProfiles 只看每条档位自己声明的 env_allow，
//     顶层那份全局名单要到执行时才被 BuildEnv 读，见 executor/env.go）。
//     这里靠闭包钉住它：syncHandlers 建闭包传的是 executorsNow()，那份取值的非 Commands
//     部分就是启动期冻结的配置，所以新 YAML 里放宽的 env_allow 到不了已经建好的处理函数。
//
// 返回值沿用 ApplyResult：Config/Stored/Degraded 是换完之后生效表的分项计数，
// Added/Removed 是这次处理函数的增减。
//
// 顺序固定，失败点全部在动手之前：
//  1. executors.enabled=false → 返回错误（登记表侧本来也拒绝，这里提前退出是为了
//     在没合成配置、没校验之前就走掉，热重载链据此把本次记成 failed）。
//  2. 把 candidate.Executors.Commands 接到冻结的非 Commands 取值上合成一份配置，
//     调 LoadProfiles（严格模式：任一条非法即整次失败，与启动同一条规则）。
//  3. 逐条 Probe，探测失败照常入表并记 warn（与 NewRegistry 同一口径）。
//  4. writeMu 内：registry.ApplyConfig(entries) 整表替换 → 重登记全部 exec. 键 →
//     摘掉不再存在的键 → 最后才把新列表记进 configCommands。
//
// 为什么第 2 步必须用冻结值而不是整份 candidate：workspace / runtime_allow 是重启档，
// 让 candidate 里放宽后的它们参与校验，就会出现"新档位按新 workspace/runtime 建、
// 既有档位仍按旧值跑"的分裂，表现是同一台机器上两条同名不同路径的档位都能过校验。
// 这条兜底把"免重启新增档位"限定在既有许可范围内，不能借热更扩边界（§9、§12 新增风险行）。
//
// 与 Apply 的三处关键差别（§3.3）：两批全部重登记、摘除范围放宽到全部 exec. 前缀键
// （被删掉的正是 config 键位，不像 Apply 那样放过 config）、降级条目不进调度器
// （重登记遍历 registry.Keys()，天然不含被顶掉的 store 条目，它的键位由那条新 config 档位占着）。
//
// 记账 configCommands 只在最后一步推进：前面任何一处失败都不会走到这里，所以一次失败的重载
// 之后，下一次 Apply 的撞名判定仍按旧列表（§5.2"非法即整次不动"的验证面）。
func (a *Applier) ApplyConfig(candidate core.Config) (ApplyResult, error) {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()

	// 步骤 1
	current := a.executorsNow()
	if !current.Enabled {
		return ApplyResult{}, fmt.Errorf("executors are disabled, executors.commands cannot be reloaded")
	}

	// 步骤 2：冻结的非 Commands 取值 + candidate 的新列表，合成一份只用于校验的配置。
	// 这份合成与测试里的 configWithCommands 是同一个构造点，避免造出实现拿不到的配置。
	synthetic := current
	synthetic.Commands = candidate.Executors.Commands
	profiles, err := LoadProfiles(core.Config{Executors: synthetic})
	if err != nil {
		return ApplyResult{}, err
	}

	// 步骤 3：探测只为 warn。登记表在 ApplyConfig 内部会对同一批再探一次，
	// 两处调的是同一个 Probe，结论不会分叉——所以这里不收集探测结果，只出日志。
	for _, profile := range profiles {
		if result := Probe(profile); !result.Available {
			a.logger.Warn("executor profile unavailable",
				"profile", profile.Name,
				"handler_key", profile.HandlerKey(),
				"kind", string(profile.Kind),
				"reason", result.Reason)
		}
	}

	// 步骤 4：整表替换在先，处理函数同步在后。registry.ApplyConfig 自身失败时旧表一字未动，
	// 而此刻调度器与 configCommands 都还没碰过，所以不存在"改了一半"的中间态。
	if err := a.registry.ApplyConfig(profiles); err != nil {
		return ApplyResult{}, err
	}

	live := a.registry.Keys()
	liveKeys := make(map[string]bool, len(live))
	var storedCount, configCount int
	for _, key := range live {
		source, ok := a.registry.SourceOf(key)
		if !ok {
			continue
		}
		liveKeys[key] = true
		if source == SourceStore {
			storedCount++
		} else {
			configCount++
		}
	}

	// 两批全部重登记：reRegister 就是整张生效表，live 也是它——
	// 摘除只动"有 exec. 前缀又不在表里"的键，被删掉的旧 config 键位正是这一批。
	added, removed := a.syncHandlers(live, liveKeys)

	result := ApplyResult{
		Stored:   storedCount,
		Config:   configCount,
		Degraded: len(a.registry.Degraded()),
		Added:    added,
		Removed:  removed,
	}

	// 记账最后一步才推进（理由见方法头注释）。
	// 这里的 append 只复制最外层切片：调用方之后对自己那份列表做增、删、换顺序，动不到这份记账；
	// 但元素是值拷贝，每条命令里的 FixedArgs / Args / ArgsRender / EnvAllow / RetryOnExit 这些
	// 内层切片（以及 Env / Headers 这类映射）仍与调用方共享同一份底层数组，原地改某一条的元素
	// 会连这里一起改。今天唯一的 candidate 生产方（热重载链）每次都把配置文件整份重新解析、
	// 交来一批全新的命令对象，不会留着旧列表去原地改，所以这里只隔离外层、不做深拷贝。
	a.configCommands = append([]core.ExecutorCommand(nil), candidate.Executors.Commands...)

	return result, nil
}
