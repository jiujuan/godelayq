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

type signalNotifier func(chan<- os.Signal, ...os.Signal)

type runtimeDeps struct {
	config       core.Config
	newStore     func() (core.Store, error)
	newScheduler func(store core.Store, retryPolicy core.RetryPolicy, eventBus *core.EventBus) schedulerAPI
	// newExecutorRegistry 建档位登记表（加载 + 探测）。它必须显式提供：
	// 缺了它执行器会静默不注册，开关打开也看不出问题。
	newExecutorRegistry func(core.Config, *slog.Logger) (*executor.Registry, error)
	// newArtifactStore 建输出产物的文件存储与清理协程，只在 executors.enabled=true 时调用。
	// 与登记表一样列入依赖完整性检查：少了它输出会静默无处安放。
	newArtifactStore func(core.Config, *slog.Logger) (*executor.ArtifactStore, error)
	// newObservabilityDB 打开 SQLite 观测库（建目录、建表、应用迁移），只在
	// observability.enabled=true 时调用。它同样列入依赖完整性检查：少装配一个闭包的后果是
	// "三张表永远是空的"，看起来正常、实际没记，必须在启动期就报错。
	newObservabilityDB func(core.Config, *slog.Logger) (observabilityDB, error)
	newServer          func(scheduler schedulerAPI, store core.Store, port string, executors *executor.Registry, artifacts *executor.ArtifactStore) (serverAPI, error)
	notifySignals      signalNotifier
	timeout            time.Duration
	logger             *slog.Logger
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
		newServer: func(scheduler schedulerAPI, store core.Store, port string, executors *executor.Registry, artifacts *executor.ArtifactStore) (serverAPI, error) {
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
			return api.NewServer(coreScheduler, store, port, security, logger,
				api.WithGroupStore(groups),
				api.WithExecutorRegistry(executors),
				// 执行器关闭时 artifacts 是 nil，等价于不注入：/jobs/:id/result 回 503，
				// 而不是在没有任何产物文件的目录上读出"结果为空"。
				api.WithArtifacts(artifacts),
				// 不带 -tags dashboard 时 web.Dist 恒为 nil，这一行等价于"不提供控制台"。
				// 写成无条件调用而不是两份装配，是为了让单二进制的差异只留在 web 包那一处。
				api.WithConsole(web.Dist)), nil
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

	executors, err := deps.newExecutorRegistry(cfg, deps.logger)
	if err != nil {
		// 档位配置非法（越界路径、引用未声明的参数等）属于装配期错误：
		// 带着半套配置启动，任务会在触发时才失败，那时已经看不出是哪一条配置的问题。
		return err
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
		cleanupCtx, stopCleanup := context.WithCancel(context.Background())
		cleanerStopped := artifacts.Start(cleanupCtx, liveJobIDs(store))
		// 先停清理协程再关存储（后声明的先执行）：否则协程可能在存储已关闭后再去读一次任务集合。
		defer func() {
			stopCleanup()
			<-cleanerStopped
		}()
	}

	// 观测层（运行事件、产物索引、写操作审计三张表）只在总开关打开时装配。
	//
	// defer 的顺序有讲究：这里的 defer 晚于上面 store.Close 的 defer 声明，因此实际执行顺序
	// 是先关观测层、后关存储（设计文档 §7.3）。反过来就会在已关闭的任务存储之上再读一次快照，
	// S03 起这条链上还要先撤掉事件总线的订阅。
	//
	// 句柄没有存到 if 外面：本卡它除了关闭之外没有读取方，而 Go 不允许声明用不上的变量。
	// S03 注入事件写入器时再把它取出来，关闭顺序仍由这一行的 defer 保证。
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
		// journal_mode 记的是实际生效值：网络文件系统上 WAL 会静默退回 delete，
		// 只看配置文件里的写法看不出降级已经发生（本卡 §9 风险 2）。
		deps.logger.Info("observability enabled",
			"path", db.Path(),
			"schema_version", stats.SchemaVersion,
			"journal_mode", db.JournalMode())
	}

	server, err := deps.newServer(scheduler, store, cfg.Server.Port, executors, artifacts)
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
