package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
	"godelayq/executor"
)

// quietLogger 吞掉测试不关心的日志，避免注册计数混进 go test 的输出。
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// tempArtifactStore 把产物写进临时目录，供注册链路构造真实执行器（TASK-E09）。
// 打开执行器却不给产物存储时 registerHandlers 会直接报装配错误，
// 所以凡是用启用状态的登记表跑一遍的用例都要带上一份。
func tempArtifactStore(t *testing.T, cfg core.Config) *executor.ArtifactStore {
	t.Helper()

	store, err := executor.NewArtifactStore(executor.ArtifactOptions{
		Dir:      filepath.Join(t.TempDir(), "exec"),
		MaxBytes: cfg.Executors.Output.MaxBytes,
	}, quietLogger())
	if err != nil {
		t.Fatalf("build artifact store: %v", err)
	}
	return store
}

// disabledRegistry 造一个开关关闭时空着的登记表，等价于"这次部署没有执行器"。
// 连同配置一起返回：注册档位要按 executors 一节求生效超时，测试里两份都得是同一套取值。
func disabledRegistry(t *testing.T) (core.Config, *executor.Registry) {
	t.Helper()

	cfg := core.DefaultConfig()
	reg, err := executor.NewRegistry(cfg, quietLogger())
	if err != nil {
		t.Fatalf("build disabled registry: %v", err)
	}
	return cfg, reg
}

// scriptRegistry 造一个启用状态的登记表，每个名字一条脚本档位。
// runtime 用当前测试程序自身：它一定存在且在 PATH 里，因此探测结论为可用，
// 断言不必依赖目标机器装了 node 或 php。
// 返回值带一份配置：注册链路要用它求生效超时（TASK-E09 之后档位处理函数是真实执行器）。
func scriptRegistry(t *testing.T, names ...string) (core.Config, *executor.Registry) {
	t.Helper()

	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test executable: %v", err)
	}

	workspace := t.TempDir()
	cfg := core.DefaultConfig()
	cfg.Executors.Enabled = true
	cfg.Executors.Workspace = workspace
	cfg.Executors.RuntimeAllow = append(cfg.Executors.RuntimeAllow, executable)

	for _, name := range names {
		scriptRel := "scripts/" + name + ".mjs"
		scriptAbs := filepath.Join(workspace, filepath.FromSlash(scriptRel))
		if err := os.MkdirAll(filepath.Dir(scriptAbs), 0o750); err != nil {
			t.Fatalf("create script dir: %v", err)
		}
		if err := os.WriteFile(scriptAbs, []byte("console.log('ok')\n"), 0o600); err != nil {
			t.Fatalf("create script file: %v", err)
		}
		cfg.Executors.Commands = append(cfg.Executors.Commands, core.ExecutorCommand{
			Name:    name,
			Kind:    "script",
			Runtime: executable,
			Script:  scriptRel,
		})
	}

	reg, err := executor.NewRegistry(cfg, quietLogger())
	if err != nil {
		t.Fatalf("build executor registry: %v", err)
	}
	return cfg, reg
}

func TestRegisterHandlers_RegistersAllSupportedJobTypes(t *testing.T) {
	server, scheduler := newRegisteredPair()

	cfg, registry := disabledRegistry(t)
	if err := registerHandlers(server, scheduler, registry, cfg, nil, quietLogger()); err != nil {
		t.Fatalf("expected registerHandlers to succeed, got %v", err)
	}

	expected := []string{"payment_check", "email_send", "data_sync", "report_generate"}
	if len(server.registered) != len(expected) {
		t.Fatalf("expected %d handlers, got %d", len(expected), len(server.registered))
	}

	for _, name := range expected {
		if _, ok := server.registered[name]; !ok {
			t.Fatalf("expected handler %q to be registered", name)
		}
	}
}

func TestRegisterHandlers_WithProfiles(t *testing.T) {
	server, scheduler := newRegisteredPair()

	cfg, registry := scriptRegistry(t, "a", "b")
	if err := registerHandlers(server, scheduler, registry, cfg, tempArtifactStore(t, cfg), quietLogger()); err != nil {
		t.Fatalf("expected registerHandlers to succeed, got %v", err)
	}

	// 四个示例 + 两条档位；档位键带 exec. 前缀，与示例名不可能重合
	if len(scheduler.registered) != 6 {
		t.Fatalf("expected 6 scheduler handlers, got %d: %v", len(scheduler.registered), scheduler.registeredKeys())
	}
	for _, name := range []string{"exec.a", "exec.b"} {
		if _, ok := scheduler.registered[name]; !ok {
			t.Fatalf("expected executor handler %q to be registered", name)
		}
	}

	// 示例仍按原路径注册，档位不会因为它们绕过 api.Server 而丢掉转发关系
	if len(server.registered) != 4 {
		t.Fatalf("expected 4 handlers on the server, got %d", len(server.registered))
	}
}

// TestRegisterHandlers_MissingArtifactStoreFails 钉住"打开执行器却没传产物存储"的结论：
// 档位的处理函数是真实执行器，没有输出落盘的地方就注定每次执行都查不到结果。
// 装配少传一个参数属于编程错误，要在注册阶段停住，而不是让任务逐个失败。
func TestRegisterHandlers_MissingArtifactStoreFails(t *testing.T) {
	server, scheduler := newRegisteredPair()
	cfg, registry := scriptRegistry(t, "a")

	err := registerHandlers(server, scheduler, registry, cfg, nil, quietLogger())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "artifact store")

	// 停在装配期：档位键一个都没进来，不会留下"注册了一半"的表
	for _, key := range scheduler.registeredKeys() {
		assert.NotEqual(t, "exec.a", key)
	}
}

func TestRegisterHandlers_ConflictFails(t *testing.T) {
	server, scheduler := newRegisteredPair()
	// 占用档位将要使用的键，模拟配置与代码对不上的情况
	scheduler.RegisterHandler("exec.a", func(context.Context, *core.Job) error { return nil })

	cfg, registry := scriptRegistry(t, "a", "b")
	err := registerHandlers(server, scheduler, registry, cfg, tempArtifactStore(t, cfg), quietLogger())
	if err == nil {
		t.Fatal("expected a conflict error")
	}
	if !strings.Contains(err.Error(), "exec.a") {
		t.Fatalf("expected the conflicting key in %q", err.Error())
	}
	if _, ok := scheduler.registered["exec.b"]; ok {
		t.Fatal("expected no partial registration when a key conflicts")
	}
}

// newRegisteredPair 造一对测试替身：假服务把注册转发给假调度器，
// 与真实进程里 api.Server 转发给 core.Scheduler 的关系一致。
func newRegisteredPair() (*fakeServer, *spyScheduler) {
	server := newFakeServer()
	scheduler := newSpyScheduler()
	server.onRegister = scheduler.RegisterHandler
	return server, scheduler
}

func TestHandlePaymentCheck(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		job := &core.Job{Payload: []byte(`{"order_id":"A-1"}`)}

		start := time.Now()
		if err := handlePaymentCheck(context.Background(), job); err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if elapsed := time.Since(start); elapsed < 1900*time.Millisecond {
			t.Fatalf("expected simulated processing delay, got %v", elapsed)
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := handlePaymentCheck(ctx, &core.Job{Payload: []byte("x")})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context canceled, got %v", err)
		}
	})

	t.Run("nil job panics", func(t *testing.T) {
		assertPanics(t, func() {
			_ = handlePaymentCheck(context.Background(), nil)
		})
	})
}

func TestHandleEmailSend(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		if err := handleEmailSend(context.Background(), &core.Job{Payload: []byte("mail")}); err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := handleEmailSend(ctx, &core.Job{Payload: []byte("mail")})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context canceled, got %v", err)
		}
	})

	t.Run("nil job panics", func(t *testing.T) {
		assertPanics(t, func() {
			_ = handleEmailSend(context.Background(), nil)
		})
	})
}

func TestHandleDataSync(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		if err := handleDataSync(context.Background(), &core.Job{Payload: []byte("sync")}); err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := handleDataSync(ctx, &core.Job{Payload: []byte("sync")})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context canceled, got %v", err)
		}
	})

	t.Run("nil job panics", func(t *testing.T) {
		assertPanics(t, func() {
			_ = handleDataSync(context.Background(), nil)
		})
	})
}

func TestHandleReportGenerate(t *testing.T) {
	t.Run("cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		start := time.Now()
		err := handleReportGenerate(ctx, &core.Job{Payload: []byte("report")})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context canceled, got %v", err)
		}
		if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
			t.Fatalf("expected early cancellation, got %v", elapsed)
		}
	})

	t.Run("deadline exceeded", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()

		err := handleReportGenerate(ctx, &core.Job{Payload: []byte("report")})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected deadline exceeded, got %v", err)
		}
	})

	t.Run("nil job panics", func(t *testing.T) {
		assertPanics(t, func() {
			_ = handleReportGenerate(context.Background(), nil)
		})
	})
}

// TestHandlers_LogPayloadWithJobID 示例 handler 走结构化日志，
// payload 作为字段而不是拼接进消息文本。
func TestHandlers_LogPayload(t *testing.T) {
	testCases := []struct {
		name       string
		handler    func(context.Context, *core.Job) error
		payload    string
		wantOutput string
	}{
		{name: "payment", handler: handlePaymentCheck, payload: "order-42", wantOutput: "processing payment check"},
		{name: "email", handler: handleEmailSend, payload: "alice@example.com", wantOutput: "sending email"},
		{name: "sync", handler: handleDataSync, payload: "crm-backup", wantOutput: "syncing data"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var logs strings.Builder
			restore := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(restore) })

			if err := tc.handler(context.Background(), &core.Job{ID: "job-1", Payload: []byte(tc.payload)}); err != nil {
				t.Fatalf("expected no error, got %v", err)
			}

			output := logs.String()
			if !strings.Contains(output, `msg="`+tc.wantOutput+`"`) {
				t.Fatalf("expected log %q to contain %q", output, tc.wantOutput)
			}
			if !strings.Contains(output, "job_id=job-1") {
				t.Fatalf("expected log %q to carry the job id", output)
			}
			if !strings.Contains(output, tc.payload) {
				t.Fatalf("expected log %q to contain payload %q", output, tc.payload)
			}
		})
	}
}

func TestDefaultRuntimeDeps(t *testing.T) {
	deps := defaultRuntimeDeps(core.DefaultConfig(), slog.Default())

	if deps.newStore == nil || deps.newScheduler == nil || deps.newExecutorRegistry == nil ||
		deps.newArtifactStore == nil || deps.newObservabilityDB == nil ||
		deps.newServer == nil || deps.notifySignals == nil {
		t.Fatal("expected all runtime dependencies to be set")
	}
	if deps.timeout != core.DefaultConfig().Scheduler.ShutdownTimeout {
		t.Fatalf("expected timeout to come from config, got %v", deps.timeout)
	}
	if deps.logger == nil {
		t.Fatal("expected default logger to be set")
	}
}

func TestDefaultNewStore_CreatesUsableStore(t *testing.T) {
	cfg := core.DefaultConfig()
	cfg.Store.Path = filepath.Join(t.TempDir(), "jobs.json")
	cfg.Store.FlushInterval = 10 * time.Millisecond

	store, err := defaultRuntimeDeps(cfg, slog.Default()).newStore()
	if err != nil {
		t.Fatalf("expected store creation to succeed, got %v", err)
	}

	job := &core.Job{
		ID:        "job-1",
		Name:      "payment_check",
		Payload:   []byte(`{"id":1}`),
		TriggerAt: time.Now().Add(time.Minute),
		Status:    core.StatusPending,
	}
	if err := store.Save(job); err != nil {
		t.Fatalf("expected save to succeed, got %v", err)
	}

	items, err := store.LoadAll()
	if err != nil {
		t.Fatalf("expected load to succeed, got %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected one stored job, got %d", len(items))
	}

	var decoded map[string]any
	if err := json.Unmarshal(items[0].Payload, &decoded); err != nil {
		t.Fatalf("expected payload to stay valid JSON, got %v", err)
	}

	// 配置的 flush_interval 应真实生效：合并写入后文件即出现在配置的路径上
	if err := store.Flush(); err != nil {
		t.Fatalf("expected flush to succeed, got %v", err)
	}
	if _, err := os.Stat(cfg.Store.Path); err != nil {
		t.Fatalf("expected store file at configured path: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("expected close to succeed, got %v", err)
	}
}

func assertPanics(t *testing.T, fn func()) {
	t.Helper()

	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()

	fn()
}
