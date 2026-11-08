// outbox_test.go 钉 TASK-RE07 的调度器分派：出箱可用时事件只进出箱、
// 不再直发总线（双发禁令）；nil 时退回直发（既有行为的回归面）。
package core

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// stubOutbox 是 EventOutbox 的内存替身：记录调用流水量。
type stubOutbox struct {
	mu      sync.Mutex
	updates []struct {
		snapshot JobSnapshot
		events   []Event
	}
	attemptUpserts []Attempt
	appends        []Event
}

func (s *stubOutbox) UpdateWithEvents(snapshot JobSnapshot, events []Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updates = append(s.updates, struct {
		snapshot JobSnapshot
		events   []Event
	}{snapshot, events})
	return nil
}

// UpdateWithEventsAndAttempt（W1/Q08）：合并通道的替身——记 attempt 并照常记
// 快照/事件，供"合并 vs 独立"的分派断言读。
func (s *stubOutbox) UpdateWithEventsAndAttempt(snapshot JobSnapshot, events []Event, attempt Attempt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attemptUpserts = append(s.attemptUpserts, attempt)
	s.updates = append(s.updates, struct {
		snapshot JobSnapshot
		events   []Event
	}{snapshot, events})
	return nil
}

func (s *stubOutbox) AppendOutbox(events []Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appends = append(s.appends, events...)
	return nil
}

// TestScheduler_OutboxModeNoDirectPublish：出箱模式下状态写与事件都进出箱，
// EventBus 收不到任何事件（投递是中继器的事）。
func TestScheduler_OutboxModeNoDirectPublish(t *testing.T) {
	store := newMockStore()
	outbox := &stubOutbox{}
	scheduler := NewScheduler(store, nil, nil, quietLogger(), WithEventOutbox(outbox))
	events := collectEvents(scheduler.GetEventBus())
	scheduler.RegisterHandler("ok", func(ctx context.Context, job *Job) error { return nil })

	scheduler.Schedule(&Job{ID: "ob-core", Name: "ok", TriggerAt: time.Now()})
	scheduler.Start()
	defer scheduler.Stop()

	// 出箱模式下快照经 UpdateWithEvents 落库（store 不再是快照的落点），
	// 等 stub 里的终态快照而不是 mockStore。
	waitForStubSnapshotStatus(t, outbox, "ob-core", StatusSuccess)

	outbox.mu.Lock()
	updates := len(outbox.updates)
	appends := len(outbox.appends)
	outbox.mu.Unlock()
	require.GreaterOrEqual(t, updates, 1, "状态写应经 UpdateWithEvents")
	require.GreaterOrEqual(t, appends, 2, "started/completed 应经 AppendOutbox（scheduled 随状态写同事务）")

	// 双发禁令：总线侧零事件
	require.False(t, events.has(EventJobScheduled, "ob-core"))
	require.False(t, events.has(EventJobStarted, "ob-core"))
	require.False(t, events.has(EventJobCompleted, "ob-core"))
}

// waitForStubSnapshotStatus 轮询等 stub 收到期望状态的快照（outbox 模式的收尾异步）。
func waitForStubSnapshotStatus(t *testing.T, outbox *stubOutbox, jobID string, want JobStatus) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		outbox.mu.Lock()
		for _, u := range outbox.updates {
			if u.snapshot.ID == jobID && JobStatus(u.snapshot.Status) == want {
				outbox.mu.Unlock()
				return
			}
		}
		outbox.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("stub 未在超时内收到 %s 的 %s 快照", jobID, want)
}

// TestScheduler_OutboxModeTerminalCarriesEvents：Schedule 的写带 scheduled 事件
// （同事务原子性的调用侧形状）；UpdatePending 的 identity 写无事件，也走出箱通道。
func TestScheduler_OutboxModeTerminalCarriesEvents(t *testing.T) {
	outbox := &stubOutbox{}
	scheduler := NewScheduler(newMockStore(), nil, nil, quietLogger(), WithEventOutbox(outbox))

	scheduler.Schedule(&Job{ID: "ob-term", Name: "n", TriggerAt: time.Now().Add(time.Hour)})
	outbox.mu.Lock()
	first := outbox.updates[0]
	outbox.mu.Unlock()
	require.Equal(t, StatusPending, JobStatus(first.snapshot.Status))
	require.Len(t, first.events, 1, "首次排期的写必须带 scheduled 事件（同事务）")
	require.Equal(t, EventJobScheduled, first.events[0].Type)

	// identity 写：状态不变、无事件，仍走出箱通道（出箱模式下它不落 store）
	_, err := scheduler.UpdatePending("ob-term", func(job *Job) error {
		job.Group = "nightly"
		return nil
	})
	require.NoError(t, err)
	outbox.mu.Lock()
	last := outbox.updates[len(outbox.updates)-1]
	outbox.mu.Unlock()
	require.Equal(t, "nightly", last.snapshot.Group)
	require.Empty(t, last.events, "identity 写没有事件")
}

// W1/Q08：running 转移与账本开始行的合并分派。
//
//   - 出箱 + 账本：走 UpdateWithEventsAndAttempt（一次调用带 attempt），
//     ledger.BeginAttempt 不再被单独调用；
//   - attempt nil（JSON 部署形状）：退回 UpdateWithEvents，stub 收不到 attempt；
//   - 无出箱 + 账本（第三方只给账本的防御性组合）：store.Update + 独立 BeginAttempt，
//     顺序与改动前的 transition→beginAttempt 一致；
//   - 非法转移：事件照发、不出箱、不记账。
func TestScheduler_TransitionWithAttempt_MergedDispatch(t *testing.T) {
	t.Run("outbox and ledger merged", func(t *testing.T) {
		outbox := &stubOutbox{}
		ledger := newMockLedger()
		scheduler := NewScheduler(newMockStore(), nil, nil, quietLogger(),
			WithEventOutbox(outbox), WithAttemptLedger(ledger, 20))

		job := &Job{ID: "w1-a", Name: "n", Status: StatusPending}
		ok := scheduler.transitionWithAttempt(job, StatusRunning, &Attempt{
			JobID: "w1-a", Seq: 1, Status: AttemptRunning,
		})
		require.True(t, ok)

		outbox.mu.Lock()
		merged := len(outbox.attemptUpserts)
		plainUpdates := len(outbox.updates)
		outbox.mu.Unlock()
		require.Equal(t, 1, merged, "合并通道恰好一次")
		require.Equal(t, 1, plainUpdates, "快照经合并调用落库")
		require.Equal(t, 0, len(ledger.begins), "ledger.BeginAttempt 不得再被单独调用")
	})

	t.Run("nil attempt falls back to plain update", func(t *testing.T) {
		outbox := &stubOutbox{}
		scheduler := NewScheduler(newMockStore(), nil, nil, quietLogger(), WithEventOutbox(outbox))

		job := &Job{ID: "w1-b", Name: "n", Status: StatusPending}
		require.True(t, scheduler.transitionWithAttempt(job, StatusRunning, nil))

		outbox.mu.Lock()
		merged := len(outbox.attemptUpserts)
		outbox.mu.Unlock()
		require.Equal(t, 0, merged, "attempt nil 不得走合并通道")
		require.Equal(t, StatusRunning, job.Status)
	})

	t.Run("direct mode keeps separate order", func(t *testing.T) {
		store := newMockStore()
		ledger := newMockLedger()
		scheduler := NewScheduler(store, nil, nil, quietLogger(), WithAttemptLedger(ledger, 20))

		job := &Job{ID: "w1-c", Name: "n", Status: StatusPending}
		require.True(t, scheduler.transitionWithAttempt(job, StatusRunning, &Attempt{
			JobID: "w1-c", Seq: 1, Status: AttemptRunning,
		}))
		require.Equal(t, 1, len(ledger.begins), "直发模式账本行独立补写")
		snap, ok := store.snapshotOf("w1-c")
		require.True(t, ok)
		require.Equal(t, StatusRunning, JobStatus(snap.Status))
	})

	t.Run("illegal transition still emits events", func(t *testing.T) {
		outbox := &stubOutbox{}
		ledger := newMockLedger()
		scheduler := NewScheduler(newMockStore(), nil, nil, quietLogger(),
			WithEventOutbox(outbox), WithAttemptLedger(ledger, 20))

		job := &Job{ID: "w1-d", Name: "n", Status: StatusSuccess}
		ok := scheduler.transitionWithAttempt(job, StatusRunning, &Attempt{JobID: "w1-d", Seq: 1},
			Event{Type: EventJobStarted, JobID: "w1-d"})
		require.False(t, ok, "终态无出边")

		outbox.mu.Lock()
		merged, updates := len(outbox.attemptUpserts), len(outbox.updates)
		outbox.mu.Unlock()
		require.Equal(t, 0, merged, "拒绝的转移不落库")
		require.Equal(t, 0, updates, "拒绝的转移不落库")
		require.Equal(t, 0, len(ledger.begins), "拒绝的转移不记账")
	})
}
