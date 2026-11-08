package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jiujuan/godelayq/internal/api"
	"github.com/jiujuan/godelayq/internal/core"
	"github.com/jiujuan/godelayq/internal/executor"
	"github.com/jiujuan/godelayq/internal/store/jobs"
	"github.com/jiujuan/godelayq/internal/store/mgr"
	"github.com/jiujuan/godelayq/internal/store/observe"
	"github.com/jiujuan/godelayq/internal/web"
)

type schedulerAPI interface {
	Start()
	Stop()
	RegisterHandler(jobType string, handler core.Handler)
	// RegisterHandlerClass 是执行器档位的注册入口：档位任务属于 JobClassExec，
	// 与普通任务分池执行（TASK-E13）。代码中示例处理函数继续走 RegisterHandler。
	RegisterHandlerClass(jobType string, handler core.Handler, class core.JobClass)
	// LookupHandler 是注册档位前的查重入口：执行器写的是调度器里那张注册表，
	// 只靠 RegisterJobHandler 这一个写入口看不到已注册的键。
	LookupHandler(jobType string) (core.Handler, bool)
	// SetConcurrency/SetQueueCapacity 是启动期装配用的（Start 之后调用只记一条 warn 并忽略）。
	SetConcurrency(n int)
	SetQueueCapacity(n int)
	// ResizeWorkers 与 SetRetryPolicy 是配置热重载的两个落点（TASK-R03、R02）：
	// 与上面那对的分别就在这里——它们专给运行期用，改完真的生效。
	// 上界不在调度器里（R03 §10.5 的 D3 只有 n<=0 的下限），由重载链补，见 reload.go。
	ResizeWorkers(n int) error
	SetRetryPolicy(p core.RetryPolicy)
	// SetExecConcurrency/SetExecQueueCapacity 把执行器池的规模交给调度器（TASK-E13）。
	// 执行器没打开时传 0：调度器连队列和协程都不建，进程与拆池之前完全一致。
	SetExecConcurrency(n int)
	SetExecQueueCapacity(n int)
	// SetEventPreviewLimit 把输出预览的字节上限交给调度器：完成/失败事件由 core 发布，
	// 尺寸限制必须在发事件的一方生效，而不能只在接口侧裁剪。
	SetEventPreviewLimit(n int)
	// SetRestoreGuard 安装崩溃恢复的状态改判钩子（TASK-E14），必须在 Start 之前。
	SetRestoreGuard(g core.RestoreGuard)
	// HandlerClass 查注册键的执行类别；第二个返回值为 false 表示这个键没登记过。
	// 守卫靠它判断"崩溃瞬间在跑的是不是执行器任务"。
	HandlerClass(key string) (core.JobClass, bool)
	// GetEventBus 取调度器持有的事件总线，观测层的事件写入器要往上挂第二个订阅者。
	// api.Server 走的是同一个入口（api/server.go 里给内存缓冲挂订阅），本卡不改 core。
	GetEventBus() *core.EventBus
	// UnregisterHandler 与 HandlerNames 是档位在线管理的两个入口（TASK-W04、W06）：
	// 同步器要能枚举现有键才知道哪几个该摘掉，并调用成对删除把档位摘干净。
	// PauseByHandlerKey 不在这里：删除档位时是 api.Server 直接调调度器，装配方不中转。
	UnregisterHandler(jobType string) bool
	HandlerNames() []string
}

type serverAPI interface {
	RegisterJobHandler(name string, handler core.Handler)
	Start() error
	Stop(ctx context.Context) error
}

// observabilityDB 是 run 对观测库句子的要求：报路径与实际生效的 PRAGMA、报迁移版本、干净关闭。
//
// 接口定义在消费方是本仓库的既有惯例（schedulerAPI、serverAPI 同样如此），这里还多一条理由：
// 关闭顺序（观测层早于 store.Close）只有在句柄可替换时才断言得出来，
// 真实 *observe.Handle 的关闭时机在测试里没有别的观察办法。
// 本卡只用到这三个方法，S03 起要把事件写入器接进装配时再扩，不提前摆空参数。
type observabilityDB interface {
	Path() string
	JournalMode() string
	Stats() (observe.Stats, error)
	Close() error
}

// eventLogAPI 是 run 对事件写入器的要求，两面都用得上：
//   - 关停面（Close / Dropped）：撤销订阅、落完最后一批、把丢弃数记进关停日志；
//   - 读取面（Events / Recent）：原样交给 api.WithEventLog，让两个事件端点改读库。
//
// 与 observabilityDB 同样定义在消费方，理由一致：关闭顺序（写入器先撤订阅并落完最后一批，
// 连接后关）只有在句柄可替换时才断言得出来。这里不列 Flush：落盘由写入器自己的周期负责，
// 关停时它自己会把剩余批次写完，run 里没有单独 Flush 的时机。
// api 侧只声明读取能力（api.server.go 的 eventReader），所以这里带上读方法不会让 api 碰驱动。
type eventLogAPI interface {
	Close() error
	Dropped() int64
	Events(jobID string, limit int) ([]core.Event, error)
	Recent(limit int) ([]core.Event, error)
	// SetRetention 是 observability.events.retention_* 两个键的热更落点（TASK-R02、R06）：
	// 下一个批量周期的淘汰用新值，不重启也不换连接。
	SetRetention(count int, age time.Duration)
}

type signalNotifier func(chan<- os.Signal, ...os.Signal)

// auditLogAPI 是 run 对台账写入器的要求，与 eventLogAPI 同一取向：
//   - 关停面（Close / Dropped）：把剩余行落盘、把丢弃数记进关停日志；
//   - 读写两面（Append / Query）：原样交给 api.WithAuditLog。
//
// 这里的 Append 让 run 也持有写面，是因为服务与中间件用的是同一个句柄：
// 台账的写入方就是查询方，分成两个闭包注入只会出现"一边写了另一边读不到"。
type auditLogAPI interface {
	Close() error
	Dropped() int64
	Append(core.AuditEntry) error
	Query(core.AuditFilter) ([]core.AuditEntry, int, error)
	// SetRetention 是 observability.audit.retention_* 两个键的热更落点（TASK-R02、R06），
	// 与事件写入器同形：下一个批量周期的淘汰用新值。
	SetRetention(count int, age time.Duration)
}

// mgrAPI 是 run 对管理库句柄的要求：报路径（启动日志）、干净关闭。
// 与 observabilityDB 同样定义在消费方；接口先收窄到这两个方法，
// mgrstore 带来更多读数需求时再扩，不提前摆空参数。
type mgrAPI interface {
	Path() string
	Close() error
}

// mgrDBHolder 晚绑定管理库句柄：defaultRuntimeDeps 建好壳，run() 打开管理库后写入；
// newServer（分组、账号）与 newExecutorProfileStore（档位）两个闭包在各自被调用时读它。
// 与 reloadStatus 同一手法——闭包在 defaultRuntimeDeps 里建，句柄要等 run() 按配置打开。
// 只在 mgrstore.enabled=true 时非 nil；两个闭包各自先看配置开关再看句柄。
type mgrDBHolder struct {
	handle *mgr.Handle
}

// observabilityAPI 是装配好的观测层写入器集合，作为 newServer 的最后一个参数交给服务。
// 整体为 nil 表示这次部署没装观测层；单个字段为 nil 表示那一个子开关关着。
//
// 合成一个参数而不是每加一个写入器就多一位：S04 用第六位传事件读取方，S06 再加就是第七位，
// 下一个观测层写入器就是第八位，而每一位都要重排 23 处 newServer 字面量。
// 合起来之后参数的语义也从"事件读取方"变成"这次部署装配了哪些观测层记录器"，
// 关闭顺序仍然在 run 自己手里（它拿的是同一批句柄）。
type observabilityAPI struct {
	events eventLogAPI
	audit  auditLogAPI
}

// profileStoreAPI 是档位在线管理的两面：档位文件的读写口，与"把文件重新生效"的同步器。
//
// 合成一个参数而不是两位，理由与 observabilityAPI 相同：位置参数每加一位
// 都要重排一批 newServer 字面量，而这两件东西本来就同生同灭
// （api 侧的 requireExecutorProfiles 也是两个都为非 nil 才放行）。
// 整体为 nil 就是 executors.web_enabled=false：默认状态。
type profileStoreAPI struct {
	store   core.ExecutorProfileStore
	applier *executor.Applier
}

// configApplier 取出档位同步器，profiles 整体为 nil 时交回 nil。
// 写成 nil 接收者的方法而不是在调用点判空：重载链的两个装配分支（建链、传给 api）都要取它，
// 而"没装配档位依赖"是一种正常状态，判空写三遍就会有一遍漏掉。
func (p *profileStoreAPI) configApplier() *executor.Applier {
	if p == nil {
		return nil
	}
	return p.applier
}

// observabilityEnv 是观测层的装配产物（setupObservability 的返回值）：
// db 在总开关打开时必非 nil；三个成员各自对应自己的子开关，关着就是 nil。
// 原来是 run() 里三个散着声明的变量（events/audit/artifactIndex）加一个局部 db，
// 合成一个 struct 之后，"这次部署装了哪些观测层记录器"有了一个可以整体传递的形状，
// run() 的观测层阶段因此收敛成一次调用加一条 defer。
type observabilityEnv struct {
	db            observabilityDB
	events        eventLogAPI
	audit         auditLogAPI
	artifactIndex executor.ArtifactIndexer
}

// close 按固定顺序收掉观测层句柄：先台账、再事件写入器、最后观测库。
// 这个顺序原来是 run() 里三条 defer 的声明位次（后声明先执行）表达的，收拢后在此显式写出：
// 写入器要先撤订阅、落完最后一批，然后才关连接（顺序反过来就是往已关闭的连接里写，
// 设计文档 §7.3）。每个写入器关掉之后都要把丢弃数报出来：库里缺页是读不出来的（设计文档 §7.1）。
// 产物索引没有独立关闭：它随观测库连接一起失效。
// 错误路径与成功路径共用这一个方法——setupObservability 失败时 env 带着已建好的句柄交回，
// run() 里那条 defer obsEnv.close 在两种路径上都走这里。
func (e *observabilityEnv) close(logger *slog.Logger) {
	if e.audit != nil {
		if err := e.audit.Close(); err != nil {
			logger.Error("failed to close observability audit writer", "error", err)
		}
		if dropped := e.audit.Dropped(); dropped > 0 {
			logger.Warn("observability audit writer dropped records",
				"dropped", dropped, "path", e.db.Path())
		}
	}
	if e.events != nil {
		if err := e.events.Close(); err != nil {
			logger.Error("failed to close observability event writer", "error", err)
		}
		if dropped := e.events.Dropped(); dropped > 0 {
			logger.Warn("observability event writer dropped records",
				"dropped", dropped, "path", e.db.Path())
		}
	}
	if e.db != nil {
		if err := e.db.Close(); err != nil {
			logger.Error("failed to close observability database", "error", err)
		}
	}
}

type runtimeDeps struct {
	config core.Config
	// configPath 是启动时真正读到的那个配置文件的绝对路径；空串表示进程在用代码默认值
	// （-config 没给、configs/config.yaml 也不存在）。热重载只盯这一份（设计文档 §5.2）：
	// 启动后凭空创建一个 configs/config.yaml 不该变成配置来源。
	configPath string
	// levelVar 是 logging.level 的热更载体，来自 main() 里的 core.NewLoggerWithLevelVar。
	// 为 nil 时那一步以错误收口（SetLogLevel 自己拒 nil 载体），不会 panic。
	levelVar     *slog.LevelVar
	newStore     func() (core.Store, error)
	newScheduler func(store core.Store, retryPolicy core.RetryPolicy, eventBus *core.EventBus) schedulerAPI
	// newExecutorRegistry 建档位登记表（加载 + 探测）。它必须显式提供：
	// 缺了它执行器会静默不注册，开关打开也看不出问题。
	newExecutorRegistry func(core.Config, *slog.Logger) (*executor.Registry, error)
	// newExecutorProfileStore 打开档位文件（executors.profiles_path）并交回存储实例，
	// 只在 executors.web_enabled=true 时调用。
	//
	// 它与登记表构造分成两个闭包，是因为这两种失败的后果不同：
	// 文件读不出来是"这台机器的档位状态不可信"，必须挡住启动；
	// 档位本身写错则是 config 侧连坐、文件侧单条跳过（executor.MergeStoreProfiles 的注释）。
	// 合成一个闭包的话，这两种脸色只能给同一种。
	//
	// 交回的是**实例**而不是记录：同一个文件在进程里有两份内存视图
	// （启动合并读的那一份、页面写入用的那一份）会让"刚建的档位"与"下一次生效"看到不同的内容。
	newExecutorProfileStore func(core.Config) (core.ExecutorProfileStore, error)
	// newArtifactStore 建输出产物的文件存储与清理协程，只在 executors.enabled=true 时调用。
	// 与登记表一样列入依赖完整性检查：少了它输出会静默无处安放。
	newArtifactStore func(core.Config, *slog.Logger) (*executor.ArtifactStore, error)
	// newObservabilityDB 打开 SQLite 观测库（建目录、建表、应用迁移），只在
	// observability.enabled=true 时调用。它同样列入依赖完整性检查：少装配一个闭包的后果是
	// "三张表永远是空的"，看起来正常、实际没记，必须在启动期就报错。
	newObservabilityDB func(core.Config, *slog.Logger) (observabilityDB, error)
	// newEventLog 给事件总线挂上第二个订阅者并把 job.* 事件写进 job_events，
	// 只在 observability.enabled 与 observability.events.enabled 同时为真时调用。
	// 它同样列入依赖完整性检查（按上一条的组合条件）：少了它事件表永远是空的。
	newEventLog func(bus *core.EventBus, db observabilityDB, cfg core.ObservabilityConfig, logger *slog.Logger) (eventLogAPI, error)
	// newArtifactIndex 给产物存储挂上输出索引，只在
	// observability.enabled && observability.artifacts.enabled && executors.enabled
	// 三者同时成立时调用（关着执行器时根本没有产物可索引，此时建索引等于在库里留一张空表）。
	// 它列入依赖完整性检查的条件与上面这条相同：少了这个闭包的部署会一边写文件一边不记索引，
	// 列表端点于是永远空着，与未启用索引看不出区别。
	newArtifactIndex func(db observabilityDB, cfg core.ObservabilityConfig, rootDir string, logger *slog.Logger) (executor.ArtifactIndexer, error)
	// newAuditLog 把每个写操作登记成 write_audit 的一行，只在
	// observability.enabled && observability.audit.enabled 时调用。
	// 列入依赖完整性检查的条件与事件写入器相同：少了它这张表永远是空的，
	// 而 /admin/audit 看起来"没有符合条件的记录"，与真的没有分不清。
	newAuditLog func(db observabilityDB, cfg core.ObservabilityConfig, logger *slog.Logger) (auditLogAPI, error)
	// newMgrDB 打开 SQLite 管理库（建目录、建表、应用迁移），只在 mgrstore.enabled=true
	// 时调用。句柄经 mgrDBHolder 晚绑定给 newServer（分组、账号）与
	// newExecutorProfileStore（档位）两个闭包——与 reloadStatus 同一手法：
	// 闭包先建、句柄后到。列入依赖完整性检查：少装配它，启用管理库的部署
	// 会在 nil 句柄上建出三张"永远写不进"的存储。
	newMgrDB  func(core.Config, *slog.Logger) (mgrAPI, error)
	newServer func(scheduler schedulerAPI, store core.Store, port string, executors *executor.Registry,
		artifacts *executor.ArtifactStore, obs *observabilityAPI, profiles *profileStoreAPI) (serverAPI, error)
	// 配置热重载（TASK-R06）的三处装配点。全部是函数值或可替换句柄，与上面那些闭包同一种风格：
	// 集成测试因此能塞一个假 watcher 进去驱动整条链，而不必真的等一次文件系统事件。
	//
	// newConfigWatcher 的默认实现就是 core.NewConfigWatcher，交回的是接口而不是具体类型：
	// 关闭顺序那条用例（§5.2 #14）要把"关掉监听器"记进调用流水，用真 watcher 记不出来。
	newConfigWatcher func(path string, debounce time.Duration, onReload core.ReloadFunc,
		logger *slog.Logger) (configWatcherAPI, error)
	// newReloadChain 建那条链。nil 时 run() 用真实实现（reloadDeps → newReloader），
	// 只有需要观察"收口"这一步的测试才替换它——链本身的行为由 cmd/server/reload_test.go 直接测。
	newReloadChain func(d reloadDeps) reloadChain
	// reloadStatus 是 /admin/runtime 的 reload 读数来源。defaultRuntimeDeps 建好它并被
	// newServer 的那个闭包捕获（注入点在服务构造时），run() 稍后把建起来的 watcher 塞进去
	// （watcher 要拿 reloader.Reload 当回调，而那条链要在服务能接请求之后才开始工作）。
	// 整体为 nil 只可能出现在测试手搓的 runtimeDeps 字面量里，run() 会自己补一个。
	reloadStatus  *reloadStatusReader
	notifySignals signalNotifier
	timeout       time.Duration
	logger        *slog.Logger
}

// defaultRuntimeDeps 把配置注入各构造闭包，run() 本身不再关心具体取值来源。
//
// configPath 与 levelVar 是热重载链要用的两样东西，都只能来自 main()：
// 前者是"盯哪个文件"的唯一答案（设计文档 §5.2），后者是 logging.level 的热更载体
// （logger 必须由 NewLoggerWithLevelVar 建出来，否则运行期换级别无从下手）。
func defaultRuntimeDeps(cfg core.Config, configPath string, levelVar *slog.LevelVar, logger *slog.Logger) runtimeDeps {
	// reloadStatus 在这里建而不是在 run() 里：newServer 那个闭包要在服务构造时就把读口交给 api，
	// 而 watcher 得到 server.Start() 之后才建（那条链不该在服务还没监听时就开始改现网）。
	// 两边靠这个晚绑定的句柄对齐，见 runtimeDeps.reloadStatus 的注释。
	reloadStatus := &reloadStatusReader{path: configPath}
	// 管理库句柄的晚绑定壳：run() 打开管理库后写入（见 runtimeDeps.newMgrDB 的注释），
	// newServer 与 newExecutorProfileStore 两个闭包读它。禁用管理库的部署永远不写它。
	mgrDB := &mgrDBHolder{}

	return runtimeDeps{
		config:       cfg,
		configPath:   configPath,
		levelVar:     levelVar,
		reloadStatus: reloadStatus,
		newStore: func() (core.Store, error) {
			switch cfg.Store.Type {
			case "json":
				return core.NewJSONFileStoreWithOptions(cfg.Store.Path, core.StoreOptions{
					Interval:     cfg.Store.FlushInterval,
					HistoryLimit: cfg.Store.HistoryLimit,
					HistoryTTL:   cfg.Store.HistoryTTL,
					Logger:       logger,
				})
			case "sqlite":
				// 直写后端：建不起库或导入失败都拦住启动——快照无处安放与
				// 配置非法是同一量级的故障，不带病上线。
				store, err := jobs.Open(cfg.Store.SQLite, cfg.Store.HistoryLimit, cfg.Store.HistoryTTL,
					cfg.Store.SlowQueryThreshold, cfg.Store.StatementTimeout, logger)
				if err != nil {
					return nil, fmt.Errorf("open jobs sqlite store: %w", err)
				}
				if err := store.ImportOnce(cfg.Store.Path); err != nil {
					_ = store.Close()
					return nil, err
				}
				return store, nil
			default:
				// Validate 已拦过未知取值，这里是装配层的最后防线。
				return nil, fmt.Errorf("unsupported store.type %q", cfg.Store.Type)
			}
		},
		newScheduler: func(store core.Store, retryPolicy core.RetryPolicy, eventBus *core.EventBus) schedulerAPI {
			opts := []core.Option{core.WithLogger(logger)}
			// 账本能力（TASK-RE02）：任务库实现了 AttemptLedger 就注入（同一连接、
			// 与快照同库）；JSON 存储没有这项能力，不注入即降级（调度器构造日志说明）。
			// keepLast 从 store.attempts_keep_last 传入，Normalized 已折过默认值。
			if ledger, ok := store.(core.AttemptLedger); ok {
				opts = append(opts, core.WithAttemptLedger(ledger, cfg.Store.AttemptsKeepLast))
			}
			// misfire 全局默认（TASK-RE06）：策略与宽限从 scheduler 节传入，
			// 任务级 misfire_policy 字段在 core 侧优先。
			opts = append(opts, core.WithMisfirePolicy(cfg.Scheduler.MisfirePolicy, cfg.Scheduler.MisfireGracePeriod))
			// 出箱能力（TASK-RE07）：任务库实现了 EventOutbox 就注入——状态变更
			// 与事件同事务落库，投递由中继器接管；JSON 存储不注入即直发（现状）。
			if ob, ok := store.(core.EventOutbox); ok {
				opts = append(opts, core.WithEventOutbox(ob))
			}
			return core.NewScheduler(store, retryPolicy, eventBus, opts...)
		},
		newExecutorRegistry: executor.NewRegistry,
		newExecutorProfileStore: func(cfg core.Config) (core.ExecutorProfileStore, error) {
			if cfg.MgrStore.Enabled {
				// run() 的装配顺序保证管理库先于本闭包打开，句柄必已就位。
				// 档位以库为准：executors.profiles_path 只剩一次性导入源的语义。
				return mgr.NewExecutorProfileStore(mgrDB.handle), nil
			}
			normalized := cfg.Normalized()
			profiles, err := core.NewJSONFileExecutorProfileStore(normalized.Executors.ProfilesPath)
			if err != nil {
				// 这条失败会拦住整个进程，所以文案必须给出自救路径：拦住它的那份文件
				// 多半是人手改坏或从别的机器拷来的，"删掉它"就是退回只由
				// executors.commands 决定档位的形态。文件路径由存储那一层带出来。
				return nil, fmt.Errorf("load executor profile store failed: %w; "+
					"fix that file or delete it to fall back to profiles declared only in executors.commands", err)
			}
			return profiles, nil
		},
		newArtifactStore: func(cfg core.Config, logger *slog.Logger) (*executor.ArtifactStore, error) {
			return executor.NewArtifactStore(executor.ArtifactOptions{
				Dir:      cfg.Executors.Output.Dir,
				MaxBytes: cfg.Executors.Output.MaxBytes,
				TTL:      cfg.Executors.Output.TTL,
			}, logger)
		},
		newObservabilityDB: func(cfg core.Config, logger *slog.Logger) (observabilityDB, error) {
			db, err := observe.Open(cfg.Observability, logger)
			if err != nil {
				// 这里必须返回 nil 而不是那个类型化的空句柄：
				// 装了 *observe.Handle(nil) 的接口值不等于 nil，调用方的判空会失效。
				return nil, err
			}
			return db, nil
		},
		newMgrDB: func(cfg core.Config, logger *slog.Logger) (mgrAPI, error) {
			handle, err := mgr.Open(cfg.MgrStore, logger)
			if err != nil {
				// 与 newObservabilityDB 同一处置：nil 而不是类型化的空指针。
				return nil, err
			}
			// 晚绑定壳在这里填上：newServer 与 newExecutorProfileStore 读它。
			mgrDB.handle = handle
			return handle, nil
		},
		newEventLog: func(bus *core.EventBus, db observabilityDB, cfg core.ObservabilityConfig, logger *slog.Logger) (eventLogAPI, error) {
			handle, ok := db.(*observe.Handle)
			if !ok {
				// 真实装配里这一步一定成立：句柄就是上面 observe.Open 建出来的那一个。
				// 不成立说明有人把假句柄接到了要真连接的写入器上，直接报错，
				// 而不是让写入器静默缺席、事件表永远为空。
				return nil, fmt.Errorf("event writer needs the real observability handle, got %T", db)
			}
			return observe.NewEventLog(bus, handle, observe.EventLogOptions{
				FlushInterval:  cfg.FlushInterval,
				QueueCapacity:  cfg.QueueCapacity,
				RetentionCount: cfg.Events.RetentionCount,
				RetentionAge:   cfg.Events.RetentionAge,
			}, logger)
		},
		newArtifactIndex: func(db observabilityDB, cfg core.ObservabilityConfig, rootDir string, logger *slog.Logger) (executor.ArtifactIndexer, error) {
			handle, ok := db.(*observe.Handle)
			if !ok {
				// 与 newEventLog 同一处置：索引要真连接，接了假句柄就报错，
				// 而不是让这张表安静地空着。
				return nil, fmt.Errorf("artifact index needs the real observability handle, got %T", db)
			}
			return observe.NewArtifactIndex(handle, observe.ArtifactIndexOptions{
				RootDir: rootDir,
			}, logger)
		},
		newAuditLog: func(db observabilityDB, cfg core.ObservabilityConfig, logger *slog.Logger) (auditLogAPI, error) {
			handle, ok := db.(*observe.Handle)
			if !ok {
				// 与 newEventLog、newArtifactIndex 同一处置：台账要真连接，
				// 接了假句柄就报错，而不是让这张表安静地空着。
				return nil, fmt.Errorf("audit log needs the real observability handle, got %T", db)
			}
			return observe.NewAuditLog(handle, observe.AuditLogOptions{
				FlushInterval:  cfg.FlushInterval,
				QueueCapacity:  cfg.QueueCapacity,
				RetentionCount: cfg.Audit.RetentionCount,
				RetentionAge:   cfg.Audit.RetentionAge,
			}, logger)
		},
		newServer: defaultNewServer(cfg, mgrDB, reloadStatus, logger),
		newConfigWatcher: func(path string, debounce time.Duration, onReload core.ReloadFunc,
			logger *slog.Logger) (configWatcherAPI, error) {
			watcher, err := core.NewConfigWatcher(path, debounce, onReload, logger)
			if err != nil {
				// 必须显式返回 nil 而不是那个类型化的空指针：装了 (*core.ConfigWatcher)(nil) 的
				// 接口值不等于 nil，调用方的判空会失效（对照 newObservabilityDB 的同一条注释）。
				return nil, err
			}
			return watcher, nil
		},
		// newReloadChain 留空：run() 在 nil 时用真实实现（newReloader）。装配方不需要
		// 把这条链换成别的什么，只有测试需要包一层记流水。
		notifySignals: signal.Notify,
		timeout:       cfg.Scheduler.ShutdownTimeout,
		logger:        logger,
	}
}

// defaultNewServer 是 newServer 的默认实现：按装配结果把分组/账号存储、观测层两面、
// 档位两面与重载读口注入 api.Server。
// 原来是 defaultRuntimeDeps 字面量里的一个闭包，体量到了值得具名的程度才搬出来：
// 捕获的四个变量（cfg、mgrDB、reloadStatus、logger）在这里变成显式参数，其余依赖仍由参数传入。
func defaultNewServer(cfg core.Config, mgrDB *mgrDBHolder, reloadStatus *reloadStatusReader,
	logger *slog.Logger) func(scheduler schedulerAPI, store core.Store, port string,
	executors *executor.Registry, artifacts *executor.ArtifactStore, obs *observabilityAPI,
	profiles *profileStoreAPI) (serverAPI, error) {
	return func(scheduler schedulerAPI, store core.Store, port string, executors *executor.Registry,
		artifacts *executor.ArtifactStore, obs *observabilityAPI, profiles *profileStoreAPI) (serverAPI, error) {
		coreScheduler, ok := scheduler.(*core.Scheduler)
		if !ok {
			return nil, fmt.Errorf("default server requires *core.Scheduler, got %T", scheduler)
		}
		security := api.Security{
			Auth:             cfg.Server.Auth,
			AllowOrigins:     cfg.Server.CORS.AllowOrigins,
			AllowCredentials: cfg.Server.CORS.AllowCredentials,
		}
		// 分组注册表：mgrstore.enabled 时以库为准，否则走 JSON 文件。
		// JSON 那条分支读不出来就别起服务：与其让 /groups 每个请求都 500，
		// 不如在启动日志里就把文件权限/格式问题摆到眼前。
		var groups core.GroupStore
		if cfg.MgrStore.Enabled {
			// run() 的装配顺序保证管理库先于服务构造打开，句柄必已就位。
			groups = mgr.NewGroupStore(mgrDB.handle)
		} else {
			var groupErr error
			groups, groupErr = core.NewJSONFileGroupStore(cfg.Store.GroupsPath)
			if groupErr != nil {
				return nil, groupErr
			}
		}
		opts := []api.Option{
			api.WithGroupStore(groups),
			api.WithExecutorRegistry(executors),
			// 执行器关闭时 artifacts 是 nil，等价于不注入：/jobs/:id/result 回 503，
			// 而不是在没有任何产物文件的目录上读出"结果为空"。
			api.WithArtifacts(artifacts),
			// 不带 -tags dashboard 时 web.Dist 恒为 nil，这一行等价于"不提供控制台"。
			// 写成无条件调用而不是两份装配，是为了让单二进制的差异只留在 web 包那一处。
			api.WithConsole(web.Dist),
		}
		// 观测层的两面都按装没装配分岔：总开关或对应子开关关闭时那一面是 nil，
		// 事件端点继续读内存缓冲、审计中间件继续只记日志（未启用时的默认行为必须一字不变）。
		if obs != nil {
			if obs.events != nil {
				opts = append(opts, api.WithEventLog(obs.events))
			}
			if obs.audit != nil {
				// 读写两面是同一个句柄：台账的写入方就是查询方
				opts = append(opts, api.WithAuditLog(obs.audit, obs.audit))
			}
		}
		// 账本读取方（TASK-RE02）：store 是任务库时注入（它同时实现了账本能力），
		// JSON 部署不注入——/jobs/:id/attempts 走 ledger:false 的降级形状。
		// store 参数就是 newStore 闭包的产物，类型断言在这里做一次，业务包不感知后端类型。
		if reader, ok := store.(interface {
			Attempts(jobID string, limit int) ([]core.Attempt, error)
		}); ok {
			opts = append(opts, api.WithAttemptReader(reader))
		}
		// 幂等登记表（TASK-RE03）：任务库实现即注入（持久，重启有效）；
		// JSON 部署退回进程内 map——幂等只在进程生命周期内成立（能力边界，
		// deployment.md 说明）。TTL 来自 store.idempotency_ttl。
		if ids, ok := store.(interface {
			LookupIdempotency(key string) (string, string, bool, error)
			PutIdempotency(key string, jobID string, fingerprint string) error
			PruneExpiredIdempotency(ttl time.Duration) error
		}); ok {
			opts = append(opts, api.WithIdempotencyStore(ids, cfg.Store.IdempotencyTTL))
		} else {
			opts = append(opts, api.WithMemoryIdempotencyStore(cfg.Store.IdempotencyTTL))
		}
		// 任务库机制性读数（G4/Q05）：store 是任务库时注入 db 段读口，JSON 部署不注入
		// ——/admin/runtime 的 db 键整个缺省（与 Reload 同一语义："没这个能力"不是
		// "读数为零"）。断言形状与上面两个注入点同一取向。
		if rt, ok := store.(interface {
			DBRuntime() (core.DBRuntimeStats, error)
		}); ok {
			opts = append(opts, api.WithDBRuntime(rt))
		}
		// 管理库模式把账号存储交给服务：认证器随之切到库模式（改账号即时生效），
		// 账号管理端点（/admin/users）也随之启用——两者共用这一个存储句柄。
		// 关闭时账号走配置快照（遗留模式），管理端点不存在。
		if cfg.MgrStore.Enabled {
			opts = append(opts, api.WithUserStore(mgr.NewUserStore(mgrDB.handle)))
		}
		// 档位的在线管理两面同生同灭：web_enabled=false 时 profiles 整体是 nil，
		// 三个写端点因此走到 503 那条明确分支，而不是"能建但建完不生效"。
		if profiles != nil {
			opts = append(opts,
				api.WithExecutorProfileStore(profiles.store),
				api.WithExecutorProfileApplier(profiles.applier),
			)
		}
		// 重载状态的读口。注入条件与"开关开没开"绑，而不是与"watcher 建没建起来"绑：
		// 后者没建起来的那些部署（没有可盯的文件、构造失败）恰恰要能被人读到结论，
		// 未启用的部署才该整个键都不出现（卡 §5.4 第 1 条）。
		// enabled 那一位给的是 cfg 里的进程配置，不是 State().Enabled（R05 的 D-R0503）。
		if cfg.Reload.Enabled {
			opts = append(opts, api.WithReloadState(reloadStatus, cfg.Reload.Enabled))
		}
		return api.NewServer(coreScheduler, store, port, security, logger, opts...), nil
	}
}

func main() {
	configPath := flag.String("config", "",
		"配置文件路径；留空则尝试 "+core.DefaultConfigPath+"，都不存在时使用默认值")
	flag.Parse()

	cfg, err := core.LoadConfig(*configPath)
	if err != nil {
		// 日志器还没建起来，只能用标准库直接失败退出
		log.Fatalf("load config failed: %v", err)
	}

	// 级别载体随 logger 一起建：没有它，logging.level 的热更就没有可拧的螺丝（R02）。
	// 换级别不换 logger 实例，所以 slog.SetDefault 之后经包级 Default() 的那一路会一起跟上。
	logger, levelVar, err := core.NewLoggerWithLevelVar(cfg.Logging.Level, cfg.Logging.Format, os.Stdout)
	if err != nil {
		log.Fatalf("init logging failed: %v", err)
	}
	// 让第三方库与示例 handler 的包级 slog 调用走同一份配置
	slog.SetDefault(logger)

	if err := run(defaultRuntimeDeps(cfg, resolvedConfigPath(*configPath), levelVar, logger)); err != nil {
		// Go 1.26 起标准库 log 桥接到 slog.Default()：走 log.Fatal 只会留下一条
		// 级别为 INFO 的记录，按 level=error 采集的告警不会触发。
		// 所以先用配置好的日志器按 error 级记一次，退出码由 os.Exit 给出。
		logger.Error("server exited with error", "error", err)
		os.Exit(1)
	}
}

// resolvedConfigPath 定出热重载该盯哪个文件（设计文档 §5.2）。
//
// 规则只有一条：只盯启动时真正读到的那一份。-config 显式给的路径优先；留空时按约定的
// core.DefaultConfigPath 找，找不到就交回空串——那种部署用的是代码默认值，
// 而"启动后凭空创建一个 configs/config.yaml"不该变成新的配置来源（它也不该被盯）。
//
// 留空那一路要试 .yaml 与 .yml 两个拼写，与 core.LoadConfig 的查找口径一致：
// 那里走的是 viper 的 SetConfigName("config")，只试 .yaml 会在部署用的是 config.yml 时
// 交回空串——热重载就此悄悄不接（有一条 warn，但"该接的没接"仍是我们不接受的形状）。
//
// 交回绝对路径：core.NewConfigWatcher 只收绝对路径，而 fsnotify 上报的也是绝对路径，
// 留着相对写法会让"盯哪个文件"取决于进程当时的运行目录。
// 绝对化失败（Getwd 出问题）时交回空串，等同于"没有文件可盯"那条 warn 路径。
func resolvedConfigPath(flagPath string) string {
	if flagPath == "" {
		for _, candidate := range []string{
			core.DefaultConfigPath,
			strings.TrimSuffix(core.DefaultConfigPath, ".yaml") + ".yml",
		} {
			if path, ok := usableConfigFile(candidate); ok {
				return path
			}
		}
		return ""
	}
	path, _ := usableConfigFile(flagPath)
	return path
}

// usableConfigFile 判断这个路径是不是一个可盯的普通文件，是则交回绝对路径。
// 目录与读不出信息的都算"没有这个文件"：前者 fsnotify 会盯上一整棵子树，后者交给 LoadConfig 报错。
func usableConfigFile(candidate string) (string, bool) {
	info, err := os.Stat(candidate)
	if err != nil || info.IsDir() {
		return "", false
	}
	abs, err := filepath.Abs(candidate)
	if err != nil {
		return "", false
	}
	return abs, true
}

// run 是进程的装配流水线：每个阶段一个函数（依赖校验 → 任务存储 → 调度器 → 管理库 →
// 执行器 → 产物存储 → 观测层 → 服务 → 热重载链 → 启动 → 等信号收尾），
// 本函数只负责按顺序接线与挂 defer。关闭顺序的单一事实来源仍然是这里的 defer 声明位次
// （后声明先执行）：清理协程 → 观测层（台账 → 事件写入器 → 观测库）→ 管理库 → 任务存储
// （设计文档 §7.3）——反过来就会在已关闭的连接上继续写入，或在已关闭的任务存储之上
// 再读一次快照。管理库的三张表都是同步落库，无需先行收尾。
func run(deps runtimeDeps) error {
	if err := validateRuntimeDeps(deps); err != nil {
		return err
	}

	cfg := deps.config.Normalized()
	if deps.timeout <= 0 {
		deps.timeout = cfg.Scheduler.ShutdownTimeout
	}
	if deps.logger == nil {
		deps.logger = slog.Default()
	}

	store, err := deps.newStore()
	if err != nil {
		return err
	}
	// 存储按周期合并落盘，退出前必须收尾（早退路径同样覆盖）
	defer func() {
		if err := store.Close(); err != nil {
			deps.logger.Error("failed to close store", "error", err)
		}
	}()

	scheduler := newConfiguredScheduler(deps, cfg, store)

	// 管理库在档位存储与服务构造之前打开：newExecutorProfileStore 与 newServer 两个闭包
	// 都要读它的句柄（mgrDBHolder 晚绑定）。打开或导入失败都拒绝启动——
	// mgrstore.enabled=true 表示部署方已决定以库为准，"半导半没导"的状态比起不来更难解释。
	// Open 成功但后续失败时，句柄会原样交回，同样走这条 defer 收掉。
	mgrdb, err := openMgrDatabase(deps, cfg)
	if mgrdb != nil {
		defer func() {
			if err := mgrdb.Close(); err != nil {
				deps.logger.Error("failed to close mgr database", "error", err)
			}
		}()
	}
	if err != nil {
		return err
	}

	executors, profileStore, err := buildExecutorStack(deps, cfg)
	if err != nil {
		return err
	}

	artifacts, err := openArtifactStore(deps, cfg)
	if err != nil {
		return err
	}

	// 观测层（运行事件、产物索引、写操作审计三张表）只在总开关打开时装配。
	// 失败时 env 会带着已建好的句柄交回（db 必非 nil），关闭统一由这条 defer 负责：
	// 错误路径与成功路径共用同一个收尾，内部顺序是台账 → 事件写入器 → 观测库。
	// 这条 defer 晚于上面 store.Close 与 mgrdb.Close 的声明，因此实际执行顺序是
	// 先关观测层、再关管理库、最后关任务存储（设计文档 §7.3）。
	obsEnv, err := setupObservability(deps, cfg, scheduler, artifacts)
	if obsEnv != nil {
		defer obsEnv.close(deps.logger)
	}
	if err != nil {
		return err
	}

	// 出箱中继器（TASK-RE07）：任务库模式下把出箱事件异步投递给事件总线。
	// 启动位次在观测层写入器挂上之后、scheduler.Start 之前——启动重放的那批
	// 崩溃残留事件要被 EventLog 收进库。JSON 存储没有出箱能力，跳过（直发现状）。
	//
	// 关停顺序：defer 声明晚于 obsEnv.close 那条，因此先执行 relay.Stop 再关
	// 观测层——发布落在已关闭的订阅通道上会 panic，这条位次是硬约束。
	var outboxRelay *jobs.OutboxRelay
	if relayStore, ok := store.(*jobs.Store); ok {
		outboxRelay, err = relayStore.NewOutboxRelay(scheduler.GetEventBus(), jobs.OutboxRelayOptions{
			FlushInterval: cfg.Store.OutboxFlushInterval,
			RetentionAge:  cfg.Store.OutboxRetentionAge,
			MaxPending:    cfg.Store.OutboxMaxPending,
		}, deps.logger)
		if err != nil {
			return err
		}
		outboxRelay.Start()
		defer func() {
			if err := outboxRelay.Stop(); err != nil {
				deps.logger.Error("failed to stop outbox relay", "error", err)
			}
		}()
	}

	// 产物清理协程在观测层挂好之后起：Start 的第一轮扫描是"孤儿清理 + TTL 清理 + 索引对账"，
	// 对账要读索引，起早了那份索引还不在。
	//
	// 这里的 defer 比观测层那条 defer（obsEnv.close）后声明，因此关停顺序是：先停清理协程，
	// 再关观测层（台账 → 事件写入器 → 观测库）、最后关任务存储。反过来就会让清理协程
	// 在索引连接已关之后去删它该删的行。
	if artifacts != nil {
		cleanupCtx, stopCleanup := context.WithCancel(context.Background())
		cleanerStopped := artifacts.Start(cleanupCtx, liveJobIDs(store))
		// 先停清理协程再关存储（后声明的先执行）：否则协程可能在存储已关闭后再去读一次任务集合。
		defer func() {
			stopCleanup()
			<-cleanerStopped
		}()
	}

	// 观测层的两个写入器合成一个参数交给服务：都没装配时传 nil，
	// 服务里两条注入都不发生（未启用观测层的部署行为与本卡之前一致）。
	var obs *observabilityAPI
	if obsEnv != nil && (obsEnv.events != nil || obsEnv.audit != nil) {
		obs = &observabilityAPI{events: obsEnv.events, audit: obsEnv.audit}
	}

	profiles, err := buildProfileStoreAPI(deps, cfg, profileStore, scheduler, executors, artifacts)
	if err != nil {
		return err
	}

	server, err := deps.newServer(scheduler, store, cfg.Server.Port, executors, artifacts, obs, profiles)
	if err != nil {
		return err
	}
	if err := registerHandlers(server, scheduler, executors, cfg, artifacts, deps.logger); err != nil {
		return err
	}

	installRestoreGuard(scheduler, cfg, deps.logger)

	// 配置热重载的链（TASK-R06）。整个 if 块在 reload.enabled=false（默认）时一个字节都不执行：
	// 不建链、不建 goroutine，进程行为与本系列之前一致。
	//
	// 建在这里而不是建在 watcher 旁边：它要引用 store、调度器、两个观测层写入器、档位同步器与
	// 级别载体，这几样到这里才齐；而它被调到的最早时机是 server.Start() 之后（下面那段）。
	var chain reloadChain
	if cfg.Reload.Enabled {
		if deps.reloadStatus == nil {
			// 只有测试手搓的 runtimeDeps 字面量会走到这里：attach/explain 得有地方写结论。
			// 回填必须留在 run：buildReloadChain 按值收 deps，字段回写带不出来。
			deps.reloadStatus = &reloadStatusReader{path: deps.configPath}
		}
		chain = buildReloadChain(deps, cfg, store, scheduler, obsEnv, profiles)
	}

	scheduler.Start()

	if err := server.Start(); err != nil {
		scheduler.Stop()
		return err
	}

	// 监听器在服务开始监听之后才起、才跑循环；时序要求与三种"起不来"的降级取舍
	// 见 startConfigWatcher。
	watcher := startConfigWatcher(deps, cfg, chain)

	return awaitSignalAndShutdown(deps, watcher, chain, server, scheduler)
}

// validateRuntimeDeps 把"这次部署用得到、却没装配"的依赖拦在启动期。
// 每一条都是"闭包在不在"与"这次部署用不用得到"的合取：开关关着时少配闭包是预期行为，
// 开关打开却少了闭包的后果写在各自的注释里——共同点是"看起来正常、实际没记"，
// 必须停在启动期，而不是让部署带着哑掉的存储或观测层上线。
func validateRuntimeDeps(deps runtimeDeps) error {
	if deps.newStore == nil || deps.newScheduler == nil || deps.newExecutorRegistry == nil ||
		deps.newArtifactStore == nil || deps.newObservabilityDB == nil ||
		deps.newServer == nil ||
		deps.notifySignals == nil {
		return fmt.Errorf("runtime dependencies are incomplete")
	}
	// 管理库闭包的必需条件：开关打开却少了闭包，后果是 nil 句柄上建出三张
	// "永远写不进"的存储——分组、档位、账号看起来都在，实际一条都存不下来。
	if deps.config.MgrStore.Enabled && deps.newMgrDB == nil {
		return fmt.Errorf("runtime dependencies are incomplete")
	}
	// 事件写入器的闭包按"这次要不要用它"来判断必需性：观测层总开关或事件子开关任一关闭时
	// 不构造才是预期行为（默认配置就该一个订阅者都不挂）。
	// 两个开关都打开却少了闭包，后果是"三张表里的事件那张永远是空的"，所以必须启动期报错。
	if deps.config.Observability.Enabled && deps.config.Observability.Events.Enabled && deps.newEventLog == nil {
		return fmt.Errorf("runtime dependencies are incomplete")
	}
	if deps.config.Observability.Enabled && deps.config.Observability.Artifacts.Enabled &&
		deps.config.Executors.Enabled && deps.newArtifactIndex == nil {
		return fmt.Errorf("runtime dependencies are incomplete")
	}
	// 台账的必需条件与事件写入器同形（总开关 + 自己的子开关），不带执行器那一条：
	// 写操作在任何部署里都可能发生。
	if deps.config.Observability.Enabled && deps.config.Observability.Audit.Enabled &&
		deps.newAuditLog == nil {
		return fmt.Errorf("runtime dependencies are incomplete")
	}
	// 档位文件只在 web_enabled 时才是必需依赖：默认关闭的部署一个文件都不碰，
	// 少配这个闭包不该改变行为（DoD 的"行为与本卡之前一致"）。
	if deps.config.Executors.WebEnabled && deps.newExecutorProfileStore == nil {
		return fmt.Errorf("runtime dependencies are incomplete")
	}
	// 监听器的构造闭包按"这次要不要用它"判断必需性，与 newEventLog 同一取向：
	// 只有开关打开、且启动时真的读到了那个文件才会用到它（没有文件可盯的部署本来就不建监听器，
	// 少配闭包不该改变行为）。真要用却少了它，后果是"改了文件永远不生效而日志上看不出来"，
	// 所以停在启动期。
	if deps.config.Reload.Enabled && deps.configPath != "" && deps.newConfigWatcher == nil {
		return fmt.Errorf("runtime dependencies are incomplete")
	}
	return nil
}

// newConfiguredScheduler 建调度器并把启动期的池规模一次配齐。
// 池规模的取值规则与执行器开关的关系见函数内注释（schedulerAPI 同名方法的说明）。
func newConfiguredScheduler(deps runtimeDeps, cfg core.Config, store core.Store) schedulerAPI {
	scheduler := deps.newScheduler(store, &core.ExponentialBackoffRetry{
		MaxDelay: cfg.Scheduler.MaxRetryDelay,
	}, nil)
	scheduler.SetConcurrency(cfg.Scheduler.Workers)
	scheduler.SetQueueCapacity(cfg.Scheduler.QueueCapacity)
	// 执行器池的规模只在打开执行器时才建：cfg.Executors.Concurrency 在 Normalized 里
	// 已经补齐默认值（显式写 0 会被配置校验拒绝），没打开时这一对调用传 0，
	// 调度器因此不建通道也不起协程，未使用执行器的进程与本卡之前一字不差。
	if cfg.Executors.Enabled {
		scheduler.SetExecConcurrency(cfg.Executors.Concurrency)
		scheduler.SetExecQueueCapacity(cfg.Executors.QueueCapacity)
	} else {
		scheduler.SetExecConcurrency(0)
		scheduler.SetExecQueueCapacity(0)
	}
	// 事件里输出预览的字节上限：取值已在 Normalized 里补齐，非法值的兜底由调度器负责。
	scheduler.SetEventPreviewLimit(cfg.Executors.Output.InlinePreview)
	return scheduler
}

// openMgrDatabase 打开 SQLite 管理库并完成一次性导入，只在 mgrstore.enabled=true 时动作，
// 否则交回 (nil, nil)。
//
// 返回值的形状要专门说明：Open 成功但后续失败（句柄断言不过、导入失败）时，
// 句柄会随错误一起交回——调用方据此把 Close 挂上 defer，早退路径同样收掉，
// 与拆分前"defer 在 Open 之后立刻声明、早退同样执行"的形状一致。
func openMgrDatabase(deps runtimeDeps, cfg core.Config) (mgrAPI, error) {
	if !cfg.MgrStore.Enabled {
		return nil, nil
	}
	mgrdb, err := deps.newMgrDB(cfg, deps.logger)
	if err != nil {
		return nil, err
	}
	handle, ok := mgrdb.(*mgr.Handle)
	if !ok {
		// 与 newEventLog 的断言同一处置：导入要真连接，接了假句柄就报错。
		return mgrdb, fmt.Errorf("mgr db needs the real handle, got %T", mgrdb)
	}
	deps.logger.Info("mgr database opened",
		"path", handle.Path(), "journal_mode", handle.JournalMode(), "synchronous", handle.Synchronous())
	if err := mgr.ImportOnce(handle, mgr.ImportSource{
		AuthUsers:    cfg.Server.Auth.Users,
		GroupsPath:   cfg.Store.GroupsPath,
		ProfilesPath: cfg.Executors.ProfilesPath,
	}, deps.logger); err != nil {
		return mgrdb, err
	}
	return mgrdb, nil
}

// buildExecutorStack 装配执行器一侧：档位文件、登记表与两者的合并。
// 登记表之外还把档位存储一并交回——buildProfileStoreAPI 要用同一个实例
// （页面写入），reload 链的 executors.commands 热更也要（ApplyConfig）。
//
// 档位文件在登记表之前读、在注册之前合。两个时点都有理由：
//   - 读不出来就没有"这一批档位"可合，那种状态不该带着起服务（见 newExecutorProfileStore）；
//   - 合批必须早于 registerHandlers，因为崩溃恢复守卫按注册表里的类别改判
//     （installRestoreGuard 与 Start 第一步的 Restore 都晚于注册）。
//     晚一步注册的那批档位，它的崩溃现场会查不到类别、跳过 paused 改判直接重排。
//     这条顺序由 TestRun_StoredProfilesAreRegisteredBeforeTheRestoreGuard 钉住。
//
// 存储实例只建这一份：启动合并与页面写入共用同一个内存视图，
// 否则"页面上刚存的档位"与"下一次 Apply 读到的内容"会各看一份文件状态。
func buildExecutorStack(deps runtimeDeps, cfg core.Config) (*executor.Registry, core.ExecutorProfileStore, error) {
	var profileStore core.ExecutorProfileStore
	var storedProfiles []core.ExecutorProfileRecord
	if cfg.Executors.WebEnabled {
		var err error
		profileStore, err = deps.newExecutorProfileStore(cfg)
		if err != nil {
			return nil, nil, err
		}
		storedProfiles, err = profileStore.List()
		if err != nil {
			return nil, nil, err
		}
	}

	executors, err := deps.newExecutorRegistry(cfg, deps.logger)
	if err != nil {
		// 档位配置非法（越界路径、引用未声明的参数等）属于装配期错误：
		// 带着半套配置启动，任务会在触发时才失败，那时已经看不出是哪一条配置的问题。
		return nil, nil, err
	}
	if cfg.Executors.WebEnabled {
		// 合并规则全在 executor 包里（一份校验、单条不连坐、撞名降级），这里只做接线。
		entries, warnings := executor.MergeStoreProfiles(cfg, storedProfiles)
		for _, warning := range warnings {
			deps.logger.Warn("stored executor profile not registered",
				"profile", warning.Name, "reason", warning.Reason)
		}
		if err := executors.ApplyStore(entries); err != nil {
			// 走到这里是"文件里两条记录占同一个注册键"或编程错误：
			// 那种表面对不上的登记表会让接口与调度器各说一套，不让它带着起服务。
			return nil, nil, err
		}
	}
	if cfg.Executors.Enabled && !cfg.Server.Auth.Enabled() {
		// 只记日志不阻止启动：测试环境需要能在没有凭据的情况下打开执行器，
		// 而生产环境漏配鉴权的后果由部署检查与这条 error 级记录共同承担。
		deps.logger.Error("executors are enabled while server authentication is disabled",
			"hint", "set server.auth.token or server.auth.users before exposing executors")
	}
	return executors, profileStore, nil
}

// openArtifactStore 建产物存储，只在打开执行器时建，否则交回 nil：
// 接口要拿它注入 api.Server，保持 nil 时 /jobs/:id/result 据此回 503，
// 而不是在没有任何产物文件的目录上读出"结果为空"。
func openArtifactStore(deps runtimeDeps, cfg core.Config) (*executor.ArtifactStore, error) {
	if !cfg.Executors.Enabled {
		return nil, nil
	}
	// 产物目录只在打开执行器时创建：关闭状态下不可能有输出需要安放，
	// 无谓地建出 ./data/exec 会让"这次部署没启用执行器"看起来像在写文件。
	artifacts, err := deps.newArtifactStore(cfg, deps.logger)
	if err != nil {
		// 目录建不起来等于输出无处可写，与档位配置非法同级：不让进程带着"结果一定会丢"启动。
		return nil, err
	}
	// 清理协程不在这里起：Start 的第一轮扫描里要做索引对账，要等观测层挂好之后再起
	//（run 里 openArtifactStore 与 setupObservability 之间的先后即为此而设）。
	return artifacts, nil
}

// setupObservability 装配观测层的三个写入器（运行事件、产物索引、写操作审计），
// 只在总开关打开时动作，否则交回 (nil, nil)。
//
// 失败路径的形状要专门说明：Open 成功之后的任何一步失败（迁移版本读不出、某个写入器建不起来），
// env 都会带着已建好的句柄原样交回（db 必非 nil），关闭由 run() 的 defer obsEnv.close 负责——
// 拆分前这是几条在 Open 之后立刻挂上的 defer，早退路径同样执行；拆分后错误路径与成功路径
// 共用同一个收尾（observabilityEnv.close），不存在第二条关闭代码路径。
//
// 事件写入器必须在 scheduler.Start() 之前挂上：Restore 是 Start 的第一步，
// 恢复阶段会重新发布一批 job.scheduled。挂晚了的后果是这一批静默不进库，
// 而"重启后时间线少了头几条"看起来跟"任务本来就没排期"一模一样，事后无从分辨。
// 它的读取面同时交给 api.Server（run 把它合进 observabilityAPI 注入）：
// 装配了事件库之后两个事件端点改读库，没装配时保持 nil，端点走内存缓冲。
func setupObservability(deps runtimeDeps, cfg core.Config, scheduler schedulerAPI,
	artifacts *executor.ArtifactStore) (*observabilityEnv, error) {
	if !cfg.Observability.Enabled {
		return nil, nil
	}
	db, err := deps.newObservabilityDB(cfg, deps.logger)
	if err != nil {
		// 建不起库等于什么都记不住，与档位配置非法、产物目录建不起来同一口径：
		// 不让进程带着"这次部署其实一行历史都不会留下"的状态上线。
		return nil, err
	}
	env := &observabilityEnv{db: db}

	stats, err := db.Stats()
	if err != nil {
		// Open 已经成功却读不出迁移版本，说明这份库文件本身有问题（权限、被外部改坏）。
		// 报出来比让三张表安静地空着强。
		return env, fmt.Errorf("observability database %s is unusable: %w", db.Path(), err)
	}

	if cfg.Observability.Events.Enabled {
		env.events, err = deps.newEventLog(scheduler.GetEventBus(), db, cfg.Observability, deps.logger)
		if err != nil {
			return env, fmt.Errorf("observability event writer: %w", err)
		}
	}

	// 产物索引挂在产物存储上：写侧（两个执行器）与读侧（列表端点）拿的是同一份可选依赖，
	// 所以这里只挂一次，不往 api 的注入参数里再加一位。
	//
	// 三个开关缺一不可：观测层总开关关着就没有这张表；事件之外单独关产物子开关时
	// 文件照写、索引不记，此时列表端点明确 503；executors.enabled=false 时压根没有产物，
	// 建索引等于在库里留一张空表。
	if cfg.Observability.Artifacts.Enabled && artifacts != nil {
		env.artifactIndex, err = deps.newArtifactIndex(db, cfg.Observability, artifacts.Dir(), deps.logger)
		if err != nil {
			return env, fmt.Errorf("observability artifact index: %w", err)
		}
		artifacts.SetIndex(env.artifactIndex)
	}

	// 写操作台账：中间件装在 api 侧，行由这里创建的写入器落盘。
	// 未启用审计子开关时不创建，中间件因此退回"只记一行结构化日志"那条路径（本卡 §3.2）。
	if cfg.Observability.Audit.Enabled {
		env.audit, err = deps.newAuditLog(db, cfg.Observability, deps.logger)
		if err != nil {
			return env, fmt.Errorf("observability audit writer: %w", err)
		}
	}

	// journal_mode 记的是实际生效值：网络文件系统上 WAL 会静默退回 delete，
	// 只看配置文件里的写法看不出降级已经发生（本卡 §9 风险 2）。
	deps.logger.Info("observability enabled",
		"path", db.Path(),
		"schema_version", stats.SchemaVersion,
		"journal_mode", db.JournalMode(),
		"events_writer", env.events != nil,
		"artifact_index", env.artifactIndex != nil,
		"audit_writer", env.audit != nil)
	return env, nil
}

// buildProfileStoreAPI 把档位在线管理的两面合成一个参数（存储 + 同步器）。
// web_enabled=false 且 reload 不涉及执行器时整体交回 nil：三个写端点走到
// requireExecutorProfiles 那条明确的 503，不会出现"能建档位、建完不生效"这种半套部署。
//
// 同步器在这里建而不是在服务里建：它要同时握调度器、登记表、存储与产物存储，
// 这四样都在装配方手里，服务只该拿到已经接好的东西。
//
// 构造条件在 web_enabled 之外再加一支 reload：热重载 executors.commands 要用同一个 Applier
// 的 ApplyConfig（config 侧整表替换）。但**不为 reload 打开档位文件**——profileStore 的
// 打开条件仍只看 web_enabled（buildExecutorStack，一字未改）。所以 web_enabled=false、
// reload=true 的部署拿到的是不接 store 的 NewConfigApplier：档位文件照旧一次都不碰
// （W01 的关闭即惰性口径），那条 store 路径（Apply）在这种部署里返回明确错误而不是 panic，
// ApplyConfig 照常可用。写端点的 503 判定看的是 profileStore 与 applier 成对在场，
// 而这里 store 是 nil，服务据此仍回 503——放宽构造条件不会把写端点打开。
func buildProfileStoreAPI(deps runtimeDeps, cfg core.Config, profileStore core.ExecutorProfileStore,
	scheduler schedulerAPI, executors *executor.Registry,
	artifacts *executor.ArtifactStore) (*profileStoreAPI, error) {
	if !cfg.Executors.WebEnabled && !(cfg.Reload.Enabled && cfg.Executors.Enabled) {
		return nil, nil
	}
	var applier *executor.Applier
	var err error
	if cfg.Executors.WebEnabled {
		applier, err = executor.NewApplier(profileStore, scheduler, executors, artifacts, deps.logger)
	} else {
		// reload 打开但 web 关闭：不接 store，只服务 config 批次热更。
		applier, err = executor.NewConfigApplier(scheduler, executors, artifacts, deps.logger)
	}
	if err != nil {
		return nil, err
	}
	return &profileStoreAPI{store: profileStore, applier: applier}, nil
}

// buildReloadChain 建热重载链（TASK-R06）。调用方（run）保证只在 reload.enabled=true 时进来，
// 并保证 deps.reloadStatus 已就位——那句测试兜底的回填必须留在 run：
// 本函数按值收 deps，字段回写带不出来。
func buildReloadChain(deps runtimeDeps, cfg core.Config, store core.Store, scheduler schedulerAPI,
	obsEnv *observabilityEnv, profiles *profileStoreAPI) reloadChain {
	var events eventLogAPI
	var audit auditLogAPI
	if obsEnv != nil {
		events = obsEnv.events
		audit = obsEnv.audit
	}
	build := deps.newReloadChain
	if build == nil {
		build = func(d reloadDeps) reloadChain { return newReloader(d) }
	}
	return build(reloadDeps{
		cfgPath:          deps.configPath,
		applied:          cfg,
		store:            store,
		scheduler:        scheduler,
		events:           events,
		audit:            audit,
		applier:          profiles.configApplier(),
		executorsEnabled: cfg.Executors.Enabled,
		levelVar:         deps.levelVar,
		logger:           deps.logger,
	})
}

// startConfigWatcher 在服务开始监听之后起配置监听器并放跑事件循环。
// 必须晚于 server.Start()：那条链会换处理函数表、改并发数，
// 在服务还没监听时就开始改现网，等于给"启动到就绪"这段窗口加了一条没人看着的写路径。
//
// 三种"起不来/起不了"的部署都继续提供服务，不阻止启动（卡 §3.4）：
// 热重载是便利，不是承重结构，因它起不来而让进程拒启等于把新能力变成新故障面。
// 但每一种都要留下说得出口的结论（I3）：一条日志，加进 /admin/runtime 的 reload 对象。
// chain 为 nil（reload 未启用）时交回 nil。
func startConfigWatcher(deps runtimeDeps, cfg core.Config, chain reloadChain) configWatcherAPI {
	if chain == nil {
		return nil
	}
	if deps.configPath == "" {
		deps.logger.Warn("config hot reload is enabled but no config file was loaded at startup",
			"hint", "用 -config 指一个真实存在的文件，或放置 configs/config.yaml 后重启；"+
				"在此之前进程按当前生效的取值一直跑下去，改文件不会有任何反应")
		deps.reloadStatus.explain("", "reload.enabled=true，但进程启动时没有读到任何配置文件，监听器没有建立")
		return nil
	}
	started, err := deps.newConfigWatcher(deps.configPath, cfg.Reload.Debounce, chain.Reload, deps.logger)
	if err != nil {
		deps.logger.Error("config watcher failed to start, serving without hot reload",
			"error", err, "path", deps.configPath)
		deps.reloadStatus.explain(deps.configPath, "监听器没有建立："+err.Error())
		return nil
	}
	// 顺序要紧：读口先接上，链先拿到防抖落点，最后才放事件循环进来。
	// 这三步都早于 go Run，所以没有任何并发方看得到"半条链"。
	deps.reloadStatus.attach(started)
	chain.bindDebounce(started)
	go started.Run(context.Background())
	deps.logger.Info("config hot reload enabled",
		"path", deps.configPath, "debounce", cfg.Reload.Debounce)
	return started
}

// awaitSignalAndShutdown 等信号并按固定四步收尾（卡 §3.4，判据是 §5.2 #14 的那条调用流水）：
//
//	watcher.Close() → chain.Stop() → server.Stop(ctx) → scheduler.Stop()
//
// 为什么写成显式一段而不是 defer：run() 里已有的那几条 defer（观测层写入器、观测库、任务存储）
// 晚于这里执行，而重载链必须在它们之前彻底停手——往已关闭的连接上写留痕是 deferred bug。
// 为什么第二步不可省：R05 的 Close 明确**不等**在途的那次 ReloadFunc（它可能正握着写链的锁，
// 等它只会把关停拖死），所以 Close 返回之后仍可能有一次 ApplyConfig 在改处理函数表，
// 而那正是设计文档 §7.8 要避免的交错。chain.Stop() 拿一次链的串行锁，等于等在途那一串走完。
func awaitSignalAndShutdown(deps runtimeDeps, watcher configWatcherAPI, chain reloadChain,
	server serverAPI, scheduler schedulerAPI) error {
	quit := make(chan os.Signal, 1)
	deps.notifySignals(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	deps.logger.Info("shutting down server")

	if watcher != nil {
		if err := watcher.Close(); err != nil {
			// 事件循环没在有界时间内退出：这既是一条 error，也要留在读数里让人事后读得到（I3）。
			watcher.MarkWatcherError(err.Error())
			deps.logger.Error("failed to close config watcher", "error", err)
		}
	}
	if chain != nil {
		chain.Stop()
	}

	ctx, cancel := context.WithTimeout(context.Background(), deps.timeout)
	defer cancel()

	if err := server.Stop(ctx); err != nil {
		deps.logger.Error("server forced to shutdown", "error", err)
	}

	scheduler.Stop()
	deps.logger.Info("server exited")
	return nil
}

// liveJobIDs 把存储里的任务 ID 集合包成产物清理需要的"还在不在"判断。
// 读不出来时返回错误，交由 ArtifactStore 跳过本轮——宁可不删，也不要在信息不全时删文件。
func liveJobIDs(store core.Store) func() (map[string]bool, error) {
	return func() (map[string]bool, error) {
		snapshots, err := store.LoadAll()
		if err != nil {
			return nil, err
		}
		live := make(map[string]bool, len(snapshots))
		for _, snapshot := range snapshots {
			live[snapshot.ID] = true
		}
		return live, nil
	}
}

// registerHandlers 注册示例处理函数，以及配置里声明的执行器档位。
//
// 示例走 api.Server、档位走调度器，是因为 api.Server 只转发写入、不转发查询，
// 而注册档位需要"先确认键没被占用再写入"，查重与写入必须落在同一张注册表上。
// 两者在真实进程里本来就是同一张表：api.Server.RegisterJobHandler 就是调度器的转发。
//
// cfg 与 artifacts 一起传给注册：档位的处理函数从这一步起是真实执行器，
// 它要按 executors 一节求生效超时，也要有产物存储可写。
func registerHandlers(server serverAPI, scheduler schedulerAPI, reg *executor.Registry,
	cfg core.Config, artifacts *executor.ArtifactStore, logger *slog.Logger) error {
	server.RegisterJobHandler("payment_check", handlePaymentCheck)
	server.RegisterJobHandler("email_send", handleEmailSend)
	server.RegisterJobHandler("data_sync", handleDataSync)
	server.RegisterJobHandler("report_generate", handleReportGenerate)

	if reg == nil {
		return nil
	}
	if cfg.Executors.Enabled && artifacts == nil {
		// 档位的处理函数从这里起会真的起进程，没有产物存储就等于每次执行都没有输出可查。
		// 装配少传一个参数属于编程错误：停在这儿比等到任务逐个报"没地方写输出"好排查。
		// 判断依据是运行配置而非登记表开关：真正决定要不要存储的是这一次启动开没开执行器。
		return fmt.Errorf("executors are enabled but no artifact store was provided")
	}
	_, err := executor.Register(scheduler, reg, cfg, artifacts, logger)
	return err
}

// installRestoreGuard 按配置决定：崩溃时正在跑的执行器任务，重启后要不要停在 paused 上等人确认。
//
// 两种情况不装守卫，行为与 TASK-E14 之前一字不差：
//   - 没打开执行器（这个进程里根本没有执行器任务）；
//   - executors.restore_policy=replay（部署方明确表态"重复执行的后果我自己承担"）。
//
// installRestoreGuard 按 scheduler.recovery_policy（TASK-RE05）决定崩溃恢复策略：
//
//   - replay：不装守卫，所有类别一律复位 pending 重放；
//   - pause：装"拦一切 running"的守卫，所有类别停 paused（保守部署）；
//   - auto（缺省，含空）：维持 TASK-E14 的既有语义——执行器启用且
//     executors.restore_policy!=replay 时装"只拦执行器类 running"的守卫，
//     普通任务照旧重放。
//
// 优先级：新键（全局）显式配置时压过旧键（只管执行器）——两者同时显式且
// 结论不同时记一条 warn（不拒启：旧键是既定契约，新键是全局覆盖，矛盾说明
// 部署方在迁移路上，warn 给迁移留痕）。
//
// 必须在 Start 之前调用：Restore 是 Start 的第一步，任务这时已经跑起来了再装就来不及了。
func installRestoreGuard(scheduler schedulerAPI, cfg core.Config, logger *slog.Logger) {
	policy := strings.TrimSpace(cfg.Scheduler.RecoveryPolicy)
	if policy == "" {
		policy = "auto"
	}

	oldPolicy := strings.TrimSpace(cfg.Executors.RestorePolicy)
	// 结论分歧判定：仅当旧键显式给出、且两键的组合不产生同一结果时 warn。
	// （replay+replay 两键同为"不装守卫"，不打扰；其余组合的作用域或结论都不同。）
	if policy != "auto" && cfg.Executors.Enabled && oldPolicy != "" &&
		!(policy == "replay" && oldPolicy == "replay") {
		logger.Warn("scheduler.recovery_policy overrides executors.restore_policy",
			"recovery_policy", policy, "restore_policy", oldPolicy)
	}

	switch policy {
	case "replay":
		return
	case "pause":
		scheduler.SetRestoreGuard(pauseAllRunningOnRestore())
		return
	default: // auto
		if !cfg.Executors.Enabled {
			return
		}
		if oldPolicy == "replay" {
			return
		}
		scheduler.SetRestoreGuard(pauseRunningExecOnRestore(scheduler))
	}
}

// pauseAllRunningOnRestore 是 scheduler.recovery_policy=pause 的守卫（TASK-RE05）：
// 拦下所有崩溃前 running 的快照，不分执行类别。running 快照的含义是"进程确实
// 起来过、结果未知"——外部副作用不可重放的任务一律停住等人确认。
// 优雅关闭不走这里：那条路径由 handleInterrupted 把任务落成 pending，重启后照常重跑。
func pauseAllRunningOnRestore() core.RestoreGuard {
	return func(snap core.JobSnapshot) (core.JobStatus, bool) {
		if core.JobStatus(snap.Status) == core.StatusRunning {
			return core.StatusPaused, true
		}
		return 0, false
	}
}

// pauseRunningExecOnRestore 是装上调度器的那个守卫：只拦"快照还是 running 的执行器任务"。
//
// running 快照的含义是"进程确实起来过、结果未知"——被强杀的进程来不及写任何结论。
// 优雅关闭不走这里：那条路径由 handleInterrupted 把任务落成 pending，重启后照常重跑。
//
// 档位被删掉之后它的历史任务查不到类别（第二个返回值为 false），这里选择"不改判"：
// 任务会被重新排期，但在 executeJob 里因为找不到处理函数直接判失败，
// 外部副作用不会真的再来一遍。别把这条读成"档位删掉=任务被重放"。
func pauseRunningExecOnRestore(scheduler schedulerAPI) core.RestoreGuard {
	return func(snap core.JobSnapshot) (core.JobStatus, bool) {
		if core.JobStatus(snap.Status) != core.StatusRunning {
			return 0, false
		}
		class, registered := scheduler.HandlerClass(snap.HandlerKey())
		if !registered || class != core.JobClassExec {
			return 0, false
		}
		return core.StatusPaused, true
	}
}

func handlePaymentCheck(ctx context.Context, job *core.Job) error {
	slog.Info("processing payment check", "job_id", job.ID, "payload", string(job.Payload))
	select {
	case <-time.After(2 * time.Second):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func handleEmailSend(ctx context.Context, job *core.Job) error {
	slog.Info("sending email", "job_id", job.ID, "payload", string(job.Payload))
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func handleDataSync(ctx context.Context, job *core.Job) error {
	slog.Info("syncing data", "job_id", job.ID, "payload", string(job.Payload))
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func handleReportGenerate(ctx context.Context, job *core.Job) error {
	slog.Info("generating report", "job_id", job.ID, "payload", string(job.Payload))
	select {
	case <-time.After(10 * time.Second):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
