package jobs

import (
	"testing"
	"time"

	"github.com/jiujuan/godelayq/internal/core"
	"github.com/stretchr/testify/require"
)

// fault_test.go 是 TASK-RE08 的存储侧故障注入：证明 UpdateWithEvents 的同事务
// 原子性——中途失败整体回滚，不存在"快照落了、出箱行没落"或反过来的中间态。
// 注入手段：预埋一行 status 非法的 jobs 行，终态写入触发的留痕淘汰会在扫描时
// 失败——这是真实代码路径的失败，不是 mock。

// seedCorruptJobRow 插一行 status 非法的 jobs 行（经 bun 构建器，方言无关）。
// trimTerminalTx 的 ParseJobStatus 会在扫描到它时报错，事务就此回滚。
func seedCorruptJobRow(t *testing.T, s *Store, id string) {
	t.Helper()
	row := &jobRow{ID: id, Name: "corrupt", Status: "not-a-status", UpdatedAt: 1, SnapshotJSON: "{}"}
	_, err := s.h.NewInsert().Model(row).Exec(bgCtx)
	require.NoError(t, err)
}

func TestUpdateWithEvents_RollsBackOnMidTxFailure(t *testing.T) {
	runWithStoreBackends(t, func(t *testing.T, open storeOpener) {
		s := open(t)
		seedCorruptJobRow(t, s, "corrupt-row")

		// 终态写入触发 trim → 扫描撞上非法状态 → 事务失败 → 整体回滚
		err := s.UpdateWithEvents(snapshot("rollback-1", core.StatusSuccess, time.Now()), []core.Event{
			{Type: core.EventJobCompleted, JobID: "rollback-1", JobName: "job-rollback-1", Status: core.StatusSuccess},
		})
		require.Error(t, err, "trim 撞上非法状态必须让事务失败")

		// 回滚的三个面：新快照不在、出箱行不在、预埋的脏行原样还在
		_, found, err := s.Get("rollback-1")
		require.NoError(t, err)
		require.False(t, found, "事务回滚后快照不得落库")

		count, err := s.h.NewSelect().Model((*outboxRow)(nil)).
			Where("job_id = ?", "rollback-1").
			Count(bgCtx)
		require.NoError(t, err)
		require.EqualValues(t, 0, count, "事务回滚后出箱行不得落库")

		all, err := s.LoadAll()
		require.NoError(t, err)
		require.Len(t, all, 1, "只有预埋的脏行留在库里")
		require.NotEqual(t, "rollback-1", all[0].ID, "回滚后新快照不得出现（脏行的空快照不算数）")
	})
}

// TestUpdateWithEvents_TerminalTrimSameTransaction：终态写、留痕淘汰与账本级联
// 在同一事务里完成（正路径的形状证明）。
func TestUpdateWithEvents_TerminalTrimSameTransaction(t *testing.T) {
	runWithStoreBackends(t, func(t *testing.T, open storeOpener) {
		s := open(t, withHistory(1, 0))
		now := time.Now()

		require.NoError(t, s.UpdateWithEvents(snapshot("trim-old", core.StatusSuccess, now), nil))
		require.NoError(t, s.BeginAttempt("trim-old", core.Attempt{
			JobID: "trim-old", Seq: 1, Status: core.AttemptSuccess, Trigger: core.AttemptTriggerScheduled,
			StartedAt: now, FinishedAt: now.Add(time.Second), DurationMs: 1000,
		}))

		// 第二笔终态写触发淘汰：trim-old 的快照与账本行一起消失，事务一次完成
		require.NoError(t, s.UpdateWithEvents(snapshot("trim-new", core.StatusSuccess, now.Add(time.Second)), []core.Event{
			{Type: core.EventJobCompleted, JobID: "trim-new", JobName: "job-trim-new", Status: core.StatusSuccess},
		}))

		all, err := s.LoadAll()
		require.NoError(t, err)
		require.Len(t, all, 1)
		require.Equal(t, "trim-new", all[0].ID)

		attempts, err := s.Attempts("trim-old", 10)
		require.NoError(t, err)
		require.Empty(t, attempts, "被淘汰任务的账本行随同事务级联删除")

		count, err := s.h.NewSelect().Model((*outboxRow)(nil)).
			Where("job_id = ?", "trim-new").
			Count(bgCtx)
		require.NoError(t, err)
		require.EqualValues(t, 1, count, "同一事务里出箱行已落库")
	})
}
