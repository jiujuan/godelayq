package sqlite

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"godelayq/core"
	"godelayq/executor"
)

// indexTime 是产物索引用例统一的时间源：created_at 由它给，读回来也按它比。
var indexTime = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// indexFixture 是一份真库 + 一个假想的产物根目录。
// 根目录在这里只是一个字符串：索引用它折相对路径，不碰文件系统。
type indexFixture struct {
	db   *DB
	root string
}

func newIndexFixture(t *testing.T) *indexFixture {
	t.Helper()

	return newIndexFixtureAt(t, filepath.Join(t.TempDir(), "exec"))
}

// newIndexFixtureAt 与上一条只差一件事：产物根目录由调用方给，
// 用于"索引要对着真的写在磁盘上的那套文件"的用例。
func newIndexFixtureAt(t *testing.T, root string) *indexFixture {
	t.Helper()

	db, err := Open(openConfig(filepath.Join(t.TempDir(), "observe.sqlite")), testLogger())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	return &indexFixture{db: db, root: root}
}

// index 按统一的时间源构造索引。
func (f *indexFixture) index(t *testing.T) *ArtifactIndex {
	t.Helper()

	idx, err := NewArtifactIndex(f.db, ArtifactIndexOptions{
		RootDir: f.root,
		Now:     func() time.Time { return indexTime },
	}, testLogger())
	if err != nil {
		t.Fatalf("NewArtifactIndex failed: %v", err)
	}
	return idx
}

// recordOf 造一次执行的登记项，路径按产物根目录下的标准布局写。
func (f *indexFixture) recordOf(jobID string, attempt int, outBytes, errBytes int64, truncated bool) executor.IndexRecord {
	dir := filepath.Join(f.root, jobID)
	return executor.IndexRecord{
		JobID:   jobID,
		Attempt: attempt,
		Kind:    string(executor.KindScript),
		Profile: "hello",
		Info: executor.ArtifactInfo{
			JobID:     jobID,
			Attempt:   attempt,
			OutPath:   filepath.Join(dir, "a"+strconv.Itoa(attempt)+".out"),
			ErrPath:   filepath.Join(dir, "a"+strconv.Itoa(attempt)+".err"),
			OutBytes:  outBytes,
			ErrBytes:  errBytes,
			Truncated: truncated,
		},
	}
}

// mustRecord 登记一次，失败就让用例停在这里。
func mustRecord(t *testing.T, idx *ArtifactIndex, rec executor.IndexRecord) {
	t.Helper()

	if err := idx.Record(rec); err != nil {
		t.Fatalf("Record(%s/%d) failed: %v", rec.JobID, rec.Attempt, err)
	}
}

// mustList 读回一个任务的行。
func mustList(t *testing.T, idx *ArtifactIndex, jobID string) []executor.IndexRecord {
	t.Helper()

	got, err := idx.List(jobID)
	if err != nil {
		t.Fatalf("List(%s) failed: %v", jobID, err)
	}
	return got
}

// countIndexRows 直接数某个任务的行数：只看 List 的长度容易把"覆盖成一行"
// 与"读到最新一行"混起来。
func countIndexRows(t *testing.T, idx *ArtifactIndex, jobID string) int {
	t.Helper()

	var count int
	if err := idx.db.sqlDB.QueryRow(
		`SELECT COUNT(*) FROM artifact_index WHERE job_id = ?`, jobID).Scan(&count); err != nil {
		t.Fatalf("count rows for %s failed: %v", jobID, err)
	}
	return count
}

func TestArtifactIndex_RecordAndList(t *testing.T) {
	f := newIndexFixture(t)
	idx := f.index(t)

	mustRecord(t, idx, f.recordOf("job-1", 2, 512, 20, false))
	mustRecord(t, idx, f.recordOf("job-1", 1, 42, 512, true))

	got := mustList(t, idx, "job-1")
	if len(got) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(got))
	}
	// 按 attempt 升序：重试链的自然阅读顺序，而不是写入顺序
	if got[0].Attempt != 1 || got[1].Attempt != 2 {
		t.Fatalf("expected attempts 1 then 2, got %d then %d", got[0].Attempt, got[1].Attempt)
	}

	first := got[0]
	if first.JobID != "job-1" || first.Kind != string(executor.KindScript) || first.Profile != "hello" {
		t.Errorf("identity columns wrong: %+v", first)
	}
	if first.Info.OutBytes != 42 || first.Info.ErrBytes != 512 {
		t.Errorf("byte counts wrong: %+v", first.Info)
	}
	if !first.Info.Truncated {
		t.Error("the truncated flag must come back")
	}
	if first.State != core.ArtifactAvailable {
		t.Errorf("expected state %q, got %q", core.ArtifactAvailable, first.State)
	}
	if !first.CreatedAt.Equal(indexTime) {
		t.Errorf("created_at %v, want %v", first.CreatedAt, indexTime)
	}

	// 库里存的是相对写法，绝对路径不入库（本卡 §3.1）
	if first.OutRel != "job-1/a1.out" || first.ErrRel != "job-1/a1.err" {
		t.Errorf("expected relative paths, got %q and %q", first.OutRel, first.ErrRel)
	}
	// 读回来的 Info.OutPath 保持为空：那两个字段是要交给 os 打开的路径
	if first.Info.OutPath != "" || first.Info.ErrPath != "" {
		t.Errorf("read rows must not hand back openable paths, got %q and %q",
			first.Info.OutPath, first.Info.ErrPath)
	}
}

func TestArtifactIndex_Upsert(t *testing.T) {
	f := newIndexFixture(t)
	idx := f.index(t)

	mustRecord(t, idx, f.recordOf("job-1", 1, 10, 0, false))
	// 同一 (job_id, attempt) 再登记一次：清理后重读、或重试撞上同一个 attempt
	mustRecord(t, idx, f.recordOf("job-1", 1, 900, 7, true))

	got := mustList(t, idx, "job-1")
	if len(got) != 1 {
		t.Fatalf("the second record must overwrite, not append, got %d rows", len(got))
	}
	if got[0].Info.OutBytes != 900 || got[0].Info.ErrBytes != 7 || !got[0].Info.Truncated {
		t.Errorf("expected the second values, got %+v", got[0].Info)
	}
	if n := countIndexRows(t, idx, "job-1"); n != 1 {
		t.Fatalf("expected one row in the table, got %d", n)
	}
}

func TestArtifactIndex_MarkPurgedScopedToAttempt(t *testing.T) {
	f := newIndexFixture(t)
	idx := f.index(t)

	mustRecord(t, idx, f.recordOf("job-1", 1, 10, 0, false))
	mustRecord(t, idx, f.recordOf("job-1", 2, 20, 0, false))

	if err := idx.MarkPurged("job-1", 1); err != nil {
		t.Fatalf("MarkPurged failed: %v", err)
	}

	got := mustList(t, idx, "job-1")
	if len(got) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(got))
	}
	if got[0].State != core.ArtifactPurged {
		t.Errorf("the attempt that failed to read must be marked, got %q", got[0].State)
	}
	// 另一次尝试的产物可能还在，全标是假话（本卡 §3.4）
	if got[1].State != core.ArtifactAvailable {
		t.Errorf("the other attempt must stay %q, got %q", core.ArtifactAvailable, got[1].State)
	}
}

func TestArtifactIndex_MarkAllPurged(t *testing.T) {
	f := newIndexFixture(t)
	idx := f.index(t)

	mustRecord(t, idx, f.recordOf("job-1", 1, 10, 0, false))
	mustRecord(t, idx, f.recordOf("job-1", 2, 20, 0, false))
	mustRecord(t, idx, f.recordOf("job-2", 1, 30, 0, false))

	if err := idx.MarkAllPurged("job-1"); err != nil {
		t.Fatalf("MarkAllPurged failed: %v", err)
	}

	for _, rec := range mustList(t, idx, "job-1") {
		if rec.State != core.ArtifactPurged {
			t.Errorf("attempt %d should be purged, got %q", rec.Attempt, rec.State)
		}
	}
	two := mustList(t, idx, "job-2")
	if len(two) != 1 || two[0].State != core.ArtifactAvailable {
		t.Fatalf("marking must stay inside the named job, got %+v", two)
	}
}

func TestArtifactIndex_AttemptZeroAllowed(t *testing.T) {
	// ArtifactStore 允许 attempt=0（TASK-E06 第 10 节第 3 条），索引这一侧不能更严
	f := newIndexFixture(t)
	idx := f.index(t)

	mustRecord(t, idx, f.recordOf("job-zero", 0, 5, 1, false))

	got := mustList(t, idx, "job-zero")
	if len(got) != 1 || got[0].Attempt != 0 {
		t.Fatalf("expected one row with attempt 0, got %+v", got)
	}
	if got[0].OutRel != "job-zero/a0.out" {
		t.Errorf("expected a0.out, got %q", got[0].OutRel)
	}
	exists, err := idx.Exists("job-zero", 0)
	if err != nil || !exists {
		t.Fatalf("Exists should find attempt 0, got %v (%v)", exists, err)
	}

	// 负数是另一回事：a-1.out 这个文件名从来不存在，写进来只会是一条查不出来的记录
	if err := idx.Record(f.recordOf("job-zero", -1, 1, 1, false)); err == nil {
		t.Fatal("a negative attempt must be rejected")
	}
}

func TestArtifactIndex_BadJobIDRejected(t *testing.T) {
	f := newIndexFixture(t)
	idx := f.index(t)

	// 校验规则与产物存储同一个来源（executor.CheckArtifactJobID），这里不另写一套
	bad := []string{"../escape", "a/b", `a\b`, "带中文", "", "job.id", "job:id"}
	for _, jobID := range bad {
		if err := idx.Record(f.recordOf(jobID, 1, 1, 1, false)); err == nil {
			t.Fatalf("Record(%q) should have been rejected", jobID)
		}
		if _, err := idx.List(jobID); err == nil {
			t.Fatalf("List(%q) should have been rejected", jobID)
		}
		if err := idx.MarkPurged(jobID, 1); err == nil {
			t.Fatalf("MarkPurged(%q) should have been rejected", jobID)
		}
		if err := idx.MarkAllPurged(jobID); err == nil {
			t.Fatalf("MarkAllPurged(%q) should have been rejected", jobID)
		}
		if err := idx.DeleteByJob(jobID); err == nil {
			t.Fatalf("DeleteByJob(%q) should have been rejected", jobID)
		}
		if _, err := idx.Exists(jobID, 1); err == nil {
			t.Fatalf("Exists(%q) should have been rejected", jobID)
		}
	}

	err := idx.Record(f.recordOf("../escape", 1, 1, 1, false))
	if !strings.Contains(err.Error(), "job id") {
		t.Fatalf("the error should name the job id, got %v", err)
	}
}

func TestArtifactIndex_DeleteByJob(t *testing.T) {
	f := newIndexFixture(t)
	idx := f.index(t)

	mustRecord(t, idx, f.recordOf("job-1", 1, 10, 0, false))
	mustRecord(t, idx, f.recordOf("job-1", 2, 20, 0, false))
	mustRecord(t, idx, f.recordOf("job-2", 1, 30, 0, false))

	if err := idx.DeleteByJob("job-1"); err != nil {
		t.Fatalf("DeleteByJob failed: %v", err)
	}
	if n := countIndexRows(t, idx, "job-1"); n != 0 {
		t.Fatalf("expected the job's rows gone, got %d", n)
	}
	if n := countIndexRows(t, idx, "job-2"); n != 1 {
		t.Fatalf("the other job must be untouched, got %d rows", n)
	}

	// 删没有行的任务不算错误：清理路径本来就可能重复触达同一个任务
	if err := idx.DeleteByJob("job-1"); err != nil {
		t.Fatalf("deleting twice should be clean, got %v", err)
	}
}

func TestArtifactIndex_Exists(t *testing.T) {
	f := newIndexFixture(t)
	idx := f.index(t)

	mustRecord(t, idx, f.recordOf("job-1", 2, 10, 0, false))

	if yes, err := idx.Exists("job-1", 2); err != nil || !yes {
		t.Fatalf("expected the recorded attempt, got %v (%v)", yes, err)
	}
	// 按 (job_id, attempt) 判，不是按任务判
	if no, err := idx.Exists("job-1", 1); err != nil || no {
		t.Fatalf("expected no row for the other attempt, got %v (%v)", no, err)
	}
	if no, err := idx.Exists("job-other", 2); err != nil || no {
		t.Fatalf("expected no row for another job, got %v (%v)", no, err)
	}
}

func TestArtifactIndex_ListIsNeverNil(t *testing.T) {
	f := newIndexFixture(t)
	idx := f.index(t)

	got := mustList(t, idx, "job-none")
	if got == nil {
		t.Fatal("an empty result must be an empty slice, not nil")
	}
	if len(got) != 0 {
		t.Fatalf("expected no rows, got %d", len(got))
	}
}

func TestArtifactIndex_ReconcileMissing(t *testing.T) {
	f := newIndexFixture(t)
	idx := f.index(t)

	mustRecord(t, idx, f.recordOf("job-here", 1, 10, 0, false))
	mustRecord(t, idx, f.recordOf("job-gone", 1, 20, 0, false))
	mustRecord(t, idx, f.recordOf("job-gone", 2, 30, 0, false))
	// 已经是 purged 的行不该被再算一次
	if err := idx.MarkPurged("job-gone", 2); err != nil {
		t.Fatalf("MarkPurged failed: %v", err)
	}

	// 两个目录都还在：一行都不标
	marked, err := idx.ReconcileMissing(func(jobID string) bool { return true })
	if err != nil || marked != 0 {
		t.Fatalf("expected nothing marked, got %d (%v)", marked, err)
	}

	// 只剩 job-here 的目录：那一行 available 的被标掉
	marked, err = idx.ReconcileMissing(func(jobID string) bool { return jobID == "job-here" })
	if err != nil {
		t.Fatalf("ReconcileMissing failed: %v", err)
	}
	if marked != 1 {
		t.Fatalf("expected exactly one row marked, got %d", marked)
	}

	for _, rec := range mustList(t, idx, "job-gone") {
		if rec.State != core.ArtifactPurged {
			t.Errorf("reconciliation marks rows, it must not delete them: %+v", rec)
		}
	}
	here := mustList(t, idx, "job-here")
	if len(here) != 1 || here[0].State != core.ArtifactAvailable {
		t.Fatalf("the job whose directory is present must stay available, got %+v", here)
	}

	if _, err := idx.ReconcileMissing(nil); !errors.Is(err, errNeedDirCheck) {
		t.Fatalf("expected the missing-check error, got %v", err)
	}
}

func TestArtifactIndex_RelativeOutsideRootRejected(t *testing.T) {
	f := newIndexFixture(t)
	idx := f.index(t)

	rec := f.recordOf("job-1", 1, 10, 0, false)
	// 手工造一个不在产物根目录下的路径：折不出相对写法，报错而不是写一条误导的记录
	rec.Info.OutPath = filepath.Join(t.TempDir(), "elsewhere", "a1.out")

	err := idx.Record(rec)
	if err == nil {
		t.Fatal("a path outside the artifact root must be rejected")
	}
	if !strings.Contains(err.Error(), "outside the artifact root") {
		t.Fatalf("the error should say why, got %v", err)
	}
}

// TestArtifactIndex_RealWriterBytesMatchDisk 用真实写盘接进来一次：
// ArtifactStore.Open + 写 + Close 得到的那份 ArtifactInfo，才是执行侧交给索引的东西。
// 这里对照三方——库里的 out_bytes、磁盘上的实际大小、写侧给出的相对路径能不能真的开到文件，
// 因为只有假数据的用例证不了"折出来的相对写法指向的就是那个文件"（本卡 DoD 第 5 条）。
func TestArtifactIndex_RealWriterBytesMatchDisk(t *testing.T) {
	root := filepath.Join(t.TempDir(), "exec")
	store, err := executor.NewArtifactStore(executor.ArtifactOptions{
		Dir:      root,
		MaxBytes: core.DefaultExecMaxOutputBytes,
	}, testLogger())
	if err != nil {
		t.Fatalf("NewArtifactStore failed: %v", err)
	}

	writer, err := store.Open("job-real", 1)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	if _, err := writer.Stdout().Write([]byte("first line\nsecond line\n")); err != nil {
		t.Fatalf("stdout write failed: %v", err)
	}
	if _, err := writer.Stderr().Write([]byte("boom\n")); err != nil {
		t.Fatalf("stderr write failed: %v", err)
	}
	info, err := writer.Close()
	if err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	idx := newIndexFixtureAt(t, root).index(t)
	mustRecord(t, idx, executor.IndexRecord{
		JobID:   "job-real",
		Attempt: 1,
		Kind:    string(executor.KindScript),
		Profile: "hello",
		Info:    info,
	})

	got := mustList(t, idx, "job-real")
	if len(got) != 1 {
		t.Fatalf("expected one row, got %d", len(got))
	}

	onDisk, err := os.Stat(info.OutPath)
	if err != nil {
		t.Fatalf("Stat(%s) failed: %v", info.OutPath, err)
	}
	if got[0].Info.OutBytes != onDisk.Size() {
		t.Fatalf("indexed out_bytes = %d, the file on disk is %d bytes", got[0].Info.OutBytes, onDisk.Size())
	}
	if got[0].Info.OutBytes != info.OutBytes {
		t.Fatalf("the round trip changed the byte count: %d then %d", info.OutBytes, got[0].Info.OutBytes)
	}

	errOnDisk, err := os.Stat(info.ErrPath)
	if err != nil {
		t.Fatalf("Stat(%s) failed: %v", info.ErrPath, err)
	}
	if got[0].Info.ErrBytes != errOnDisk.Size() {
		t.Fatalf("indexed err_bytes = %d, the file on disk is %d bytes", got[0].Info.ErrBytes, errOnDisk.Size())
	}

	// 折出来的相对写法必须还能对着同一个文件：路径列不进接口，但它是对账与排查的线索
	relatives := []struct{ rel, abs string }{{got[0].OutRel, info.OutPath}, {got[0].ErrRel, info.ErrPath}}
	for _, pair := range relatives {
		if strings.Contains(pair.rel, `\`) || pair.rel == "" {
			t.Fatalf("expected a slash-separated relative path, got %q", pair.rel)
		}
		joined := filepath.Join(root, filepath.FromSlash(pair.rel))
		if joined != filepath.Clean(pair.abs) {
			t.Fatalf("%q resolves to %s, the written file is %s", pair.rel, joined, pair.abs)
		}
	}
}

func TestNewArtifactIndex_RejectsNilDeps(t *testing.T) {
	f := newIndexFixture(t)

	if _, err := NewArtifactIndex(nil, ArtifactIndexOptions{RootDir: f.root}, testLogger()); err == nil {
		t.Fatal("a nil database must be rejected")
	}
	if _, err := NewArtifactIndex(f.db, ArtifactIndexOptions{RootDir: "   "}, testLogger()); err == nil {
		t.Fatal("an empty artifact root must be rejected: nothing can be relativised against it")
	}

	// logger 为 nil 时回落到 slog.Default()，与 sqlite.Open、NewEventLog 同一口径
	idx, err := NewArtifactIndex(f.db, ArtifactIndexOptions{RootDir: f.root}, nil)
	if err != nil {
		t.Fatalf("NewArtifactIndex with a nil logger failed: %v", err)
	}
	mustRecord(t, idx, f.recordOf("job-1", 1, 1, 1, false))
}

func TestArtifactIndex_FoldsSeparatorsToSlash(t *testing.T) {
	f := newIndexFixture(t)
	idx := f.index(t)

	mustRecord(t, idx, f.recordOf("job-1", 1, 10, 0, false))

	got := mustList(t, idx, "job-1")
	if len(got) != 1 {
		t.Fatalf("expected one row, got %d", len(got))
	}
	// 库里统一正斜杠：Windows 的反斜杠不该跟着一路走到接口与导出文件里
	if strings.Contains(got[0].OutRel, `\`) || !strings.HasPrefix(got[0].OutRel, "job-1/") {
		t.Fatalf("expected a slash-separated relative path, got %q", got[0].OutRel)
	}
}
