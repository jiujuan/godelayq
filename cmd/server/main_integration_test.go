package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"godelayq/core"
	"godelayq/executor"
)

func TestRun_RegistersHandlersStartsAndStops(t *testing.T) {
	store := newStubStore()
	scheduler := newSpyScheduler()
	server := newFakeServer()
	server.onRegister = scheduler.RegisterHandler
	var logs strings.Builder
	executors := scriptRegistry(t, "smoke")
	registryCalls := 0

	cfg := core.Config{}
	cfg.Server.Port = "9999"
	cfg.Scheduler.Workers = 7
	cfg.Scheduler.QueueCapacity = 3
	cfg.Scheduler.MaxRetryDelay = 11 * time.Second
	cfg.Scheduler.ShutdownTimeout = 40 * time.Millisecond
	cfg.Store.Type = "json"
	cfg.Store.Path = "custom/jobs.json"
	cfg.Store.FlushInterval = 10 * time.Millisecond

	deps := runtimeDeps{
		config:   cfg,
		newStore: func() (core.Store, error) { return store, nil },
		newScheduler: func(gotStore core.Store, retryPolicy core.RetryPolicy, eventBus *core.EventBus) schedulerAPI {
			if gotStore != store {
				t.Fatal("expected scheduler to receive created store")
			}
			if eventBus != nil {
				t.Fatal("expected nil event bus for default construction")
			}
			backoff, ok := retryPolicy.(*core.ExponentialBackoffRetry)
			if !ok {
				t.Fatalf("expected exponential retry policy, got %T", retryPolicy)
			}
			if backoff.MaxDelay != cfg.Scheduler.MaxRetryDelay {
				t.Fatalf("expected max delay %v, got %v", cfg.Scheduler.MaxRetryDelay, backoff.MaxDelay)
			}
			return scheduler
		},
		newExecutorRegistry: func(gotCfg core.Config, logger *slog.Logger) (*executor.Registry, error) {
			// 登记表拿到的必须是归一化后的配置：档位超时、并发这些缺省值靠它补齐
			if gotCfg.Executors.Concurrency != core.DefaultExecConcurrency {
				t.Fatalf("expected normalized executor defaults, got concurrency=%d", gotCfg.Executors.Concurrency)
			}
			if gotCfg.Server.Port != cfg.Server.Port {
				t.Fatalf("expected configured port, got %q", gotCfg.Server.Port)
			}
			if logger == nil {
				t.Fatal("expected the runtime logger to reach the registry")
			}
			registryCalls++
			return executors, nil
		},
		newServer: func(gotScheduler schedulerAPI, gotStore core.Store, port string, gotExecutors *executor.Registry) (serverAPI, error) {
			if gotScheduler != scheduler {
				t.Fatalf("expected server to receive scheduler stub, got %T", gotScheduler)
			}
			if gotStore != store {
				t.Fatal("expected server to receive created store")
			}
			if port != cfg.Server.Port {
				t.Fatalf("expected configured port %q, got %q", cfg.Server.Port, port)
			}
			if gotExecutors != executors {
				t.Fatal("expected server to receive the executor registry")
			}
			return server, nil
		},
		notifySignals: func(ch chan<- os.Signal, sig ...os.Signal) {
			if len(sig) != 2 || sig[0] != syscall.SIGINT || sig[1] != syscall.SIGTERM {
				t.Fatalf("unexpected signals: %v", sig)
			}
			go func() {
				ch <- syscall.SIGTERM
			}()
		},
		timeout: 40 * time.Millisecond,
		logger:  newCaptureLogger(&logs),
	}

	if err := run(deps); err != nil {
		t.Fatalf("expected run to succeed, got %v", err)
	}

	if registryCalls != 1 {
		t.Fatalf("expected the executor registry to be built once, got %d", registryCalls)
	}

	if scheduler.concurrency != cfg.Scheduler.Workers || scheduler.queueCapacity != cfg.Scheduler.QueueCapacity {
		t.Fatalf("expected config to reach scheduler, got workers=%d queue=%d",
			scheduler.concurrency, scheduler.queueCapacity)
	}

	if scheduler.startCalls != 1 {
		t.Fatalf("expected scheduler start once, got %d", scheduler.startCalls)
	}
	if scheduler.stopCalls != 1 {
		t.Fatalf("expected scheduler stop once, got %d", scheduler.stopCalls)
	}
	if server.startCalls != 1 {
		t.Fatalf("expected server start once, got %d", server.startCalls)
	}
	if server.stopCalls != 1 {
		t.Fatalf("expected server stop once, got %d", server.stopCalls)
	}

	expected := []string{"payment_check", "email_send", "data_sync", "report_generate"}
	for _, name := range expected {
		if _, ok := server.registered[name]; !ok {
			t.Fatalf("expected handler %q to be registered", name)
		}
		if _, ok := scheduler.registered[name]; !ok {
			t.Fatalf("expected scheduler handler %q to be registered", name)
		}
	}
	// 档位走调度器而不是 api.Server，因此只出现在后者那张表里
	if _, ok := scheduler.registered["exec.smoke"]; !ok {
		t.Fatalf("expected executor handler to be registered, got %v", scheduler.registeredKeys())
	}
	if _, ok := server.registered["exec.smoke"]; ok {
		t.Fatal("did not expect executor handlers to go through RegisterJobHandler")
	}
	if len(scheduler.registered) != len(expected)+1 {
		t.Fatalf("expected %d scheduler handlers, got %v", len(expected)+1, scheduler.registeredKeys())
	}

	logText := logs.String()
	if !strings.Contains(logText, `msg="shutting down server"`) {
		t.Fatalf("expected shutdown log, got %q", logText)
	}
	if !strings.Contains(logText, `msg="server exited"`) {
		t.Fatalf("expected exit log, got %q", logText)
	}
	if server.stopCtx == nil {
		t.Fatal("expected stop context to be passed")
	}
	if _, ok := server.stopCtx.Deadline(); !ok {
		t.Fatal("expected stop context to have deadline")
	}
}

func TestRun_ServerStartFailureStopsScheduler(t *testing.T) {
	store := newStubStore()
	scheduler := newSpyScheduler()
	server := newFakeServer()
	server.startErr = errors.New("listen failed")

	err := run(runtimeDeps{
		newStore: func() (core.Store, error) { return store, nil },
		newScheduler: func(store core.Store, retryPolicy core.RetryPolicy, eventBus *core.EventBus) schedulerAPI {
			return scheduler
		},
		newExecutorRegistry: staticExecutorRegistry(nil, nil),
		newServer: func(gotScheduler schedulerAPI, store core.Store, port string, executors *executor.Registry) (serverAPI, error) {
			return server, nil
		},
		notifySignals: func(ch chan<- os.Signal, sig ...os.Signal) {},
		timeout:       5 * time.Second,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	if !errors.Is(err, server.startErr) {
		t.Fatalf("expected start error, got %v", err)
	}
	if scheduler.startCalls != 1 {
		t.Fatalf("expected scheduler start once, got %d", scheduler.startCalls)
	}
	if scheduler.stopCalls != 1 {
		t.Fatalf("expected scheduler stop after start failure, got %d", scheduler.stopCalls)
	}
	if server.stopCalls != 0 {
		t.Fatalf("expected server stop not to be called, got %d", server.stopCalls)
	}
}

func TestRun_StoreCreationFailure(t *testing.T) {
	wantErr := errors.New("store create failed")

	err := run(runtimeDeps{
		newStore: func() (core.Store, error) { return nil, wantErr },
		newScheduler: func(store core.Store, retryPolicy core.RetryPolicy, eventBus *core.EventBus) schedulerAPI {
			t.Fatal("scheduler should not be created when store creation fails")
			return nil
		},
		newExecutorRegistry: func(core.Config, *slog.Logger) (*executor.Registry, error) {
			t.Fatal("registry should not be built when store creation fails")
			return nil, nil
		},
		newServer: func(gotScheduler schedulerAPI, store core.Store, port string, executors *executor.Registry) (serverAPI, error) {
			t.Fatal("server should not be created when store creation fails")
			return nil, nil
		},
		notifySignals: func(ch chan<- os.Signal, sig ...os.Signal) {},
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	if !errors.Is(err, wantErr) {
		t.Fatalf("expected store creation error, got %v", err)
	}
}

// TestRun_ExecutorRegistryError 固定住档位配置错误的后果：非法配置（越界路径、
// 引用未声明的参数）在建表时就失败，进程不启动，也不会留下没关掉的存储。
func TestRun_ExecutorRegistryError(t *testing.T) {
	store := newStubStore()
	scheduler := newSpyScheduler()
	server := newFakeServer()
	wantErr := errors.New("executors.commands[0]: script path escapes workspace")

	err := run(runtimeDeps{
		newStore: func() (core.Store, error) { return store, nil },
		newScheduler: func(store core.Store, retryPolicy core.RetryPolicy, eventBus *core.EventBus) schedulerAPI {
			return scheduler
		},
		newExecutorRegistry: staticExecutorRegistry(nil, wantErr),
		newServer: func(gotScheduler schedulerAPI, store core.Store, port string, executors *executor.Registry) (serverAPI, error) {
			t.Fatal("server should not be created when the registry fails")
			return nil, nil
		},
		notifySignals: func(ch chan<- os.Signal, sig ...os.Signal) {},
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	if !errors.Is(err, wantErr) {
		t.Fatalf("expected registry error, got %v", err)
	}
	if scheduler.startCalls != 0 {
		t.Fatalf("expected scheduler not to start, got %d", scheduler.startCalls)
	}
	if len(server.registered) != 0 {
		t.Fatal("expected no handler to be registered")
	}
	if store.closeCalls != 1 {
		t.Fatalf("expected store close once, got %d", store.closeCalls)
	}
}

// TestRun_LogsErrorWhenAuthDisabled 是本任务给运维的提示：执行器能跑机器上的程序，
// 接口却没有鉴权就等于把执行能力开放给任何来源，因此启动日志必须留下 error 级记录。
func TestRun_LogsErrorWhenAuthDisabled(t *testing.T) {
	store := newStubStore()
	scheduler := newSpyScheduler()
	server := newFakeServer()
	var logs strings.Builder

	cfg := core.DefaultConfig()
	cfg.Executors.Enabled = true

	err := run(runtimeDeps{
		config:              cfg,
		newStore:            func() (core.Store, error) { return store, nil },
		newScheduler:        func(core.Store, core.RetryPolicy, *core.EventBus) schedulerAPI { return scheduler },
		newExecutorRegistry: executor.NewRegistry,
		newServer: func(schedulerAPI, core.Store, string, *executor.Registry) (serverAPI, error) {
			return server, nil
		},
		notifySignals: func(ch chan<- os.Signal, sig ...os.Signal) {
			go func() {
				ch <- syscall.SIGTERM
			}()
		},
		timeout: 20 * time.Millisecond,
		logger:  newCaptureLogger(&logs),
	})

	if err != nil {
		t.Fatalf("expected run to succeed, got %v", err)
	}
	output := logs.String()
	if !strings.Contains(output, `level=ERROR`) ||
		!strings.Contains(output, "executors are enabled while server authentication is disabled") {
		t.Fatalf("expected an error-level banner, got %q", output)
	}
	// 只记日志不拦启动：生产环境的漏配由部署检查负责，测试环境要能开着执行器跑
	if !strings.Contains(output, `msg="server exited"`) {
		t.Fatalf("expected the process to reach the normal exit, got %q", output)
	}
	// 开了开关却没声明档位，是同一条链路上另一个常见错法，也要留在日志里
	if !strings.Contains(output, "executors are enabled but no profile is declared") {
		t.Fatalf("expected the empty-profile warning, got %q", output)
	}
}

func TestRun_WithIncompleteDependencies(t *testing.T) {
	err := run(runtimeDeps{})
	if err == nil || !strings.Contains(err.Error(), "runtime dependencies are incomplete") {
		t.Fatalf("expected incomplete dependency error, got %v", err)
	}

	// 缺登记表构造器也算不完整：少了它执行器会静默不注册，开关打开也看不出问题
	err = run(runtimeDeps{
		newStore:     func() (core.Store, error) { return newStubStore(), nil },
		newScheduler: func(core.Store, core.RetryPolicy, *core.EventBus) schedulerAPI { return newSpyScheduler() },
		newServer: func(schedulerAPI, core.Store, string, *executor.Registry) (serverAPI, error) {
			return newFakeServer(), nil
		},
		notifySignals: func(chan<- os.Signal, ...os.Signal) {},
	})
	if err == nil || !strings.Contains(err.Error(), "runtime dependencies are incomplete") {
		t.Fatalf("expected missing registry constructor to be reported, got %v", err)
	}
}

func TestRun_ServerStopErrorIsLoggedAndIgnored(t *testing.T) {
	store := newStubStore()
	scheduler := newSpyScheduler()
	server := newFakeServer()
	server.stopErr = errors.New("shutdown failed")
	var logs strings.Builder

	err := run(runtimeDeps{
		newStore: func() (core.Store, error) { return store, nil },
		newScheduler: func(store core.Store, retryPolicy core.RetryPolicy, eventBus *core.EventBus) schedulerAPI {
			return scheduler
		},
		newExecutorRegistry: staticExecutorRegistry(nil, nil),
		newServer: func(gotScheduler schedulerAPI, store core.Store, port string, executors *executor.Registry) (serverAPI, error) {
			return server, nil
		},
		notifySignals: func(ch chan<- os.Signal, sig ...os.Signal) {
			go func() {
				ch <- syscall.SIGINT
			}()
		},
		timeout: 20 * time.Millisecond,
		logger:  newCaptureLogger(&logs),
	})

	if err != nil {
		t.Fatalf("expected run to ignore stop error, got %v", err)
	}
	if !strings.Contains(logs.String(), `msg="server forced to shutdown"`) ||
		!strings.Contains(logs.String(), "shutdown failed") {
		t.Fatalf("expected stop error log, got %q", logs.String())
	}
}

func TestRun_UsesDefaultTimeoutAndLoggerWhenUnset(t *testing.T) {
	store := newStubStore()
	scheduler := newSpyScheduler()
	server := newFakeServer()

	err := run(runtimeDeps{
		newStore: func() (core.Store, error) { return store, nil },
		newScheduler: func(store core.Store, retryPolicy core.RetryPolicy, eventBus *core.EventBus) schedulerAPI {
			return scheduler
		},
		newExecutorRegistry: staticExecutorRegistry(nil, nil),
		newServer: func(gotScheduler schedulerAPI, store core.Store, port string, executors *executor.Registry) (serverAPI, error) {
			return server, nil
		},
		notifySignals: func(ch chan<- os.Signal, sig ...os.Signal) {
			go func() {
				ch <- syscall.SIGTERM
			}()
		},
	})

	if err != nil {
		t.Fatalf("expected run to succeed with defaults, got %v", err)
	}
	if server.stopCtx == nil {
		t.Fatal("expected stop context to be captured")
	}
	deadline, ok := server.stopCtx.Deadline()
	if !ok {
		t.Fatal("expected default deadline to be applied")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > 5*time.Second {
		t.Fatalf("expected remaining deadline within default window, got %v", remaining)
	}
}

func TestRun_ServerFactoryError(t *testing.T) {
	store := newStubStore()
	scheduler := newSpyScheduler()
	wantErr := errors.New("server factory failed")

	err := run(runtimeDeps{
		newStore: func() (core.Store, error) { return store, nil },
		newScheduler: func(store core.Store, retryPolicy core.RetryPolicy, eventBus *core.EventBus) schedulerAPI {
			return scheduler
		},
		newExecutorRegistry: staticExecutorRegistry(nil, nil),
		newServer: func(gotScheduler schedulerAPI, store core.Store, port string, executors *executor.Registry) (serverAPI, error) {
			return nil, wantErr
		},
		notifySignals: func(ch chan<- os.Signal, sig ...os.Signal) {},
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	if !errors.Is(err, wantErr) {
		t.Fatalf("expected server factory error, got %v", err)
	}
	if scheduler.startCalls != 0 {
		t.Fatalf("expected scheduler not to start, got %d", scheduler.startCalls)
	}
}

type spyScheduler struct {
	mu            sync.Mutex
	startCalls    int
	stopCalls     int
	registered    map[string]core.Handler
	concurrency   int
	queueCapacity int
}

func newSpyScheduler() *spyScheduler {
	return &spyScheduler{
		registered: make(map[string]core.Handler),
	}
}

func (s *spyScheduler) SetConcurrency(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.concurrency = n
}

func (s *spyScheduler) SetQueueCapacity(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queueCapacity = n
}

func (s *spyScheduler) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startCalls++
}

func (s *spyScheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopCalls++
}

func (s *spyScheduler) RegisterHandler(jobType string, handler core.Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registered[jobType] = handler
}

func (s *spyScheduler) LookupHandler(jobType string) (core.Handler, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	handler, ok := s.registered[jobType]
	return handler, ok
}

// registeredKeys 按字典序返回已注册的键，让失败信息能直接读出注册表内容。
func (s *spyScheduler) registeredKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	keys := make([]string, 0, len(s.registered))
	for key := range s.registered {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

type fakeServer struct {
	mu         sync.Mutex
	registered map[string]core.Handler
	onRegister func(name string, handler core.Handler)
	startErr   error
	stopErr    error
	startCalls int
	stopCalls  int
	stopCtx    context.Context
}

func newFakeServer() *fakeServer {
	return &fakeServer{
		registered: make(map[string]core.Handler),
	}
}

func (s *fakeServer) RegisterJobHandler(name string, handler core.Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registered[name] = handler
	if s.onRegister != nil {
		s.onRegister(name, handler)
	}
}

func (s *fakeServer) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startCalls++
	return s.startErr
}

func (s *fakeServer) Stop(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopCalls++
	s.stopCtx = ctx
	return s.stopErr
}

// stubStore 记录 Close 次数：早退路径（登记表建不起来、服务起不来）也必须收尾，
// 这条断言固定住"存储一定被关掉"，防止装配代码插入 return 时漏掉 defer。
type stubStore struct {
	mu         sync.Mutex
	closeCalls int
}

func newStubStore() *stubStore {
	return &stubStore{}
}

func (s *stubStore) Save(job *core.Job) error {
	return nil
}

func (s *stubStore) Update(snapshot core.JobSnapshot) error {
	return nil
}

func (s *stubStore) Delete(jobID string) error {
	return nil
}

func (s *stubStore) LoadAll() ([]core.JobSnapshot, error) {
	return nil, nil
}

func (s *stubStore) Flush() error {
	return nil
}

func (s *stubStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeCalls++
	return nil
}

// staticExecutorRegistry 返回一个给出固定登记表（或固定构造错误）的闭包。
// 登记表传 nil 表示"这次部署没有执行器"，与开关关闭时的空表效果一致。
func staticExecutorRegistry(reg *executor.Registry, err error) func(core.Config, *slog.Logger) (*executor.Registry, error) {
	return func(core.Config, *slog.Logger) (*executor.Registry, error) {
		return reg, err
	}
}

// newCaptureLogger 返回把文本日志写进 sb 的日志器，供断言运行期日志内容
func newCaptureLogger(sb *strings.Builder) *slog.Logger {
	return slog.New(slog.NewTextHandler(sb, nil))
}
