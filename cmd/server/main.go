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
)

type schedulerAPI interface {
	Start()
	Stop()
	RegisterHandler(jobType string, handler core.Handler)
	SetConcurrency(n int)
	SetQueueCapacity(n int)
}

type serverAPI interface {
	RegisterJobHandler(name string, handler core.Handler)
	Start() error
	Stop(ctx context.Context) error
}

type signalNotifier func(chan<- os.Signal, ...os.Signal)

type runtimeDeps struct {
	config        core.Config
	newStore      func() (core.Store, error)
	newScheduler  func(store core.Store, retryPolicy core.RetryPolicy, eventBus *core.EventBus) schedulerAPI
	newServer     func(scheduler schedulerAPI, store core.Store, port string) (serverAPI, error)
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
		newServer: func(scheduler schedulerAPI, store core.Store, port string) (serverAPI, error) {
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
				api.WithGroupStore(groups)), nil
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
		log.Fatal(err)
	}
}

func run(deps runtimeDeps) error {
	if deps.newStore == nil || deps.newScheduler == nil || deps.newServer == nil || deps.notifySignals == nil {
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

	server, err := deps.newServer(scheduler, store, cfg.Server.Port)
	if err != nil {
		return err
	}
	registerHandlers(server)

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

func registerHandlers(server serverAPI) {
	server.RegisterJobHandler("payment_check", handlePaymentCheck)
	server.RegisterJobHandler("email_send", handleEmailSend)
	server.RegisterJobHandler("data_sync", handleDataSync)
	server.RegisterJobHandler("report_generate", handleReportGenerate)
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
