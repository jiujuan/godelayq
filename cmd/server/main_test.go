package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"godelayq/core"
)

func TestRegisterHandlers_RegistersAllSupportedJobTypes(t *testing.T) {
	server := newFakeServer()

	registerHandlers(server)

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

	if deps.newStore == nil || deps.newScheduler == nil || deps.newServer == nil || deps.notifySignals == nil {
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

