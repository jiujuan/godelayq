package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"godelayq/api"
	"godelayq/core"
	"godelayq/executor"
	"godelayq/store/sqlite"
	"godelayq/web"
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
	SetConcurrency(n int)
	SetQueueCapacity(n int)
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
// 真实 *sqlite.DB 的关闭时机在测试里没有别的观察办法。
// 本卡只用到这三个方法，S03 起要把事件写入器接进装配时再扩，不提前摆空参数。
type observabilityDB interface {
	Path() string
	JournalMode() string
	Stats() (sqlite.Stats, error)
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
	Append(api.AuditEntry) error
	Query(api.AuditFilter) ([]api.AuditEntry, int, error)
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

type runtimeDeps struct {
	config       core.Config
	newStore     func() (core.Store, error)
	newScheduler func(store core.Store, retryPolicy core.RetryPolicy, eventBus *core.EventBus) schedulerAPI
	// newExecutorRegistry 建档位登记表（加载 + 探测）。它必须显式提供：
	// 缺了它执行器会静默不注册，开关打开也看不出问题。
	newExecutorRegistry func(core.Config, *slog.Logger) (*executor.Registry, error)
	// newExecutorProfiles 读档位文件（executors.profiles_path）里的全部记录，
	// 只在 executors.web_enabled=true 时调用。
	//
	// 它与登记表构造分成两个闭包，是因为这两种失败的后果不同：
	// 文件读不出来是"这台机器的档位状态不可信"，必须挡住启动；
	// 档位本身写错则是 config 侧连坐、文件侧单条跳过（executor.MergeStoreProfiles 的注释）。
	// 合成一个闭包的话，这两种脸色只能给同一种。
	newExecutorProfiles func(core.Config) ([]core.ExecutorProfileRecord, error)
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
	newServer   func(scheduler schedulerAPI, store core.Store, port string, executors *executor.Registry,
		artifacts *executor.ArtifactStore, obs *observabilityAPI) (serverAPI, error)
	notifySignals signalNotifier
	timeout       time.Duration
	logger        *slog.Logger
}

// defaultRuntimeDeps 把配置注入各构造闭包，run() 本身不再关心具体取值来源
func defaultRuntimeDeps(cfg core.Config, logger *slog.Logger) runtimeDeps {
	return runtimeDeps{
		config: cfg,
		newStore: func() (core.Store, error) {
			return core.NewJSONFileStoreWithOptions(cfg.Store.Path, core.StoreOptions{
				Interval:     cfg.Store.FlushInterval,
				HistoryLimit: cfg.Store.HistoryLimit,
				HistoryTTL:   cfg.Store.HistoryTTL,
				Logger:       logger,
			})
		},
		newScheduler: func(store core.Store, retryPolicy core.RetryPolicy, eventBus *core.EventBus) schedulerAPI {
			return core.NewScheduler(store, retryPolicy, eventBus, core.WithLogger(logger))
		},
		newExecutorRegistry: executor.NewRegistry,
		newExecutorProfiles: func(cfg core.Config) ([]core.ExecutorProfileRecord, error) {
			normalized := cfg.Normalized()
			profiles, err := core.NewJSONFileExecutorProfileStore(normalized.Executors.ProfilesPath)
			if err != nil {
				// 这条失败会拦住整个进程，所以文案必须给出自救路径：拦住它的那份文件
				// 多半是人手改坏或从别的机器拷来的，"删掉它"就是退回只由
				// executors.commands 决定档位的形态。文件路径由存储那一层带出来。
				return nil, fmt.Errorf("load executor profile store failed: %w; "+
					"fix that file or delete it to fall back to profiles declared only in executors.commands", err)
			}
			records, err := profiles.List()
			if err != nil {
				return nil, fmt.Errorf("list executor profiles failed: %w; "+
					"delete the file to fall back to profiles declared only in executors.commands", err)
			}
			return records, nil
		},
		newArtifactStore: func(cfg core.Config, logger *slog.Logger) (*executor.ArtifactStore, error) {
			return executor.NewArtifactStore(executor.ArtifactOptions{
				Dir:      cfg.Executors.Output.Dir,
				MaxBytes: cfg.Executors.Output.MaxBytes,
				TTL:      cfg.Executors.Output.TTL,
			}, logger)
		},
		newObservabilityDB: func(cfg core.Config, logger *slog.Logger) (observabilityDB, error) {
			db, err := sqlite.Open(cfg.Observability, logger)
			if err != nil {
				// 这里必须返回 nil 而不是那个类型化的空句柄：
				// 装了 *sqlite.DB(nil) 的接口值不等于 nil，调用方的判空会失效。
				return nil, err
			}
			return db, nil
		},
		newEventLog: func(bus *core.EventBus, db observabilityDB, cfg core.ObservabilityConfig, logger *slog.Logger) (eventLogAPI, error) {
			handle, ok := db.(*sqlite.DB)
			if !ok {
				// 真实装配里这一步一定成立：句柄就是上面 sqlite.Open 建出来的那一个。
				// 不成立说明有人把假句柄接到了要真连接的写入器上，直接报错，
				// 而不是让写入器静默缺席、事件表永远为空。
				return nil, fmt.Errorf("event writer needs the real observability handle, got %T", db)
			}
			return sqlite.NewEventLog(bus, handle, sqlite.EventLogOptions{
				FlushInterval:  cfg.FlushInterval,
				QueueCapacity:  cfg.QueueCapacity,
				RetentionCount: cfg.Events.RetentionCount,
				RetentionAge:   cfg.Events.RetentionAge,
			}, logger)
		},
		newArtifactIndex: func(db observabilityDB, cfg core.ObservabilityConfig, rootDir string, logger *slog.Logger) (executor.ArtifactIndexer, error) {
			handle, ok := db.(*sqlite.DB)
			if !ok {
				// 与 newEventLog 同一处置：索引要真连接，接了假句柄就报错，
				// 而不是让这张表安静地空着。
				return nil, fmt.Errorf("artifact index needs the real observability handle, got %T", db)
			}
			return sqlite.NewArtifactIndex(handle, sqlite.ArtifactIndexOptions{
				RootDir: rootDir,
			}, logger)
		},
		newAuditLog: func(db observabilityDB, cfg core.ObservabilityConfig, logger *slog.Logger) (auditLogAPI, error) {
			handle, ok := db.(*sqlite.DB)
			if !ok {
				// 与 newEventLog、newArtifactIndex 同一处置：台账要真连接，
				// 接了假句柄就报错，而不是让这张表安静地空着。
				return nil, fmt.Errorf("audit log needs the real observability handle, got %T", db)
			}
			return sqlite.NewAuditLog(handle, sqlite.AuditLogOptions{
				FlushInterval:  cfg.FlushInterval,
				QueueCapacity:  cfg.QueueCapacity,
				RetentionCount: cfg.Audit.RetentionCount,
				RetentionAge:   cfg.Audit.RetentionAge,
			}, logger)
		},
		newServer: func(scheduler schedulerAPI, store core.Store, port string, executors *executor.Registry,
			artifacts *executor.ArtifactStore, obs *observabilityAPI) (serverAPI, error) {
			coreScheduler, ok := scheduler.(*core.Scheduler)
			if !ok {
				return nil, fmt.Errorf("default server requires *core.Scheduler, got %T", scheduler)
			}
			security := api.Security{
				Auth:             cfg.Server.Auth,
				AllowOrigins:     cfg.Server.CORS.AllowOrigins,
				AllowCredentials: cfg.Server.CORS.AllowCredentials,
			}
			// 分组注册表读不出来就别起服务：与其让 /groups 每个请求都 500，
			// 不如在启动日志里就把文件权限/格式问题摆到眼前。
			groups, err := core.NewJSONFileGroupStore(cfg.Store.GroupsPath)
			if err != nil {
				return nil, err
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
			return api.NewServer(coreScheduler, store, port, security, logger, opts...), nil
		},
		notifySignals: signal.Notify,
		timeout:       cfg.Scheduler.ShutdownTimeout,
		logger:        logger,
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

	logger, err := core.NewLogger(cfg.Logging.Level, cfg.Logging.Format, os.Stdout)
	if err != nil {
		log.Fatalf("init logging failed: %v", err)
	}
	// 让第三方库与示例 handler 的包级 slog 调用走同一份配置
	slog.SetDefault(logger)

	if err := run(defaultRuntimeDeps(cfg, logger)); err != nil {
		// Go 1.26 起标准库 log 桥接到 slog.Default()：走 log.Fatal 只会留下一条
		// 级别为 INFO 的记录，按 level=error 采集的告警不会触发。
		// 所以先用配置好的日志器按 error 级记一次，退出码由 os.Exit 给出。
		logger.Error("server exited with error", "error", err)
		os.Exit(1)
	}
}

func run(deps runtimeDeps) error {
	if deps.newStore == nil || deps.newScheduler == nil || deps.newExecutorRegistry == nil ||
		deps.newArtifactStore == nil || deps.newObservabilityDB == nil || deps.newServer == nil ||
		deps.notifySignals == nil {
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
	if deps.config.Executors.WebEnabled && deps.newExecutorProfiles == nil {
		return fmt.Errorf("runtime dependencies are incomplete")
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

	// 档位文件在登记表之前读、在注册之前合。两个时点都有理由：
	//   - 读不出来就没有"这一批档位"可合，那种状态不该带着起服务（见 newExecutorProfiles）；
	//   - 合批必须早于 registerHandlers，因为崩溃恢复守卫按注册表里的类别改判
	//     （installRestoreGuard 与 Start 第一步的 Restore 都晚于注册）。
	//     晚一步注册的那批档位，它的崩溃现场会查不到类别、跳过 paused 改判直接重排。
	//     这条顺序由 TestRun_StoredProfilesAreRegisteredBeforeTheRestoreGuard 钉住。
	var storedProfiles []core.ExecutorProfileRecord
	if cfg.Executors.WebEnabled {
		storedProfiles, err = deps.newExecutorProfiles(cfg)
		if err != nil {
			return err
		}
	}

	executors, err := deps.newExecutorRegistry(cfg, deps.logger)
	if err != nil {
		// 档位配置非法（越界路径、引用未声明的参数等）属于装配期错误：
		// 带着半套配置启动，任务会在触发时才失败，那时已经看不出是哪一条配置的问题。
		return err
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
			return err
		}
	}
	if cfg.Executors.Enabled && !cfg.Server.Auth.Enabled() {
		// 只记日志不阻止启动：测试环境需要能在没有凭据的情况下打开执行器，
		// 而生产环境漏配鉴权的后果由部署检查与这条 error 级记录共同承担。
		deps.logger.Error("executors are enabled while server authentication is disabled",
			"hint", "set server.auth.token or server.auth.users before exposing executors")
	}

	// 产物存储与清理协程只在打开执行器时建。变量声明在 if 之外：
	// 接口要拿它注入 api.Server，关闭时保持 nil，/jobs/:id/result 据此回 503。
	var artifacts *executor.ArtifactStore
	if cfg.Executors.Enabled {
		// 产物目录只在打开执行器时创建：关闭状态下不可能有输出需要安放，
		// 无谓地建出 ./data/exec 会让"这次部署没启用执行器"看起来像在写文件。
		var err error
		artifacts, err = deps.newArtifactStore(cfg, deps.logger)
		if err != nil {
			// 目录建不起来等于输出无处可写，与档位配置非法同级：不让进程带着"结果一定会丢"启动。
			return err
		}
		// 清理协程到观测层挂好之后再起：Start 的第一轮扫描里要做索引对账
		// （见下面观测层那段），提前起就看不到索引。
	}

	// 事件写入器在观测层装配那段里创建，声明放在外面：它同时是 api.Server 的事件读取方，
	// 而 newServer 在观测层之后调用。未启用时保持 nil，两个事件端点因此走内存缓冲。
	var events eventLogAPI
	// 产物索引同样声明在外面：挂给它的是产物存储（SetIndex），而日志要知道这次到底挂没挂。
	var artifactIndex executor.ArtifactIndexer
	// 台账也声明在外面：它既要在观测层那段里创建与关停，又要作为观测层参数交给服务。
	var audit auditLogAPI

	// 观测层（运行事件、产物索引、写操作审计三张表）只在总开关打开时装配。
	//
	// defer 的顺序有讲究：这里的 defer 晚于上面 store.Close 的 defer 声明，因此实际执行顺序
	// 是先关事件写入器、再关观测库、最后关任务存储（设计文档 §7.3）。反过来就会在已关闭的
	// 连接上继续写入，或在已关闭的任务存储之上再读一次快照。
	if cfg.Observability.Enabled {
		db, err := deps.newObservabilityDB(cfg, deps.logger)
		if err != nil {
			// 建不起库等于什么都记不住，与档位配置非法、产物目录建不起来同一口径：
			// 不让进程带着"这次部署其实一行历史都不会留下"的状态上线。
			return err
		}
		// 收尾立刻挂上，再读迁移版本：Stats 失败也要把这份句柄关掉，
		// 否则早退路径留下一条没人认领的连接。
		defer func() {
			if err := db.Close(); err != nil {
				deps.logger.Error("failed to close observability database", "error", err)
			}
		}()

		stats, err := db.Stats()
		if err != nil {
			// Open 已经成功却读不出迁移版本，说明这份库文件本身有问题（权限、被外部改坏）。
			// 报出来比让三张表安静地空着强。
			return fmt.Errorf("observability database %s is unusable: %w", db.Path(), err)
		}

		// 事件写入器必须在 scheduler.Start() 之前挂上：Restore 是 Start 的第一步，
		// 恢复阶段会重新发布一批 job.scheduled。挂晚了的后果是这一批静默不进库，
		// 而"重启后时间线少了头几条"看起来跟"任务本来就没排期"一模一样，事后无从分辨。
		//
		// 它的读取面同时交给 api.Server（下面的 deps.newServer 把它作为事件读取方注入）：
		// 装配了事件库之后两个事件端点改读库，没装配时这里保持 nil，端点走内存缓冲。
		if cfg.Observability.Events.Enabled {
			events, err = deps.newEventLog(scheduler.GetEventBus(), db, cfg.Observability, deps.logger)
			if err != nil {
				return fmt.Errorf("observability event writer: %w", err)
			}
			// 这条 defer 晚于上面 db.Close 的那条，因此先执行：撤订阅 → 等转发协程退出 →
			// 落完最后一批，然后才关连接（顺序反过来就是往已关闭的连接里写）。
			defer func() {
				if err := events.Close(); err != nil {
					deps.logger.Error("failed to close observability event writer", "error", err)
				}
				// 丢了多少必须在关停时也能看见：库里缺页是读不出来的（设计文档 §7.1）。
				if dropped := events.Dropped(); dropped > 0 {
					deps.logger.Warn("observability event writer dropped records",
						"dropped", dropped, "path", db.Path())
				}
			}()
		}

		// 产物索引挂在产物存储上：写侧（两个执行器）与读侧（列表端点）拿的是同一份可选依赖，
		// 所以这里只挂一次，不往 api 的注入参数里再加一位。
		//
		// 三个开关缺一不可：观测层总开关关着就没有这张表；事件之外单独关产物子开关时
		// 文件照写、索引不记，此时列表端点明确 503；executors.enabled=false 时压根没有产物，
		// 建索引等于在库里留一张空表。
		if cfg.Observability.Artifacts.Enabled && artifacts != nil {
			artifactIndex, err = deps.newArtifactIndex(db, cfg.Observability, artifacts.Dir(), deps.logger)
			if err != nil {
				return fmt.Errorf("observability artifact index: %w", err)
			}
			artifacts.SetIndex(artifactIndex)
		}

		// 写操作台账：中间件装在 api 侧，行由这里创建的写入器落盘。
		// 未启用审计子开关时不创建，中间件因此退回"只记一行结构化日志"那条路径（本卡 §3.2）。
		if cfg.Observability.Audit.Enabled {
			audit, err = deps.newAuditLog(db, cfg.Observability, deps.logger)
			if err != nil {
				return fmt.Errorf("observability audit writer: %w", err)
			}
			// 与事件写入器同一位置：晚于 db.Close 声明，因此先执行——剩余行先落盘再关连接。
			defer func() {
				if err := audit.Close(); err != nil {
					deps.logger.Error("failed to close observability audit writer", "error", err)
				}
				if dropped := audit.Dropped(); dropped > 0 {
					deps.logger.Warn("observability audit writer dropped records",
						"dropped", dropped, "path", db.Path())
				}
			}()
		}

		// journal_mode 记的是实际生效值：网络文件系统上 WAL 会静默退回 delete，
		// 只看配置文件里的写法看不出降级已经发生（本卡 §9 风险 2）。
		deps.logger.Info("observability enabled",
			"path", db.Path(),
			"schema_version", stats.SchemaVersion,
			"journal_mode", db.JournalMode(),
			"events_writer", events != nil,
			"artifact_index", artifactIndex != nil,
			"audit_writer", audit != nil)
	}

	// 产物清理协程在观测层挂好之后起：Start 的第一轮扫描是"孤儿清理 + TTL 清理 + 索引对账"，
	// 对账要读索引，起早了那份索引还不在。
	//
	// 这里的 defer 比观测层那两个 defer 后声明，因此关停顺序是：先停清理协程，
	// 再撤事件订阅、关观测库、最后关任务存储。反过来就会让清理协程在索引连接已关之后
	// 去删它该删的行。
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
	if events != nil || audit != nil {
		obs = &observabilityAPI{events: events, audit: audit}
	}

	server, err := deps.newServer(scheduler, store, cfg.Server.Port, executors, artifacts, obs)
	if err != nil {
		return err
	}
	if err := registerHandlers(server, scheduler, executors, cfg, artifacts, deps.logger); err != nil {
		return err
	}

	installRestoreGuard(scheduler, cfg)

	scheduler.Start()

	if err := server.Start(); err != nil {
		scheduler.Stop()
		return err
	}

	quit := make(chan os.Signal, 1)
	deps.notifySignals(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	deps.logger.Info("shutting down server")

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
// 必须在 Start 之前调用：Restore 是 Start 的第一步，任务这时已经跑起来了再装就来不及了。
func installRestoreGuard(scheduler schedulerAPI, cfg core.Config) {
	if !cfg.Executors.Enabled {
		return
	}
	if strings.TrimSpace(cfg.Executors.RestorePolicy) == "replay" {
		return
	}
	scheduler.SetRestoreGuard(pauseRunningExecOnRestore(scheduler))
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
