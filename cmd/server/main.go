package main

import (
	"context"
	"flag"
	"fmt"
	"log"
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
	logger        *log.Logger
}

// defaultRuntimeDeps 把配置注入各构造闭包，run() 本身不再关心具体取值来源
func defaultRuntimeDeps(cfg core.Config) runtimeDeps {
	return runtimeDeps{
		config: cfg,
		newStore: func() (core.Store, error) {
			return core.NewJSONFileStoreWithInterval(cfg.Store.Path, cfg.Store.FlushInterval)
		},
		newScheduler: func(store core.Store, retryPolicy core.RetryPolicy, eventBus *core.EventBus) schedulerAPI {
			return core.NewScheduler(store, retryPolicy, eventBus)
		},
		newServer: func(scheduler schedulerAPI, store core.Store, port string) (serverAPI, error) {
			coreScheduler, ok := scheduler.(*core.Scheduler)
			if !ok {
				return nil, fmt.Errorf("default server requires *core.Scheduler, got %T", scheduler)
			}
			return api.NewServer(coreScheduler, store, port), nil
		},
		notifySignals: signal.Notify,
		timeout:       cfg.Scheduler.ShutdownTimeout,
		logger:        log.Default(),
	}
}

func main() {
	configPath := flag.String("config", "",
		"配置文件路径；留空则尝试 "+core.DefaultConfigPath+"，都不存在时使用默认值")
	flag.Parse()

	cfg, err := core.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("load config failed: %v", err)
	}

	if err := run(defaultRuntimeDeps(cfg)); err != nil {
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
		deps.logger = log.Default()
	}

	store, err := deps.newStore()
	if err != nil {
		return err
	}
	// 存储按周期合并落盘，退出前必须收尾（早退路径同样覆盖）
	defer func() {
		if err := store.Close(); err != nil {
			deps.logger.Printf("Failed to close store: %v", err)
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

	deps.logger.Println("Shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), deps.timeout)
	defer cancel()

	if err := server.Stop(ctx); err != nil {
		deps.logger.Printf("Server forced to shutdown: %v", err)
	}

	scheduler.Stop()
	deps.logger.Println("Server exited")
	return nil
}

func registerHandlers(server serverAPI) {
	server.RegisterJobHandler("payment_check", handlePaymentCheck)
	server.RegisterJobHandler("email_send", handleEmailSend)
	server.RegisterJobHandler("data_sync", handleDataSync)
	server.RegisterJobHandler("report_generate", handleReportGenerate)
}

func handlePaymentCheck(ctx context.Context, job *core.Job) error {
	fmt.Printf("processing payment check: %s\n", string(job.Payload))
	select {
	case <-time.After(2 * time.Second):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func handleEmailSend(ctx context.Context, job *core.Job) error {
	fmt.Printf("sending email: %s\n", string(job.Payload))
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func handleDataSync(ctx context.Context, job *core.Job) error {
	fmt.Printf("syncing data: %s\n", string(job.Payload))
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func handleReportGenerate(ctx context.Context, job *core.Job) error {
	fmt.Printf("generating report: %s\n", string(job.Payload))
	select {
	case <-time.After(10 * time.Second):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
