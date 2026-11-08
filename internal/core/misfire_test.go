// misfire_test.go 是 TASK-RE06 的行为测试：misfire 判定（宽限/策略/封顶）、
// Restore 与 Schedule 两个判定点、时区展开与 DST 边界。
package core

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestCronParser_CountMissedWindows：窗口计数、封顶截断、空区间。
func TestCronParser_CountMissedWindows(t *testing.T) {
	parser := NewCronParser()
	// 每分钟一次：30 分钟跨度 = 30 个窗口
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	from := now.Add(-30 * time.Minute)
	count, truncated := parser.CountMissedWindows("0 * * * * *", from, now)
	require.Equal(t, 30, count)
	require.False(t, truncated)

	// 空区间（to 不晚于 from 的下一个触发）：0 且不截断
	count, truncated = parser.CountMissedWindows("0 * * * * *", now, now)
	require.Equal(t, 0, count)
	require.False(t, truncated)

	// 封顶：每秒一次、2000 秒跨度 → 数到 1000 截断
	count, truncated = parser.CountMissedWindows("* * * * * *", now.Add(-2000*time.Second), now)
	require.Equal(t, maxMisfireWindowScan, count)
	require.True(t, truncated)

	// 非法表达式：0 且不截断（调用方各自有解析报错路径）
	count, truncated = parser.CountMissedWindows("not a cron", from, now)
	require.Equal(t, 0, count)
	require.False(t, truncated)
}

// TestCronParser_NextWithLocation：声明时区后"每天 09:00"按该时区落点。
func TestCronParser_NextWithLocation(t *testing.T) {
	parser := NewCronParser()
	loc, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)

	// 纽约 2026-03-07 09:00 之后的下一个"0 0 9 * * *" = 03-08 09:00（EST）
	//（03-08 是 2026 年春季令切换日，09:00 在切换之后，本地墙钟不变）
	after := time.Date(2026, 3, 7, 9, 0, 0, 0, loc)
	next, err := parser.Next("0 0 9 * * *", after, loc)
	require.NoError(t, err)
	nyNext := next.In(loc)
	require.Equal(t, 9, nyNext.Hour(), "跨 DST 切换后墙钟仍是 09:00")
	require.Equal(t, 3, int(nyNext.Month()))
	require.Equal(t, 8, nyNext.Day())

	// 秋季令（2026-11-01）同理：墙钟 09:00 不漂移
	afterFall := time.Date(2026, 10, 31, 9, 0, 0, 0, loc)
	next, err = parser.Next("0 0 9 * * *", afterFall, loc)
	require.NoError(t, err)
	nyNext = next.In(loc)
	require.Equal(t, 9, nyNext.Hour())
	require.Equal(t, 11, int(nyNext.Month()))
	require.Equal(t, 1, nyNext.Day())
}

// misfireScenario 装配一个带 misfire 配置与事件收集的调度器。
func misfireScenario(t *testing.T, policy string, grace time.Duration) (*Scheduler, *mockStore, *eventLog) {
	t.Helper()
	store := newMockStore()
	scheduler := NewScheduler(store, nil, nil, quietLogger(), WithMisfirePolicy(policy, grace))
	return scheduler, store, collectEvents(scheduler.GetEventBus())
}

// cronSnapshot 构造一条 cron 任务快照，TriggerAt 相对 now 偏移。
func cronSnapshot(id, expr string, triggerAt time.Time, misfirePolicy string) JobSnapshot {
	return JobSnapshot{
		ID: id, Name: id, Type: id,
		CronExpr: expr, IsRepeat: true,
		MisfirePolicy: misfirePolicy,
		TriggerAt:     triggerAt,
		Status:        int(StatusPending),
		CreatedAt:     triggerAt.Add(-time.Hour), UpdatedAt: triggerAt,
	}
}

// TestRestore_MisfireFireOnce：停机跨 3 个窗口，默认策略立即补跑一次——
// 触发时刻保留、派发来源盖 misfire 章、事件带 missed_count=3。
func TestRestore_MisfireFireOnce(t *testing.T) {
	scheduler, store, events := misfireScenario(t, "fire_once", time.Minute)
	now := time.Now()
	// 每分钟一次，起点 3 个窗口之前
	require.NoError(t, store.Update(cronSnapshot("mis-fire", "0 * * * * *", now.Add(-3*time.Minute), "")))

	require.NoError(t, scheduler.Restore())

	item := scheduler.heap.Get("mis-fire")
	require.NotNil(t, item, "fire_once 应立即入堆补跑")
	job := item.(*Job)
	require.Equal(t, now.Add(-3*time.Minute).UnixNano(), job.TriggerAt.UnixNano(), "触发时刻保留（立即补跑）")
	require.Equal(t, AttemptTriggerMisfire, job.dispatchSource)

	events.waitFor(t, EventJobMisfired, "mis-fire", time.Second)
	misfired, _ := events.last(EventJobMisfired, "mis-fire")
	require.Equal(t, 3, misfired.Metadata["missed_count"])
	require.Equal(t, "fire_once", misfired.Metadata["policy"])
}

// TestRestore_MisfireSkip：skip 策略跳到下一个未来窗口——触发时刻被改写、
// 派发来源回 scheduled（之后那次是正常窗口）、事件留痕。
func TestRestore_MisfireSkip(t *testing.T) {
	scheduler, store, events := misfireScenario(t, "fire_once", time.Minute)
	now := time.Now()
	require.NoError(t, store.Update(cronSnapshot("mis-skip", "0 * * * * *", now.Add(-3*time.Minute), "skip")))

	require.NoError(t, scheduler.Restore())

	item := scheduler.heap.Get("mis-skip")
	require.NotNil(t, item)
	job := item.(*Job)
	require.True(t, job.TriggerAt.After(now), "skip 应改写为下一个未来窗口")
	require.Equal(t, AttemptTriggerScheduled, job.dispatchSource)

	events.waitFor(t, EventJobMisfired, "mis-skip", time.Second)
	misfired, _ := events.last(EventJobMisfired, "mis-skip")
	require.Equal(t, "skip", misfired.Metadata["policy"])
}

// TestRestore_MisfireGrace：宽限内的过期时刻不算错过——无事件、来源保持 restore。
func TestRestore_MisfireGrace(t *testing.T) {
	scheduler, store, events := misfireScenario(t, "fire_once", time.Minute)
	now := time.Now()
	require.NoError(t, store.Update(cronSnapshot("mis-grace", "0 * * * * *", now.Add(-10*time.Second), "")))

	require.NoError(t, scheduler.Restore())

	require.NotNil(t, scheduler.heap.Get("mis-grace"), "宽限内照常入堆")
	require.False(t, events.has(EventJobMisfired, "mis-grace"), "宽限内不发 misfired 事件")
}

// TestRestore_MisfireTruncationCap：病态跨度下计数封顶并带 truncated 标记。
func TestRestore_MisfireTruncationCap(t *testing.T) {
	scheduler, store, events := misfireScenario(t, "fire_once", time.Minute)
	now := time.Now()
	// 每秒一次、2000 秒前 → 数到 1000 截断
	require.NoError(t, store.Update(cronSnapshot("mis-cap", "* * * * * *", now.Add(-2000*time.Second), "")))

	require.NoError(t, scheduler.Restore())

	events.waitFor(t, EventJobMisfired, "mis-cap", time.Second)
	misfire, _ := events.last(EventJobMisfired, "mis-cap")
	require.Equal(t, maxMisfireWindowScan, misfire.Metadata["missed_count"])
	require.Equal(t, true, misfire.Metadata["missed_count_truncated"])
}

// TestSchedule_MisfireSkipOnSubmit：提交侧判定——带着过期 TriggerAt 的 cron
// 任务 + skip 策略，Schedule 直接改写为下一窗口并发事件；任务级字段优先于全局。
func TestSchedule_MisfireSkipOnSubmit(t *testing.T) {
	scheduler, _, events := misfireScenario(t, "fire_once", time.Minute)

	job := &Job{
		ID: "submit-skip", Name: "tick", Type: "tick",
		CronExpr: "0 * * * * *", IsRepeat: true,
		MisfirePolicy: "skip",
		TriggerAt:     time.Now().Add(-5 * time.Minute),
	}
	require.NoError(t, scheduler.Schedule(job))

	require.True(t, job.TriggerAt.After(time.Now()), "skip 应改写为下一个未来窗口")
	events.waitFor(t, EventJobMisfired, "submit-skip", time.Second)
	misfire, _ := events.last(EventJobMisfired, "submit-skip")
	require.Equal(t, 5, misfire.Metadata["missed_count"])
}

// TestSchedule_MisfireWithinGraceKeepsMoment：宽限内的过期时刻原样保留
//（Schedule 判定点与 Restore 同一口径），无事件。
func TestSchedule_MisfireWithinGraceKeepsMoment(t *testing.T) {
	scheduler, _, events := misfireScenario(t, "fire_once", time.Minute)

	past := time.Now().Add(-10 * time.Second)
	job := &Job{
		ID: "submit-grace", Name: "tick", Type: "tick",
		CronExpr: "0 * * * * *", IsRepeat: true,
		TriggerAt: past,
	}
	require.NoError(t, scheduler.Schedule(job))

	require.Equal(t, past.UnixNano(), job.TriggerAt.UnixNano(), "宽限内保留原时刻（立即触发）")
	require.False(t, events.has(EventJobMisfired, "submit-grace"))
}

// TestJobSnapshot_TimezoneRoundTrip：时区与策略随快照往返不丢。
func TestJobSnapshot_TimezoneRoundTrip(t *testing.T) {
	job := &Job{
		ID: "tz-1", Name: "tick", CronExpr: "0 0 9 * * *", IsRepeat: true,
		Timezone: "America/New_York", MisfirePolicy: "skip",
	}
	snap := job.ToSnapshot()
	require.Equal(t, "America/New_York", snap.Timezone)
	require.Equal(t, "skip", snap.MisfirePolicy)

	restored := &Job{}
	restored.FromSnapshot(snap)
	require.Equal(t, "America/New_York", restored.Timezone)
	require.Equal(t, "skip", restored.MisfirePolicy)
	require.Equal(t, locNY(t), restored.EffectiveLocation())
}

// TestJob_EffectiveLocationFallback：非法时区名回本地（提交侧已拒，这里是防御）。
func TestJob_EffectiveLocationFallback(t *testing.T) {
	job := &Job{Timezone: "Not/AZone"}
	require.Equal(t, time.Local, job.EffectiveLocation())
	job2 := &Job{}
	require.Equal(t, time.Local, job2.EffectiveLocation())
}

func locNY(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	return loc
}
