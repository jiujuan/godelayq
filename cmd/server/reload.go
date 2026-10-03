package main

// 本文件是配置热重载的链本体（设计文档 §4、§7.8，任务卡 TASK-R06 §3）。
//
// R01 交付判据（分档表 + Diff + ReloadState）、R02/R03/R04 交付落点（六个 setter、
// ResizeWorkers、ApplyConfig）、R05 交付触发时机（ConfigWatcher），这里把它们接成一条固定顺序的链：
//
//	读通整份文件 → Normalized → Diff → 拒绝档整次作废 → 无变化就什么也不做
//	→ 按 §3.2 的顺序逐项应用（每项成功后把"用旧值重放同一段代码"压栈）
//	→ 中途失败逆序回滚 → 全部成功才把 applied 换成 candidate
//
// 三条不变量在这里各自的落点：
//   - I1：applied 只在这条链全部成功时往前推，别处不写它；
//   - I2：拒绝档早于任何写入，应用失败逆序回滚，回滚再失败才进 degraded；
//   - I3：没生效的键必须有归宿（ignored_keys / error / warn），不许只留一行日志。
//
// 为什么是一个类型而不是 run() 里的一串闭包：这条链的状态（applied、串行锁、待回滚的旧值）
// 跨多次重载存活，塞进 main.go 会让那个文件同时承担装配与运行期行为两件事。

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"godelayq/core"
	"godelayq/executor"
)

// 热更键路径。它们与 core/config_reload.go 的 configClasses 里标成 ClassHot 的那些条目一一对应，
// 单列成常量是为了让"分派表覆盖到哪条键"能被用例逐条比出来
// （cmd/server/reload_test.go 的 TestReloadEveryHotKeyHasDispatchEntry）。
//
// 这里加一条而 core 那边漏一条，由该用例判红；反过来 core 新增热更键而这里没加，
// 由 applyChange 里那条"未分派的热更键"前置检查判红（结果 failed，一条都不应用）。
const (
	keyLoggingLevel            = "logging.level"
	keySchedulerWorkers        = "scheduler.workers"
	keySchedulerMaxRetryDelay  = "scheduler.max_retry_delay"
	keyStoreHistoryLimit       = "store.history_limit"
	keyStoreHistoryTTL         = "store.history_ttl"
	keyObservabilityEventCount = "observability.events.retention_count"
	keyObservabilityEventAge   = "observability.events.retention_age"
	keyObservabilityAuditCount = "observability.audit.retention_count"
	keyObservabilityAuditAge   = "observability.audit.retention_age"
	keyExecutorsCommands       = "executors.commands"
	keyReloadDebounce          = "reload.debounce"
)

// maxHotReloadWorkers 是 scheduler.workers 在运行期允许热更到的上界。
//
// 它挡的是 R03 登记给本卡的那条缺陷（task-r03 §10.5 D3）：ResizeWorkers 只有 n<=0 的下限判定，
// 把配置文件里写错的 scheduler.workers: 100000 直接透传下去，就会在那一刻真的起十万条协程——
// 而那份取值照样过 core 的 Validate（Validate 只拦负数，见 core/config.go 的 scheduler.workers 一条）。
// 4096 的取法：默认并发是 100（core.DefaultConcurrency），4096 已经留出四十倍的余量，
// 再往上就不再是"调并发"而是把配置文件当 DoS 载荷。真需要更高的并发应当改启动配置并重启，
// 那条路径上还有队列容量与执行器池一起配着，不该由一次文件保存独力决定。
//
// 这条上限同时挡住了 int32 溢出：Scheduler.targetWorkers 是 atomic.Int32，
// 超过 math.MaxInt32 的取值会绕成负数，那比多起协程更难查。
//
// 它落在链上（buildPlan 的 #6 那一步）而不是 newReloader 给 setWorkers 包的那一层，
// 是因为"这一批校验"属于重载链的职责（R03 交接过来的原话就是"在重载链补上界校验"），
// 也让它能被替身测到：newReloader 里那一层在单测里根本不被调用。
const maxHotReloadWorkers = 4096

func checkWorkerBound(n int) error {
	if n > maxHotReloadWorkers {
		return fmt.Errorf("scheduler.workers=%d 超过运行期可热更的上界 %d，本次重载作废（要更高的并发请改启动配置并重启）",
			n, maxHotReloadWorkers)
	}
	return nil
}

// reloadTargets 是八个热更落点的集合（卡 §3.2）。全部是函数值，与 runtimeDeps 的风格一致：
// run() 与 reloader 都不认识具体实现，测试替身因此不需要真的起进程。
//
// 与卡面签名的唯一差异：那五个"生产实现不返回错误"的落点也带一个 error 结果，
// 恒为 nil。理由是不加它就做不到"回滚失败"这条判据可被用例触发（§5.1 #3 要 setRetry 的 undo 失败），
// 而为了造那条形状去引入类型断言或额外的注册表，都比统一多一个返回值更糟。
type reloadTargets struct {
	setLevel          func(level string) error                 // R02: core.SetLogLevel(levelVar, ...)
	setWorkers        func(n int) error                        // R03: core.Scheduler.ResizeWorkers
	setRetry          func(maxDelay time.Duration) error       // R02: scheduler.SetRetryPolicy
	setRetention      func(limit int, ttl time.Duration) error // R02: store.SetHistoryRetention
	setEventRetention func(count int, age time.Duration) error // R02: sqlite.EventLog.SetRetention（未启用时 nil）
	setAuditRetention func(count int, age time.Duration) error // R02: sqlite.AuditLog.SetRetention（未启用时 nil）
	setCommands       func(candidate core.Config) error        // R04: Applier.ApplyConfig（未装配 applier 时 nil）
	setDebounce       func(d time.Duration) error              // R05: ConfigWatcher.SetDebounce（watcher 未建时 nil）
}

// unavailableOutcome 是"这一步在现在的部署里落不下去"时的结论。三条都有明确的读出口，
// 因为"没生效必须说出口"（I3）是本系列的靶子，三种情形给三种说法。
type unavailableOutcome int

const (
	// unavailableFails 装配缺入口：这是编程错误，整次作废并且一条都不应用。
	// 静默跳过等于让"归档成热更、但没人应用"的键悄悄不生效。
	unavailableFails unavailableOutcome = iota
	// unavailableIgnored 那一节没启用（观测层写入器、监听器）：键进 ignored_keys，不算失败。
	// 总开关关着时本节的取值本来就不生效（与 §6.2 的重启档同一个脸色）。
	unavailableIgnored
	// unavailableNotInEffect 改动已经记录、但这一节没启用（executors.enabled=false）：
	// 键留在 applied_keys 并记一条 warn，结论仍是 ok。卡 §3.1 第三条的特殊处理。
	unavailableNotInEffect
)

// reloadStep 是 §3.2 那张顺序表上的一行。
type reloadStep struct {
	// label 进日志与错误文本，说清失败发生在哪一步
	label string
	// keys 是这一步认领的热更键路径（精确匹配）
	keys []string
	// prefix 非空时按前缀认领：executors.commands 会摊成 executors.commands.<name>.<field>
	prefix string
	// matched 是本次真的改到、并由这一步认领的那些键（构造计划时填）
	matched []string
	// run 用给定那一份配置的取值落这个点。传 candidate 就是应用，传旧的 applied 就是回滚——
	// 同一段代码两个用途，所以"回滚漏了一项"这种形状在结构上不存在（卡 §9 风险表第一行）。
	run func(cfg core.Config) error
	// available 报告落点在不在。它在构造计划时就算好，运行期不再变。
	available bool
	// unavailable 是落点不在时该报哪种脸色（那一节没启用 / 装配缺入口），与结论一一对应。
	unavailable unavailableOutcome
}

// claims 判断一条热更键路径归不归这一步。
func (s *reloadStep) claims(path string) bool {
	if s.prefix != "" {
		return path == s.prefix || strings.HasPrefix(path, s.prefix+".")
	}
	for _, key := range s.keys {
		if key == path {
			return true
		}
	}
	return false
}

// reloadChain 是装配方眼里的重载链：走一次重载、把防抖落点接上、收口。
//
// 为什么要这三个动作的一张接口而不是直接用 *reloader：
//   - chain.Reload 要交给 watcher 当回调，而 newConfigWatcher 是 runtimeDeps 的函数值字段，
//     签名里拿到的只能是这个值，测试塞假 watcher 时也一样；
//   - bindDebounce 是链与 watcher 之间那个环形依赖的开口（watcher 要链的回调，链要 watcher 的
//     SetDebounce），接进来之后 reloader 与替身走的是同一条装配路径，不需要类型断言；
//   - Stop 要能被 §5.2 #14 的替身记进调用流水——关闭顺序那条判据看的正是"链收口"这一步的位次，
//     真实实现只有一个 *reloader，记不出这一步。
type reloadChain interface {
	Reload() (core.ReloadState, error)
	Stop()
	bindDebounce(watcher configWatcherAPI)
}

// reloader 持有"当前生效的那份配置"，并把一次文件变化走完一条固定顺序的链。
type reloader struct {
	// mu 串行整条链。两层串行别混（R05 已交付的事实）：
	//   1. watcher 持自己的 reloadMu 把"调用 + 保存状态"整段串行，所以同一个 watcher
	//      不会并发调进来两次（调用点是防抖计时器的协程，不是事件循环）；
	//   2. 这把 mu 保护的是**另一件事**：applied 这份权威同时被装配阶段与关停路径读，
	//      而链本身也要能被直接调（§5.1 #11 就是两个 goroutine 直接调 Reload），
	//      链必须自己站得住，不能指望上游替它串行。
	mu      sync.Mutex
	applied core.Config // 当前生效的那份配置；只有整条链成功才换（I1）
	// processEnabled 是**启动时**那份 reload.enabled，建链时定下，之后不再改。
	// 它单独存一份而不是每次读 applied.Reload.Enabled：开关属重启档，而第 7 步的
	// applied = candidate 会把文件里那份新值一并带进权威（那正是"重启档不再重复报一遍"要的），
	// 于是第二次重载起 applied 里那一位说的是文件说了什么，而不是进程正在按什么跑。
	// 读数要的是后者（R05 交给本卡的 D-R0503），所以在这里把它钉死在建立时那一份上。
	processEnabled bool
	// cfgPath 是启动时真正读到的那个文件的绝对路径；空串表示进程在用默认值
	cfgPath string
	targets reloadTargets
	logger  *slog.Logger
	// stopped 由 Stop 在持锁时置真：之后的 Reload 立即返回空结论，不读文件也不动落点。
	stopped bool
}

// reloadDeps 是装配一条重载链要的下游句柄。全部按窄接口给出，reloader 里因此没有任何类型断言。
type reloadDeps struct {
	cfgPath   string
	applied   core.Config
	store     core.Store
	scheduler schedulerAPI
	events    eventLogAPI
	audit     auditLogAPI
	// applier 是档位同步器；nil 表示这次部署没装配它（web_enabled 与 reload 都没打开）。
	applier *executor.Applier
	// executorsEnabled 决定档位那一步是"落下去"还是"本节未启用"。它必须由装配方给：
	// reloader 只认识 core.Config 的取值，而 executors.enabled 是重启档，
	// 读 candidate 里那份会允许人靠改文件打开执行器。
	executorsEnabled bool
	levelVar         *slog.LevelVar
	logger           *slog.Logger
}

// newReloader 按 §3.2 的顺序表把下游包成一条链。
//
// 它不在这里接 watcher 的 SetDebounce：watcher 要拿 reloader.Reload 当回调，两者互相引用。
// 装配方建好 watcher 之后调 bindDebounce 补上那一只落点——那一步早于 watcher.Run，
// 所以没有任何并发方看得到"半条链"。
func newReloader(d reloadDeps) *reloader {
	logger := d.logger
	if logger == nil {
		logger = slog.Default()
	}
	targets := reloadTargets{
		// #1 日志级别：最先，后面每一步都要用它记日志，级别改了应当立刻生效于本次重载自己的日志。
		setLevel: func(level string) error {
			return core.SetLogLevel(d.levelVar, level)
		},
		// #6 并发数：唯一会起协程的一步。上界校验在链的 #6 步里（见 maxHotReloadWorkers）。
		setWorkers: func(n int) error {
			return d.scheduler.ResizeWorkers(n)
		},
		// #3 重试上限：只换策略实例，无副作用。构造方式与启动期同一条（main.go 建的就是这一种）。
		setRetry: func(maxDelay time.Duration) error {
			d.scheduler.SetRetryPolicy(&core.ExponentialBackoffRetry{MaxDelay: maxDelay})
			return nil
		},
		// #4 留痕：两个键必须一起给（R02 §3.5 的口径），下一次写入触发的 trim 才用到新值。
		setRetention: func(limit int, ttl time.Duration) error {
			d.store.SetHistoryRetention(limit, ttl)
			return nil
		},
	}
	if d.events != nil {
		targets.setEventRetention = func(count int, age time.Duration) error {
			d.events.SetRetention(count, age)
			return nil
		}
	}
	if d.audit != nil {
		targets.setAuditRetention = func(count int, age time.Duration) error {
			d.audit.SetRetention(count, age)
			return nil
		}
	}
	if d.applier != nil && d.executorsEnabled {
		targets.setCommands = func(candidate core.Config) error {
			if _, err := d.applier.ApplyConfig(candidate); err != nil {
				return err
			}
			return nil
		}
	}
	return &reloader{
		applied:        d.applied,
		processEnabled: d.applied.Reload.Enabled,
		cfgPath:        d.cfgPath,
		targets:        targets,
		logger:         logger,
	}
}

// bindDebounce 补上 reload.debounce 的落点（#2）。watcher 为 nil 时什么都不做：
// 那条路径上 setDebounce 保持 nil，改了 reload.debounce 会进 ignored_keys 而不是失败。
func (r *reloader) bindDebounce(watcher configWatcherAPI) {
	if watcher == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.targets.setDebounce = func(d time.Duration) error {
		watcher.SetDebounce(d)
		return nil
	}
}

// Stop 等在途那一串走完，并让之后的调用直接返回（不再读配置、不再动任何落点）。
//
// 为什么需要它而不是只关 watcher：R05 的 Close 明确不等在途重载（那一次调用可能正握着
// 写链的锁，等它只会把关停拖死），所以 Close 返回之后仍可能有一次 ApplyConfig 在改处理函数表，
// 而那正是设计文档 §7.8 要避免的交错。
//
// 形状：先 r.mu.Lock() —— 它就是整条链的串行锁，拿到它等于在途链已经交还；
// 然后置 stopped 再解锁。此后就算有人直接调 Reload 也只是立刻返回一个空结论，
// 不会有人新起一条链（触发源已被 watcher.Close() 断掉）。
func (r *reloader) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped = true
}

// configFileCarriesNoKeys 判断这份配置文件是否已经不表达任何取值：整份文件除了空白、
// 注释与 YAML 的文档分隔符以外什么都不剩。
//
// 它只回答"空不空"这一件事，不判定内容对不对——那是 core.LoadConfig 第 1 步的活。
// 读不出文件（被删、没权限）时把错误原样交回，调用方会走第 1 步那条统一口径，
// 不在这里另报一份"文件读不到"。
//
// 剩下的口子只有一个，说清：这一判据与 core.LoadConfig 是**同一次存盘的两次读**，
// 两者之间如果恰好被一次截断写入插进去，这次会按截断前的样子判过、随后把截断后的内容读成
// 一份全默认配置并应用到现网（结论仍是 ok）。窗口是微秒级、要靠下一次存盘才可能撞上，
// 而撞上的那一次没法在这里补判——除非让 LoadConfig 接受内容入参，那是另一张卡的事。
func configFileCarriesNoKeys(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(configFileText(data), "\n") {
		content := strings.TrimSpace(line)
		if content == "" || strings.HasPrefix(content, "#") {
			continue
		}
		if rest, ok := cutDocumentMarker(content); ok {
			content = strings.TrimSpace(rest)
			if content == "" || strings.HasPrefix(content, "#") {
				continue
			}
		}
		return false, nil
	}
	return true, nil
}

// cutDocumentMarker 剥掉一行开头的 YAML 文档标记（`---` / `...`），标记必须独占这一列或后面
// 紧跟空白才算数。
//
// 为什么要单独抽出来：`--- # 还没填` 是一行分隔符，而 `--- {logging: {level: debug}}` 是一份
// 写在流式映射里的真配置。只按前缀跳过分隔符会把后者也当成空行，那种文件是真有取值的。
func cutDocumentMarker(content string) (string, bool) {
	for _, marker := range [...]string{"---", "..."} {
		if !strings.HasPrefix(content, marker) {
			continue
		}
		rest := content[len(marker):]
		if rest == "" || rest[0] == ' ' || rest[0] == '\t' {
			return rest, true
		}
	}
	return "", false
}

// configFileText 把配置文件原文换成可以逐行扫的文本。
//
// 为什么需要这一步：YAML 解析器容忍开头的 BOM，也认**带 BOM 的** UTF-16LE/BE（不带 BOM 的
// UTF-16 它按 UTF-8 处理、直接报解析错，那一侧由第 1 步兜住，不在这里处理），而 Windows
// PowerShell 5.1 的 `>` 与 Out-File 默认写的就是带 BOM 的 UTF-16LE。不先还原成文本，
// "这份文件还表达取值吗"会对这一类形状答错（`BOM + 只有注释` 被当成"有内容"，于是空文件照样
// 走完这条链换成默认取值），而这条判据要防的正是那种"看着像编辑器误操作"的现场。
func configFileText(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte{0xEF, 0xBB, 0xBF}):
		return string(data[3:])
	case bytes.HasPrefix(data, []byte{0xFF, 0xFE}), bytes.HasPrefix(data, []byte{0xFE, 0xFF}):
		little := data[0] == 0xFF
		words := make([]uint16, 0, len(data)/2)
		for i := 2; i+1 < len(data); i += 2 {
			if little {
				words = append(words, binary.LittleEndian.Uint16(data[i:]))
			} else {
				words = append(words, binary.BigEndian.Uint16(data[i:]))
			}
		}
		return string(utf16.Decode(words))
	default:
		return string(data)
	}
}

// Reload 走完整条链，它就是交给 core.ConfigWatcher 的那个 ReloadFunc。
//
// 八步，顺序固定（前七步是卡 §3.1 那张表，第 0 步是 R07 场景 16E 补的那道问）：
//  0. configFileCarriesNoKeys：整份文件只剩空白、注释与文档分隔符 → 结论 rejected，applied 不动。
//     空文件在第 1 步那里是一份**合法的**"全部取默认值"的配置而不是读失败，不问这一句，
//     "绝不退回默认值"就会从第 1 步的失败分支漏到成功分支上去（D-R0702）。
//  1. core.LoadConfig(cfgPath)：失败（语法错、未知键、Validate 不过、文件被删）→ 结论 rejected，
//     applied 不动。**绝不退回默认值**：那会让整台机器静默变成默认配置在跑。
//  2. candidate.Normalized()：Diff 要求两侧都已归一化（D-R0102），否则 0 值与默认值的差别
//     会被当成一次改动。
//  3. core.Diff(applied, candidate)。
//  4. 有拒绝项 → 结论 rejected，applied 不动。这一步早于任何写入，所以拒绝档的代价是
//     "什么都没发生"，不是"改了一半"。
//  5. 无变化 → 结论 unchanged，applied 不动（内容相同的存盘不该重登记档位）。
//  6. 逐项应用热更档（§3.2 的顺序），每项成功后把"用 applied 重放同一段代码"压栈；
//     任一项失败 → 逆序执行回滚，结论 failed（回滚自己又失败则是 degraded）。
//  7. 全部成功 → applied = candidate，结论 ok，重启档的键名抄进 IgnoredKeys。
//
// 结论的保存与并发可读由 R05 的 ConfigWatcher 负责（Reload 把结论作为返回值交回它），
// 所以 reloader 不存状态、也没有 State 方法：api 的读口注入的是 watcher。
// 唯一要在这里说的是 LastAppliedAt 由本方法填（只有它知道"这次真的应用了"）。
func (r *reloader) Reload() (core.ReloadState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.stopped {
		// 关停收口之后：不读配置、不动落点，交回一份全空的结论（watcher 那侧也不再存新状态了）。
		return core.ReloadState{}, nil
	}

	// Enabled 取建链时那一份而不是 applied 里的当前值：reload.enabled 是重启档，
	// 改了它进程照旧在跑监听器，读数说的必须是"这台进程是按哪一份开关起来的"
	// （/admin/runtime 的 enabled 另有出处，见 api/reload_state.go 与 D-R0503）。
	state := core.ReloadState{
		Enabled:       r.processEnabled,
		LastAttemptAt: time.Now(),
	}

	if r.cfgPath == "" {
		// 没有可盯的文件就不该有人调这条链；真走到了要说清，不能报"无事发生"。
		msg := "配置热重载没有可用的配置文件路径（进程启动时没读到任何文件），本次重载没有读取任何配置"
		state.Result = core.ReloadRejected
		state.Error = msg
		r.logger.Error("config reload rejected: no config file path is known", "hint",
			"用 -config 指一个真实存在的文件，或放置 configs/config.yaml 后重启")
		return state, errors.New(msg)
	}

	// 第 1 步之前先问一句：这份文件还表达任何取值吗。空文件在第 1 步那里是一份**合法的**
	// "全部取默认值"的配置而不是读失败，不问这一句，"绝不退回默认值"就从失败分支漏到了成功分支。
	// 现场量在 TASK-R07 场景 16E：无凭据部署写空文件之后结论是 ok，workers 从 7 变成代码默认的 100。
	if noKeys, err := configFileCarriesNoKeys(r.cfgPath); err == nil && noKeys {
		msg := fmt.Sprintf("配置文件里一个键都没有（空文件或只有注释），本次重载作废、现网继续按当前生效的取值运行（%s）：空文件会被读成一份全部取默认值的配置，要把取值退回默认请显式写出每一项并重启进程",
			r.cfgPath)
		state.Result = core.ReloadRejected
		state.Error = msg
		r.logger.Error("config reload rejected: the config file carries no keys",
			"path", r.cfgPath,
			"hint", "空文件不等于默认配置；这份文件恢复成有内容的样子即可自动恢复监听")
		return state, errors.New(msg)
	}

	// 第 1 步：整份读通。
	candidate, err := core.LoadConfig(r.cfgPath)
	if err != nil {
		// 坏文件、未知键、Validate 不过、文件被删都是这一条。绝不退回默认值（I1 的另一半）。
		msg := fmt.Sprintf("重新读取配置文件失败，本次重载作废、现网继续按当前生效的取值运行（%s）：%v",
			r.cfgPath, err)
		state.Result = core.ReloadRejected
		state.Error = msg
		r.logger.Error("config reload rejected: the config file cannot be read",
			"path", r.cfgPath, "error", err,
			"hint", "修好这个文件即可自动恢复，不必重启")
		return state, errors.New(msg)
	}

	// 第 2 步：归一化。applied 这一侧由装配方保证也已经是归一化的（run() 传进来的就是
	// cfg := deps.config.Normalized()），而每次成功后换进来的 candidate 也是这一行处理过的。
	candidate = candidate.Normalized()

	// 第 3 步：比对。
	change := core.Diff(r.applied, candidate)

	// 第 4 步：拒绝档早于任何写入。
	if change.HasRejections() {
		state.Result = core.ReloadRejected
		state.RejectedKeys = changedPaths(change.Reject)
		msg := fmt.Sprintf("改动触碰了拒绝档（凭据与执行许可字段），整次作废、一项都没有应用：%s；"+
			"这类改动只能改完文件再重启进程", strings.Join(state.RejectedKeys, ", "))
		state.Error = msg
		r.logger.Error("config reload rejected: it touches credentials or execution permissions",
			"rejected_keys", strings.Join(state.RejectedKeys, ","),
			"applied_keys_ignored", len(change.Hot),
			"hint", "拒绝档不会有任何一项被应用，旧取值原样生效")
		return state, errors.New(msg)
	}

	// 第 5 步：内容没变。文件被 touch 但取值等价不算一次重载。
	if !change.HasChanges() {
		state.Result = core.ReloadUnchanged
		// 这条文案必须说清"环境变量压住的键看起来也是无变化"（设计文档 §5.3、待拍板 P4）：
		// 区分"文件没变"与"变了但被环境变量压住"要的是原始来源，本期不新增那份判定。
		r.logger.Info("config reload found no change",
			"path", r.cfgPath,
			"note", "取值以配置文件与环境变量的合并结果为准：被 GODELAYQ_* 压住的键在这里也显示为无变化")
		return state, nil
	}

	// 第 6 步：按 §3.2 的顺序逐项应用，失败逆序回滚。
	outcome, failure, rollbackFailures := r.applyChange(change, candidate)
	state.AppliedKeys = outcome.appliedKeys
	state.IgnoredKeys = outcome.ignoredKeys

	if failure != nil {
		state.Error = failure.Error()
		if len(rollbackFailures) > 0 {
			// I2 唯一允许"内存里半新半旧"的状态，而它必须被读出来，不能只留一行日志。
			state.Result = core.ReloadDegraded
			state.Error = fmt.Sprintf("%v；回滚也有失败项：%s", failure, strings.Join(rollbackFailures, "；"))
			r.logger.Error("config reload degraded: rollback itself failed",
				"error", state.Error,
				"failed_steps", strings.Join(rollbackFailures, ","),
				"hint", "进程里可能同时存在新旧两套取值，请核对后重启")
		} else {
			state.Result = core.ReloadFailed
			r.logger.Error("config reload failed and was rolled back",
				"error", state.Error,
				"rolled_back", strings.Join(outcome.appliedKeys, ","))
		}
		// 两种结论都不推进 applied：那一份权威仍然是上一次成功的那套值（I1）。
		// AppliedKeys 也清空：那一串已经被回滚掉了，留着它会把"回滚过"读成"生效了"。
		state.AppliedKeys = nil
		return state, errors.New(state.Error)
	}

	// 第 7 步：全部成功才换权威。
	r.applied = candidate
	state.Result = core.ReloadOK
	if len(state.AppliedKeys) > 0 {
		// 只有它知道"这次真的应用了"，所以这个时间戳由本方法填（卡 §3.1 末尾那句）。
		state.LastAppliedAt = time.Now()
	}
	r.logger.Info("config reload applied",
		"applied_keys", strings.Join(state.AppliedKeys, ","),
		"ignored_keys", strings.Join(state.IgnoredKeys, ","),
		"restart_keys", len(change.Restart))
	// reload.enabled 属重启档，但它比一般重启档多一个误解要破：把文件里的它改成 false
	// 不会让正在跑的监听器停下来。不写这一句，运维会以为已经关掉热重载了（卡 §3.1 第二条）。
	if containsKeyPath(state.IgnoredKeys, "reload.enabled") {
		r.logger.Warn("reload.enabled is a restart-class key: the running watcher does not stop because of it",
			"path", r.cfgPath,
			"hint", "要真的停掉热重载，把它改回 true 之外还得重启进程；"+
				"重启档一律进 ignored_keys，本系列不提供运行期关监听的口子")
	}
	return state, nil
}

// applyOutcome 是第 6 步的返回形状。
type applyOutcome struct {
	appliedKeys []string
	ignoredKeys []string
}

// applyChange 按 §3.2 的顺序应用热更档，并在任一步失败时逆序回滚。
//
// 返回的三个值分别是：键清单（applied / ignored）、链失败的错误、回滚失败的逐条文本。
// 后两个互斥到这种程度：failure 为 nil 时 rollbackFailures 必为 nil。
func (r *reloader) applyChange(change core.ConfigChange, candidate core.Config) (applyOutcome, error, []string) {
	hot := changedPaths(change.Hot)
	plan, unmatched := r.buildPlan(hot)

	// 前置检查（应用之前，一条都不放过）：有一条热更键没有分派分支就是装配错误。
	// 这条检查是"归档成热更、但没人应用"的保险丝，也是本卡对设计文档 §8 那条
	// TestHotKeysAreEffective 的落地形状——它宁可让整次重载失败，也不悄悄跳过一条键。
	if len(unmatched) > 0 {
		return applyOutcome{ignoredKeys: mergeKeys(changedPaths(change.Restart))},
			fmt.Errorf("热更键 %s 在重载链里没有分派分支（分档表与链失去同步），整次作废",
				strings.Join(unmatched, ", ")), nil
	}
	// 前置检查第二条：无条件落点缺席（装配少接了一样东西）时同样不动手。
	// 它与"分派表漏一行"是同一种脸色——编程错误宁可让整次作废，也不要先把日志级别换掉、
	// 再在第四步失败、然后靠回滚把现场擦回去（回滚本身也可能失败，那会把 degraded 摊出来）。
	// 有条件的缺席（观测层未启用、watcher 未建、执行器未启用）不走这里：它们各有明确结论。
	for _, step := range plan {
		if !step.available && step.unavailable == unavailableFails {
			return applyOutcome{ignoredKeys: mergeKeys(changedPaths(change.Restart))},
				fmt.Errorf("热更键 %s 的落点在这次的装配里不存在（缺依赖），整次作废、一项都没应用",
					strings.Join(step.matched, ", ")), nil
		}
	}

	var (
		outcome  applyOutcome
		undo     []func() error
		failure  error
		ignored  = map[string]bool{}
		position int
	)
	for _, step := range plan {
		position++
		if !step.available {
			// 走到这里只可能是 Ignored 或 NotInEffect 两种脸色：unavailableFails 那种
			// "装配缺入口"已经被上面的前置检查第二条整次拦在应用之前，这里不再重复一遍判据
			// （重复的那一份永远跑不到，留着只会让人以为运行期还有第二条退路）。
			// 维护提醒：给 unavailableOutcome 加第四种取值时必须在这里补一支——
			// 少了那一支不会编译失败，只会让这一步的键悄悄留在 applied_keys 里。
			switch step.unavailable {
			case unavailableIgnored:
				for _, key := range step.matched {
					ignored[key] = true
				}
				r.logger.Warn("config reload skipped a disabled section",
					"step", step.label,
					"keys", strings.Join(step.matched, ","),
					"hint", "那一节的总开关没打开，改它的取值本来就不生效；打开它需要改配置并重启")
			case unavailableNotInEffect:
				// 卡 §3.1 第三条：档位改动已记录，但执行器未启用 ⇒ 本次不生效。
				// 结论仍是 ok、键仍留在 AppliedKeys，另记一条 warn 说明没生效的原因——
				// 理由与 executors.enabled=false 时"本节取值全部不生效"的既有口径一致，
				// 而不是让一次无意的档位改动把整次重载判成失败。
				r.logger.Warn("executors are disabled: executors.commands was recorded but is not in effect",
					"keys", strings.Join(step.matched, ","),
					"hint", "打开 executors.enabled 需要改配置并重启，热重载不会替你打开执行器")
			}
			continue
		}

		// 旧值就是 applied 里那一份，不需要额外的读取口（卡 §3.2 末段）。
		previous := r.applied
		apply := step.run
		if err := apply(candidate); err != nil {
			failure = fmt.Errorf("第 %d 步 %s 应用失败：%w", position, step.label, err)
			break
		}
		undo = append(undo, func() error { return apply(previous) })
	}

	if failure == nil {
		outcome.appliedKeys = exceptKeys(hot, ignored)
		outcome.ignoredKeys = mergeKeys(changedPaths(change.Restart), mapKeys(ignored))
		return outcome, nil, nil
	}

	// 逆序回滚。undo 栈里只放成功过的步，所以失败的那一步本身不回滚：
	// 每个落点都必须是"要么整批换、要么一动不动"的（executor.Applier.ApplyConfig 与
	// Registry.ApplyConfig 按这个契约交付），链不替它们补一半。
	var rollbackFailures []string
	for i := len(undo) - 1; i >= 0; i-- {
		if err := undo[i](); err != nil {
			rollbackFailures = append(rollbackFailures, err.Error())
		}
	}
	outcome.ignoredKeys = mergeKeys(changedPaths(change.Restart), mapKeys(ignored))
	return outcome, failure, rollbackFailures
}

// buildPlan 按 §3.2 那张表挑出这次要走的步，并把热更键分派给它们。
// 返回的第二份是没有任何步骤认领的键（分派表漏项的判据）。
//
// 表的顺序即依赖顺序，逐条理由在 stepTable 的注释里；这里只按行挑，不重排。
func (r *reloader) buildPlan(hot []string) ([]reloadStep, []string) {
	t := r.targets
	steps := []reloadStep{
		// #1 logging.level —— 最先：后面每一步都要用它记日志，级别改了应当立刻生效于本次重载自己的日志。
		{label: keyLoggingLevel, keys: []string{keyLoggingLevel}, available: t.setLevel != nil,
			unavailable: unavailableFails,
			run:         func(cfg core.Config) error { return t.setLevel(cfg.Logging.Level) }},
		// #2 reload.debounce —— 早于任何可能失败的动作，让"窗口写小了"立刻在下一次事件起作用。
		// 它的 undo 就是把窗口写回 applied 里那份旧值。落点是 nil 只可能因为 watcher 没建起来。
		{label: keyReloadDebounce, keys: []string{keyReloadDebounce}, available: t.setDebounce != nil,
			unavailable: unavailableIgnored,
			run:         func(cfg core.Config) error { return t.setDebounce(cfg.Reload.Debounce) }},
		// #3 scheduler.max_retry_delay —— 只换策略实例，无副作用，放前面降低回滚复杂度。
		{label: keySchedulerMaxRetryDelay, keys: []string{keySchedulerMaxRetryDelay}, available: t.setRetry != nil,
			unavailable: unavailableFails,
			run:         func(cfg core.Config) error { return t.setRetry(cfg.Scheduler.MaxRetryDelay) }},
		// #4 store.history_limit / history_ttl —— 与 #3 一样是取值替换；下一次写入的 trim 才用到。
		// 两个键算一步：R02 定的口径是一次给齐两个值，拆开调会把"新条数配旧时长"的窗口摊给现网（D-R0201）。
		{label: "store 留痕保留策略", keys: []string{keyStoreHistoryLimit, keyStoreHistoryTTL},
			available: t.setRetention != nil, unavailable: unavailableFails,
			run: func(cfg core.Config) error {
				return t.setRetention(cfg.Store.HistoryLimit, cfg.Store.HistoryTTL)
			}},
		// #5 observability.*.retention_* —— 写入器未启用时对应函数为 nil：那两条键改动跳过并记入
		// IgnoredKeys 而不是失败（总开关关着时本节的取值本来不生效）。
		{label: "observability.events 保留策略",
			keys:      []string{keyObservabilityEventCount, keyObservabilityEventAge},
			available: t.setEventRetention != nil, unavailable: unavailableIgnored,
			run: func(cfg core.Config) error {
				return t.setEventRetention(cfg.Observability.Events.RetentionCount, cfg.Observability.Events.RetentionAge)
			}},
		{label: "observability.audit 保留策略",
			keys:      []string{keyObservabilityAuditCount, keyObservabilityAuditAge},
			available: t.setAuditRetention != nil, unavailable: unavailableIgnored,
			run: func(cfg core.Config) error {
				return t.setAuditRetention(cfg.Observability.Audit.RetentionCount, cfg.Observability.Audit.RetentionAge)
			}},
		// #6 scheduler.workers —— 唯一会起协程的一步，放在纯取值替换之后；失败时前面几步的 undo 都已就位。
		// 上界校验就在这一步：ResizeWorkers 只有 n<=0 的下限（R03 §10.5 D3 交接给本卡的那条），
		// 而 scheduler.workers: 100000 照样过 core 的 Validate —— 不拦就等于让一次文件保存起十万条协程。
		{label: keySchedulerWorkers, keys: []string{keySchedulerWorkers}, available: t.setWorkers != nil,
			unavailable: unavailableFails,
			run: func(cfg core.Config) error {
				if err := checkWorkerBound(cfg.Scheduler.Workers); err != nil {
					return err
				}
				return t.setWorkers(cfg.Scheduler.Workers)
			}},
		// #7 executors.commands —— 最后：它是唯一会改调度器处理函数表的一步，R04 保证它失败时
		// 不动任何东西，放在末尾让"表换了但别的东西没换"的窗口最短。
		// 它的 undo 是"用旧的 applied 再调一次 ApplyConfig"——R04 那条链可重入且幂等，逆序重放安全。
		// 档位摊成 executors.commands.<name>.<field>，所以按前缀认领；整批只调一次。
		{label: keyExecutorsCommands, prefix: keyExecutorsCommands, available: t.setCommands != nil,
			unavailable: unavailableNotInEffect,
			run:         func(cfg core.Config) error { return t.setCommands(cfg) }},
	}

	claimed := make(map[string]bool, len(hot))
	var plan []reloadStep
	for i := range steps {
		step := &steps[i]
		for _, path := range hot {
			if step.claims(path) {
				step.matched = append(step.matched, path)
				claimed[path] = true
			}
		}
		if len(step.matched) > 0 {
			plan = append(plan, *step)
		}
	}

	var unmatched []string
	for _, path := range hot {
		if !claimed[path] {
			unmatched = append(unmatched, path)
		}
	}
	return plan, unmatched
}

// configWatcherAPI 是装配方对监听器的要求：跑事件循环、关停、改防抖窗口、读最近结论、
// 记监听器自身的故障。真实实现就是 *core.ConfigWatcher。
//
// 定义成接口而不是直接用具体类型，是为了 §5.2 那三条装配用例：它们要能把"关掉监听器"
// 这一步记进调用流水，而不必真的等一次文件系统事件（那既慢又不可靠）。
type configWatcherAPI interface {
	Run(ctx context.Context)
	Close() error
	SetDebounce(d time.Duration)
	State() core.ReloadState
	MarkWatcherError(msg string)
}

// reloadStatusReader 是 /admin/runtime 的读口。它做两件 reloader 与 watcher 都不该做的事：
//
//  1. 把"进程配置里 reload.enabled 是什么"与"监听器建没建起来"分开。开关的读数只能来自
//     进程配置（R05 交给本卡的 D-R0503），所以它由装配方在注入时显式给，见 api.WithReloadState；
//     这个类型只负责交回 watcher 那一份状态，或在根本没有 watcher 时给出一个明确的原因。
//  2. 让注入点与建 watcher 的点不必对齐：读口在服务构造时就交给 api，watcher 稍后建好再
//     attach 进来（run() 里 watcher 是在 server.Start 之后建的，因为那条链不该在服务还没
//     监听时就开始改现网）。
type reloadStatusReader struct {
	mu      sync.Mutex
	watcher configWatcherAPI
	// path 与 note 只在没有 watcher 时有意义：前者是要盯的那个文件，后者是没建起来的结论。
	path string
	note string
}

// attach 把建好的 watcher 接进读口。
func (s *reloadStatusReader) attach(watcher configWatcherAPI) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watcher = watcher
	s.note = ""
}

// explain 记录"这次部署没有监听器"这个结论（I3：没生效要说出口）。
func (s *reloadStatusReader) explain(path, note string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watcher = nil
	s.path = path
	s.note = note
}

// State 满足 api.ReloadStateReader。
//
// 交回的三份键清单与 watcher 内部那份共享底层数组（R05 的 D-R0514 说的是同一件事），
// 所以这里绝不排序、绝不改写；要整理就自己复制一份（api 那侧的转换就是这么做的）。
func (s *reloadStatusReader) State() core.ReloadState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.watcher != nil {
		return s.watcher.State()
	}
	return core.ReloadState{WatchedPath: s.path, WatcherError: s.note}
}

// changedPaths 把 Diff 的一份清单折成键路径列表（取值一律不带出去，见 core.ChangedKey 的注释）。
func changedPaths(keys []core.ChangedKey) []string {
	paths := make([]string, 0, len(keys))
	for _, key := range keys {
		paths = append(paths, key.Path)
	}
	return paths
}

// containsKeyPath 判断一份键清单里有没有某条精确路径。
func containsKeyPath(keys []string, want string) bool {
	for _, key := range keys {
		if key == want {
			return true
		}
	}
	return false
}

// mapKeys 把一个集合摊成列表并按字典序排好，供与重启档合并。
func mapKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// mergeKeys 合并两份已各自有序的清单，去重后按字典序返回。
// 两份都可能为空，结果可能为 nil —— nil 才是"这个键清单没有内容"的序列化形状（omitempty）。
func mergeKeys(groups ...[]string) []string {
	seen := map[string]bool{}
	var merged []string
	for _, group := range groups {
		for _, key := range group {
			if seen[key] {
				continue
			}
			seen[key] = true
			merged = append(merged, key)
		}
	}
	if len(merged) == 0 {
		return nil
	}
	sort.Strings(merged)
	return merged
}

// exceptKeys 返回 hot 里没有落下去的那部分（保序：Diff 已经按路径字典序排好）。
func exceptKeys(hot []string, excluded map[string]bool) []string {
	var kept []string
	for _, key := range hot {
		if excluded[key] {
			continue
		}
		kept = append(kept, key)
	}
	return kept
}
