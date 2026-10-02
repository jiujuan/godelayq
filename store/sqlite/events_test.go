package sqlite

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"godelayq/core"
)

// fixedTime 是本文件统一使用的"当前时间"：所有需要时间戳的地方都由它推出来，
// 用例不依赖真实时钟，也不靠 sleep 等结果（本卡 §9 风险 3）。
var fixedTime = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// newTestEventLog 建一份真库 + 真总线 + 事件写入器。
// 默认的 FlushInterval 是一小时，也就是说落盘只由用例显式 Flush 或 Close 触发。
func newTestEventLog(t *testing.T, tune func(*EventLogOptions)) (*EventLog, *DB, *core.EventBus) {
	t.Helper()

	db, err := Open(openConfig(filepath.Join(t.TempDir(), "observe.sqlite")), testLogger())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	bus := core.NewEventBus(100)

	opts := EventLogOptions{
		FlushInterval: time.Hour,
		QueueCapacity: 4096,
		Now:           func() time.Time { return fixedTime },
	}
	if tune != nil {
		tune(&opts)
	}
	log, err := NewEventLog(bus, db, opts, testLogger())
	if err != nil {
		t.Fatalf("NewEventLog failed: %v", err)
	}
	// 关闭顺序照装配方：先撤订阅再关连接（幂等，重复关闭无害）
	t.Cleanup(func() {
		_ = log.Close()
		_ = db.Close()
	})
	return log, db, bus
}

// waitQueued 等到写入器队列里的条数达到 n。转发协程是异步的，用例要先确认事件
// 已经从总线走到队列，才能谈"落盘了几条"；这里等的是条件，不是固定时长。
func waitQueued(t *testing.T, log *EventLog, n int) {
	t.Helper()

	deadline := time.After(2 * time.Second)
	for log.batch.queued() != n {
		select {
		case <-deadline:
			t.Fatalf("queue did not reach %d events in time, got %d", n, log.batch.queued())
		case <-time.After(200 * time.Microsecond):
		}
	}
}

// queryEvents 按 seq 升序读回库内的事件行，用于对照写入顺序。
func queryEvents(t *testing.T, db *DB) []eventRecord {
	t.Helper()

	rows, err := db.sqlDB.Query(`SELECT ts_us, type, job_id, job_name, status, data, metadata FROM job_events ORDER BY seq`)
	if err != nil {
		t.Fatalf("query events failed: %v", err)
	}
	defer rows.Close()

	var got []eventRecord
	for rows.Next() {
		var record eventRecord
		if err := rows.Scan(&record.timestampUS, &record.typ, &record.jobID, &record.jobName,
			&record.status, &record.data, &record.metadata); err != nil {
			t.Fatalf("scan events failed: %v", err)
		}
		got = append(got, record)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate events failed: %v", err)
	}
	return got
}

func TestEventLog_PersistsAllEventTypes(t *testing.T) {
	log, db, bus := newTestEventLog(t, nil)

	types := []struct {
		eventType core.EventType
		status    core.JobStatus
	}{
		{core.EventJobScheduled, core.StatusPending},
		{core.EventJobStarted, core.StatusRunning},
		{core.EventJobCompleted, core.StatusSuccess},
		{core.EventJobFailed, core.StatusFailed},
		{core.EventJobCancelled, core.StatusCancelled},
		{core.EventJobRetrying, core.StatusPending},
		{core.EventJobPaused, core.StatusPaused},
		{core.EventJobResumed, core.StatusPending},
	}
	for i, item := range types {
		bus.Publish(core.Event{
			Type:      item.eventType,
			JobID:     fmt.Sprintf("job-%d", i),
			JobName:   "demo",
			Status:    item.status,
			Timestamp: fixedTime.Add(time.Duration(i) * time.Second),
		})
	}
	waitQueued(t, log, len(types))
	if err := log.Flush(); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	count, err := log.Count()
	if err != nil {
		t.Fatalf("Count failed: %v", err)
	}
	if count != int64(len(types)) {
		t.Fatalf("expected %d rows, got %d", len(types), count)
	}

	got := queryEvents(t, db)
	for i, item := range types {
		if got[i].typ != string(item.eventType) {
			t.Errorf("row %d: expected type %q, got %q", i, item.eventType, got[i].typ)
		}
		if got[i].status != int(item.status) {
			t.Errorf("row %d: expected status %d, got %d", i, item.status, got[i].status)
		}
		if got[i].jobID != fmt.Sprintf("job-%d", i) || got[i].jobName != "demo" {
			t.Errorf("row %d: job fields wrong: %+v", i, got[i])
		}
	}
	// 八种类型各自一条，说明写入侧没有按类型漏挂（设计文档 §4.1 的 8 种 job.*）
	if len(got) != 8 {
		t.Fatalf("expected 8 rows, got %d", len(got))
	}
}

func TestEventLog_IgnoresEventsWithoutJobID(t *testing.T) {
	log, db, bus := newTestEventLog(t, nil)

	// 没有归属的事件（api/history.go 的 record 同样跳过）塞进任何任务的时间线都是噪音
	bus.Publish(core.Event{Type: core.EventHeapUpdate, JobID: "", Status: core.StatusPending, Timestamp: fixedTime})
	if err := log.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	if count, err := log.Count(); err != nil || count != 0 {
		t.Fatalf("expected no rows for an ownerless event, got count=%d err=%v", count, err)
	}
	if got := queryEvents(t, db); len(got) != 0 {
		t.Fatalf("expected an empty table, got %v", got)
	}
	// 跳过不是丢弃：这条不该计入 Dropped，否则队列丢多少就看不出真实原因了
	if dropped := log.Dropped(); dropped != 0 {
		t.Fatalf("skipping an ownerless event must not count as a drop, got %d", dropped)
	}
}

func TestEventLog_PreservesDataAndMetadata(t *testing.T) {
	log, db, bus := newTestEventLog(t, nil)

	raw := json.RawMessage(`{"error":"exit status 3","hint":"keep 原文"}`)
	bus.Publish(core.Event{
		Type:      core.EventJobFailed,
		JobID:     "job-1",
		Status:    core.StatusFailed,
		Timestamp: fixedTime,
		Data:      raw,
		Metadata:  map[string]any{"retry_count": 2, "permanent": true},
	})
	bus.Publish(core.Event{
		Type:      core.EventJobScheduled,
		JobID:     "job-2",
		Status:    core.StatusPending,
		Timestamp: fixedTime,
	})
	waitQueued(t, log, 2)
	if err := log.Flush(); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	got := queryEvents(t, db)
	if len(got) != 2 {
		t.Fatalf("expected two rows, got %d", len(got))
	}
	// data 存原文：重新序列化会改键序，排障时比对的就不是同一段文本了
	if got[0].data.String != string(raw) || !got[0].data.Valid {
		t.Fatalf("expected the raw JSON in data, got %q (valid=%v)", got[0].data.String, got[0].data.Valid)
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(got[0].metadata.String), &metadata); err != nil {
		t.Fatalf("metadata is not valid JSON: %v (column=%q)", err, got[0].metadata.String)
	}
	if metadata["retry_count"] != float64(2) || metadata["permanent"] != true {
		t.Fatalf("metadata keys drifted after the round trip: %v", metadata)
	}
	// 没有 Data 的事件存 NULL，而不是字符串 "null"
	if got[1].data.Valid {
		t.Fatalf("expected a NULL data column for an event without data, got %q", got[1].data.String)
	}
	if got[1].metadata.Valid {
		t.Fatalf("expected a NULL metadata column, got %q", got[1].metadata.String)
	}
}

// TestEventLog_MapEvent 覆盖映射函数本身（纯函数，不碰库）：
// 三种边界都在这一条里钉住——无归属跳过、零时间戳走注入的时间源、元数据序列化失败存 NULL。
func TestEventLog_MapEvent(t *testing.T) {
	log, _, _ := newTestEventLog(t, nil)

	if _, ok := log.mapEvent(core.Event{Type: core.EventHeapUpdate}); ok {
		t.Error("an event without a job id must be skipped")
	}

	record, ok := log.mapEvent(core.Event{Type: core.EventJobStarted, JobID: "j", Status: core.StatusRunning})
	if !ok {
		t.Fatal("a normal event must be mapped")
	}
	if record.timestampUS != fixedTime.UnixMicro() {
		t.Errorf("expected the injected Now for a zero timestamp, got %d", record.timestampUS)
	}
	if record.status != int(core.StatusRunning) {
		t.Errorf("expected status %d, got %d", core.StatusRunning, record.status)
	}
	if record.data.Valid || record.metadata.Valid {
		t.Errorf("expected both optional columns to be NULL, got %+v", record)
	}

	// 含 channel 的元数据无法序列化：这条存 NULL 并记 warn，不能挡住整批
	broken, ok := log.mapEvent(core.Event{
		Type:     core.EventJobFailed,
		JobID:    "j",
		Metadata: map[string]any{"bad": make(chan int)},
	})
	if !ok {
		t.Fatal("an unserializable metadata must not drop the whole event")
	}
	if broken.metadata.Valid {
		t.Errorf("expected a NULL metadata column after the marshal failure, got %q", broken.metadata.String)
	}
}

func TestEventLog_OrderIsStableWithinSameMicrosecond(t *testing.T) {
	log, db, bus := newTestEventLog(t, nil)

	// 一次执行会在同一毫秒内连发 scheduled 与 started：只看 ts_us 排不出因果，
	// 这条用例守的就是"必须有 seq 作为定序依据"（设计文档 §6）
	for _, item := range []core.EventType{core.EventJobScheduled, core.EventJobStarted} {
		bus.Publish(core.Event{Type: item, JobID: "job-1", Status: core.StatusPending, Timestamp: fixedTime})
	}
	waitQueued(t, log, 2)
	if err := log.Flush(); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	got := queryEvents(t, db)
	if len(got) != 2 {
		t.Fatalf("expected two rows, got %d", len(got))
	}
	if got[0].typ != string(core.EventJobScheduled) || got[1].typ != string(core.EventJobStarted) {
		t.Fatalf("expected the publish order back from the table, got %q then %q", got[0].typ, got[1].typ)
	}
	if got[0].timestampUS != got[1].timestampUS {
		t.Fatalf("the two rows must carry the same timestamp for this case to mean anything: %d vs %d",
			got[0].timestampUS, got[1].timestampUS)
	}
	// seq 由 AUTOINCREMENT 给出并且严格递增，是读侧 ORDER BY 的依据
	var seqs []int64
	rows, err := db.sqlDB.Query(`SELECT seq FROM job_events ORDER BY seq`)
	if err != nil {
		t.Fatalf("query seq failed: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			t.Fatalf("scan seq failed: %v", err)
		}
		seqs = append(seqs, seq)
	}
	if len(seqs) != 2 || !(seqs[0] < seqs[1]) {
		t.Fatalf("expected two increasing seq values, got %v", seqs)
	}
}

// TestEventLog_DoesNotBlockPublisher 是 D4 的直接证据：队列打满时发布方照常返回。
// 断言的是耗时上界，不是落库条数——落库要等 Flush，而这条用例故意不让它落。
func TestEventLog_DoesNotBlockPublisher(t *testing.T) {
	const total = 400
	log, _, bus := newTestEventLog(t, func(opts *EventLogOptions) {
		opts.QueueCapacity = 4
	})

	start := time.Now()
	for i := 0; i < total; i++ {
		bus.Publish(core.Event{
			Type:      core.EventJobScheduled,
			JobID:     fmt.Sprintf("job-%d", i),
			Status:    core.StatusPending,
			Timestamp: fixedTime,
		})
	}
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("publishing %d events took %v; the writer is putting backpressure on the bus", total, elapsed)
	}
	if log.batch.queued() > 4 {
		t.Fatalf("the queue must stay bounded by its capacity, got %d", log.batch.queued())
	}
}

// TestEventLog_DroppedIsCounted 钉住"少掉的每条都记在账上"。
//
// 发的条数正好等于总线给每个订阅者分配的缓冲（core.NewEventBus(100)），这样事件一律能
// 进到转发协程，写入器这边的账才是确定的：落库 4 条、丢弃 96 条。发得更多就会先在总线
// 那一层丢掉一部分（Publish 同样是 select/default），那部分不计进 Dropped()，
// 等式就不成立了——它守的是写入器的账，不是总线的账。
func TestEventLog_DroppedIsCounted(t *testing.T) {
	const total = 100
	log, _, bus := newTestEventLog(t, func(opts *EventLogOptions) {
		opts.QueueCapacity = 4
	})

	for i := 0; i < total; i++ {
		bus.Publish(core.Event{
			Type:      core.EventJobScheduled,
			JobID:     fmt.Sprintf("job-%d", i),
			Status:    core.StatusPending,
			Timestamp: fixedTime,
		})
	}
	// Close 会撤订阅、等转发协程把总线里剩下的都读完，再落最后一批：
	// 因此这里的数字是确定的，不需要等时间
	if err := log.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	count, err := log.Count()
	if err != nil {
		t.Fatalf("Count failed: %v", err)
	}
	if count != 4 {
		t.Fatalf("expected exactly the queue capacity to land, got %d", count)
	}
	dropped := log.Dropped()
	if int64(total)-count != dropped {
		t.Fatalf("expected %d dropped, got %d", int64(total)-count, dropped)
	}
	// 丢弃的条数必须能在写入器这一侧读到，否则一张看起来完整的表实际缺页无从判断
	if dropped == 0 {
		t.Fatal("expected the writer to count the records it gave up")
	}
}

func TestEventLog_RetentionCount(t *testing.T) {
	log, db, bus := newTestEventLog(t, func(opts *EventLogOptions) {
		opts.RetentionCount = 5
	})

	for i := 0; i < 20; i++ {
		bus.Publish(core.Event{
			Type:      core.EventJobScheduled,
			JobID:     fmt.Sprintf("job-%d", i),
			Status:    core.StatusPending,
			Timestamp: fixedTime.Add(time.Duration(i) * time.Minute),
		})
	}
	waitQueued(t, log, 20)
	if err := log.Flush(); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	got := queryEvents(t, db)
	if len(got) != 5 {
		t.Fatalf("expected the retention count to keep 5 rows, got %d", len(got))
	}
	// 留的是最新的 5 条，不是最早那 5 条
	for i, record := range got {
		want := fmt.Sprintf("job-%d", 15+i)
		if record.jobID != want {
			t.Fatalf("expected the newest rows, got %v (row %d should be %s)", jobIDs(got), i, want)
		}
	}
}

func TestEventLog_RetentionAge(t *testing.T) {
	log, db, bus := newTestEventLog(t, func(opts *EventLogOptions) {
		opts.RetentionAge = time.Hour
	})

	// 第一批：三小时前的两条（注入的 Now 固定在 fixedTime，所以截止点是 fixedTime-1h）
	for i := 0; i < 2; i++ {
		bus.Publish(core.Event{
			Type: core.EventJobCompleted, JobID: fmt.Sprintf("old-%d", i),
			Status: core.StatusSuccess, Timestamp: fixedTime.Add(-3 * time.Hour),
		})
	}
	waitQueued(t, log, 2)
	if err := log.Flush(); err != nil {
		t.Fatalf("first Flush failed: %v", err)
	}
	if count, err := log.Count(); err != nil || count != 0 {
		t.Fatalf("expected the over-age rows pruned away, got count=%d err=%v", count, err)
	}

	// 第二批：十分钟前的两条，落在保留窗口内
	for i := 0; i < 2; i++ {
		bus.Publish(core.Event{
			Type: core.EventJobStarted, JobID: fmt.Sprintf("new-%d", i),
			Status: core.StatusRunning, Timestamp: fixedTime.Add(-10 * time.Minute),
		})
	}
	waitQueued(t, log, 2)
	if err := log.Flush(); err != nil {
		t.Fatalf("second Flush failed: %v", err)
	}
	got := queryEvents(t, db)
	if len(got) != 2 || got[0].jobID != "new-0" || got[1].jobID != "new-1" {
		t.Fatalf("expected only the recent rows, got %v", jobIDs(got))
	}
}

func TestEventLog_RetentionAgeZeroKeepsEverything(t *testing.T) {
	log, db, bus := newTestEventLog(t, func(opts *EventLogOptions) {
		opts.RetentionAge = 0
		opts.RetentionCount = 1000
	})

	// 归一化之后的配置里 retention_age=0 就是"不按时间淘汰"，一条都不该被删
	bus.Publish(core.Event{Type: core.EventJobCompleted, JobID: "ancient", Status: core.StatusSuccess,
		Timestamp: fixedTime.Add(-400 * 24 * time.Hour)})
	waitQueued(t, log, 1)
	if err := log.Flush(); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	if got := queryEvents(t, db); len(got) != 1 || got[0].jobID != "ancient" {
		t.Fatalf("expected the four-hundred-day-old row to survive, got %v", jobIDs(got))
	}
}

func TestEventLog_CloseUnsubscribes(t *testing.T) {
	log, _, bus := newTestEventLog(t, nil)

	bus.Publish(core.Event{Type: core.EventJobScheduled, JobID: "before", Status: core.StatusPending, Timestamp: fixedTime})
	waitQueued(t, log, 1)
	if err := log.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	before, err := log.Count()
	if err != nil {
		t.Fatalf("Count failed: %v", err)
	}

	// Close 之后再发布：订阅已经撤掉，总线不会再把事件投给这里，也不会有"向已关闭连接写入"
	for i := 0; i < 5; i++ {
		bus.Publish(core.Event{Type: core.EventJobStarted, JobID: fmt.Sprintf("after-%d", i),
			Status: core.StatusRunning, Timestamp: fixedTime})
	}
	if err := log.Close(); err != nil {
		t.Fatalf("the repeated Close must stay clean, got %v", err)
	}
	after, err := log.Count()
	if err != nil {
		t.Fatalf("Count failed: %v", err)
	}
	if after != before {
		t.Fatalf("expected no growth after Close, got %d before and %d after", before, after)
	}
	// Close 之后的 Flush 返回错误而不是 panic
	if err := log.Flush(); !errors.Is(err, ErrBatcherClosed) {
		t.Fatalf("expected ErrBatcherClosed after Close, got %v", err)
	}
}

func TestEventLog_FlushIsNoOpWhenEmpty(t *testing.T) {
	log, db, bus := newTestEventLog(t, nil)

	for i := 0; i < 5; i++ {
		if err := log.Flush(); err != nil {
			t.Fatalf("Flush #%d failed: %v", i+1, err)
		}
	}
	if got := log.writeRounds.Load(); got != 0 {
		t.Fatalf("expected five empty flushes to open no transaction, got %d write rounds", got)
	}

	// 真写了两条之后再空转五轮：落盘轮次仍然是 1，行数也没变
	bus.Publish(core.Event{Type: core.EventJobScheduled, JobID: "a", Status: core.StatusPending, Timestamp: fixedTime})
	bus.Publish(core.Event{Type: core.EventJobStarted, JobID: "a", Status: core.StatusRunning, Timestamp: fixedTime})
	waitQueued(t, log, 2)
	if err := log.Flush(); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}
	roundsAfterWrite := log.writeRounds.Load()
	for i := 0; i < 5; i++ {
		_ = log.Flush()
	}
	if got := log.writeRounds.Load(); got != roundsAfterWrite {
		t.Fatalf("expected no extra write rounds, got %d instead of %d", got, roundsAfterWrite)
	}
	if got := queryEvents(t, db); len(got) != 2 {
		t.Fatalf("expected two rows, got %d", len(got))
	}
}

func TestNewEventLog_RejectsNilDeps(t *testing.T) {
	db, err := Open(openConfig(filepath.Join(t.TempDir(), "observe.sqlite")), testLogger())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()
	bus := core.NewEventBus(10)

	if _, err := NewEventLog(nil, db, EventLogOptions{}, testLogger()); err == nil {
		t.Error("expected a nil bus to be rejected")
	}
	if _, err := NewEventLog(bus, nil, EventLogOptions{}, testLogger()); err == nil {
		t.Error("expected a nil database to be rejected")
	}
}

// TestEventLog_DefaultsForNonPositiveOptions 固定写入器对非正取值的解释：
// 条数上界回落到默认值（0 会被读成"不限量"，那等于让漏配把表推成无界增长），
// 而时长上界的 0 保持"不按时间淘汰"。
func TestEventLog_DefaultsForNonPositiveOptions(t *testing.T) {
	log, _, _ := newTestEventLog(t, func(opts *EventLogOptions) {
		opts.RetentionCount = 0
		opts.RetentionAge = -time.Hour
		opts.QueueCapacity = 0
		opts.FlushInterval = 0
	})

	// 两个策略位现在是原子字段（重载链会运行期写它们），断言只换读法：
	// 补齐口径仍然是 NewEventLog 那一条，取值一个没变。
	if log.retentionCount.Load() != int64(core.DefaultObserveEventRetentionCount) {
		t.Errorf("expected the default retention count, got %d", log.retentionCount.Load())
	}
	if log.retentionAge.Load() != 0 {
		t.Errorf("expected a negative age to become 0 (no time pruning), got %d", log.retentionAge.Load())
	}
	if log.batch.capacity != core.DefaultObserveQueueCapacity || log.batch.interval != core.DefaultObserveFlushInterval {
		t.Errorf("expected queue defaults to come from core, got capacity=%d interval=%v",
			log.batch.capacity, log.batch.interval)
	}
}

// TestEventLog_WriteFailureIsRetriedOnce 把 batcher 的"重试一次"接到真实 SQL 上：
// 库被关掉之后落盘失败，第二批也失败，两次失败之后数据按丢弃收口而不是无限重试。
func TestEventLog_WriteFailureIsRetriedOnce(t *testing.T) {
	log, db, bus := newTestEventLog(t, nil)
	path := db.Path()

	if err := db.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	for i := 0; i < 3; i++ {
		bus.Publish(core.Event{Type: core.EventJobScheduled, JobID: fmt.Sprintf("job-%d", i),
			Status: core.StatusPending, Timestamp: fixedTime})
	}
	waitQueued(t, log, 3)

	if err := log.Flush(); err == nil {
		t.Fatal("expected the first flush against a closed database to fail")
	}
	if got := log.Dropped(); got != 0 {
		t.Fatalf("a first failure must keep the batch, got %d dropped", got)
	}
	if err := log.Flush(); err == nil {
		t.Fatal("expected the retry to fail as well")
	}
	if got := log.Dropped(); got != 3 {
		t.Fatalf("expected all three records counted as dropped, got %d", got)
	}

	// 写失败不该把库文件弄坏：重新打开还能读到那张表，只是这三条没进去
	again, err := Open(openConfig(path), testLogger())
	if err != nil {
		t.Fatalf("reopening after the failed writes should work: %v", err)
	}
	defer again.Close()
	if rows := queryEvents(t, again); len(rows) != 0 {
		t.Fatalf("expected nothing to have landed, got %v", jobIDs(rows))
	}
}

func jobIDs(records []eventRecord) []string {
	out := make([]string, len(records))
	for i, record := range records {
		out[i] = record.jobID
	}
	return out
}

// --- 读侧（TASK-S04）：两个端点要拿到的形状 ---

// TestEventLog_EventsRoundTrip 写进去的一批事件经 Events() 读回后逐字段等于原值。
//
// 时间戳的断言口径是"同一时刻、精度到微秒"：库里 ts_us 只存到微秒，纳秒部分在写入时就丢了，
// 所以这里给原值带上纳秒，比较用微秒截断后的相等（本卡 §9 第一条风险的固定点）。
// 顺带一条同样要写进接口文档的：time.UnixMicro 还原出来的是本地时区表示的同一时刻，
// 因此 JSON 里的时区偏移与写入时不一定相同，比较时刻而不是文本。
func TestEventLog_EventsRoundTrip(t *testing.T) {
	log, _, bus := newTestEventLog(t, nil)

	data := json.RawMessage(`{"error":"boom","stdout":"a\nb"}`)
	original := core.Event{
		Type:      core.EventJobFailed,
		JobID:     "job-rt",
		JobName:   "payment_check",
		Status:    core.StatusFailed,
		Timestamp: fixedTime.Add(123 * time.Nanosecond),
		Data:      data,
		Metadata: map[string]interface{}{
			"retry_count": 2.0,
			"permanent":   false,
			"trigger_at":  "2026-10-01T12:00:00Z",
		},
	}
	bus.Publish(original)
	waitQueued(t, log, 1)
	if err := log.Flush(); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	got, err := log.Events("job-rt", 0)
	if err != nil {
		t.Fatalf("Events failed: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}

	read := got[0]
	if read.Type != original.Type || read.JobID != original.JobID ||
		read.JobName != original.JobName || read.Status != original.Status {
		t.Fatalf("identity fields changed: %+v", read)
	}
	if string(read.Data) != string(data) {
		t.Errorf("data must come back byte for byte, got %q", string(read.Data))
	}
	if !read.Timestamp.Truncate(time.Microsecond).Equal(original.Timestamp.Truncate(time.Microsecond)) {
		t.Errorf("expected %v, got %v", original.Timestamp, read.Timestamp)
	}
	if read.Metadata["retry_count"] != 2.0 || read.Metadata["permanent"] != false ||
		read.Metadata["trigger_at"] != "2026-10-01T12:00:00Z" {
		t.Errorf("metadata decoded wrong: %#v", read.Metadata)
	}
}

// TestEventLog_EventsAscending 库里 10 条同任务事件，limit=3 拿到的是最后 3 条且升序。
// 降序查询 + 整体反转是读侧唯一的定序办法，这条把它钉住。
func TestEventLog_EventsAscending(t *testing.T) {
	log, _, bus := newTestEventLog(t, nil)

	for i := 0; i < 10; i++ {
		bus.Publish(core.Event{
			Type: core.EventType(fmt.Sprintf("job.e%d", i)), JobID: "job-order",
			Status: core.StatusRunning, Timestamp: fixedTime.Add(time.Duration(i) * time.Second),
		})
	}
	waitQueued(t, log, 10)
	if err := log.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	got, err := log.Events("job-order", 3)
	if err != nil {
		t.Fatalf("Events failed: %v", err)
	}
	want := []string{"job.e7", "job.e8", "job.e9"}
	if len(got) != len(want) {
		t.Fatalf("expected %d events, got %d", len(want), len(got))
	}
	for i, wantType := range want {
		if string(got[i].Type) != wantType {
			t.Errorf("position %d: expected %s, got %s", i, wantType, got[i].Type)
		}
	}

	// limit 给得比库存还大时不报错，返回全部
	all, err := log.Events("job-order", 50)
	if err != nil {
		t.Fatalf("Events with a larger limit failed: %v", err)
	}
	if len(all) != 10 {
		t.Fatalf("expected the whole set, got %d", len(all))
	}
}

// TestEventLog_EventsOrderWithinSameMicrosecond 同一微秒连发的两条按 seq 定序读回，
// 而不是并列乱序：时间线把 scheduled 在 started 之前当作因果。
func TestEventLog_EventsOrderWithinSameMicrosecond(t *testing.T) {
	log, _, bus := newTestEventLog(t, nil)

	bus.Publish(core.Event{Type: core.EventJobScheduled, JobID: "job-tie",
		Status: core.StatusPending, Timestamp: fixedTime})
	bus.Publish(core.Event{Type: core.EventJobStarted, JobID: "job-tie",
		Status: core.StatusRunning, Timestamp: fixedTime})
	waitQueued(t, log, 2)
	if err := log.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	got, err := log.Events("job-tie", 0)
	if err != nil {
		t.Fatalf("Events failed: %v", err)
	}
	if len(got) != 2 || got[0].Type != core.EventJobScheduled || got[1].Type != core.EventJobStarted {
		t.Fatalf("expected scheduled then started, got %+v", got)
	}
	if !got[0].Timestamp.Equal(got[1].Timestamp) {
		t.Fatalf("the two events were meant to share a timestamp, got %v and %v",
			got[0].Timestamp, got[1].Timestamp)
	}
}

// TestEventLog_NilMetadata 写入时 Metadata 与 Data 都是 nil，读回必须是 nil 而不是空 map：
// core.Event 的两个字段都带 omitempty，空值会让响应的 JSON 形状多出一对键。
func TestEventLog_NilMetadata(t *testing.T) {
	log, _, bus := newTestEventLog(t, nil)

	bus.Publish(core.Event{Type: core.EventJobStarted, JobID: "job-nil",
		Status: core.StatusRunning, Timestamp: fixedTime})
	waitQueued(t, log, 1)
	if err := log.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	got, err := log.Events("job-nil", 0)
	if err != nil {
		t.Fatalf("Events failed: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	if got[0].Metadata != nil {
		t.Errorf("expected nil metadata, got %#v", got[0].Metadata)
	}
	if got[0].Data != nil {
		t.Errorf("expected nil data, got %q", string(got[0].Data))
	}

	encoded, err := json.Marshal(got[0])
	if err != nil {
		t.Fatalf("marshaling the read event failed: %v", err)
	}
	if strings.Contains(string(encoded), `"metadata"`) || strings.Contains(string(encoded), `"data"`) {
		t.Fatalf("an event without metadata or data must not carry those keys, got %s", encoded)
	}
}

// TestEventLog_Recent 全局读取跨任务、按写入顺序升序，且与单任务读取共用一套还原逻辑。
func TestEventLog_Recent(t *testing.T) {
	log, _, bus := newTestEventLog(t, nil)

	for _, id := range []string{"job-a", "job-b"} {
		bus.Publish(core.Event{Type: core.EventJobScheduled, JobID: id,
			Status: core.StatusPending, Timestamp: fixedTime})
	}
	waitQueued(t, log, 2)
	bus.Publish(core.Event{Type: core.EventJobCompleted, JobID: "job-a",
		Status: core.StatusSuccess, Timestamp: fixedTime.Add(time.Second)})
	waitQueued(t, log, 3)
	if err := log.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	all, err := log.Recent(0)
	if err != nil {
		t.Fatalf("Recent failed: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 events, got %d", len(all))
	}

	tail, err := log.Recent(2)
	if err != nil {
		t.Fatalf("Recent with a limit failed: %v", err)
	}
	if len(tail) != 2 || tail[0].JobID != "job-b" || tail[1].Type != core.EventJobCompleted {
		t.Fatalf("expected the last two in write order, got %+v", tail)
	}
}

// TestEventLog_EventsIsolateJobs 一个任务的时间线不该出现另一个任务的事件。
func TestEventLog_EventsIsolateJobs(t *testing.T) {
	log, _, bus := newTestEventLog(t, nil)

	for _, id := range []string{"job-a", "job-b", "job-a"} {
		bus.Publish(core.Event{Type: core.EventJobStarted, JobID: id,
			Status: core.StatusRunning, Timestamp: fixedTime})
	}
	waitQueued(t, log, 3)
	if err := log.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	got, err := log.Events("job-a", 0)
	if err != nil {
		t.Fatalf("Events failed: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected only job-a's two events, got %+v", got)
	}
	for _, event := range got {
		if event.JobID != "job-a" {
			t.Fatalf("the timeline leaked another job: %+v", event)
		}
	}
}

// TestEventLog_EventsAfterClose 读方法只依赖连接，不依赖订阅：写入器关停后仍可查，
// 否则关停顺序里任何一个时点都会让端点突然 500。
func TestEventLog_EventsAfterClose(t *testing.T) {
	log, _, bus := newTestEventLog(t, nil)

	bus.Publish(core.Event{Type: core.EventJobScheduled, JobID: "job-late",
		Status: core.StatusPending, Timestamp: fixedTime})
	waitQueued(t, log, 1)
	if err := log.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	got, err := log.Events("job-late", 0)
	if err != nil {
		t.Fatalf("Events after Close failed: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected the flushed event, got %+v", got)
	}
}
