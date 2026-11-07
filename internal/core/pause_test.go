package core

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// collectEvents 订阅全部事件并在后台收纳，供断言"某个动作用户能看见"。
func collectEvents(bus *EventBus) *eventLog {
	log := &eventLog{}
	_, events := bus.SubscribeAll()

	go func() {
		for event := range events {
			log.mu.Lock()
			log.items = append(log.items, event)
			log.mu.Unlock()
		}
	}()

	return log
}

type eventLog struct {
	mu    sync.Mutex
	items []Event
}

func (l *eventLog) has(eventType EventType, jobID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, event := range l.items {
		if event.Type == eventType && event.JobID == jobID {
			return true
		}
	}
	return false
}

// last 返回最后一次出现的某类事件。
func (l *eventLog) last(eventType EventType, jobID string) (Event, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var found Event
	matched := false
	for _, event := range l.items {
		if event.Type == eventType && event.JobID == jobID {
			found = event
			matched = true
		}
	}
	return found, matched
}

// waitFor 在超时内等待某类事件出现。
func (l *eventLog) waitFor(t *testing.T, eventType EventType, jobID string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if l.has(eventType, jobID) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("在 %v 内没有收到 %s 事件（job_id=%s）", timeout, eventType, jobID)
}

func TestScheduler_Pause_TakesJobOutOfHeapButKeepsSnapshot(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)
	events := collectEvents(scheduler.GetEventBus())

	job := &Job{ID: "pause-1", Name: "test-job", TriggerAt: time.Now().Add(time.Hour), Group: "nightly"}
	if err := scheduler.Schedule(job); err != nil {
		t.Fatalf("Schedule 失败: %v", err)
	}

	paused, err := scheduler.Pause("pause-1")
	if err != nil {
		t.Fatalf("Pause 失败: %v", err)
	}
	if paused.Status != StatusPaused {
		t.Errorf("返回值应为 paused，实际 %s", paused.Status)
	}
	// 分组必须跟着走，否则暂停一次就从分组视图里消失
	if paused.Group != "nightly" {
		t.Errorf("暂停不应丢分组，实际 %q", paused.Group)
	}
	if scheduler.heap.Len() != 0 {
		t.Errorf("暂停后堆应为空，实际长度 %d", scheduler.heap.Len())
	}

	snap, ok := store.snapshotOf("pause-1")
	if !ok {
		t.Fatal("Pause 必须保留快照（与 Cancel 的本质差别），存储里却查不到")
	}
	if JobStatus(snap.Status) != StatusPaused {
		t.Errorf("快照状态应为 paused(5)，实际 %d", snap.Status)
	}

	events.waitFor(t, EventJobPaused, "pause-1", time.Second)
}

func TestScheduler_Pause_RunningJobIsRejected(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})

	scheduler := NewScheduler(nil, nil, nil)
	scheduler.SetConcurrency(1)
	scheduler.RegisterHandler("slow", func(ctx context.Context, job *Job) error {
		close(started)
		<-release
		return nil
	})

	if err := scheduler.Schedule(&Job{ID: "pause-running", Name: "slow", Type: "slow", TriggerAt: time.Now()}); err != nil {
		t.Fatalf("Schedule 失败: %v", err)
	}
	scheduler.Start()
	defer func() {
		close(release)
		scheduler.Stop()
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("任务没有开始执行")
	}

	// 执行中的任务由 worker 持有，普通暂停拒绝它（要中止执行是 ForcePause）
	if _, err := scheduler.Pause("pause-running"); !errors.Is(err, ErrJobNotPending) {
		t.Errorf("暂停执行中的任务应返回 ErrJobNotPending，实际 %v", err)
	}
}

func TestScheduler_Pause_UnknownJobAndIdempotentRepeat(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)

	if _, err := scheduler.Pause("missing"); !errors.Is(err, ErrJobNotFound) {
		t.Errorf("未知任务应返回 ErrJobNotFound，实际 %v", err)
	}

	if err := scheduler.Schedule(&Job{ID: "pause-twice", Name: "test-job", TriggerAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("Schedule 失败: %v", err)
	}
	if _, err := scheduler.Pause("pause-twice"); err != nil {
		t.Fatalf("首次 Pause 失败: %v", err)
	}

	// 控制器双击/超时重试都会重发，第二次不该变成 409
	again, err := scheduler.Pause("pause-twice")
	if err != nil {
		t.Fatalf("重复 Pause 应幂等，实际报错 %v", err)
	}
	if again == nil || again.Status != StatusPaused {
		t.Errorf("重复 Pause 应返回当前暂停态，实际 %+v", again)
	}
	if scheduler.heap.Len() != 0 {
		t.Errorf("重复 Pause 不该把任务塞回堆里，堆长度 %d", scheduler.heap.Len())
	}
}

func TestScheduler_Resume_PutsJobBackIntoHeap(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)
	events := collectEvents(scheduler.GetEventBus())

	future := time.Now().Add(2 * time.Hour)
	if err := scheduler.Schedule(&Job{ID: "resume-1", Name: "test-job", Group: "billing", TriggerAt: future}); err != nil {
		t.Fatalf("Schedule 失败: %v", err)
	}
	if _, err := scheduler.Pause("resume-1"); err != nil {
		t.Fatalf("Pause 失败: %v", err)
	}

	resumed, err := scheduler.Resume("resume-1")
	if err != nil {
		t.Fatalf("Resume 失败: %v", err)
	}
	if resumed.Status != StatusPending {
		t.Errorf("恢复后应为 pending，实际 %s", resumed.Status)
	}
	// ID 不变是硬要求：控制台按同一 ID 追时间线，换 ID 就等于历史断链
	if resumed.ID != "resume-1" {
		t.Errorf("Resume 必须保留原 ID，实际 %q", resumed.ID)
	}
	if resumed.Group != "billing" {
		t.Errorf("Resume 不应丢分组，实际 %q", resumed.Group)
	}
	if scheduler.heap.Len() != 1 {
		t.Errorf("恢复后任务应回到堆里，堆长度 %d", scheduler.heap.Len())
	}
	if !resumed.TriggerAt.Equal(future) {
		t.Errorf("一次性任务恢复后应沿用原触发时间，实际 %v（原 %v）", resumed.TriggerAt, future)
	}

	events.waitFor(t, EventJobResumed, "resume-1", time.Second)
}

func TestScheduler_Resume_ExpiredOneShotRunsImmediately(t *testing.T) {
	// 暂停期间早已到期的任务，恢复时不该被吞掉：与崩溃恢复同口径，立即补跑
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)

	var mu sync.Mutex
	ran := make(chan struct{}, 1)
	scheduler.RegisterHandler("echo", func(ctx context.Context, job *Job) error {
		mu.Lock()
		ran <- struct{}{}
		mu.Unlock()
		return nil
	})

	if err := scheduler.Schedule(&Job{ID: "resume-expired", Name: "echo", Type: "echo", TriggerAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatalf("Schedule 失败: %v", err)
	}
	if _, err := scheduler.Pause("resume-expired"); err != nil {
		t.Fatalf("Pause 失败: %v", err)
	}

	scheduler.Start()
	defer scheduler.Stop()

	if _, err := scheduler.Resume("resume-expired"); err != nil {
		t.Fatalf("Resume 失败: %v", err)
	}

	select {
	case <-ran:
	case <-time.After(3 * time.Second):
		t.Fatal("过期任务恢复后没有立即补跑")
	}
}

func TestScheduler_Resume_CronJobGetsFutureTrigger(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)

	before := time.Now()
	job := &Job{
		ID:        "resume-cron",
		Name:      "tick",
		Type:      "tick",
		CronExpr:  "*/5 * * * * *", // 每 5 秒
		IsRepeat:  true,
		TriggerAt: before.Add(-time.Minute),
	}
	if err := scheduler.Schedule(job); err != nil {
		t.Fatalf("Schedule 失败: %v", err)
	}
	if _, err := scheduler.Pause("resume-cron"); err != nil {
		t.Fatalf("Pause 失败: %v", err)
	}

	resumed, err := scheduler.Resume("resume-cron")
	if err != nil {
		t.Fatalf("Resume 失败: %v", err)
	}
	if !resumed.TriggerAt.After(before) {
		t.Errorf("Cron 任务恢复后应排在将来时刻，实际 %v（早于 %v）", resumed.TriggerAt, before)
	}
	if resumed.CronExpr != "*/5 * * * * *" {
		t.Errorf("恢复不该丢 Cron 表达式，实际 %q", resumed.CronExpr)
	}
}

func TestScheduler_Resume_RejectsNonPausedAndUnknown(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)

	if _, err := scheduler.Resume("ghost"); !errors.Is(err, ErrJobNotFound) {
		t.Errorf("未知任务应返回 ErrJobNotFound，实际 %v", err)
	}

	if err := scheduler.Schedule(&Job{ID: "resume-live", Name: "test-job", TriggerAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("Schedule 失败: %v", err)
	}
	if _, err := scheduler.Resume("resume-live"); !errors.Is(err, ErrJobNotPaused) {
		t.Errorf("未暂停的任务恢复应返回 ErrJobNotPaused，实际 %v", err)
	}
	if scheduler.heap.Len() != 1 {
		t.Errorf("失败的 Resume 不该改动堆，堆长度 %d", scheduler.heap.Len())
	}

	// 终态留痕同样不可恢复：它已经是历史，重新排期等于凭空造出一个新执行
	snap := JobSnapshot{ID: "resume-done", Name: "test-job", Status: int(StatusSuccess)}
	if err := store.Update(snap); err != nil {
		t.Fatalf("预置快照失败: %v", err)
	}
	if _, err := scheduler.Resume("resume-done"); !errors.Is(err, ErrJobNotPaused) {
		t.Errorf("已结束任务恢复应返回 ErrJobNotPaused，实际 %v", err)
	}
}

func TestScheduler_Cancel_PausedJobIsStillDeletable(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)
	events := collectEvents(scheduler.GetEventBus())

	if err := scheduler.Schedule(&Job{ID: "cancel-paused", Name: "test-job", TriggerAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("Schedule 失败: %v", err)
	}
	if _, err := scheduler.Pause("cancel-paused"); err != nil {
		t.Fatalf("Pause 失败: %v", err)
	}

	// 暂停后不在堆里也不在执行中，若 Cancel 只认这两种，任务就再也删不掉了
	if err := scheduler.Cancel("cancel-paused"); err != nil {
		t.Fatalf("Cancel 暂停任务失败: %v", err)
	}
	if _, ok := store.snapshotOf("cancel-paused"); ok {
		t.Error("Cancel 应连同快照一起删除")
	}
	if store.GetDeleteCalls() != 1 {
		t.Errorf("应发生一次存储删除，实际 %d 次", store.GetDeleteCalls())
	}

	events.waitFor(t, EventJobCancelled, "cancel-paused", time.Second)
}

// 真存储 + 两次进程：模拟"暂停后重启"，验证暂停不会被恢复流程悄悄解除。
func TestScheduler_Restore_KeepsPausedJobsParked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.json")
	store, err := NewJSONFileStoreWithOptions(path, StoreOptions{Interval: time.Millisecond})
	if err != nil {
		t.Fatalf("创建存储失败: %v", err)
	}

	var mu sync.Mutex
	executed := 0
	handler := func(ctx context.Context, job *Job) error {
		mu.Lock()
		executed++
		mu.Unlock()
		return nil
	}

	first := NewScheduler(store, nil, nil)
	first.SetConcurrency(1)
	first.RegisterHandler("tick", handler)

	// 一个暂停的、一个正常待执行的，都排在将来：重启后只有后者应被重新入队
	if err := first.Schedule(&Job{ID: "parked", Name: "tick", Type: "tick", TriggerAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("Schedule 失败: %v", err)
	}
	if _, err := first.Pause("parked"); err != nil {
		t.Fatalf("Pause 失败: %v", err)
	}
	if err := first.Schedule(&Job{ID: "waiting", Name: "tick", Type: "tick", TriggerAt: time.Now().Add(2 * time.Hour)}); err != nil {
		t.Fatalf("Schedule 失败: %v", err)
	}

	// 合并落盘是异步的，Close 会收尾
	if err := store.Close(); err != nil {
		t.Fatalf("关闭存储失败: %v", err)
	}

	second, err := NewJSONFileStoreWithOptions(path, StoreOptions{Interval: time.Millisecond})
	if err != nil {
		t.Fatalf("重开存储失败: %v", err)
	}
	defer second.Close()

	restarted := NewScheduler(second, nil, nil)
	restarted.SetConcurrency(1)
	restarted.RegisterHandler("tick", handler)
	restarted.Start()
	defer restarted.Stop()

	if restarted.heap.Len() != 1 {
		snap, _ := second.LoadAll()
		statuses := make(map[string]int, len(snap))
		for _, item := range snap {
			statuses[item.ID] = item.Status
		}
		t.Fatalf("重启后应只有 1 个任务重新入队（parked 必须保持暂停），实际堆长度 %d，快照状态 %+v", restarted.heap.Len(), statuses)
	}
	if got := restarted.heap.Get("parked"); got != nil {
		t.Error("被暂停的任务不应在重启后被复活入队")
	}

	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if executed != 0 {
		t.Errorf("两个任务都排在将来，不应有执行发生，实际执行 %d 次", executed)
	}
}

// waitRunning 等待任务进入执行（cancelMap 登记完成），避免和调度竞态。
func (s *Scheduler) waitRunning(t *testing.T, jobID string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.isRunning(jobID) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("任务 %s 在 %v 内没有进入执行", jobID, timeout)
}

func TestScheduler_ForcePause_StopsRunningJobWithoutCountingRetry(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)
	events := collectEvents(scheduler.GetEventBus())
	scheduler.SetConcurrency(1)

	blocked := make(chan struct{})
	scheduler.RegisterHandler("coop", func(ctx context.Context, job *Job) error {
		close(blocked)
		<-ctx.Done() // 守规矩的 Handler：取消即返回
		return ctx.Err()
	})

	if err := scheduler.Schedule(&Job{
		ID: "fp-coop", Name: "coop", Type: "coop",
		TriggerAt:  time.Now(),
		MaxRetries: 3,
		RetryDelay: time.Millisecond,
	}); err != nil {
		t.Fatalf("Schedule 失败: %v", err)
	}

	scheduler.Start()
	defer scheduler.Stop()

	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("Handler 没有开始执行")
	}

	if _, err := scheduler.ForcePause("fp-coop"); err != nil {
		t.Fatalf("ForcePause 失败: %v", err)
	}
	events.waitFor(t, EventJobPaused, "fp-coop", 2*time.Second)

	snap, ok := store.snapshotOf("fp-coop")
	if !ok {
		t.Fatal("强制暂停必须留下快照（不是删除）")
	}
	if JobStatus(snap.Status) != StatusPaused {
		t.Errorf("状态应为 paused，实际 %s", JobStatus(snap.Status))
	}
	// 核心承诺：中止不算失败，也不吃掉重试次数
	if snap.RetryCount != 0 {
		t.Errorf("强制暂停不应消耗重试次数，实际 retry_count=%d", snap.RetryCount)
	}
	if JobStatus(snap.Status) == StatusFailed {
		t.Error("不应被判为失败")
	}
	if scheduler.heap.Len() != 0 {
		t.Errorf("不应产生重试副本入堆，堆长度 %d", scheduler.heap.Len())
	}
}

// 不检查 ctx 的 Handler 是本仓库一直承认的现实：它会在被取消后照常返回 nil。
// 没有收尾守卫的话，Cron 的下一轮会被 handleSuccess 悄悄排上，暂停像只生效了几秒。
func TestScheduler_ForcePause_HandlerIgnoringCancelStillParksCron(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)
	events := collectEvents(scheduler.GetEventBus())
	scheduler.SetConcurrency(1)

	returned := make(chan struct{})
	scheduler.RegisterHandler("stubborn", func(ctx context.Context, job *Job) error {
		time.Sleep(80 * time.Millisecond) // 完全不看 ctx
		close(returned)
		return nil
	})

	if err := scheduler.Schedule(&Job{
		ID: "fp-stubborn", Name: "stubborn", Type: "stubborn",
		// 每秒一轮：Schedule 会把 Cron 任务的触发点重算到下一个边界，
		// 用 */5 会让"等它进入执行"白等五秒
		TriggerAt: time.Now(), CronExpr: "* * * * * *", IsRepeat: true,
	}); err != nil {
		t.Fatalf("Schedule 失败: %v", err)
	}

	scheduler.Start()
	defer scheduler.Stop()

	scheduler.waitRunning(t, "fp-stubborn", 3*time.Second)
	if _, err := scheduler.ForcePause("fp-stubborn"); err != nil {
		t.Fatalf("ForcePause 失败: %v", err)
	}

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("Handler 没有返回")
	}
	events.waitFor(t, EventJobPaused, "fp-stubborn", 2*time.Second)

	// 给守卫一点时间跑完收尾
	time.Sleep(100 * time.Millisecond)

	// 事件必须由成功收尾的守卫发出（而不是"看起来像正常取消"），且只发一次：
	// 重复的 job.paused 会让前端时间线出现两个暂停点，掩盖真实的中止耗时
	event, ok := events.last(EventJobPaused, "fp-stubborn")
	if !ok {
		t.Fatal("没有取到 job.paused 事件")
	}
	if event.Metadata["forced"] != true {
		t.Errorf("强制暂停的事件应标记 forced，实际 %+v", event.Metadata)
	}
	if event.Metadata["note"] != "handler_completed_despite_cancel" {
		t.Errorf("应由成功收尾守卫兜底，实际 note=%v", event.Metadata["note"])
	}

	if scheduler.heap.Len() != 0 {
		t.Errorf("被强制暂停的 Cron 任务不该再排下一轮，堆长度 %d", scheduler.heap.Len())
	}
	snap, ok := store.snapshotOf("fp-stubborn")
	if !ok {
		t.Fatal("快照应仍在存储里")
	}
	if JobStatus(snap.Status) != StatusPaused {
		t.Errorf("收尾后状态应钉回 paused，实际 %s", JobStatus(snap.Status))
	}
}

func TestScheduler_ForcePause_HandlerIgnoringErrorStillParks(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)
	scheduler.SetConcurrency(1)

	scheduler.RegisterHandler("loud", func(ctx context.Context, job *Job) error {
		time.Sleep(60 * time.Millisecond)
		return errors.New("boom") // 被取消后仍自己报错：不该走重试
	})

	if err := scheduler.Schedule(&Job{
		ID: "fp-error", Name: "loud", Type: "loud",
		TriggerAt: time.Now(), MaxRetries: 3, RetryDelay: time.Millisecond,
	}); err != nil {
		t.Fatalf("Schedule 失败: %v", err)
	}

	scheduler.Start()
	defer scheduler.Stop()

	scheduler.waitRunning(t, "fp-error", 2*time.Second)
	if _, err := scheduler.ForcePause("fp-error"); err != nil {
		t.Fatalf("ForcePause 失败: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if snap, ok := store.snapshotOf("fp-error"); ok && JobStatus(snap.Status) == StatusPaused {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	snap, _ := store.snapshotOf("fp-error")
	if JobStatus(snap.Status) != StatusPaused {
		t.Errorf("收尾应把状态钉在 paused，实际 %s", JobStatus(snap.Status))
	}
	if snap.RetryCount != 0 {
		t.Errorf("不应消耗重试次数，实际 %d", snap.RetryCount)
	}
	if scheduler.heap.Len() != 0 {
		t.Errorf("不该产生重试副本，堆长度 %d", scheduler.heap.Len())
	}
}

func TestScheduler_ForcePause_PendingJobBehavesLikePause(t *testing.T) {
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil)
	events := collectEvents(scheduler.GetEventBus())

	if err := scheduler.Schedule(&Job{ID: "fp-pending", Name: "test-job", TriggerAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("Schedule 失败: %v", err)
	}

	// 竞态下任务还没出堆：强制暂停退化成"更强的普通暂停"，保证一次都不执行
	if _, err := scheduler.ForcePause("fp-pending"); err != nil {
		t.Fatalf("ForcePause 失败: %v", err)
	}
	if scheduler.heap.Len() != 0 {
		t.Errorf("任务应离开堆，堆长度 %d", scheduler.heap.Len())
	}
	events.waitFor(t, EventJobPaused, "fp-pending", time.Second)
}

func TestScheduler_ForcePause_UnknownJobAndResumeRace(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)
	if _, err := scheduler.ForcePause("ghost"); !errors.Is(err, ErrJobNotFound) {
		t.Errorf("未知任务应返回 ErrJobNotFound，实际 %v", err)
	}

	store := newMockStore()
	scheduler2 := NewScheduler(store, nil, nil)
	scheduler2.SetConcurrency(1)

	release := make(chan struct{})
	scheduler2.RegisterHandler("stall", func(ctx context.Context, job *Job) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return nil
		}
	})
	if err := scheduler2.Schedule(&Job{ID: "fp-race", Name: "stall", Type: "stall", TriggerAt: time.Now()}); err != nil {
		t.Fatalf("Schedule 失败: %v", err)
	}
	scheduler2.Start()
	defer func() {
		close(release)
		scheduler2.Stop()
	}()

	scheduler2.waitRunning(t, "fp-race", 2*time.Second)
	if _, err := scheduler2.ForcePause("fp-race"); err != nil {
		t.Fatalf("ForcePause 失败: %v", err)
	}

	// Handler 还没返回时恢复：会与"即将钉回 paused"的守卫打架，必须被拒绝
	if _, err := scheduler2.Resume("fp-race"); !errors.Is(err, ErrJobNotPending) {
		t.Errorf("强制暂停未落定时恢复应返回 ErrJobNotPending，实际 %v", err)
	}
}
