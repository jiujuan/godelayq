package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"godelayq/api"
	"godelayq/core"
)

const (
	defaultDataPath = "./data/jobs.json"
	defaultPort     = "8080"
)

type schedulerAPI interface {
	Start()
	Stop()
	RegisterHandler(jobType string, handler core.Handler)
}

type serverAPI interface {
	RegisterJobHandler(name string, handler core.Handler)
	Start() error
	Stop(ctx context.Context) error
}

type signalNotifier func(chan<- os.Signal, ...os.Signal)

type runtimeDeps struct {
	newStore      func(path string) (core.Store, error)
	newScheduler  func(store core.Store, retryPolicy core.RetryPolicy, eventBus *core.EventBus) schedulerAPI
	newServer     func(scheduler schedulerAPI, store core.Store, port string) (serverAPI, error)
	notifySignals signalNotifier
	timeout       time.Duration
	logger        *log.Logger
}

func defaultRuntimeDeps() runtimeDeps {
	return runtimeDeps{
		newStore: func(path string) (core.Store, error) {
			return core.NewJSONFileStore(path)
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
		timeout:       5 * time.Second,
		logger:        log.Default(),
	}
}

func main() {
	if err := run(defaultRuntimeDeps()); err != nil {
		log.Fatal(err)
	}
}

func run(deps runtimeDeps) error {
	if deps.newStore == nil || deps.newScheduler == nil || deps.newServer == nil || deps.notifySignals == nil {
		return fmt.Errorf("runtime dependencies are incomplete")
	}
	if deps.timeout <= 0 {
		deps.timeout = 5 * time.Second
	}
	if deps.logger == nil {
		deps.logger = log.Default()
	}

	store, err := deps.newStore(defaultDataPath)
	if err != nil {
		return err
	}

	scheduler := deps.newScheduler(store, &core.ExponentialBackoffRetry{
		MaxDelay: 30 * time.Minute,
	}, nil)

	server, err := deps.newServer(scheduler, store, defaultPort)
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
