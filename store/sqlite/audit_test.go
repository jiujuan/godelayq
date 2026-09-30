package sqlite

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"godelayq/api"
)

// auditFixedTime 是本文件统一使用的"当前时间"：行上的时间是请求到达的那一刻，
// 用例由它推时间窗，不依赖真实时钟。
var auditFixedTime = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// newTestAuditLog 建一份真库 + 真台账写入器。
// 默认落盘周期是一小时，也就是说写盘只由用例显式 Flush 或 Close 触发（与事件用例同形）。
func newTestAuditLog(t *testing.T, tune func(*AuditLogOptions)) (*AuditLog, *DB) {
	t.Helper()

	db, err := Open(openConfig(filepath.Join(t.TempDir(), "observe.sqlite")), testLogger())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	opts := AuditLogOptions{
		FlushInterval: time.Hour,
		QueueCapacity: 4096,
		Now:           func() time.Time { return auditFixedTime },
	}
	if tune != nil {
		tune(&opts)
	}
	log, err := NewAuditLog(db, opts, testLogger())
	if err != nil {
		t.Fatalf("NewAuditLog failed: %v", err)
	}
	t.Cleanup(func() {
		_ = log.Close()
		_ = db.Close()
	})
	return log, db
}

// auditEntry 造一行台账，缺省字段是"一次普通的成功建任务"。
// 序号进 Action 之外的字段会让过滤用例的期望值写成一堆字符串比对，所以这里用 Method 拼序号。
func auditEntry(seq int) api.AuditEntry {
	return api.AuditEntry{
		Time:      auditFixedTime.Add(time.Duration(seq) * time.Second),
		Actor:     fmt.Sprintf("operator%02d", seq%3),
		ActorKind: "user",
		Role:      "operator",
		Action:    "job.create",
		Method:    "POST",
		Route:     "/api/v1/jobs",
		Status:    201,
		Latency:   time.Duration(1000+seq) * time.Microsecond,
		Verdict:   "ok",
		RemoteIP:  "127.0.0.1",
		UserAgent: "curl/8",
	}
}

// mustFlush 落一次盘，失败就让用例停在这里。
func mustFlush(t *testing.T, log *AuditLog) {
	t.Helper()

	if err := log.Flush(); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}
}

// mustQuery 读一次台账。
func mustQuery(t *testing.T, log *AuditLog, f api.AuditFilter) ([]api.AuditEntry, int) {
	t.Helper()

	items, total, err := log.Query(f)
	if err != nil {
		t.Fatalf("Query failed: %v", err)
	}
	return items, total
}

// appendAll 写入若干行台账。
func appendAll(t *testing.T, log *AuditLog, entries ...api.AuditEntry) {
	t.Helper()

	for _, entry := range entries {
		if err := log.Append(entry); err != nil {
			t.Fatalf("Append failed: %v", err)
		}
	}
}

// TestAuditLog_RoundTrip 把一行台账的所有列写进去再读回来，逐字段对照。
// 时间是微秒精度（库里存 ts_us），所以比对也按微秒取整。
func TestAuditLog_RoundTrip(t *testing.T) {
	log, _ := newTestAuditLog(t, nil)

	executor := auditEntry(1)
	executor.Action = "job.create"
	executor.Method = "POST"
	executor.Verdict = "denied"
	executor.Status = 403
	executor.Actor = ""
	executor.ActorKind = "anonymous"
	executor.Role = ""
	executor.ExecVerdict = "role_denied"
	executor.ExecReasonCode = "admin"
	executor.HandlerKey = "exec.nightly_report"
	executor.Profile = "nightly_report"
	executor.JobID = ""
	executor.UserAgent = strings.Repeat("x", 300)

	plain := auditEntry(2)
	appendAll(t, log, executor, plain)
	mustFlush(t, log)

	items, total := mustQuery(t, log, api.AuditFilter{})
	if total != 2 {
		t.Fatalf("expected 2 rows, got total=%d items=%d", total, len(items))
	}
	// 顺序是 seq 降序：台账的用法是"刚发生了什么"，与事件时间线的升序相反
	if items[0].Action != plain.Action || items[0].Verdict != "ok" {
		t.Fatalf("expected the newest row first, got %+v", items[0])
	}
	if items[0].Time.Before(items[1].Time) {
		t.Fatalf("expected descending order, got %v then %v", items[0].Time, items[1].Time)
	}

	got := items[1]
	if got.ActorKind != "anonymous" {
		t.Errorf("actor_kind = %q", got.ActorKind)
	}
	if got.ExecVerdict != "role_denied" || got.ExecReasonCode != "admin" {
		t.Errorf("exec columns = %q / %q", got.ExecVerdict, got.ExecReasonCode)
	}
	if got.HandlerKey != "exec.nightly_report" || got.Profile != "nightly_report" {
		t.Errorf("handler/profile = %q / %q", got.HandlerKey, got.Profile)
	}
	if got.Status != 403 || got.Verdict != "denied" {
		t.Errorf("status/verdict = %d / %q", got.Status, got.Verdict)
	}
	if want := executor.Latency.Microseconds(); got.Latency.Microseconds() != want {
		t.Errorf("latency = %v, want %d microseconds", got.Latency, want)
	}
	// 按微秒整数比而不是 TimeEqual：库里存的是 ts_us，读回来的 Time 是本地时区表示
	// （S04 同一口径），两个 Time 可以表示同一时刻而 TimeZone 不同。
	if got.Time.UnixMicro() != executor.Time.UnixMicro() {
		t.Errorf("time = %v (%d us), want %v", got.Time, got.Time.UnixMicro(), executor.Time)
	}
	if len(got.UserAgent) != 300 {
		t.Errorf("user agent was not stored verbatim: %d characters", len(got.UserAgent))
	}

	// 普通那一行的执行器四列读回来是空串而不是别的占位值
	newest := items[0]
	if newest.ExecVerdict != "" || newest.HandlerKey != "" || newest.JobID != plain.JobID {
		t.Errorf("the plain row should have empty executor columns, got %+v", newest)
	}
	if newest.JobID != "" {
		t.Errorf("job_id = %q, want empty", newest.JobID)
	}
}

// TestAuditLog_FilterAndPagination 覆盖五个过滤条件与翻页：
// 翻页不重不漏，total 是不带 limit 的匹配数。
func TestAuditLog_FilterAndPagination(t *testing.T) {
	log, _ := newTestAuditLog(t, nil)

	entries := make([]api.AuditEntry, 0, 9)
	for i := 0; i < 9; i++ {
		entry := auditEntry(i)
		switch i {
		case 1, 4:
			entry.Action = "group.delete"
			entry.Verdict = "denied"
			entry.Status = 403
		case 2:
			entry.Action = "job.cancel"
			entry.Verdict = "not_found"
			entry.Status = 404
		case 3:
			entry.Actor = "ops01"
			entry.ActorKind = "machine"
		}
		entries = append(entries, entry)
	}
	appendAll(t, log, entries...)
	mustFlush(t, log)

	cases := []struct {
		name   string
		filter api.AuditFilter
		want   int
	}{
		{"no filter", api.AuditFilter{}, 9},
		{"by actor", api.AuditFilter{Actor: "operator01"}, 3},
		{"by machine actor", api.AuditFilter{Actor: "ops01"}, 1},
		{"by action", api.AuditFilter{Action: "group.delete"}, 2},
		{"by verdict", api.AuditFilter{Verdict: "denied"}, 2},
		{"actor and action", api.AuditFilter{Actor: "operator01", Action: "job.create"}, 1},
		{"unknown actor", api.AuditFilter{Actor: "nobody"}, 0},
		{
			"time window",
			api.AuditFilter{
				Since: pointerTo(auditFixedTime.Add(2 * time.Second)),
				Until: pointerTo(auditFixedTime.Add(5 * time.Second)),
			},
			4,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			items, total := mustQuery(t, log, tc.filter)
			if total != tc.want || len(items) != tc.want {
				t.Fatalf("got %d items / total %d, want %d", len(items), total, tc.want)
			}
		})
	}

	// 翻页：每页 2 条走完全表，行不重不漏
	seen := map[int64]bool{}
	for offset := 0; offset < 9; offset += 2 {
		items, total := mustQuery(t, log, api.AuditFilter{Limit: 2, Offset: offset})
		if total != 9 {
			t.Fatalf("total must ignore paging, got %d", total)
		}
		want := 2
		if offset+2 > 9 {
			want = 9 - offset
		}
		if len(items) != want {
			t.Fatalf("offset %d: got %d items, want %d", offset, len(items), want)
		}
		for _, entry := range items {
			key := entry.Time.UnixMicro()
			if seen[key] {
				t.Fatalf("offset %d re-returned a row already seen", offset)
			}
			seen[key] = true
		}
	}
	if len(seen) != 9 {
		t.Fatalf("paging covered %d rows, want 9", len(seen))
	}

	// 越界 offset 是空列表而不是报错
	items, total := mustQuery(t, log, api.AuditFilter{Limit: 5, Offset: 50})
	if len(items) != 0 || total != 9 {
		t.Fatalf("offset past the end: %d items / total %d", len(items), total)
	}
}

// pointerTo 是给过滤条件用的辅助：Since/Until 是指针，"没传"与"传了零值"是两回事。
func pointerTo(value time.Time) *time.Time { return &value }

// TestAuditLog_ListIsNeverNil 与事件、产物列表同一口径：空结果读出来是空列表。
func TestAuditLog_ListIsNeverNil(t *testing.T) {
	log, _ := newTestAuditLog(t, nil)

	items, total := mustQuery(t, log, api.AuditFilter{})
	if items == nil {
		t.Fatal("Query returned a nil slice for an empty table")
	}
	if len(items) != 0 || total != 0 {
		t.Fatalf("empty table: %d items / total %d", len(items), total)
	}
}

// TestAuditLog_FallbackLimit 守住"忘记传 limit 不会把整表读进内存"。
func TestAuditLog_FallbackLimit(t *testing.T) {
	log, db := newTestAuditLog(t, nil)

	entries := make([]api.AuditEntry, 0, auditReadLimitFallback+5)
	for i := 0; i < auditReadLimitFallback+5; i++ {
		entries = append(entries, auditEntry(i))
	}
	appendAll(t, log, entries...)
	mustFlush(t, log)

	items, total := mustQuery(t, log, api.AuditFilter{})
	if len(items) != auditReadLimitFallback {
		t.Fatalf("expected the fallback cap of %d rows, got %d", auditReadLimitFallback, len(items))
	}
	if total != auditReadLimitFallback+5 {
		t.Fatalf("total should count everything: %d", total)
	}

	// 负数 limit 与负 offset 同样不会把语句写成非法形状
	items, _ = mustQuery(t, log, api.AuditFilter{Limit: -3, Offset: -7})
	if len(items) != auditReadLimitFallback {
		t.Fatalf("negative limit should fall back, got %d rows", len(items))
	}

	var count int64
	if err := db.sqlDB.QueryRow(`SELECT COUNT(*) FROM write_audit`).Scan(&count); err != nil {
		t.Fatalf("count failed: %v", err)
	}
	if count != int64(auditReadLimitFallback+5) {
		t.Fatalf("rows in table = %d", count)
	}
}

// TestAuditLog_DroppedWhenFull 是队满那条：丢弃计数上升、请求路径不阻塞、库里少行。
//
// 条数按队列容量精确给（本卡 §9 与 S03 同一条口径：总线那层不在这里，但 batcher 的
// 关闭与队满边界一样不能靠"发两倍容量"来猜）。
func TestAuditLog_DroppedWhenFull(t *testing.T) {
	log, _ := newTestAuditLog(t, func(opts *AuditLogOptions) {
		opts.QueueCapacity = 4
	})

	// 落盘周期是一小时，所以 append 不会有任何东西被消费掉
	for i := 0; i < 10; i++ {
		if err := log.Append(auditEntry(i)); err != nil {
			t.Fatalf("Append(%d) failed: %v", i, err)
		}
	}
	if got := log.batch.queued(); got != 4 {
		t.Fatalf("queue should hold exactly its capacity, got %d", got)
	}
	if dropped := log.Dropped(); dropped != 6 {
		t.Fatalf("dropped = %d, want 6", dropped)
	}

	// 丢弃不改变已入队的那些：落盘后库里是 4 行
	mustFlush(t, log)
	_, total := mustQuery(t, log, api.AuditFilter{})
	if total != 4 {
		t.Fatalf("expected the 4 queued rows, got %d", total)
	}
}

// TestAuditLog_AppendAfterClose 钉住关停顺序写错时能看见：Close 之后的 Append 返回错误，
// 而不是静默把行计入丢弃。
func TestAuditLog_AppendAfterClose(t *testing.T) {
	log, _ := newTestAuditLog(t, nil)

	if err := log.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	err := log.Append(auditEntry(1))
	if err == nil {
		t.Fatal("Append after Close must report the mistake")
	}
	if !strings.Contains(err.Error(), "audit log is closed") {
		t.Fatalf("unexpected error: %v", err)
	}

	// Close 幂等：第二次调用不再落盘、也不报错
	if err := log.Close(); err != nil {
		t.Fatalf("second Close failed: %v", err)
	}
}

// TestAuditLog_Retention 覆盖两条淘汰：条数上界切在 seq 上，时间淘汰按 ts_us。
func TestAuditLog_Retention(t *testing.T) {
	t.Run("by count", func(t *testing.T) {
		log, _ := newTestAuditLog(t, func(opts *AuditLogOptions) {
			opts.RetentionCount = 3
			opts.RetentionAge = 0
		})

		appendAll(t, log, auditEntry(1), auditEntry(2), auditEntry(3), auditEntry(4))
		mustFlush(t, log)

		items, total := mustQuery(t, log, api.AuditFilter{})
		if total != 3 || len(items) != 3 {
			t.Fatalf("retention by count left %d rows (total %d)", len(items), total)
		}
		// 留的是最后三条：seq 就是写入顺序，所以最旧那条（+1 秒）不在了
		// 降序读回，所以是最新的在前
		want := []time.Duration{4 * time.Second, 3 * time.Second, 2 * time.Second}
		for i, age := range want {
			if got := items[i].Time.Sub(auditFixedTime); got != age {
				t.Fatalf("row %d has time +%v, want +%v (the newest must come first)", i, got, age)
			}
		}
	})

	t.Run("by age", func(t *testing.T) {
		log, _ := newTestAuditLog(t, func(opts *AuditLogOptions) {
			opts.RetentionCount = 1000
			opts.RetentionAge = 3 * time.Second
			// 时间源就是 auditFixedTime，而行的时间是 fixedTime+1..+5 秒
		})

		appendAll(t, log, auditEntry(1), auditEntry(2), auditEntry(3), auditEntry(4), auditEntry(5))
		mustFlush(t, log)

		_, total := mustQuery(t, log, api.AuditFilter{})
		// 截止点是 fixedTime-3s，所以五条都还在——这一条判的是"淘汰没有走过界"
		if total != 5 {
			t.Fatalf("nothing should be pruned yet, got %d rows", total)
		}

		// 把时间源推到五行都过期之后再来一行新的，淘汰随这一批发生
		fresh := auditEntry(6)
		fresh.Time = auditFixedTime.Add(time.Hour)
		log.now = func() time.Time { return auditFixedTime.Add(time.Hour) }
		appendAll(t, log, fresh)
		mustFlush(t, log)

		items, total := mustQuery(t, log, api.AuditFilter{})
		if total != 1 || len(items) != 1 {
			t.Fatalf("expected only the newest row, got %d (total %d)", len(items), total)
		}
	})

	t.Run("age zero disables time pruning", func(t *testing.T) {
		log, _ := newTestAuditLog(t, func(opts *AuditLogOptions) {
			opts.RetentionCount = 1000
			opts.RetentionAge = 0
		})

		// 行的时间在 2026 年，真实时钟的 now 会让任何非零上界立刻把它们淘汰掉；
		// 0 的语义是"不按时间淘汰"，所以这里应当一条都不动。
		appendAll(t, log, auditEntry(1), auditEntry(2))
		mustFlush(t, log)

		_, total := mustQuery(t, log, api.AuditFilter{})
		if total != 2 {
			t.Fatalf("retention_age=0 must not prune, got %d rows", total)
		}
	})
}

// TestNewAuditLog_RejectsNilDB 与产物索引那条同形：nil 库要报错而不是 panic。
func TestNewAuditLog_RejectsNilDB(t *testing.T) {
	if _, err := NewAuditLog(nil, AuditLogOptions{}, testLogger()); err == nil {
		t.Fatal("a nil database must be rejected")
	}

	// logger 为 nil 时回落到 slog.Default()，与 sqlite.Open、NewEventLog、NewArtifactIndex 同一口径
	db, err := Open(openConfig(filepath.Join(t.TempDir(), "observe.sqlite")), testLogger())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	log, err := NewAuditLog(db, AuditLogOptions{FlushInterval: time.Hour}, nil)
	if err != nil {
		t.Fatalf("NewAuditLog with a nil logger failed: %v", err)
	}
	if err := log.Append(auditEntry(1)); err != nil {
		t.Fatalf("Append failed: %v", err)
	}
	if err := log.Flush(); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}
	count, err := log.Count()
	if err != nil {
		t.Fatalf("Count failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("Count = %d, want 1", count)
	}
}

// TestAuditLog_QueryErrorIsReturned 守住查询失败不被吞掉：端点据此回 500。
// 这里用一条指向不存在列的语句没法构造，改为关掉库再问——
// 关库之后连接上的查询必然失败。
func TestAuditLog_QueryErrorIsReturned(t *testing.T) {
	log, db := newTestAuditLog(t, nil)
	appendAll(t, log, auditEntry(1))
	mustFlush(t, log)

	if err := db.Close(); err != nil {
		t.Fatalf("closing the database failed: %v", err)
	}
	// t.Cleanup 里那次 db.Close 会变成重复关闭，无害

	_, _, err := log.Query(api.AuditFilter{})
	if err == nil {
		t.Fatal("expected the query on a closed database to fail")
	}
	var unwanted error
	if errors.Is(err, unwanted) {
		t.Fatal("the error must be reported, not compared against a sentinel")
	}
	if !strings.Contains(err.Error(), "audit") {
		t.Fatalf("the error should say which query broke: %v", err)
	}
}
