package jobs

import (
	"testing"
	"time"

	"github.com/jiujuan/godelayq/internal/core"
	"github.com/stretchr/testify/require"
)

// attempts_test.go 是账本的行为断言（TASK-RE02）：SQL 形状由 sql_snapshot_test.go
// 钉住，这里证明"读出来的数据对"。全部用例在 SQLite 与 PostgreSQL 两个后端上复用。

func TestAttemptLedgerRoundTrip(t *testing.T) {
	runWithStoreBackends(t, func(t *testing.T, open storeOpener) {
		s := open(t)
		start := time.Now().Add(-time.Minute)

		// 开始行：running、finished_at 为 NULL
		require.NoError(t, s.BeginAttempt("job-a", core.Attempt{
			JobID: "job-a", Seq: 1, Status: core.AttemptRunning, Trigger: core.AttemptTriggerScheduled,
			StartedAt: start,
		}))
		rows, err := s.Attempts("job-a", 10)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		require.Equal(t, core.AttemptRunning, rows[0].Status)
		require.Equal(t, core.AttemptTriggerScheduled, rows[0].Trigger)
		require.True(t, rows[0].FinishedAt.IsZero(), "在途行的 finished_at 必须是零值")

		// 收口行：结论字段带 Set 标记的落列，未判定的落 NULL
		finished := start.Add(30 * time.Second)
		require.NoError(t, s.FinishAttempt("job-a", 1, core.Attempt{
			JobID: "job-a", Seq: 1, Status: core.AttemptFailed, Trigger: core.AttemptTriggerRetry,
			StartedAt: start, FinishedAt: finished, DurationMs: 30000,
			Class: "timeout", Retryable: true, RetryableSet: true,
			ExitCode: 0, ExitCodeSet: true,
			Error: "profile \"x\": timed out",
		}))
		rows, err = s.Attempts("job-a", 10)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		got := rows[0]
		require.Equal(t, core.AttemptFailed, got.Status)
		require.False(t, got.FinishedAt.IsZero())
		require.Equal(t, int64(30000), got.DurationMs)
		require.Equal(t, "timeout", got.Class)
		require.True(t, got.RetryableSet)
		require.True(t, got.Retryable)
		require.True(t, got.ExitCodeSet)
		require.Equal(t, 0, got.ExitCode, "退出码 0 与未判定必须分得开")
		require.False(t, got.HTTPStatusSet)
		require.Equal(t, "profile \"x\": timed out", got.Error)

		// 未判定字段：不设 Set 标记，读回来必须三列全 NULL
		require.NoError(t, s.BeginAttempt("job-a", core.Attempt{
			JobID: "job-a", Seq: 2, Status: core.AttemptRunning, Trigger: core.AttemptTriggerRetry,
			StartedAt: finished,
		}))
		require.NoError(t, s.FinishAttempt("job-a", 2, core.Attempt{
			JobID: "job-a", Seq: 2, Status: core.AttemptSuccess, Trigger: core.AttemptTriggerRetry,
			StartedAt: finished, FinishedAt: finished.Add(time.Second), DurationMs: 1000,
		}))
		rows, err = s.Attempts("job-a", 10)
		require.NoError(t, err)
		require.Len(t, rows, 2)
		success := rows[0] // seq 降序，seq=2 在前
		require.Equal(t, 2, success.Seq)
		require.Equal(t, core.AttemptSuccess, success.Status)
		require.False(t, success.RetryableSet)
		require.False(t, success.ExitCodeSet)
		require.Empty(t, success.Class)
	})
}

func TestAttemptLedgerUpsertIsIdempotent(t *testing.T) {
	runWithStoreBackends(t, func(t *testing.T, open storeOpener) {
		s := open(t)
		start := time.Now()

		rec := core.Attempt{JobID: "job-b", Seq: 1, Status: core.AttemptRunning, Trigger: core.AttemptTriggerScheduled, StartedAt: start}
		require.NoError(t, s.BeginAttempt("job-b", rec))
		require.NoError(t, s.BeginAttempt("job-b", rec)) // 同 seq 重放：覆盖而不是第二行
		rows, err := s.Attempts("job-b", 10)
		require.NoError(t, err)
		require.Len(t, rows, 1)
	})
}

func TestAttemptLedgerAbandon(t *testing.T) {
	runWithStoreBackends(t, func(t *testing.T, open storeOpener) {
		s := open(t)
		start := time.Now().Add(-time.Hour)

		// 两行 running（seq 1/2）+ 一行已收口的 failed（seq 3）
		for seq := 1; seq <= 2; seq++ {
			require.NoError(t, s.BeginAttempt("job-c", core.Attempt{
				JobID: "job-c", Seq: seq, Status: core.AttemptRunning, Trigger: core.AttemptTriggerScheduled,
				StartedAt: start.Add(time.Duration(seq) * time.Minute),
			}))
		}
		require.NoError(t, s.FinishAttempt("job-c", 3, core.Attempt{
			JobID: "job-c", Seq: 3, Status: core.AttemptFailed, Trigger: core.AttemptTriggerRetry,
			StartedAt: start.Add(3 * time.Minute), FinishedAt: start.Add(4 * time.Minute), DurationMs: 60000,
		}))

		// 放弃到 seq 3：两行 running 变 abandoned，failed 行不动
		at := time.Now()
		require.NoError(t, s.AbandonAttempts("job-c", 3, at))
		rows, err := s.Attempts("job-c", 10)
		require.NoError(t, err)
		require.Len(t, rows, 3)
		for _, row := range rows {
			switch row.Seq {
			case 1, 2:
				require.Equal(t, core.AttemptAbandoned, row.Status)
				require.False(t, row.FinishedAt.IsZero())
				require.Positive(t, row.DurationMs, "abandoned 行的时长按 started_at 补算")
			case 3:
				require.Equal(t, core.AttemptFailed, row.Status, "已收口的行不受放弃影响")
			}
		}

		// 放弃一个没有 running 行的任务：无害空操作
		require.NoError(t, s.AbandonAttempts("job-c", 3, at))
	})
}

func TestAttemptLedgerPrune(t *testing.T) {
	runWithStoreBackends(t, func(t *testing.T, open storeOpener) {
		s := open(t)
		start := time.Now().Add(-time.Hour)

		for seq := 1; seq <= 5; seq++ {
			require.NoError(t, s.BeginAttempt("job-d", core.Attempt{
				JobID: "job-d", Seq: seq, Status: core.AttemptSuccess, Trigger: core.AttemptTriggerScheduled,
				StartedAt: start.Add(time.Duration(seq) * time.Minute),
				FinishedAt: start.Add(time.Duration(seq)*time.Minute + time.Second), DurationMs: 1000,
			}))
		}

		// 保最近 2 条：seq 4/5 在，1/2/3 删
		require.NoError(t, s.PruneAttempts("job-d", 2))
		rows, err := s.Attempts("job-d", 10)
		require.NoError(t, err)
		require.Len(t, rows, 2)
		require.Equal(t, 5, rows[0].Seq)
		require.Equal(t, 4, rows[1].Seq)

		// 裁剪是幂等的：再裁一次不删新行、不报错
		require.NoError(t, s.PruneAttempts("job-d", 2))
		rows, err = s.Attempts("job-d", 10)
		require.NoError(t, err)
		require.Len(t, rows, 2)

		// 裁一个没有账本行的任务：无害
		require.NoError(t, s.PruneAttempts("job-none", 2))
	})
}

// TestAttemptTrimCascade 证明"快照被终态留痕淘汰时账本一起走"（TASK-RE02）：
// 留痕上限 1，第二笔终态写入触发 trim，第一笔的账本行必须同事务消失。
func TestAttemptTrimCascade(t *testing.T) {
	runWithStoreBackends(t, func(t *testing.T, open storeOpener) {
		s := open(t, withHistory(1, 0))
		start := time.Now().Add(-time.Minute)

		require.NoError(t, s.Update(snapshot("trim-1", core.StatusSuccess, start)))
		require.NoError(t, s.BeginAttempt("trim-1", core.Attempt{
			JobID: "trim-1", Seq: 1, Status: core.AttemptSuccess, Trigger: core.AttemptTriggerScheduled,
			StartedAt: start, FinishedAt: start.Add(time.Second), DurationMs: 1000,
		}))
		require.NoError(t, s.Update(snapshot("trim-2", core.StatusSuccess, start.Add(time.Second))))

		all, err := s.LoadAll()
		require.NoError(t, err)
		require.Len(t, all, 1, "留痕上限 1，trim-1 的快照应被淘汰")
		rows, err := s.Attempts("trim-1", 10)
		require.NoError(t, err)
		require.Empty(t, rows, "快照被淘汰的任务，账本行必须级联删除")
	})
}
