package main

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jiujuan/godelayq/internal/core"
	"github.com/stretchr/testify/require"
)

// recovery_policy_test.go 是 TASK-RE05 的恢复矩阵：scheduler.recovery_policy
// 三取值 × 任务类别（执行器/普通）× 与 executors.restore_policy 的组合，
// 证明 auto 与既有行为逐字节等价、replay/pause 全局生效、新键压过旧键时留 warn。

// fakeStore 是 core.Store 的最小替身（core 的 mockStore 在测试包内不可导入）：
// 只服务 Restore 的 LoadAll 与守卫的 Update 落盘。
type fakeStore struct {
	mu    sync.Mutex
	snap  map[string]core.JobSnapshot
	calls []string
}

func newFakeStore(snapshots ...core.JobSnapshot) *fakeStore {
	s := &fakeStore{snap: make(map[string]core.JobSnapshot)}
	for _, snap := range snapshots {
		s.snap[snap.ID] = snap
	}
	return s
}

func (s *fakeStore) Save(job *core.Job) error { return s.Update(job.ToSnapshot()) }

func (s *fakeStore) Update(snapshot core.JobSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "update:"+snapshot.ID)
	s.snap[snapshot.ID] = snapshot
	return nil
}

func (s *fakeStore) Delete(jobID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "delete:"+jobID)
	delete(s.snap, jobID)
	return nil
}

func (s *fakeStore) LoadAll() ([]core.JobSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]core.JobSnapshot, 0, len(s.snap))
	for _, snap := range s.snap {
		out = append(out, snap)
	}
	return out, nil
}

func (s *fakeStore) Flush() error                           { return nil }
func (s *fakeStore) Close() error                           { return nil }
func (s *fakeStore) SetHistoryRetention(int, time.Duration) {}

func (s *fakeStore) statusOf(jobID string) core.JobStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return core.JobStatus(s.snap[jobID].Status)
}

// runningSnapshot 构造一条崩溃现场的 running 快照。
func runningSnapshot(id, handlerKey string) core.JobSnapshot {
	return core.JobSnapshot{
		ID: id, Name: handlerKey, Type: handlerKey,
		TriggerAt: time.Now().Add(-time.Hour), CreatedAt: time.Now().Add(-2 * time.Hour), UpdatedAt: time.Now().Add(-time.Hour),
		Status: int(core.StatusRunning),
	}
}

// TestRecoveryPolicy_MatrixCases 把恢复矩阵跑全：三取值 × 两类别 × 新旧键组合。
// auto 与既有行为逐字节等价；replay/pause 全局生效；新键压过旧键时留 warn。
func TestRecoveryPolicy_MatrixCases(t *testing.T) {
	base := core.Config{}
	base.Executors.Enabled = true

	cases := []struct {
		name      string
		policy    string // scheduler.recovery_policy（"" = 缺省）
		oldPolicy string // executors.restore_policy
		execHeld  bool   // 崩溃前 running 的执行器任务该停 paused？
		plainHeld bool   // 崩溃前 running 的普通任务该停 paused？
	}{
		{"auto default", "", "", true, false},
		{"auto with old replay", "", "replay", false, false},
		{"replay global", "replay", "", false, false},
		{"pause global", "pause", "", true, true},
		{"pause overrides old replay", "pause", "replay", true, true},
		{"replay overrides old pause", "replay", "pause", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			cfg.Scheduler.RecoveryPolicy = tc.policy
			cfg.Executors.RestorePolicy = tc.oldPolicy

			store := newFakeStore(
				runningSnapshot("exec-one", "exec.report"),
				runningSnapshot("plain-one", "plain"),
			)
			scheduler := core.NewScheduler(store, nil, nil)
			scheduler.RegisterHandlerClass("exec.report", func(context.Context, *core.Job) error { return nil }, core.JobClassExec)
			scheduler.RegisterHandler("plain", func(context.Context, *core.Job) error { return nil })

			var logs strings.Builder
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			installRestoreGuard(scheduler, cfg, logger)
			require.NoError(t, scheduler.Restore())

			execStatus := store.statusOf("exec-one")
			plainStatus := store.statusOf("plain-one")
			if tc.execHeld {
				require.Equal(t, core.StatusPaused, execStatus, "执行器任务应停 paused")
			} else {
				require.NotEqual(t, core.StatusPaused, execStatus, "执行器任务应重放")
			}
			if tc.plainHeld {
				require.Equal(t, core.StatusPaused, plainStatus, "普通任务应停 paused")
			} else {
				require.NotEqual(t, core.StatusPaused, plainStatus, "普通任务应重放")
			}

			// 新键压过旧键的组合要留 warn（replay+replay 同结论不打扰）
			wantsWarn := tc.policy != "" && tc.policy != "auto" && tc.oldPolicy != "" &&
				!(tc.policy == "replay" && tc.oldPolicy == "replay")
			if wantsWarn {
				require.Contains(t, logs.String(), "overrides executors.restore_policy")
			} else {
				require.NotContains(t, logs.String(), "overrides executors.restore_policy")
			}
		})
	}
}

// TestRecoveryPolicy_AutoWithoutExecutors：auto 且执行器未启用时不装守卫——
// 与 TASK-E14 之前的世界等价（普通任务即便按 exec. 前缀命名也照常重放）。
func TestRecoveryPolicy_AutoWithoutExecutors(t *testing.T) {
	cfg := core.Config{} // Executors.Enabled=false

	store := newFakeStore(runningSnapshot("looks-exec", "exec.report"))
	scheduler := core.NewScheduler(store, nil, nil)
	scheduler.RegisterHandlerClass("exec.report", func(context.Context, *core.Job) error { return nil }, core.JobClassExec)

	installRestoreGuard(scheduler, cfg, slog.Default())
	require.NoError(t, scheduler.Restore())

	require.Equal(t, 1, scheduler.HeapLen(), "没装守卫：running 快照复位重放")
	require.NotEqual(t, core.StatusPaused, store.statusOf("looks-exec"))
}
