package sqlite

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"godelayq/core"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// openConfig 建一份指向 path 的观测层配置，取值一律用 core 的默认，
// 只把路径换成临时目录，免得测试写到仓库的 data/。
func openConfig(path string) core.ObservabilityConfig {
	return core.ObservabilityConfig{
		Enabled:       true,
		Path:          path,
		FlushInterval: core.DefaultObserveFlushInterval,
		QueueCapacity: core.DefaultObserveQueueCapacity,
		BusyTimeout:   core.DefaultObserveBusyTimeout,
		Synchronous:   core.DefaultObserveSynchronous,
	}
}

// countQuery 读一个标量查询结果，用于直接查 sqlite_master 核对表与索引。
func countQuery(t *testing.T, db *DB, query string, args ...any) int {
	t.Helper()

	var n int
	if err := db.sqlDB.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("query %q failed: %v", query, err)
	}
	return n
}

func TestOpen_CreatesFileAndSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "observe.sqlite")

	db, err := Open(openConfig(path), testLogger())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close failed: %v", err)
		}
	}()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected the database file to exist: %v", err)
	}
	stats, err := db.Stats()
	if err != nil {
		t.Fatalf("Stats failed: %v", err)
	}
	if stats.SchemaVersion != 1 {
		t.Fatalf("expected schema version 1, got %d", stats.SchemaVersion)
	}
	if stats.Events != 0 || stats.Artifacts != 0 || stats.AuditRows != 0 {
		t.Fatalf("expected empty tables, got %+v", stats)
	}

	// 三张表都在，名字与设计文档 §6 逐字一致
	for _, table := range []string{"job_events", "artifact_index", "write_audit"} {
		if n := countQuery(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table); n != 1 {
			t.Errorf("table %q missing (found %d)", table, n)
		}
	}
	// 八个索引：事件 3 + 产物 1 + 审计 4（设计文档 §6.1/§6.2/§6.3）
	for _, index := range []string{
		"idx_events_job", "idx_events_ts", "idx_events_type",
		"idx_artifact_state",
		"idx_audit_ts", "idx_audit_actor", "idx_audit_action", "idx_audit_verdict",
	} {
		if n := countQuery(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, index); n != 1 {
			t.Errorf("index %q missing (found %d)", index, n)
		}
	}
	// 迁移版本表带 observe_ 前缀，与设计文档 §13 风险 6 的约定一致
	if n := countQuery(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'observe_schema_migrations'`); n != 1 {
		t.Error("table observe_schema_migrations missing")
	}
}

func TestOpen_ParentDirCreated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b", "observe.sqlite")

	db, err := Open(openConfig(path), testLogger())
	if err != nil {
		t.Fatalf("Open should create missing parent dirs, got %v", err)
	}
	defer db.Close()

	if info, err := os.Stat(filepath.Dir(path)); err != nil || !info.IsDir() {
		t.Fatalf("expected the parent dirs to exist: %v", err)
	}
}

func TestOpen_InvalidPathRejected(t *testing.T) {
	// 路径指向一个已存在的目录：建不出库文件，必须报错而不是 panic
	dir := t.TempDir()
	if _, err := Open(openConfig(dir), testLogger()); err == nil {
		t.Fatal("expected an error when the path is an existing directory")
	}

	for name, cfg := range map[string]core.ObservabilityConfig{
		"empty path":         {Enabled: true, Synchronous: "normal"},
		"whitespace path":    {Enabled: true, Path: "   ", Synchronous: "normal"},
		"path with query":    {Enabled: true, Path: "./data/a?b.sqlite", Synchronous: "normal"},
		"path with fragment": {Enabled: true, Path: "./data/a#b.sqlite", Synchronous: "normal"},
		"bad synchronous":    {Enabled: true, Path: "./data/x.sqlite", Synchronous: "off"},
	} {
		if _, err := Open(cfg, testLogger()); err == nil {
			t.Errorf("%s: expected a construction error", name)
		} else {
			t.Logf("%s rejected with: %v", name, err)
		}
	}
}

func TestOpen_WALModeEnabled(t *testing.T) {
	db, err := Open(openConfig(filepath.Join(t.TempDir(), "observe.sqlite")), testLogger())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	// 生效值来自查询本身，而不是 DSN 字符串：网络文件系统上 WAL 可能静默退回 delete
	var mode string
	if err := db.sqlDB.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("read journal_mode failed: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("expected journal_mode wal, got %q (read back by Open: %q)", mode, db.JournalMode())
	}
	if db.JournalMode() != mode {
		t.Errorf("JournalMode()=%q disagrees with the query result %q", db.JournalMode(), mode)
	}
	// foreign_keys 与 busy_timeout 也按卡片 §3.4 落地
	if n := countQuery(t, db, `PRAGMA foreign_keys`); n != 1 {
		t.Errorf("expected foreign_keys ON, got %d", n)
	}
	if n := countQuery(t, db, `PRAGMA busy_timeout`); n != int(core.DefaultObserveBusyTimeout.Milliseconds()) {
		t.Errorf("expected busy_timeout %d ms, got %d", int64(core.DefaultObserveBusyTimeout/time.Millisecond), n)
	}
}

func TestOpen_SynchronousFull(t *testing.T) {
	cfg := openConfig(filepath.Join(t.TempDir(), "normal.sqlite"))
	normal, err := Open(cfg, testLogger())
	if err != nil {
		t.Fatalf("Open with synchronous=normal failed: %v", err)
	}
	defer normal.Close()
	if normal.Synchronous() != "normal" {
		t.Fatalf("expected synchronous normal, got %q", normal.Synchronous())
	}

	fullCfg := cfg
	fullCfg.Path = filepath.Join(t.TempDir(), "full.sqlite")
	fullCfg.Synchronous = "full"
	full, err := Open(fullCfg, testLogger())
	if err != nil {
		t.Fatalf("Open with synchronous=full failed: %v", err)
	}
	defer full.Close()
	if full.Synchronous() != "full" {
		t.Fatalf("expected synchronous full, got %q", full.Synchronous())
	}
}

func TestIdempotentMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "observe.sqlite")

	db, err := Open(openConfig(path), testLogger())
	if err != nil {
		t.Fatalf("first Open failed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("first Close failed: %v", err)
	}

	again, err := Open(openConfig(path), testLogger())
	if err != nil {
		t.Fatalf("second Open failed: %v", err)
	}
	defer again.Close()

	stats, err := again.Stats()
	if err != nil {
		t.Fatalf("Stats failed: %v", err)
	}
	if stats.SchemaVersion != 1 {
		t.Fatalf("expected schema version to stay 1, got %d", stats.SchemaVersion)
	}
	if n := countQuery(t, again, `SELECT COUNT(*) FROM observe_schema_migrations`); n != 1 {
		t.Fatalf("expected exactly one migration row, got %d", n)
	}
}

func TestClose_Idempotent(t *testing.T) {
	db, err := Open(openConfig(filepath.Join(t.TempDir(), "observe.sqlite")), testLogger())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := db.Close(); err != nil {
			t.Fatalf("Close #%d failed: %v", i+1, err)
		}
	}

	// 关掉之后 Stats 必须报错，不能把"读不出来"报成"表是空的"
	if _, err := db.Stats(); err == nil {
		t.Fatal("expected Stats to fail after Close")
	}
}

// TestOpen_FilePermissions 固定库文件 0640、父目录 0750（设计文档 §10.4）。
// Windows 上 NTFS 权限不由 mode bits 表达，os.Chmod 只影响只读位，
// 这条断言留给 Unix 与 CI（照 executor/artifact_test.go 的既有处理）。
func TestOpen_FilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上 NTFS 权限不由 mode bits 表达，这条断言在 Unix 与 CI 生效")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "observe.sqlite")
	db, err := Open(openConfig(path), testLogger())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat file failed: %v", err)
	}
	if perm := info.Mode().Perm(); perm != filePerm {
		t.Errorf("expected database file mode %04o, got %04o", filePerm, perm)
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir failed: %v", err)
	}
	if perm := parent.Mode().Perm(); perm != dirPerm {
		t.Errorf("expected parent dir mode %04o, got %04o", dirPerm, perm)
	}
}

// TestSchemaColumnsMatchDesign 把三张表的列名、类型与 NOT NULL 钉在用例里。
//
// 这是 DoD"表名、列名、索引名与设计文档 §6 逐字一致，S03/S05/S06 不需要再执行任何 DDL"的
// 唯一可执行守卫：DDL 写在字符串里，改错一个列名只有靠读侧查询失败才会发现，
// 而那时已经是一次的运行期错误。
func TestSchemaColumnsMatchDesign(t *testing.T) {
	db, err := Open(openConfig(filepath.Join(t.TempDir(), "observe.sqlite")), testLogger())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	expected := map[string][]string{
		// 每一项是 name|type|notnull|pk|default，pk 是主键内的序号（0 表示不在主键里）
		"job_events": {
			"seq|INTEGER|0|1|", "ts_us|INTEGER|1|0|", "type|TEXT|1|0|", "job_id|TEXT|1|0|",
			"job_name|TEXT|0|0|", "status|INTEGER|1|0|", "data|TEXT|0|0|", "metadata|TEXT|0|0|",
		},
		// 主键是 (job_id, attempt) 复合键，与"每次尝试一个文件"的布局一一对应
		"artifact_index": {
			"job_id|TEXT|1|1|", "attempt|INTEGER|1|2|", "kind|TEXT|1|0|", "profile|TEXT|0|0|",
			"out_rel|TEXT|0|0|", "err_rel|TEXT|0|0|", "out_bytes|INTEGER|1|0|0",
			"err_bytes|INTEGER|1|0|0", "truncated|INTEGER|1|0|0", "state|TEXT|1|0|",
			"created_at|INTEGER|1|0|",
		},
		"write_audit": {
			"seq|INTEGER|0|1|", "ts_us|INTEGER|1|0|", "actor|TEXT|0|0|", "actor_kind|TEXT|1|0|",
			"role|TEXT|0|0|", "action|TEXT|1|0|", "method|TEXT|1|0|", "route|TEXT|1|0|",
			"status|INTEGER|1|0|", "latency_us|INTEGER|1|0|", "verdict|TEXT|1|0|",
			"exec_verdict|TEXT|0|0|", "exec_reason_code|TEXT|0|0|", "handler_key|TEXT|0|0|",
			"profile|TEXT|0|0|", "job_id|TEXT|0|0|", "remote_ip|TEXT|0|0|", "user_agent|TEXT|0|0|",
		},
	}

	for table, want := range expected {
		rows, err := db.sqlDB.Query(`SELECT name || '|' || type || '|' || "notnull" || '|' || pk || '|' || COALESCE(dflt_value, '') FROM pragma_table_info(?) ORDER BY cid`, table)
		if err != nil {
			t.Fatalf("pragma_table_info(%s) failed: %v", table, err)
		}
		var got []string
		for rows.Next() {
			var column string
			if err := rows.Scan(&column); err != nil {
				rows.Close()
				t.Fatalf("scan %s failed: %v", table, err)
			}
			got = append(got, column)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatalf("iterate %s failed: %v", table, err)
		}
		rows.Close()

		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("table %s columns drifted:\n got %v\nwant %v", table, got, want)
		}
	}
}

// TestCloseCheckpointsWAL 固定"干净关闭会把 WAL 旁文件合并回收"这条备份口径的依据：
// 写入事务进行中时 -wal 存在，Close 之后 -wal 与 -shm 都不应留在目录里。
//
// 设计文档 §12 的手工验收要看的就是这个现象；放在用例里而不是只在 Windows 手工跑，
// 是因为外部无法向控制台进程发出优雅 SIGTERM（taskkill /F 是强杀，走不到 checkpoint）。
func TestCloseCheckpointsWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "observe.sqlite")
	db, err := Open(openConfig(path), testLogger())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	// 这里的 INSERT 只是测试手段：本卡不提供任何表的读写方法（§8），
	// 但没有一次写事务就观察不到 WAL 旁文件。
	if _, err := db.sqlDB.Exec(`INSERT INTO job_events (ts_us, type, job_id, status) VALUES (1727000000000000, 'job.created', 'job-1', 1)`); err != nil {
		t.Fatalf("test insert failed: %v", err)
	}
	if _, err := os.Stat(path + "-wal"); err != nil {
		t.Fatalf("expected the -wal sidecar to exist while the connection is open: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); !os.IsNotExist(err) {
			t.Errorf("expected %s to be checkpointed and removed after Close, stat said %v", suffix, err)
		}
	}
	// 主文件仍在，数据也还在：关一次不该丢东西
	again, err := Open(openConfig(path), testLogger())
	if err != nil {
		t.Fatalf("reopening after Close failed: %v", err)
	}
	defer again.Close()
	if n := countQuery(t, again, `SELECT COUNT(*) FROM job_events`); n != 1 {
		t.Fatalf("expected the row to survive the close, got %d rows", n)
	}
}

// TestStatsReportsQueryError 钉住 Stats 的错误语义：表被外部删掉时报错，而不是返回 0 行。
// 这条是"库文件是加速器而非账本"（设计文档 D8）之外唯一能观察到的失败形态：
// 读不出来必须让运维看得见，否则 /admin/runtime 上的行数会与"这个部署没跑任务"无法区分。
func TestStatsReportsQueryError(t *testing.T) {
	db, err := Open(openConfig(filepath.Join(t.TempDir(), "observe.sqlite")), testLogger())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	if _, err := db.sqlDB.Exec(`DROP TABLE write_audit`); err != nil {
		t.Fatalf("drop table failed: %v", err)
	}
	if _, err := db.Stats(); err == nil {
		t.Fatal("expected Stats to report the missing table instead of reading it as empty")
	}
}
