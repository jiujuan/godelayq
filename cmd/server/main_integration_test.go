package main

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"godelayq/core"
)

func TestRun_RegistersHandlersStartsAndStops(t *testing.T) {
	store := newStubStore()
	scheduler := newSpyScheduler()
	server := newFakeServer()
	server.onRegister = scheduler.RegisterHandler
	var logs strings.Builder

	deps := runtimeDeps{
		newStore: func(path string) (core.Store, error) {
			if path != defaultDataPath {
				t.Fatalf("expected default path %q, got %q", defaultDataPath, path)
			}
			return store, nil
		},
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
			if backoff.MaxDelay != 30*time.Minute {
				t.Fatalf("expected max delay 30m, got %v", backoff.MaxDelay)
			}
			return scheduler
		},
		newServer: func(gotScheduler schedulerAPI, gotStore core.Store, port string) (serverAPI, error) {
			if gotScheduler != scheduler {
				t.Fatalf("expected server to receive scheduler stub, got %T", gotScheduler)
			}
			if gotStore != store {
				t.Fatal("expected server to receive created store")
			}
			if port != defaultPort {
				t.Fatalf("expected default port %q, got %q", defaultPort, port)
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
		logger:  log.New(&logs, "", 0),
	}

	if err := run(deps); err != nil {
		t.Fatalf("expected run to succeed, got %v", err)
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

	logText := logs.String()
	if !strings.Contains(logText, "Shutting down server...") {
		t.Fatalf("expected shutdown log, got %q", logText)
	}
	if !strings.Contains(logText, "Server exited") {
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
		newStore: func(path string) (core.Store, error) { return store, nil },
		newScheduler: func(store core.Store, retryPolicy core.RetryPolicy, eventBus *core.EventBus) schedulerAPI {
			return scheduler
		},
		newServer: func(gotScheduler schedulerAPI, store core.Store, port string) (serverAPI, error) {
			return server, nil
		},
		notifySignals: func(ch chan<- os.Signal, sig ...os.Signal) {},
		timeout:       5 * time.Second,
		logger:        log.New(io.Discard, "", 0),
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
		newStore: func(path string) (core.Store, error) { return nil, wantErr },
		newScheduler: func(store core.Store, retryPolicy core.RetryPolicy, eventBus *core.EventBus) schedulerAPI {
			t.Fatal("scheduler should not be created when store creation fails")
			return nil
		},
		newServer: func(gotScheduler schedulerAPI, store core.Store, port string) (serverAPI, error) {
			t.Fatal("server should not be created when store creation fails")
			return nil, nil
		},
		notifySignals: func(ch chan<- os.Signal, sig ...os.Signal) {},
		logger:        log.New(io.Discard, "", 0),
	})

	if !errors.Is(err, wantErr) {
		t.Fatalf("expected store creation error, got %v", err)
	}
}

func TestRun_WithIncompleteDependencies(t *testing.T) {
	err := run(runtimeDeps{})
	if err == nil || !strings.Contains(err.Error(), "runtime dependencies are incomplete") {
		t.Fatalf("expected incomplete dependency error, got %v", err)
	}
}

func TestRun_ServerStopErrorIsLoggedAndIgnored(t *testing.T) {
	store := newStubStore()
	scheduler := newSpyScheduler()
	server := newFakeServer()
	server.stopErr = errors.New("shutdown failed")
	var logs strings.Builder

	err := run(runtimeDeps{
		newStore: func(path string) (core.Store, error) { return store, nil },
		newScheduler: func(store core.Store, retryPolicy core.RetryPolicy, eventBus *core.EventBus) schedulerAPI {
			return scheduler
		},
		newServer: func(gotScheduler schedulerAPI, store core.Store, port string) (serverAPI, error) {
			return server, nil
		},
		notifySignals: func(ch chan<- os.Signal, sig ...os.Signal) {
			go func() {
				ch <- syscall.SIGINT
			}()
		},
		timeout: 20 * time.Millisecond,
		logger:  log.New(&logs, "", 0),
	})

	if err != nil {
		t.Fatalf("expected run to ignore stop error, got %v", err)
	}
	if !strings.Contains(logs.String(), "Server forced to shutdown: shutdown failed") {
		t.Fatalf("expected stop error log, got %q", logs.String())
	}
}

func TestRun_UsesDefaultTimeoutAndLoggerWhenUnset(t *testing.T) {
	store := newStubStore()
	scheduler := newSpyScheduler()
	server := newFakeServer()

	err := run(runtimeDeps{
		newStore: func(path string) (core.Store, error) { return store, nil },
		newScheduler: func(store core.Store, retryPolicy core.RetryPolicy, eventBus *core.EventBus) schedulerAPI {
			return scheduler
		},
		newServer: func(gotScheduler schedulerAPI, store core.Store, port string) (serverAPI, error) {
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
		newStore: func(path string) (core.Store, error) { return store, nil },
		newScheduler: func(store core.Store, retryPolicy core.RetryPolicy, eventBus *core.EventBus) schedulerAPI {
			return scheduler
		},
		newServer: func(gotScheduler schedulerAPI, store core.Store, port string) (serverAPI, error) {
			return nil, wantErr
		},
		notifySignals: func(ch chan<- os.Signal, sig ...os.Signal) {},
		logger:        log.New(io.Discard, "", 0),
	})

	if !errors.Is(err, wantErr) {
		t.Fatalf("expected server factory error, got %v", err)
	}
	if scheduler.startCalls != 0 {
		t.Fatalf("expected scheduler not to start, got %d", scheduler.startCalls)
	}
}

type spyScheduler struct {
	mu         sync.Mutex
	startCalls int
	stopCalls  int
	registered map[string]core.Handler
}

func newSpyScheduler() *spyScheduler {
	return &spyScheduler{
		registered: make(map[string]core.Handler),
	}
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

type stubStore struct{}

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
