package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"godelayq/api"
	"godelayq/core"
	"godelayq/executor"
	"godelayq/web"
)

type schedulerAPI interface {
	Start()
	Stop()
	RegisterHandler(jobType string, handler core.Handler)
	// LookupHandler 是注册档位前的查重入口：执行器写的是调度器里那张注册表，
	// 只靠 api.Server.RegisterJobHandler 这一个写入口看不到已注册的键。
	LookupHandler(jobType string) (core.Handler, bool)
	SetConcurrency(n int)
	SetQueueCapacity(n int)
	// SetEventPreviewLimit 把输出预览的字节上限交给调度器：完成/失败事件由 core 发布，
	// 尺寸限制必须在发事件的一方生效，而不能只在接口侧裁剪。
	SetEventPreviewLimit(n int)
}

type serverAPI interface {
	RegisterJobHandler(name string, handler core.Handler)
	Start() error
	Stop(ctx context.Context) error
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
	newServer        func(scheduler schedulerAPI, store core.Store, port string, executors *executor.Registry, artifacts *executor.ArtifactStore) (serverAPI, error)
	notifySignals    signalNotifier
	timeout          time.Duration
	logger           *slog.Logger
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
		deps.newArtifactStore == nil || deps.newServer == nil || deps.notifySignals == nil {
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

	server, err := deps.newServer(scheduler, store, cfg.Server.Port, executors, artifacts)
	if err != nil {
		return err
	}
	if err := registerHandlers(server, scheduler, executors, deps.logger); err != nil {
		return err
	}

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
func registerHandlers(server serverAPI, scheduler schedulerAPI, reg *executor.Registry, logger *slog.Logger) error {
	server.RegisterJobHandler("payment_check", handlePaymentCheck)
	server.RegisterJobHandler("email_send", handleEmailSend)
	server.RegisterJobHandler("data_sync", handleDataSync)
	server.RegisterJobHandler("report_generate", handleReportGenerate)

	if reg == nil {
		return nil
	}
	_, err := executor.Register(scheduler, reg, logger)
	return err
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
