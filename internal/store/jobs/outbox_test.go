package jobs

import (
	"testing"
	"time"

	"github.com/jiujuan/godelayq/internal/core"
	"github.com/stretchr/testify/require"
)

// outbox_test.go 是 TASK-RE07 的行为断言：UpdateWithEvents 的同事务原子性、
// AppendOutbox 入队、中继器的投递/标记/重放/淘汰。SQL 形状由快照钉住。

// outboxPending 读 pending 行数（断言辅助）。
func outboxPending(t *testing.T, s *Store) int64 {
	t.Helper()
	count, err := s.h.NewSelect().Model((*outboxRow)(nil)).
		Where("status = ?", outboxStatusPending).
		Count(bgCtx)
	require.NoError(t, err)
	return count
}

func TestUpdateWithEvents(t *testing.T) {
	runWithStoreBackends(t, func(t *testing.T, open storeOpener) {
		s := open(t)
		now := time.Now()

		events := []core.Event{
			{Type: core.EventJobScheduled, JobID: "ob-1", JobName: "job-ob-1", Status: core.StatusPending},
			{Type: core.EventJobStarted, JobID: "ob-1", JobName: "job-ob-1", Status: core.StatusRunning},
		}
		require.NoError(t, s.UpdateWithEvents(snapshot("ob-1", core.StatusRunning, now), events))

		// 快照与出箱行都在：同事务落库的两个产物
		snap, found, err := s.Get("ob-1")
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, core.StatusRunning, core.JobStatus(snap.Status))
		require.EqualValues(t, 2, outboxPending(t, s), "两条事件各占一行 pending")

		// 终态写走同一通道：快照 + 事件 + 留痕淘汰同事务（limit 1 时旧终态连带账本级联）
		require.NoError(t, s.UpdateWithEvents(snapshot("ob-done", core.StatusSuccess, now), events[:1]))
		snap, found, err = s.Get("ob-done")
		require.NoError(t, err)
		require.True(t, found)
		_ = snap
	})
}

func TestAppendOutbox(t *testing.T) {
	runWithStoreBackends(t, func(t *testing.T, open storeOpener) {
		s := open(t)

		// 空批次无事发生
		require.NoError(t, s.AppendOutbox(nil))

		require.NoError(t, s.AppendOutbox([]core.Event{
			{Type: core.EventJobCompleted, JobID: "ob-2", Status: core.StatusSuccess},
		}))
		require.EqualValues(t, 1, outboxPending(t, s))
	})
}

// TestOutboxRelayDispatch：中继器把 pending 按 seq 顺序投递给总线（OutboxSeq 回填）、
// 同批标记 dispatched；再跑一轮无新投递（重放幂等的正向面）。
func TestOutboxRelayDispatch(t *testing.T) {
	runWithStoreBackends(t, func(t *testing.T, open storeOpener) {
		s := open(t)
		bus := core.NewEventBus(100)
		relay, err := s.NewOutboxRelay(bus, OutboxRelayOptions{}, nil)
		require.NoError(t, err)

		// 订阅收集：断言顺序与 OutboxSeq 回填
		_, inbox := bus.SubscribeAll()
		received := make(chan core.Event, 16)
		go func() {
			for event := range inbox {
				received <- event
			}
		}()

		require.NoError(t, s.AppendOutbox([]core.Event{
			{Type: core.EventJobScheduled, JobID: "relay-1", Status: core.StatusPending},
			{Type: core.EventJobStarted, JobID: "relay-1", Status: core.StatusRunning},
			{Type: core.EventJobCompleted, JobID: "relay-1", Status: core.StatusSuccess},
		}))
		require.EqualValues(t, 3, outboxPending(t, s))

		relay.Start()
		defer func() { _ = relay.Stop() }()

		// 三条事件按序到达，OutboxSeq 由中继器回填
		for want := int64(1); want <= 3; want++ {
			select {
			case event := <-received:
				require.Equal(t, "relay-1", event.JobID)
				require.Equal(t, want, event.OutboxSeq, "事件按出箱 seq 顺序投递")
			case <-time.After(3 * time.Second):
				t.Fatalf("第 %d 条事件未在超时内投递", want)
			}
		}

		// 投递完成：pending 清零（标记 dispatched）
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if outboxPending(t, s) == 0 {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("pending 未清零，实际 %d", outboxPending(t, s))
	})
}

// TestOutboxRelayReplayAfterCrash：手工把 dispatched 改回 pending（模拟
// "投递成功、标记前崩溃"的重放面），重启中继后重复投递、不再有 pending 残留。
// 读侧去重由 observe 的 outbox_dedup_test.go 证明。
func TestOutboxRelayReplayAfterCrash(t *testing.T) {
	runWithStoreBackends(t, func(t *testing.T, open storeOpener) {
		s := open(t)
		bus := core.NewEventBus(100)
		relay, err := s.NewOutboxRelay(bus, OutboxRelayOptions{}, nil)
		require.NoError(t, err)

		require.NoError(t, s.AppendOutbox([]core.Event{
			{Type: core.EventJobStarted, JobID: "crash-1", Status: core.StatusRunning},
		}))
		relay.Start()
		waitOutboxDrained(t, s)
		require.NoError(t, relay.Stop())

		// 崩溃现场：标记回滚
		_, err = s.h.NewUpdate().Model((*outboxRow)(nil)).
			Set("status = ?", outboxStatusPending).
			Set("dispatched_at = ?", nil).
			Where("status = ?", outboxStatusDispatched).
			Exec(bgCtx)
		require.NoError(t, err)
		require.EqualValues(t, 1, outboxPending(t, s))

		// 重启重放：投出去、标记回来
		relay2, err := s.NewOutboxRelay(bus, OutboxRelayOptions{}, nil)
		require.NoError(t, err)
		relay2.Start()
		defer func() { _ = relay2.Stop() }()
		waitOutboxDrained(t, s)
	})
}

// TestOutboxRelayStopIdempotent：Stop 可重复调用（关停路径的幂等要求）。
func TestOutboxRelayStopIdempotent(t *testing.T) {
	runWithStoreBackends(t, func(t *testing.T, open storeOpener) {
		s := open(t)
		relay, err := s.NewOutboxRelay(core.NewEventBus(10), OutboxRelayOptions{}, nil)
		require.NoError(t, err)
		relay.Start()
		require.NoError(t, relay.Stop())
		require.NoError(t, relay.Stop(), "重复 Stop 必须无害")
	})
}

// TestOutboxPrune：dispatched 按 TTL 淘汰；pending 积压超限按最旧截断。
func TestOutboxPrune(t *testing.T) {
	runWithStoreBackends(t, func(t *testing.T, open storeOpener) {
		s := open(t)
		bus := core.NewEventBus(100)
		relay, err := s.NewOutboxRelay(bus, OutboxRelayOptions{}, nil)
		require.NoError(t, err)

		for i := 0; i < 5; i++ {
			require.NoError(t, s.AppendOutbox([]core.Event{
				{Type: core.EventJobStarted, JobID: "prune-1", Status: core.StatusRunning},
			}))
		}
		relay.Start()
		waitOutboxDrained(t, s)
		require.NoError(t, relay.Stop())

		// dispatched 行拨回 TTL 之外，再走一轮 prune：全部淘汰
		_, err = s.h.NewUpdate().Model((*outboxRow)(nil)).
			Set("dispatched_at = ?", time.Now().Add(-2*core.DefaultOutboxRetentionAge).UnixNano()).
			Where("status = ?", outboxStatusDispatched).
			Exec(bgCtx)
		require.NoError(t, err)
		relay2, err := s.NewOutboxRelay(bus, OutboxRelayOptions{}, nil)
		require.NoError(t, err)
		relay2.Start()
		waitOutboxDrained(t, s)
		require.NoError(t, relay2.Stop())

		count, err := s.h.NewSelect().Model((*outboxRow)(nil)).Count(bgCtx)
		require.NoError(t, err)
		require.EqualValues(t, 0, count, "TTL 之外的 dispatched 行应被淘汰")

		// pending 积压截断：max=2，塞 5 条 pending（不启动中继），手动跑一轮 prune
		for i := 0; i < 5; i++ {
			require.NoError(t, s.AppendOutbox([]core.Event{
				{Type: core.EventJobStarted, JobID: "prune-2", Status: core.StatusRunning},
			}))
		}
		relay3, err := s.NewOutboxRelay(bus, OutboxRelayOptions{MaxPending: 2}, nil)
		require.NoError(t, err)
		relay3.prune()
		require.LessOrEqual(t, outboxPending(t, s), int64(2), "积压应被截断到上限内")
	})
}

// waitOutboxDrained 轮询等 pending 清零（中继协程是异步的）。
func waitOutboxDrained(t *testing.T, s *Store) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if outboxPending(t, s) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pending 未在超时内清零，实际 %d", outboxPending(t, s))
}
