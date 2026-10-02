package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"godelayq/api"
	"godelayq/core"
	"godelayq/executor"
	"godelayq/store/sqlite"
)

func TestRun_RegistersHandlersStartsAndStops(t *testing.T) {
	store := newStubStore()
	scheduler := newSpyScheduler()
	server := newFakeServer()
	server.onRegister = scheduler.RegisterHandler
	var logs strings.Builder
	// 这条用例的运行配置没开执行器，登记表只是被传进来的物件，用不到它那份配置
	_, executors := scriptRegistry(t, "smoke")
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
		// 这条用例的配置没开执行器：产物存储与清理协程都不该被建起来
		newArtifactStore: func(core.Config, *slog.Logger) (*executor.ArtifactStore, error) {
			t.Fatal("artifact store must not be built while executors are disabled")
			return nil, nil
		},
		newObservabilityDB: unopenedObservabilityDB(t),
		newServer: func(gotScheduler schedulerAPI, gotStore core.Store, port string, gotExecutors *executor.Registry, gotArtifacts *executor.ArtifactStore, gotObs *observabilityAPI, _ *profileStoreAPI) (serverAPI, error) {
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
			// 执行器关闭时产物存储是 nil：结果端点因此回 503，而不是读一个不存在的目录
			if gotArtifacts != nil {
				t.Fatal("expected no artifact store while executors are disabled")
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
	// 事件里的输出预览上限同样要送到调度器：这份配置没写执行器取值，
	// 于是走 Normalized 补上的默认值，装配不该把它变成 0（0 会让事件里一行摘要都不剩）。
	if scheduler.previewLimit != core.DefaultExecInlinePreview {
		t.Fatalf("expected the preview limit to reach the scheduler, got %d", scheduler.previewLimit)
	}
	// 没开执行器时两个规模都必须是 0：传默认值会让调度器凭空建出一条队列和一组协程，
	// 而这台部署从来没有档位（TASK-E13 §3.7）。
	if scheduler.execConcurrency != 0 || scheduler.execQueueCapacity != 0 {
		t.Fatalf("expected no executor pool, got concurrency=%d queue=%d",
			scheduler.execConcurrency, scheduler.execQueueCapacity)
	}
	// 档位必须以 JobClassExec 注册，否则它会退回共享池，分池隔离等于没做。
	if got := scheduler.classes["exec.smoke"]; got != core.JobClassExec {
		t.Fatalf("expected the profile class to be %d, got %d", core.JobClassExec, got)
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
		newArtifactStore:    staticArtifactStore(nil, nil),
		newObservabilityDB:  unopenedObservabilityDB(t),
		newServer: func(schedulerAPI, core.Store, string, *executor.Registry, *executor.ArtifactStore, *observabilityAPI, *profileStoreAPI) (serverAPI, error) {
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
		newArtifactStore: func(core.Config, *slog.Logger) (*executor.ArtifactStore, error) {
			t.Fatal("artifact store should not be built when store creation fails")
			return nil, nil
		},
		newObservabilityDB: unopenedObservabilityDB(t),
		newServer: func(schedulerAPI, core.Store, string, *executor.Registry, *executor.ArtifactStore, *observabilityAPI, *profileStoreAPI) (serverAPI, error) {
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
		newArtifactStore: func(core.Config, *slog.Logger) (*executor.ArtifactStore, error) {
			t.Fatal("artifact store should not be built when the registry fails")
			return nil, nil
		},
		newObservabilityDB: unopenedObservabilityDB(t),
		newServer: func(schedulerAPI, core.Store, string, *executor.Registry, *executor.ArtifactStore, *observabilityAPI, *profileStoreAPI) (serverAPI, error) {
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
	artifactDir := t.TempDir()

	cfg := core.DefaultConfig()
	cfg.Executors.Enabled = true
	// 产物目录指向临时目录：打开执行器会让 run 真的建一次目录与清理协程
	cfg.Executors.Output.Dir = filepath.Join(artifactDir, "exec")

	err := run(runtimeDeps{
		config:              cfg,
		newStore:            func() (core.Store, error) { return store, nil },
		newScheduler:        func(core.Store, core.RetryPolicy, *core.EventBus) schedulerAPI { return scheduler },
		newExecutorRegistry: executor.NewRegistry,
		newArtifactStore:    artifactStoreFromConfig,
		newObservabilityDB:  unopenedObservabilityDB(t),
		newServer: func(schedulerAPI, core.Store, string, *executor.Registry, *executor.ArtifactStore, *observabilityAPI, *profileStoreAPI) (serverAPI, error) {
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
	if _, err := os.Stat(cfg.Executors.Output.Dir); err != nil {
		t.Fatalf("expected the artifact dir to be created: %v", err)
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

// installedGuard 取装上的崩溃恢复钩子，第二个返回值表示安装时 Start 还没跑过。
func (s *spyScheduler) installedGuard() (core.RestoreGuard, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restoreGuard, s.guardSetBeforeStart
}

// runWithExecutorConfig 用给定配置跑一次完整装配（不起真服务、不建监听），
// 返回替身调度器与它的启动日志。三条守卫装配用例只差在配置取值上，共用这一份骨架。
func runWithExecutorConfig(t *testing.T, tune func(*core.Config)) (*spyScheduler, string) {
	t.Helper()

	store := newStubStore()
	scheduler := newSpyScheduler()
	server := newFakeServer()
	var logs strings.Builder

	cfg := core.DefaultConfig()
	cfg.Executors.Enabled = true
	// 产物目录指向临时目录：打开执行器会让 run 真的建一次目录与清理协程
	cfg.Executors.Output.Dir = filepath.Join(t.TempDir(), "exec")
	if tune != nil {
		tune(&cfg)
	}

	// 档位表用测试程序自身当解释器：探测结论必为可用，用例不依赖本机装了什么语言
	_, executors := scriptRegistry(t, "smoke")

	err := run(runtimeDeps{
		config:              cfg,
		newStore:            func() (core.Store, error) { return store, nil },
		newScheduler:        func(core.Store, core.RetryPolicy, *core.EventBus) schedulerAPI { return scheduler },
		newExecutorRegistry: func(core.Config, *slog.Logger) (*executor.Registry, error) { return executors, nil },
		newArtifactStore:    artifactStoreFromConfig,
		newObservabilityDB:  unopenedObservabilityDB(t),
		newServer: func(schedulerAPI, core.Store, string, *executor.Registry, *executor.ArtifactStore, *observabilityAPI, *profileStoreAPI) (serverAPI, error) {
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
	return scheduler, logs.String()
}

// TestRun_InstallsRestoreGuardWhenPausePolicy 覆盖第 5.2 条：打开执行器且策略为 pause 时装守卫，
// 并且守卫的判定确实是"running 的档位任务停住、其它一律放行"。
// 只断言"装了"是不够的：装配方与调度器之间传的是函数值，装错一个闭包看不出差别。
func TestRun_InstallsRestoreGuardWhenPausePolicy(t *testing.T) {
	scheduler, _ := runWithExecutorConfig(t, func(cfg *core.Config) {
		cfg.Executors.RestorePolicy = "pause"
	})

	guard, beforeStart := scheduler.installedGuard()
	if guard == nil {
		t.Fatal("executors.enabled=true 且 restore_policy=pause 时应装上崩溃恢复守卫")
	}
	if !beforeStart {
		t.Error("守卫必须在 Start 之前装上：Start 的第一步就是 Restore")
	}

	// exec.smoke 由档位注册链路以 JobClassExec 登记（同文件的 E13 断言）
	if status, hold := guard(core.JobSnapshot{ID: "crashed", Type: "exec.smoke", Status: int(core.StatusRunning)}); !hold || status != core.StatusPaused {
		t.Errorf("崩溃时正在跑的档位任务应停在 paused，实际 (%s, %v)", status, hold)
	}
	if _, hold := guard(core.JobSnapshot{ID: "queued", Type: "exec.smoke", Status: int(core.StatusPending)}); hold {
		t.Error("没跑过的档位任务不该被停住")
	}
	if _, hold := guard(core.JobSnapshot{ID: "plain", Type: "payment_check", Status: int(core.StatusRunning)}); hold {
		t.Error("普通任务与本卡无关，照旧自动重跑")
	}
	// 档位被删掉的历史任务查不到类别：放行（重排后会在 executeJob 里因找不到处理函数而失败）
	if _, hold := guard(core.JobSnapshot{ID: "gone", Type: "exec.deleted_profile", Status: int(core.StatusRunning)}); hold {
		t.Error("查不到类别的快照应按不改判处理")
	}
}

// TestRun_NoRestoreGuardOnReplayAndWhenDisabled 覆盖第 5.2 的另一半：
// 策略写成 replay 或干脆没打开执行器时，守卫不装，恢复行为与本卡之前一致。
func TestRun_NoRestoreGuardOnReplayAndWhenDisabled(t *testing.T) {
	replay, _ := runWithExecutorConfig(t, func(cfg *core.Config) {
		cfg.Executors.RestorePolicy = "replay"
	})
	if guard, _ := replay.installedGuard(); guard != nil {
		t.Error("restore_policy=replay 是部署方明确要重跑，不该装守卫")
	}

	// 策略取值带空格时按 pause 处理：配置校验用 TrimSpace 比较，
	// 装配也照样比较，落到"认不出的取值就走安全侧"这条一致的口径上。
	spaced, _ := runWithExecutorConfig(t, func(cfg *core.Config) {
		cfg.Executors.RestorePolicy = " pause "
	})
	if guard, _ := spaced.installedGuard(); guard == nil {
		t.Error("带空格的 pause 仍应装上守卫")
	}

	disabled, logs := runWithExecutorConfig(t, func(cfg *core.Config) {
		cfg.Executors.Enabled = false
	})
	if guard, _ := disabled.installedGuard(); guard != nil {
		t.Error("没打开执行器的进程里没有档位任务，不该装守卫")
	}
	if strings.Contains(logs, "restore guard") {
		t.Errorf("关闭状态下不该出现守卫相关的日志，实际日志：%q", logs)
	}
}

// TestRun_ExecPoolSizingReachesScheduler 钉住配置里两个执行器池取值到调度器的接线：
// 打开执行器后，调度器要按配置建池。数字对不上等于档位规模被静默丢弃，
// 运维在 /admin/runtime 上看到的 worker 数与配置文件就成了两套说法。
func TestRun_ExecPoolSizingReachesScheduler(t *testing.T) {
	store := newStubStore()
	scheduler := newSpyScheduler()
	server := newFakeServer()
	artifactDir := t.TempDir()

	cfg := core.DefaultConfig()
	cfg.Executors.Enabled = true
	cfg.Executors.Concurrency = 5
	cfg.Executors.QueueCapacity = 9
	cfg.Executors.Output.Dir = filepath.Join(artifactDir, "exec")

	err := run(runtimeDeps{
		config:              cfg,
		newStore:            func() (core.Store, error) { return store, nil },
		newScheduler:        func(core.Store, core.RetryPolicy, *core.EventBus) schedulerAPI { return scheduler },
		newExecutorRegistry: executor.NewRegistry,
		newArtifactStore:    artifactStoreFromConfig,
		newObservabilityDB:  unopenedObservabilityDB(t),
		newServer: func(schedulerAPI, core.Store, string, *executor.Registry, *executor.ArtifactStore, *observabilityAPI, *profileStoreAPI) (serverAPI, error) {
			return server, nil
		},
		notifySignals: func(ch chan<- os.Signal, sig ...os.Signal) {
			go func() {
				ch <- syscall.SIGTERM
			}()
		},
		timeout: 20 * time.Millisecond,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	if err != nil {
		t.Fatalf("expected run to succeed, got %v", err)
	}
	if scheduler.execConcurrency != 5 || scheduler.execQueueCapacity != 9 {
		t.Fatalf("expected the configured pool size to reach the scheduler, got concurrency=%d queue=%d",
			scheduler.execConcurrency, scheduler.execQueueCapacity)
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
		newServer: func(schedulerAPI, core.Store, string, *executor.Registry, *executor.ArtifactStore, *observabilityAPI, *profileStoreAPI) (serverAPI, error) {
			return newFakeServer(), nil
		},
		notifySignals: func(chan<- os.Signal, ...os.Signal) {},
	})
	if err == nil || !strings.Contains(err.Error(), "runtime dependencies are incomplete") {
		t.Fatalf("expected missing registry constructor to be reported, got %v", err)
	}

	// 缺产物存储构造器同样是不完整：打开了执行器却没有输出落点，
	// 静默跳过会让人以为 data/exec 空目录是部署问题而不是装配缺失
	err = run(runtimeDeps{
		newStore:            func() (core.Store, error) { return newStubStore(), nil },
		newScheduler:        func(core.Store, core.RetryPolicy, *core.EventBus) schedulerAPI { return newSpyScheduler() },
		newExecutorRegistry: staticExecutorRegistry(nil, nil),
		newServer: func(schedulerAPI, core.Store, string, *executor.Registry, *executor.ArtifactStore, *observabilityAPI, *profileStoreAPI) (serverAPI, error) {
			return newFakeServer(), nil
		},
		notifySignals: func(chan<- os.Signal, ...os.Signal) {},
	})
	if err == nil || !strings.Contains(err.Error(), "runtime dependencies are incomplete") {
		t.Fatalf("expected missing artifact store constructor to be reported, got %v", err)
	}
}

// TestRun_ArtifactStoreErrorStopsStartup 与登记表失败同理：输出落点建不起来就别启动。
func TestRun_ArtifactStoreErrorStopsStartup(t *testing.T) {
	store := newStubStore()
	scheduler := newSpyScheduler()
	wantErr := errors.New("artifact store: create dir: permission denied")

	cfg := core.DefaultConfig()
	cfg.Executors.Enabled = true
	cfg.Executors.Output.Dir = filepath.Join(t.TempDir(), "exec")

	err := run(runtimeDeps{
		config:              cfg,
		newStore:            func() (core.Store, error) { return store, nil },
		newScheduler:        func(core.Store, core.RetryPolicy, *core.EventBus) schedulerAPI { return scheduler },
		newExecutorRegistry: executor.NewRegistry,
		newArtifactStore:    staticArtifactStore(nil, wantErr),
		newObservabilityDB:  unopenedObservabilityDB(t),
		newServer: func(schedulerAPI, core.Store, string, *executor.Registry, *executor.ArtifactStore, *observabilityAPI, *profileStoreAPI) (serverAPI, error) {
			t.Fatal("server should not be created when the artifact store fails")
			return nil, nil
		},
		notifySignals: func(chan<- os.Signal, ...os.Signal) {},
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	if !errors.Is(err, wantErr) {
		t.Fatalf("expected the artifact store error, got %v", err)
	}
	if scheduler.startCalls != 0 {
		t.Fatalf("expected scheduler not to start, got %d", scheduler.startCalls)
	}
	if store.closeCalls != 1 {
		t.Fatalf("expected store close once, got %d", store.closeCalls)
	}
}

// TestRun_ArtifactCleanerStopsWithRun 钉住关闭顺序：run 返回时清理协程必须已经退出，
// 否则它会在存储关闭之后再读一次任务集合。这里用真实存储实现建协程，
// 再用 done 通道确认收尾（不靠 sleep 猜时间）。
func TestRun_ArtifactCleanerStopsWithRun(t *testing.T) {
	store := newStubStore()
	scheduler := newSpyScheduler()
	server := newFakeServer()
	dir := filepath.Join(t.TempDir(), "exec")

	cfg := core.DefaultConfig()
	cfg.Executors.Enabled = true
	cfg.Executors.Output.Dir = dir

	var stores []*executor.ArtifactStore
	err := run(runtimeDeps{
		config:              cfg,
		newStore:            func() (core.Store, error) { return store, nil },
		newScheduler:        func(core.Store, core.RetryPolicy, *core.EventBus) schedulerAPI { return scheduler },
		newExecutorRegistry: executor.NewRegistry,
		newArtifactStore: func(core.Config, *slog.Logger) (*executor.ArtifactStore, error) {
			artifacts, err := artifactStoreFromConfig(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				return nil, err
			}
			stores = append(stores, artifacts)
			return artifacts, nil
		},
		newObservabilityDB: unopenedObservabilityDB(t),
		newServer: func(_ schedulerAPI, _ core.Store, _ string, _ *executor.Registry, gotArtifacts *executor.ArtifactStore, _ *observabilityAPI, _ *profileStoreAPI) (serverAPI, error) {
			// 接口拿到的必须就是 run 里那一份：换成新建的存储会读不到刚写出的产物
			if len(stores) != 1 || gotArtifacts != stores[0] {
				t.Errorf("expected the server to receive the built artifact store, got %p of %v", gotArtifacts, stores)
			}
			return server, nil
		},
		notifySignals: func(ch chan<- os.Signal, sig ...os.Signal) {
			go func() {
				ch <- syscall.SIGTERM
			}()
		},
		timeout: 20 * time.Millisecond,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	if err != nil {
		t.Fatalf("expected run to succeed, got %v", err)
	}
	if len(stores) != 1 {
		t.Fatalf("expected one artifact store, got %d", len(stores))
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("expected the artifact dir to exist: %v", err)
	}
	// run 能返回本身就说明清理协程已经退出：协程没停的话，收尾的 <-cleanerStopped 会一直阻塞
}

// unopenedObservabilityDB 是给"配置没开观测层"的用例用的闭包：一旦被调用就让测试失败。
// 写法与产物存储那条（executors 关闭时不该建存储）一致，守的是"默认关闭 = 不创建任何文件"。
func unopenedObservabilityDB(t *testing.T) func(core.Config, *slog.Logger) (observabilityDB, error) {
	return func(core.Config, *slog.Logger) (observabilityDB, error) {
		t.Fatal("observability database must not be opened while observability.enabled is false")
		return nil, nil
	}
}

// observabilityStub 是观测库句柄的替身。真实 *sqlite.DB 的关闭时机在测试里没有旁路可看，
// 而"观测层先关、存储后关"正是本卡要钉住的顺序（设计文档 §7.3），所以 run 拿的是接口。
type observabilityStub struct {
	path       string
	journal    string
	stats      sqlite.Stats
	statsErr   error
	closeErr   error
	statsCalls int
	closeCalls int
	// order 非空时把关闭动作记进共享的顺序表（与 stubStore.order 同一张表）。
	order *[]string
}

func (s *observabilityStub) Path() string { return s.path }

func (s *observabilityStub) JournalMode() string {
	if s.journal != "" {
		return s.journal
	}
	return "wal"
}

func (s *observabilityStub) Stats() (sqlite.Stats, error) {
	s.statsCalls++
	return s.stats, s.statsErr
}

func (s *observabilityStub) Close() error {
	s.closeCalls++
	if s.order != nil {
		*s.order = append(*s.order, "observability")
	}
	return s.closeErr
}

// eventLogStub 是事件写入器的替身：它只回答"关了几次、什么时候关的、丢了多少"，
// 以及"是不是同一个实例被交给了服务"。真实写入器的读写行为在 store/sqlite 与 api
// 两侧各有用例，这里守的是装配与关停顺序。
type eventLogStub struct {
	closeCalls int
	dropped    int64
	closeErr   error
	order      *[]string
}

func (s *eventLogStub) Close() error {
	s.closeCalls++
	if s.order != nil {
		*s.order = append(*s.order, "event_log")
	}
	return s.closeErr
}

func (s *eventLogStub) Dropped() int64 { return s.dropped }

// Events 与 Recent 只为满足 eventLogAPI 的读取面：装配用例断言的是实例交接，
// 不是查询结果。真读库的内容由 api 的替身用例与 store/sqlite 的用例覆盖。
func (s *eventLogStub) Events(string, int) ([]core.Event, error) { return nil, nil }

func (s *eventLogStub) Recent(int) ([]core.Event, error) { return nil, nil }

// auditLogStub 是台账写入器的替身：装配用例只关心"交出去的是不是同一个实例"、
// 关了几次、什么时候关的。真实的写入与查询在 store/sqlite 有用例。
type auditLogStub struct {
	closeCalls int
	dropped    int64
	closeErr   error
	order      *[]string
}

func (s *auditLogStub) Close() error {
	s.closeCalls++
	if s.order != nil {
		*s.order = append(*s.order, "audit_log")
	}
	return s.closeErr
}

func (s *auditLogStub) Dropped() int64 { return s.dropped }

// Append 与 Query 只为满足 auditLogAPI 的读写面：装配断言的是实例交接。
func (s *auditLogStub) Append(api.AuditEntry) error { return nil }

func (s *auditLogStub) Query(api.AuditFilter) ([]api.AuditEntry, int, error) { return nil, 0, nil }

// observabilityCase 是观测层装配用例共用的输入：配置改法、各闭包的返回值、
// "在 newServer 那一刻能拿到总线"的钩子（那时事件写入器已经挂上，发出去的事件会走完整链路），
// 以及 newServer 实际收到的观测层两面（未启用时应当是 nil）。
type observabilityCase struct {
	tune      func(*core.Config)
	newDB     func(core.Config, *slog.Logger) (observabilityDB, error)
	newLog    func(*core.EventBus, observabilityDB, core.ObservabilityConfig, *slog.Logger) (eventLogAPI, error)
	newAudit  func(observabilityDB, core.ObservabilityConfig, *slog.Logger) (auditLogAPI, error)
	onServer  func(*core.EventBus)
	gotEvents func(eventLogAPI)
	gotAudit  func(auditLogAPI)
}

// runWithObservability 用一份观测层配置跑一次完整装配（不起真服务），返回三个替身、
// 替身存储、共享的关闭顺序表与启动日志。
func runWithObservability(t *testing.T, tc observabilityCase) (*observabilityStub, *eventLogStub, *stubStore, []string, string) {
	t.Helper()

	var order []string
	store := &stubStore{order: &order}
	scheduler := newSpyScheduler()
	server := newFakeServer()
	var logs strings.Builder

	cfg := core.DefaultConfig()
	if tc.tune != nil {
		tc.tune(&cfg)
	}

	stub := &observabilityStub{path: cfg.Observability.Path, order: &order,
		stats: sqlite.Stats{SchemaVersion: 1}}
	logStub := &eventLogStub{order: &order}
	auditStub := &auditLogStub{order: &order}
	// 闭包由用例决定返回替身还是报错；传 nil 就用上面两份替身
	newDB := tc.newDB
	if newDB == nil {
		newDB = func(core.Config, *slog.Logger) (observabilityDB, error) { return stub, nil }
	}
	newLog := tc.newLog
	if newLog == nil {
		newLog = func(*core.EventBus, observabilityDB, core.ObservabilityConfig, *slog.Logger) (eventLogAPI, error) {
			return logStub, nil
		}
	}
	newAudit := tc.newAudit
	if newAudit == nil {
		newAudit = func(observabilityDB, core.ObservabilityConfig, *slog.Logger) (auditLogAPI, error) {
			return auditStub, nil
		}
	}

	err := run(runtimeDeps{
		config:              cfg,
		newStore:            func() (core.Store, error) { return store, nil },
		newScheduler:        func(core.Store, core.RetryPolicy, *core.EventBus) schedulerAPI { return scheduler },
		newExecutorRegistry: staticExecutorRegistry(nil, nil),
		newArtifactStore:    staticArtifactStore(nil, nil),
		newObservabilityDB:  newDB,
		newEventLog:         newLog,
		newAuditLog:         newAudit,
		newServer: func(_ schedulerAPI, _ core.Store, _ string, _ *executor.Registry, _ *executor.ArtifactStore, gotObs *observabilityAPI, _ *profileStoreAPI) (serverAPI, error) {
			// 装配已经走到"观测层挂好、服务还没起"这一步，总线此刻可用
			if tc.onServer != nil {
				tc.onServer(scheduler.GetEventBus())
			}
			if tc.gotEvents != nil {
				var events eventLogAPI
				if gotObs != nil {
					events = gotObs.events
				}
				tc.gotEvents(events)
			}
			if tc.gotAudit != nil {
				var audit auditLogAPI
				if gotObs != nil {
					audit = gotObs.audit
				}
				tc.gotAudit(audit)
			}
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
	return stub, logStub, store, append([]string(nil), order...), logs.String()
}

// TestRun_ObservabilityDisabledOpensNothing 是本卡的第一条 DoD：
// 默认配置（observability.enabled=false）下闭包一次都不调用，配置的路径上也不出现文件。
// 只看"没报错"不够——静默打开一个库同样会被当成"这次部署启用了观测层"。
func TestRun_ObservabilityDisabledOpensNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "observe.sqlite")

	var logs strings.Builder
	store := newStubStore()
	calls := 0
	err := run(runtimeDeps{
		config: func() core.Config {
			cfg := core.DefaultConfig()
			cfg.Observability.Path = path // 总开关保持默认的 false
			return cfg
		}(),
		newStore:            func() (core.Store, error) { return store, nil },
		newScheduler:        func(core.Store, core.RetryPolicy, *core.EventBus) schedulerAPI { return newSpyScheduler() },
		newExecutorRegistry: staticExecutorRegistry(nil, nil),
		newArtifactStore:    staticArtifactStore(nil, nil),
		newObservabilityDB: func(core.Config, *slog.Logger) (observabilityDB, error) {
			calls++
			return nil, nil
		},
		newServer: func(schedulerAPI, core.Store, string, *executor.Registry, *executor.ArtifactStore, *observabilityAPI, *profileStoreAPI) (serverAPI, error) {
			return newFakeServer(), nil
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

	if calls != 0 {
		t.Fatalf("expected the observability closure not to be called, got %d calls", calls)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected no database file at %s, stat said %v", path, err)
	}
	// 目录本身也不该被顺手建出来：run 用的是 t.TempDir()，所以只看日志与文件
	if output := logs.String(); strings.Contains(output, "observability enabled") {
		t.Fatalf("the disabled path must not log an enabled observability layer, got %q", output)
	}
	if store.closeCalls != 1 {
		t.Fatalf("expected the store to still be closed once, got %d", store.closeCalls)
	}
}

// TestRun_ObservabilityOpenFailureStopsStartup 固定住"建不起库就别启动"：
// 带着三张永远为空的表上线，比启动失败更难被发现（设计文档 §11）。
func TestRun_ObservabilityOpenFailureStopsStartup(t *testing.T) {
	store := newStubStore()
	scheduler := newSpyScheduler()
	wantErr := errors.New("sqlite: unable to open database file: permission denied")

	cfg := core.DefaultConfig()
	cfg.Observability.Enabled = true
	cfg.Observability.Path = filepath.Join(t.TempDir(), "observe.sqlite")

	err := run(runtimeDeps{
		config:              cfg,
		newStore:            func() (core.Store, error) { return store, nil },
		newScheduler:        func(core.Store, core.RetryPolicy, *core.EventBus) schedulerAPI { return scheduler },
		newExecutorRegistry: staticExecutorRegistry(nil, nil),
		newArtifactStore:    staticArtifactStore(nil, nil),
		newObservabilityDB: func(core.Config, *slog.Logger) (observabilityDB, error) {
			return nil, wantErr
		},
		newAuditLog: func(observabilityDB, core.ObservabilityConfig, *slog.Logger) (auditLogAPI, error) {
			return &auditLogStub{}, nil
		},
		newEventLog: func(*core.EventBus, observabilityDB, core.ObservabilityConfig, *slog.Logger) (eventLogAPI, error) {
			t.Fatal("the event writer should not be constructed when the database is unavailable")
			return nil, nil
		},
		newServer: func(schedulerAPI, core.Store, string, *executor.Registry, *executor.ArtifactStore, *observabilityAPI, *profileStoreAPI) (serverAPI, error) {
			t.Fatal("the server should not be created when the observability database fails")
			return nil, nil
		},
		notifySignals: func(chan<- os.Signal, ...os.Signal) {},
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	if !errors.Is(err, wantErr) {
		t.Fatalf("expected the observability error, got %v", err)
	}
	if scheduler.startCalls != 0 {
		t.Fatalf("expected the scheduler not to start, got %d", scheduler.startCalls)
	}
	// 早退路径也必须收尾存储：它与观测层无关，但 defer 不能因为插入 return 而失效
	if store.closeCalls != 1 {
		t.Fatalf("expected the store to be closed once, got %d", store.closeCalls)
	}
}

// TestRun_ObservabilityStatsFailureStopsStartup 钉住 Open 成功但库读不出版本时的处置：
// 这种库文件多半被外部改坏或权限不对，此时报"不可用"比留下一个空台账有用。
func TestRun_ObservabilityStatsFailureStopsStartup(t *testing.T) {
	store := newStubStore()
	cfg := core.DefaultConfig()
	cfg.Observability.Enabled = true
	path := filepath.Join(t.TempDir(), "observe.sqlite")
	cfg.Observability.Path = path

	stub := &observabilityStub{path: path, statsErr: errors.New("database disk image is malformed")}
	err := run(runtimeDeps{
		config:              cfg,
		newStore:            func() (core.Store, error) { return store, nil },
		newScheduler:        func(core.Store, core.RetryPolicy, *core.EventBus) schedulerAPI { return newSpyScheduler() },
		newExecutorRegistry: staticExecutorRegistry(nil, nil),
		newArtifactStore:    staticArtifactStore(nil, nil),
		newObservabilityDB: func(core.Config, *slog.Logger) (observabilityDB, error) {
			return stub, nil
		},
		newAuditLog: func(observabilityDB, core.ObservabilityConfig, *slog.Logger) (auditLogAPI, error) {
			return &auditLogStub{}, nil
		},
		newEventLog: func(*core.EventBus, observabilityDB, core.ObservabilityConfig, *slog.Logger) (eventLogAPI, error) {
			t.Fatal("the event writer should not be constructed when the database is unusable")
			return nil, nil
		},
		newServer: func(schedulerAPI, core.Store, string, *executor.Registry, *executor.ArtifactStore, *observabilityAPI, *profileStoreAPI) (serverAPI, error) {
			t.Fatal("the server should not be created when the observability database is unusable")
			return nil, nil
		},
		notifySignals: func(chan<- os.Signal, ...os.Signal) {},
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("expected the error to name the unusable database path, got %v", err)
	}
	if store.closeCalls != 1 {
		t.Fatalf("expected the store to be closed once, got %d", store.closeCalls)
	}
	// Stats 失败后句柄仍要关掉：不然这份库文件的连接就一直留在进程里
	if stub.closeCalls != 1 {
		t.Fatalf("expected the failed database handle to be closed, got %d", stub.closeCalls)
	}
}

// TestRun_ObservabilityClosedBeforeStore 是本卡唯一能用断言表达的关闭顺序（设计文档 §7.3）：
// 观测层的 defer 晚于 store.Close 的 defer 声明，因此先执行。顺序颠倒时，
// S03 起的事件写入器会在已关闭的任务存储之上再读一次快照。
func TestRun_ObservabilityClosedBeforeStore(t *testing.T) {
	stub, logStub, store, order, logs := runWithObservability(t, observabilityCase{
		tune: func(cfg *core.Config) {
			cfg.Observability.Enabled = true
			cfg.Observability.Path = filepath.Join(t.TempDir(), "observe.sqlite")
		},
	})

	if stub.closeCalls != 1 {
		t.Fatalf("expected the observability handle to be closed once, got %d", stub.closeCalls)
	}
	if store.closeCalls != 1 {
		t.Fatalf("expected the store to be closed once, got %d", store.closeCalls)
	}
	// 这一条才是关闭顺序断言：两个写入器先落完最后一批、观测库再关连接、任务存储最后关。
	// 只断言"都关过"是不够的——顺序反过来同样能通过，而那时就是往已关闭的连接里写、
	// 或在已关闭的存储之上再读一次快照。
	// 两个写入器之间是审计先于事件（defer 后声明的先执行），这条没有讲究：
	// 需要守住的只有"都在观测库之前"。
	if len(order) != 4 || order[0] != "audit_log" || order[1] != "event_log" ||
		order[2] != "observability" || order[3] != "store" {
		t.Fatalf("expected the close order [audit_log, event_log, observability, store], got %v", order)
	}
	if logStub.closeCalls != 1 {
		t.Fatalf("expected the event writer to be closed once, got %d", logStub.closeCalls)
	}

	// 关观测层时报的 Stats 调用次数固定为 1（启动日志那一行），关闭本身不再查
	if stub.statsCalls != 1 {
		t.Fatalf("expected the startup log to read the schema version once, got %d", stub.statsCalls)
	}

	output := logs
	if !strings.Contains(output, `msg="observability enabled"`) {
		t.Fatalf("expected the observability startup line, got %q", output)
	}
	if !strings.Contains(output, "schema_version=1") || !strings.Contains(output, "journal_mode=wal") {
		t.Fatalf("the startup line must carry the schema version and the effective journal mode, got %q", output)
	}
}

// TestRun_ObservabilityEnabledOpensAndClosesForReal 用真实的 sqlite.Open 走一遍装配：
// 闭包的映射（cfg.Observability → store/sqlite）与 defer 的关闭都是被执行过的那条路径，
// 替身断言不了这条。库文件建出来、启动日志有那一行、run 返回后连接确实已关。
func TestRun_ObservabilityEnabledOpensAndClosesForReal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "observe.sqlite")

	var opened observabilityDB
	_, _, _, _, logs := runWithObservability(t, observabilityCase{
		tune: func(cfg *core.Config) {
			cfg.Observability.Enabled = true
			cfg.Observability.Path = path
		},
		newDB: func(cfg core.Config, logger *slog.Logger) (observabilityDB, error) {
			db, err := sqlite.Open(cfg.Observability, logger)
			if err != nil {
				return nil, err
			}
			opened = db
			return db, nil
		},
		newLog: defaultRuntimeDeps(core.DefaultConfig(), slog.Default()).newEventLog,
		// 装配完成、服务未起：此刻事件写入器已经挂上总线，发两条就该进库
		onServer: func(bus *core.EventBus) {
			bus.Publish(core.Event{Type: core.EventJobScheduled, JobID: "job-real",
				JobName: "demo", Status: core.StatusPending, Timestamp: time.Now()})
			bus.Publish(core.Event{Type: core.EventJobStarted, JobID: "job-real",
				JobName: "demo", Status: core.StatusRunning, Timestamp: time.Now()})
		},
	})

	if opened == nil {
		t.Fatal("expected the real database handle to reach run")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected the database file at the configured path: %v", err)
	}
	// run 返回即观测层已关：关掉之后 Stats 必须报错，而不是还能读出行数
	if _, err := opened.Stats(); err == nil {
		t.Fatal("expected the database to be closed when run returns")
	}
	if !strings.Contains(logs, `msg="observability enabled"`) || !strings.Contains(logs, path) {
		t.Fatalf("expected the startup line to name the path, got %q", logs)
	}
	// 生产闭包接上了真句柄：类型断言那条路径只有在这里被真的走过，
	// 才能确认装配时事件写入器真的挂上了（events_writer=true）
	if !strings.Contains(logs, "events_writer=true") {
		t.Fatalf("expected the event writer to be assembled, got %q", logs)
	}

	// 端到端：从总线发出去的两条事件，经过订阅、队列、批量事务落到库里；
	// 重新打开这份文件还能读到那两行，就是"重启后历史仍在"这条需求的本体。
	again, err := sqlite.Open(sqliteOptionsForTest(path), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("reopening the database failed: %v", err)
	}
	defer again.Close()
	reopened, err := again.Stats()
	if err != nil {
		t.Fatalf("reading the reopened stats failed: %v", err)
	}
	if reopened.Events != 2 {
		t.Fatalf("expected the two published events in the table, got %d", reopened.Events)
	}
	if reopened.SchemaVersion != 1 {
		t.Fatalf("expected schema version 1 to come back unchanged, got %d", reopened.SchemaVersion)
	}
}

// sqliteOptionsForTest 把路径包成一份打开观测库用的配置，其余取值一律用 core 的默认。
// store/sqlite 的用例里有同名的 openConfig，这里是要跨包用，所以放在装配侧的这一份测试里。
func sqliteOptionsForTest(path string) core.ObservabilityConfig {
	cfg := core.DefaultConfig()
	cfg.Observability.Enabled = true
	cfg.Observability.Path = path
	return cfg.Observability
}

// TestRun_EventLogDisabledOpensNoWriter 固定事件子开关关闭时的形态：库照建、表照在，
// 但写入器一个都不构造（闭包被调用即失败），启动日志里 events_writer=false。
func TestRun_EventLogDisabledOpensNoWriter(t *testing.T) {
	_, logStub, store, order, logs := runWithObservability(t, observabilityCase{
		tune: func(cfg *core.Config) {
			cfg.Observability.Enabled = true
			cfg.Observability.Path = filepath.Join(t.TempDir(), "observe.sqlite")
			cfg.Observability.Events.Enabled = false
		},
		newLog: func(*core.EventBus, observabilityDB, core.ObservabilityConfig, *slog.Logger) (eventLogAPI, error) {
			t.Fatal("the event writer must not be built while observability.events.enabled is false")
			return nil, nil
		},
	})

	if logStub.closeCalls != 0 {
		t.Fatalf("no writer was built, so nothing should be closed, got %d", logStub.closeCalls)
	}
	if store.closeCalls != 1 {
		t.Fatalf("expected the store to be closed once, got %d", store.closeCalls)
	}
	if len(order) != 3 || order[0] != "audit_log" || order[1] != "observability" || order[2] != "store" {
		t.Fatalf("expected the close order [audit_log, observability, store], got %v", order)
	}
	if !strings.Contains(logs, "events_writer=false") {
		t.Fatalf("expected the startup line to say the writer is off, got %q", logs)
	}
}

// TestRun_EventLogOpenFailureStopsStartup：写入器建不起来就别启动。
// 这一条与"库建不起来"同口径：静默跳过会让人以为只是这次没跑任务。
func TestRun_EventLogOpenFailureStopsStartup(t *testing.T) {
	store := newStubStore()
	scheduler := newSpyScheduler()
	wantErr := errors.New("event writer: subscribe failed")

	cfg := core.DefaultConfig()
	cfg.Observability.Enabled = true
	cfg.Observability.Path = filepath.Join(t.TempDir(), "observe.sqlite")

	err := run(runtimeDeps{
		config:              cfg,
		newStore:            func() (core.Store, error) { return store, nil },
		newScheduler:        func(core.Store, core.RetryPolicy, *core.EventBus) schedulerAPI { return scheduler },
		newExecutorRegistry: staticExecutorRegistry(nil, nil),
		newArtifactStore:    staticArtifactStore(nil, nil),
		newObservabilityDB: func(core.Config, *slog.Logger) (observabilityDB, error) {
			return &observabilityStub{path: cfg.Observability.Path, stats: sqlite.Stats{SchemaVersion: 1}}, nil
		},
		newAuditLog: func(observabilityDB, core.ObservabilityConfig, *slog.Logger) (auditLogAPI, error) {
			return &auditLogStub{}, nil
		},
		newEventLog: func(*core.EventBus, observabilityDB, core.ObservabilityConfig, *slog.Logger) (eventLogAPI, error) {
			return nil, wantErr
		},
		newServer: func(schedulerAPI, core.Store, string, *executor.Registry, *executor.ArtifactStore, *observabilityAPI, *profileStoreAPI) (serverAPI, error) {
			t.Fatal("the server should not be created when the event writer fails")
			return nil, nil
		},
		notifySignals: func(chan<- os.Signal, ...os.Signal) {},
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	if !errors.Is(err, wantErr) {
		t.Fatalf("expected the event writer error, got %v", err)
	}
	if scheduler.startCalls != 0 {
		t.Fatalf("expected the scheduler not to start, got %d", scheduler.startCalls)
	}
	if store.closeCalls != 1 {
		t.Fatalf("expected the store to be closed once, got %d", store.closeCalls)
	}
}

// TestRun_EventLogClosureIsMandatoryWhenEnabled 把"少了写入器闭包就不许启动"钉住：
// 只打开观测层与事件子开关、不给闭包 → 依赖不完整；同样配置下关掉事件子开关 → 不该再要求它。
func TestRun_EventLogClosureIsMandatoryWhenEnabled(t *testing.T) {
	newDeps := func(eventsEnabled bool) runtimeDeps {
		cfg := core.DefaultConfig()
		cfg.Observability.Enabled = true
		cfg.Observability.Events.Enabled = eventsEnabled
		cfg.Observability.Path = filepath.Join(t.TempDir(), "observe.sqlite")
		return runtimeDeps{
			config:              cfg,
			newStore:            func() (core.Store, error) { return newStubStore(), nil },
			newScheduler:        func(core.Store, core.RetryPolicy, *core.EventBus) schedulerAPI { return newSpyScheduler() },
			newExecutorRegistry: staticExecutorRegistry(nil, nil),
			newArtifactStore:    staticArtifactStore(nil, nil),
			newObservabilityDB: func(core.Config, *slog.Logger) (observabilityDB, error) {
				return &observabilityStub{stats: sqlite.Stats{SchemaVersion: 1}}, nil
			},
			// 台账闭包照给：这一条用例判的是"少了事件写入器"，不该被审计那条检查先挡住
			newAuditLog: func(observabilityDB, core.ObservabilityConfig, *slog.Logger) (auditLogAPI, error) {
				return &auditLogStub{}, nil
			},
			newServer: func(schedulerAPI, core.Store, string, *executor.Registry, *executor.ArtifactStore, *observabilityAPI, *profileStoreAPI) (serverAPI, error) {
				return newFakeServer(), nil
			},
			notifySignals: func(chan<- os.Signal, ...os.Signal) {},
			timeout:       5 * time.Millisecond,
			logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
	}

	deps := newDeps(true)
	err := run(deps)
	if err == nil || !strings.Contains(err.Error(), "runtime dependencies are incomplete") {
		t.Fatalf("expected a missing event writer closure to be reported, got %v", err)
	}

	// 同样的配置只关事件子开关：不构造写入器是预期行为，不该因为它报错。
	// 这一条会跑到等信号那一步，所以给它一个立刻发 SIGTERM 的替身。
	deps = newDeps(false)
	deps.notifySignals = func(ch chan<- os.Signal, sig ...os.Signal) {
		go func() {
			ch <- syscall.SIGTERM
		}()
	}
	if err := run(deps); err != nil {
		t.Fatalf("expected run to work without the closure while events are off, got %v", err)
	}
}

// TestRun_EventLogIsTheServerReader 钉住"注入给服务的事件读取方就是那个写入器"。
//
// 装配把同一个句柄分成两面用：run 拿它关停，api.Server 拿它读库。两条路各自都可能有
// 人接手时接错（比如传了个新建的空写入器），而错的表现是"端点静默回到内存缓冲"——
// 与未启用观测层一模一样，看不出问题。所以这里比的是实例是否同一个。
func TestRun_EventLogIsTheServerReader(t *testing.T) {
	var received []eventLogAPI
	_, logStub, _, _, _ := runWithObservability(t, observabilityCase{
		tune: func(cfg *core.Config) {
			cfg.Observability.Enabled = true
			cfg.Observability.Path = filepath.Join(t.TempDir(), "observe.sqlite")
		},
		gotEvents: func(events eventLogAPI) {
			received = append(received, events)
		},
	})

	if len(received) != 1 {
		t.Fatalf("expected the server factory to be called once, got %d", len(received))
	}
	if received[0] != eventLogAPI(logStub) {
		t.Fatalf("the server got a different event reader than the assembled writer")
	}
}

// TestRun_NoServerReaderWhenEventsDisabled 是上一条的另一半：事件子开关关闭时交给服务的
// 读取方必须是 nil，两个端点因此留在内存缓冲这条路上（未启用时行为一字不变）。
func TestRun_NoServerReaderWhenEventsDisabled(t *testing.T) {
	called := false
	var received []eventLogAPI
	runWithObservability(t, observabilityCase{
		tune: func(cfg *core.Config) {
			cfg.Observability.Enabled = true
			cfg.Observability.Path = filepath.Join(t.TempDir(), "observe.sqlite")
			cfg.Observability.Events.Enabled = false
		},
		gotEvents: func(events eventLogAPI) {
			called = true
			received = append(received, events)
		},
	})

	if !called {
		t.Fatal("expected the server factory to receive the reader argument")
	}
	if received[0] != nil {
		t.Fatalf("expected no event reader while observability.events.enabled is false, got %v", received[0])
	}
}

// artifactIndexStub 是产物索引的替身：装配用例只回答"闭包返回的那一个实例有没有挂到产物存储上"。
// 索引真正的读写行为在 store/sqlite 与 executor 两侧各有用例。
type artifactIndexStub struct{}

func (s *artifactIndexStub) Record(executor.IndexRecord) error { return nil }
func (s *artifactIndexStub) MarkPurged(string, int) error      { return nil }
func (s *artifactIndexStub) MarkAllPurged(string) error        { return nil }
func (s *artifactIndexStub) DeleteByJob(string) error          { return nil }
func (s *artifactIndexStub) List(string) ([]executor.IndexRecord, error) {
	return nil, nil
}
func (s *artifactIndexStub) Exists(string, int) (bool, error) { return false, nil }

// artifactIndexRun 是一次装配能看到的结果：交给服务的产物存储、newServer 被调了几次、启动日志。
type artifactIndexRun struct {
	store       *executor.ArtifactStore
	serverCalls int
	logs        string
}

// runWithArtifactIndexFlags 按三个开关的任意组合跑一次完整装配（不起真服务）。
// newIndex 传 nil 就是"这份装配没有索引闭包"，正是依赖完整性检查要拦的那种。
// 事件子开关一律关着，并且给事件闭包放了个会失败的钩子：产物索引不该顺手把事件写入器也拉起来。
func runWithArtifactIndexFlags(t *testing.T, tune func(*core.Config),
	newIndex func(observabilityDB, core.ObservabilityConfig, string, *slog.Logger) (executor.ArtifactIndexer, error),
) (artifactIndexRun, error) {
	t.Helper()

	dir := t.TempDir()
	cfg := core.DefaultConfig()
	cfg.Executors.Output.Dir = filepath.Join(dir, "exec")
	cfg.Observability.Path = filepath.Join(dir, "observe.sqlite")
	cfg.Observability.Events.Enabled = false
	if tune != nil {
		tune(&cfg)
	}

	var result artifactIndexRun
	var logs strings.Builder
	logger := newCaptureLogger(&logs)

	deps := runtimeDeps{
		config:              cfg,
		newStore:            func() (core.Store, error) { return newStubStore(), nil },
		newScheduler:        func(core.Store, core.RetryPolicy, *core.EventBus) schedulerAPI { return newSpyScheduler() },
		newExecutorRegistry: staticExecutorRegistry(nil, nil),
		newArtifactStore: func(core.Config, *slog.Logger) (*executor.ArtifactStore, error) {
			// 照 defaultRuntimeDeps 建真存储：索引挂在它身上，只有拿到同一个实例才看得见
			return artifactStoreFromConfig(cfg, logger)
		},
		newObservabilityDB: unopenedObservabilityDB(t),
		newEventLog: func(*core.EventBus, observabilityDB, core.ObservabilityConfig, *slog.Logger) (eventLogAPI, error) {
			t.Fatal("the artifact index must not drag the event writer along")
			return nil, nil
		},
		// 审计子开关默认是开的，这份装配因此必须提供一个台账闭包（依赖完整性检查会挡住缺它的部署）。
		// 这里给一个替身：本组用例判的是索引的装配，台账不在它们的断言里。
		newAuditLog: func(observabilityDB, core.ObservabilityConfig, *slog.Logger) (auditLogAPI, error) {
			return &auditLogStub{}, nil
		},
		newArtifactIndex: newIndex,
		newServer: func(_ schedulerAPI, _ core.Store, _ string, _ *executor.Registry,
			artifacts *executor.ArtifactStore, _ *observabilityAPI, _ *profileStoreAPI) (serverAPI, error) {
			result.serverCalls++
			result.store = artifacts
			return newFakeServer(), nil
		},
		notifySignals: func(ch chan<- os.Signal, sig ...os.Signal) {
			go func() {
				ch <- syscall.SIGTERM
			}()
		},
		timeout: 20 * time.Millisecond,
		logger:  logger,
	}
	if cfg.Observability.Enabled {
		deps.newObservabilityDB = func(core.Config, *slog.Logger) (observabilityDB, error) {
			return &observabilityStub{stats: sqlite.Stats{SchemaVersion: 1}}, nil
		}
	}

	err := run(deps)
	result.logs = logs.String()
	return result, err
}

// TestRun_ArtifactIndexIsSetOnStore 钉住索引的交接：闭包只在三开关齐备时被调用一次，
// 拿到的是产物存储的根目录，返回的实例确实挂在了交给服务的那一份存储上。
//
// 这里比的是实例同一性而不是"挂没挂"：写侧（两个执行器）与读侧（列表端点）拿的必须是
// 同一份可选依赖，接错成另一份存储时列表端点会永远看不见刚写下的产物。
func TestRun_ArtifactIndexIsSetOnStore(t *testing.T) {
	var gotRoot string
	var calls int
	stub := &artifactIndexStub{}

	result, err := runWithArtifactIndexFlags(t, func(cfg *core.Config) {
		cfg.Executors.Enabled = true
		cfg.Observability.Enabled = true
		cfg.Observability.Artifacts.Enabled = true
	}, func(_ observabilityDB, _ core.ObservabilityConfig, rootDir string, _ *slog.Logger) (executor.ArtifactIndexer, error) {
		calls++
		gotRoot = rootDir
		return stub, nil
	})
	if err != nil {
		t.Fatalf("expected run to succeed, got %v", err)
	}

	if calls != 1 {
		t.Fatalf("expected the index closure to be called once, got %d", calls)
	}
	if result.store == nil {
		t.Fatal("expected an artifact store while executors are enabled")
	}
	if gotRoot != result.store.Dir() {
		t.Fatalf("expected the artifact root %q, got %q", result.store.Dir(), gotRoot)
	}
	if result.store.Index() != executor.ArtifactIndexer(stub) {
		t.Fatal("the store got a different index than the assembled one")
	}
	if !strings.Contains(result.logs, "artifact_index=true") {
		t.Fatalf("expected the startup log to say the index is on, got %s", result.logs)
	}
}

// TestRun_ArtifactIndexOffByEachSwitch 是上一条的反面：三个开关任一关闭都不建索引，
// 而产物路径照常工作（"没装索引时端点怎么回答"由 api 的 503 用例负责）。
func TestRun_ArtifactIndexOffByEachSwitch(t *testing.T) {
	cases := []struct {
		name string
		tune func(*core.Config)
	}{
		{"executors off", func(cfg *core.Config) {
			cfg.Observability.Enabled = true
			cfg.Observability.Artifacts.Enabled = true
		}},
		{"observability off", func(cfg *core.Config) {
			cfg.Executors.Enabled = true
			cfg.Observability.Artifacts.Enabled = true
		}},
		{"artifacts off", func(cfg *core.Config) {
			cfg.Executors.Enabled = true
			cfg.Observability.Enabled = true
			cfg.Observability.Artifacts.Enabled = false
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			result, err := runWithArtifactIndexFlags(t, tc.tune,
				func(observabilityDB, core.ObservabilityConfig, string, *slog.Logger) (executor.ArtifactIndexer, error) {
					calls++
					return &artifactIndexStub{}, nil
				})
			if err != nil {
				t.Fatalf("expected run to succeed, got %v", err)
			}
			if calls != 0 {
				t.Fatalf("the index closure must not run while %s", tc.name)
			}
			if result.store != nil && result.store.Index() != nil {
				t.Fatal("an artifact store must not carry an index here")
			}
			if result.serverCalls != 1 {
				t.Fatalf("expected the server to be built once, got %d", result.serverCalls)
			}
			if strings.Contains(result.logs, "artifact_index=true") {
				t.Fatalf("startup log must not claim the index is on: %s", result.logs)
			}
		})
	}
}

// TestRun_ArtifactIndexClosureIsMandatoryWhenEnabled 与事件写入器那条同口径：
// 三开关齐备却没有闭包，后果是"一边写文件一边不记索引"，与未启用看不出区别，必须启动期报错。
func TestRun_ArtifactIndexClosureIsMandatoryWhenEnabled(t *testing.T) {
	result, err := runWithArtifactIndexFlags(t, func(cfg *core.Config) {
		cfg.Executors.Enabled = true
		cfg.Observability.Enabled = true
		cfg.Observability.Artifacts.Enabled = true
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "runtime dependencies are incomplete") {
		t.Fatalf("expected a missing index closure to be reported, got %v", err)
	}
	if result.serverCalls != 0 {
		t.Fatalf("the server must not be built, got %d calls", result.serverCalls)
	}

	// 同样的配置只关产物子开关：不构造索引是预期行为，不该因此报错
	_, err = runWithArtifactIndexFlags(t, func(cfg *core.Config) {
		cfg.Executors.Enabled = true
		cfg.Observability.Enabled = true
		cfg.Observability.Artifacts.Enabled = false
	}, nil)
	if err != nil {
		t.Fatalf("expected run to work without the closure while artifacts are off, got %v", err)
	}
}

// TestRun_ArtifactIndexOpenFailureStopsStartup 与事件写入器、观测库那条同口径：
// 建不起索引就不起服务，而不是让列表端点永远空着。
func TestRun_ArtifactIndexOpenFailureStopsStartup(t *testing.T) {
	wantErr := errors.New("artifact index table is missing")
	result, err := runWithArtifactIndexFlags(t, func(cfg *core.Config) {
		cfg.Executors.Enabled = true
		cfg.Observability.Enabled = true
		cfg.Observability.Artifacts.Enabled = true
	}, func(observabilityDB, core.ObservabilityConfig, string, *slog.Logger) (executor.ArtifactIndexer, error) {
		return nil, wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected the index build error, got %v", err)
	}
	if !strings.Contains(err.Error(), "observability artifact index") {
		t.Fatalf("expected the error to name the assembly step, got %v", err)
	}
	if result.serverCalls != 0 {
		t.Fatalf("the server must not be built, got %d calls", result.serverCalls)
	}
}

// TestRun_AuditLogIsHandedToTheServer 钉住台账的交接：闭包只在两开关齐备时被调用一次，
// 交出去的就是那一个实例，而关停时它的丢弃数要说出来。
//
// 实例同一性是这条注入唯一能证的东西：接错成的那一份台账没人写、也没人读，
// 而端点回 200 空列表与"这次部署没记"看起来一样。
func TestRun_AuditLogIsHandedToTheServer(t *testing.T) {
	stub := &auditLogStub{dropped: 3}
	var calls int
	var received []auditLogAPI

	_, _, _, _, logs := runWithObservability(t, observabilityCase{
		tune: func(cfg *core.Config) {
			cfg.Observability.Enabled = true
			cfg.Observability.Path = filepath.Join(t.TempDir(), "observe.sqlite")
		},
		newAudit: func(observabilityDB, core.ObservabilityConfig, *slog.Logger) (auditLogAPI, error) {
			calls++
			return stub, nil
		},
		gotAudit: func(audit auditLogAPI) {
			received = append(received, audit)
		},
	})

	if calls != 1 {
		t.Fatalf("expected the audit closure to be called once, got %d", calls)
	}
	if len(received) != 1 || received[0] != auditLogAPI(stub) {
		t.Fatalf("the server got a different audit handle than the assembled one: %v", received)
	}
	if !strings.Contains(logs, "audit_writer=true") {
		t.Fatalf("expected the startup line to report the writer, got %s", logs)
	}
	if stub.closeCalls != 1 {
		t.Fatalf("expected the writer to be closed once, got %d", stub.closeCalls)
	}
	// 丢弃数必须在关停时也能看见：台账缺页是读不出来的（设计文档 §7.1）
	if !strings.Contains(logs, "observability audit writer dropped records") ||
		!strings.Contains(logs, "dropped=3") {
		t.Fatalf("expected the dropped count at shutdown, got %s", logs)
	}
}

// TestRun_AuditWriterOffByEachSwitch 是上一条的反面：总开关或审计子开关任一关闭都不建写入器，
// 而中间件那边退回"只记一行日志"（api 侧有用例）。
func TestRun_AuditWriterOffByEachSwitch(t *testing.T) {
	cases := []struct {
		name string
		tune func(*core.Config)
	}{
		{"audit sub-switch off", func(cfg *core.Config) {
			cfg.Observability.Enabled = true
			cfg.Observability.Audit.Enabled = false
		}},
		{"observability off", func(cfg *core.Config) {
			cfg.Observability.Audit.Enabled = true
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			var received []auditLogAPI
			// runWithObservability 自己会在 run 返回错误时终止用例，这里不需要再看一遍
			_, _, _, _, _ = runWithObservability(t, observabilityCase{
				tune: tc.tune,
				newAudit: func(observabilityDB, core.ObservabilityConfig, *slog.Logger) (auditLogAPI, error) {
					calls++
					return &auditLogStub{}, nil
				},
				gotAudit: func(audit auditLogAPI) { received = append(received, audit) },
			})
			if calls != 0 {
				t.Fatalf("the audit closure must not run while %s", tc.name)
			}
			if len(received) != 1 || received[0] != nil {
				t.Fatalf("the server must be handed no audit writer, got %v", received)
			}
		})
	}
}

// TestRun_ObservabilityParamIsNilWhenNothingAssembled 钉住合并后的那个参数：
// 两个写入器都没装配时交出去的是 nil 而不是一个空壳，
// 服务里的两条注入都靠它判空。
func TestRun_ObservabilityParamIsNilWhenNothingAssembled(t *testing.T) {
	var sawValue bool
	_, _, _, _, _ = runWithObservability(t, observabilityCase{
		tune: func(cfg *core.Config) {
			cfg.Observability.Enabled = true
			cfg.Observability.Events.Enabled = false
			cfg.Observability.Audit.Enabled = false
		},
		newLog: func(*core.EventBus, observabilityDB, core.ObservabilityConfig, *slog.Logger) (eventLogAPI, error) {
			t.Fatal("no event writer should be built here")
			return nil, nil
		},
		newAudit: func(observabilityDB, core.ObservabilityConfig, *slog.Logger) (auditLogAPI, error) {
			t.Fatal("no audit writer should be built here")
			return nil, nil
		},
		gotEvents: func(events eventLogAPI) { sawValue = events != nil },
		gotAudit:  func(audit auditLogAPI) { sawValue = sawValue || audit != nil },
	})

	if sawValue {
		t.Fatal("both writers were off, so the server should have received nothing")
	}
}

// TestRun_AuditLogClosureIsMandatoryWhenEnabled 与事件、索引那两条同口径：
// 开关说要记而闭包不在，就得起不动。
func TestRun_AuditLogClosureIsMandatoryWhenEnabled(t *testing.T) {
	newDeps := func(auditEnabled bool) runtimeDeps {
		cfg := core.DefaultConfig()
		cfg.Observability.Enabled = true
		cfg.Observability.Audit.Enabled = auditEnabled
		cfg.Observability.Events.Enabled = false
		cfg.Observability.Path = filepath.Join(t.TempDir(), "observe.sqlite")
		deps := runtimeDeps{
			config:              cfg,
			newStore:            func() (core.Store, error) { return newStubStore(), nil },
			newScheduler:        func(core.Store, core.RetryPolicy, *core.EventBus) schedulerAPI { return newSpyScheduler() },
			newExecutorRegistry: staticExecutorRegistry(nil, nil),
			newArtifactStore:    staticArtifactStore(nil, nil),
			newObservabilityDB: func(core.Config, *slog.Logger) (observabilityDB, error) {
				return &observabilityStub{stats: sqlite.Stats{SchemaVersion: 1}}, nil
			},
			newServer: func(schedulerAPI, core.Store, string, *executor.Registry, *executor.ArtifactStore, *observabilityAPI, *profileStoreAPI) (serverAPI, error) {
				return newFakeServer(), nil
			},
			notifySignals: func(ch chan<- os.Signal, sig ...os.Signal) {
				go func() {
					ch <- syscall.SIGTERM
				}()
			},
			timeout: 20 * time.Millisecond,
			logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		return deps
	}

	// 开着审计却不给闭包：启动期报错，服务也不建
	if err := run(newDeps(true)); err == nil ||
		!strings.Contains(err.Error(), "runtime dependencies are incomplete") {
		t.Fatalf("expected a missing audit closure to be reported, got %v", err)
	}

	// 同样配置只关审计子开关：不构造写入器是预期行为
	if err := run(newDeps(false)); err != nil {
		t.Fatalf("expected run to work without the closure while audit is off, got %v", err)
	}
}

// TestRun_AuditLogOpenFailureStopsStartup 与事件写入器、观测库那两条同口径：
// 建不起台账写入器就不起服务，而不是让 /admin/audit 永远回空。
func TestRun_AuditLogOpenFailureStopsStartup(t *testing.T) {
	wantErr := errors.New("audit table is missing")

	cfg := core.DefaultConfig()
	cfg.Observability.Enabled = true
	cfg.Observability.Events.Enabled = false
	cfg.Observability.Path = filepath.Join(t.TempDir(), "observe.sqlite")

	serverCalls := 0
	err := run(runtimeDeps{
		config:              cfg,
		newStore:            func() (core.Store, error) { return newStubStore(), nil },
		newScheduler:        func(core.Store, core.RetryPolicy, *core.EventBus) schedulerAPI { return newSpyScheduler() },
		newExecutorRegistry: staticExecutorRegistry(nil, nil),
		newArtifactStore:    staticArtifactStore(nil, nil),
		newObservabilityDB: func(core.Config, *slog.Logger) (observabilityDB, error) {
			return &observabilityStub{stats: sqlite.Stats{SchemaVersion: 1}}, nil
		},
		newAuditLog: func(observabilityDB, core.ObservabilityConfig, *slog.Logger) (auditLogAPI, error) {
			return nil, wantErr
		},
		newServer: func(schedulerAPI, core.Store, string, *executor.Registry, *executor.ArtifactStore, *observabilityAPI, *profileStoreAPI) (serverAPI, error) {
			serverCalls++
			return newFakeServer(), nil
		},
		notifySignals: func(chan<- os.Signal, ...os.Signal) {},
		timeout:       5 * time.Millisecond,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	if !errors.Is(err, wantErr) {
		t.Fatalf("expected the audit build error, got %v", err)
	}
	if !strings.Contains(err.Error(), "observability audit writer") {
		t.Fatalf("expected the error to name the assembly step, got %v", err)
	}
	if serverCalls != 0 {
		t.Fatalf("the server must not be built, got %d calls", serverCalls)
	}
}

// TestLiveJobIDs 检查"存储里的 ID 集合"包装：内容一致，存储报错时原样透出
// （PurgeOrphans 依赖这个错误决定跳过本轮删除）。
func TestLiveJobIDs(t *testing.T) {
	store := newStubStore()
	store.snapshots = []core.JobSnapshot{{ID: "job-a"}, {ID: "job-b"}}

	live, err := liveJobIDs(store)()
	if err != nil {
		t.Fatalf("expected live ids, got %v", err)
	}
	if len(live) != 2 || !live["job-a"] || !live["job-b"] {
		t.Fatalf("unexpected live set: %v", live)
	}

	store.loadErr = errors.New("store unreadable")
	if _, err := liveJobIDs(store)(); err == nil {
		t.Fatal("expected the store error to reach the caller")
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
		newArtifactStore:    staticArtifactStore(nil, nil),
		newObservabilityDB:  unopenedObservabilityDB(t),
		newServer: func(schedulerAPI, core.Store, string, *executor.Registry, *executor.ArtifactStore, *observabilityAPI, *profileStoreAPI) (serverAPI, error) {
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
		newArtifactStore:    staticArtifactStore(nil, nil),
		newObservabilityDB:  unopenedObservabilityDB(t),
		newServer: func(schedulerAPI, core.Store, string, *executor.Registry, *executor.ArtifactStore, *observabilityAPI, *profileStoreAPI) (serverAPI, error) {
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
		newArtifactStore:    staticArtifactStore(nil, nil),
		newObservabilityDB:  unopenedObservabilityDB(t),
		newServer: func(schedulerAPI, core.Store, string, *executor.Registry, *executor.ArtifactStore, *observabilityAPI, *profileStoreAPI) (serverAPI, error) {
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
	classes       map[string]core.JobClass
	concurrency   int
	queueCapacity int
	// execConcurrency 与 execQueueCapacity 记录执行器池的装配结果：
	// 没打开执行器时必须都是 0，否则进程凭空多出一组协程。
	execConcurrency   int
	execQueueCapacity int
	previewLimit      int
	// restoreGuard 记录装配上来的崩溃恢复钩子：nil 与非 nil 就是"装没装"这条断言。
	// 用例可以拿它直接喂快照，验证装配的不只是一个空函数。
	restoreGuard core.RestoreGuard
	// guardSetBeforeStart 记录装钩子时 Start 还没被调用过（装配顺序断言，见 SetRestoreGuard）
	guardSetBeforeStart bool
	// calls 按发生顺序记下三类关键调用（register:<键> / restore_guard / start），
	// 用于断言"注册早于守卫、守卫早于 Start"这条硬顺序——TASK-W05 的档位合并按它把关。
	// guardSetBeforeStart 只能说钩子早于 Start，说不出 handler 是不是在钩子之前登记的。
	calls []string
	// eventBus 是替身调度器自带的总线：观测层的事件写入器从这里取订阅入口，
	// 用例也可以在装配完成之后往它上头发事件，看写入链路是不是真的通了。
	eventBus *core.EventBus
}

func newSpyScheduler() *spyScheduler {
	return &spyScheduler{
		registered: make(map[string]core.Handler),
		classes:    make(map[string]core.JobClass),
		eventBus:   core.NewEventBus(100),
	}
}

// GetEventBus 满足 schedulerAPI。真实调度器返回的是它自己那一条总线，
// 替身返回用例可见的那一条，装配链路在两种实现下走的是同一个入口。
func (s *spyScheduler) GetEventBus() *core.EventBus {
	return s.eventBus
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

func (s *spyScheduler) SetEventPreviewLimit(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.previewLimit = n
}

func (s *spyScheduler) SetRestoreGuard(g core.RestoreGuard) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.restoreGuard = g
	s.calls = append(s.calls, "restore_guard")
	// 钩子必须在 Start 之前装上：Start 的第一步就是 Restore。
	// 真实调度器在运行后收到这个调用只记日志并忽略，替身跟着忽略就会把装配顺序写错
	// 这件事咽下去，所以这里额外记一笔，由用例断言。
	s.guardSetBeforeStart = s.startCalls == 0
}

// HandlerClass 按注册时记下的类别回答，没注册过的键返回 false——
// 与真实调度器一致，守卫的"档位被删掉"分支靠的就是这条返回值。
func (s *spyScheduler) HandlerClass(key string) (core.JobClass, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	class, ok := s.classes[key]
	return class, ok
}

func (s *spyScheduler) SetExecConcurrency(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.execConcurrency = n
}

func (s *spyScheduler) SetExecQueueCapacity(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.execQueueCapacity = n
}

func (s *spyScheduler) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startCalls++
	s.calls = append(s.calls, "start")
}

func (s *spyScheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopCalls++
}

func (s *spyScheduler) RegisterHandler(jobType string, handler core.Handler) {
	s.RegisterHandlerClass(jobType, handler, core.JobClassDefault)
}

// RegisterHandlerClass 把类别一起记下：档位必须以 JobClassExec 进来，
// 否则装配链路退化成共享池，E13 的隔离就不成立了。
func (s *spyScheduler) RegisterHandlerClass(jobType string, handler core.Handler, class core.JobClass) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registered[jobType] = handler
	s.classes[jobType] = class
	s.calls = append(s.calls, "register:"+jobType)
}

// callIndex 返回某个关键调用第一次出现的位置，没发生过返回 -1。
// 顺序断言用它：register 与 restore_guard、start 之间的先后是档位合并能不能
// 改判崩溃现场的唯一凭据，只看最终注册表内容读不出这个。
func (s *spyScheduler) callIndex(call string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, item := range s.calls {
		if item == call {
			return i
		}
	}
	return -1
}

func (s *spyScheduler) LookupHandler(jobType string) (core.Handler, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	handler, ok := s.registered[jobType]
	return handler, ok
}

// UnregisterHandler 与真实调度器一样成对删两张表，并记一次调用流水。
func (s *spyScheduler) UnregisterHandler(jobType string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.registered[jobType]
	delete(s.registered, jobType)
	delete(s.classes, jobType)
	s.calls = append(s.calls, "unregister:"+jobType)
	return ok
}

// HandlerNames 是档位同步器用来枚举现有键的入口，替身直接复用 registeredKeys 的有序输出。
func (s *spyScheduler) HandlerNames() []string {
	return s.registeredKeys()
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
// snapshots / loadErr 让读路径（产物清理要的"任务还活着"集合）可被测试编排。
// order 非空时把关闭动作记进共享的顺序表，供"观测层早于存储"那条断言使用。
type stubStore struct {
	mu         sync.Mutex
	closeCalls int
	snapshots  []core.JobSnapshot
	loadErr    error
	order      *[]string
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	return s.snapshots, nil
}

func (s *stubStore) Flush() error {
	return nil
}

// SetHistoryRetention 是替身：淘汰行为在 core 包里用真存储验证（core/store_hot_test.go），
// 这里只需要接口完整。
func (s *stubStore) SetHistoryRetention(limit int, ttl time.Duration) {}

func (s *stubStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeCalls++
	if s.order != nil {
		*s.order = append(*s.order, "store")
	}
	return nil
}

// staticExecutorRegistry 返回一个给出固定登记表（或固定构造错误）的闭包。
// 登记表传 nil 表示"这次部署没有执行器"，与开关关闭时的空表效果一致。
func staticExecutorRegistry(reg *executor.Registry, err error) func(core.Config, *slog.Logger) (*executor.Registry, error) {
	return func(core.Config, *slog.Logger) (*executor.Registry, error) {
		return reg, err
	}
}

// staticArtifactStore 返回一个给出固定产物存储（或固定构造错误）的闭包。
// 存储传 nil 只可能出现在"配置没开执行器"的用例里，那种情况下闭包根本不会被调用，
// 而 run 也只在打开执行器时才调用它，所以这里的 nil 不会真的传进注册与清理链路。
func staticArtifactStore(store *executor.ArtifactStore, err error) func(core.Config, *slog.Logger) (*executor.ArtifactStore, error) {
	return func(core.Config, *slog.Logger) (*executor.ArtifactStore, error) {
		return store, err
	}
}

// artifactStoreFromConfig 照 defaultRuntimeDeps 的映射建真实存储，
// 用于需要看到目录与清理协程的用例。
func artifactStoreFromConfig(cfg core.Config, logger *slog.Logger) (*executor.ArtifactStore, error) {
	return executor.NewArtifactStore(executor.ArtifactOptions{
		Dir:      cfg.Executors.Output.Dir,
		MaxBytes: cfg.Executors.Output.MaxBytes,
		TTL:      cfg.Executors.Output.TTL,
	}, logger)
}

// newCaptureLogger 返回把文本日志写进 sb 的日志器，供断言运行期日志内容
func newCaptureLogger(sb *strings.Builder) *slog.Logger {
	return slog.New(slog.NewTextHandler(sb, nil))
}
