package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
)

// purgeMark 是一次 MarkPurged 调用。
type purgeMark struct {
	JobID   string
	Attempt int
}

// deletedCall 是一次 DeleteByJob 调用，连同当时产物目录还在不在。
// "先删目录、再删行"这条顺序只有被调用方能看见，所以由 fake 来回看。
type deletedCall struct {
	JobID      string
	DirPresent bool
}

// fakeIndex 是 ArtifactIndexer 的内存实现：记下每次调用，并能给每个方法单独注入失败。
// root 是产物根目录，删行那一刻用它回看目录状态。
type fakeIndex struct {
	root string

	records   []IndexRecord
	purged    []purgeMark
	allPurged []string
	deleted   []deletedCall
	lists     []string

	recordErr   error
	markErr     error
	deleteErr   error
	listRows    []IndexRecord
	listErr     error
	existsValue bool

	// 对账：probe 是要问产物存储的那批 ID，answers 记下它答了什么。
	reconcileCalls   int
	reconcileMarked  int
	reconcileErr     error
	reconcileProbe   []string
	reconcileAnswers []string
}

func (f *fakeIndex) Record(rec IndexRecord) error {
	if f.recordErr != nil {
		return f.recordErr
	}
	f.records = append(f.records, rec)
	return nil
}

func (f *fakeIndex) MarkPurged(jobID string, attempt int) error {
	if f.markErr != nil {
		return f.markErr
	}
	f.purged = append(f.purged, purgeMark{JobID: jobID, Attempt: attempt})
	return nil
}

func (f *fakeIndex) MarkAllPurged(jobID string) error {
	f.allPurged = append(f.allPurged, jobID)
	return nil
}

func (f *fakeIndex) DeleteByJob(jobID string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	info, err := os.Stat(filepath.Join(f.root, jobID))
	f.deleted = append(f.deleted, deletedCall{
		JobID:      jobID,
		DirPresent: err == nil && info.IsDir(),
	})
	return nil
}

func (f *fakeIndex) List(jobID string) ([]IndexRecord, error) {
	f.lists = append(f.lists, jobID)
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listRows, nil
}

func (f *fakeIndex) Exists(string, int) (bool, error) { return f.existsValue, nil }

// ReconcileMissing 让 fake 也支持可选的对账接口：把执行侧给出的目录判断逐个问一遍，
// 用例据此判断那个判断覆盖到了哪些目录。
func (f *fakeIndex) ReconcileMissing(dirExists func(jobID string) bool) (int, error) {
	f.reconcileCalls++
	if f.reconcileErr != nil {
		return 0, f.reconcileErr
	}
	for _, id := range f.reconcileProbe {
		if dirExists(id) {
			f.reconcileAnswers = append(f.reconcileAnswers, id+":yes")
			continue
		}
		f.reconcileAnswers = append(f.reconcileAnswers, id+":no")
	}
	return f.reconcileMarked, nil
}

func TestRunner_RecordsIndex(t *testing.T) {
	fixture := newRunnerFixture(t, shellCommand(t, "echo hello && echo boom >&2"), nil)
	index := &fakeIndex{root: fixture.artifacts.Dir()}
	fixture.artifacts.SetIndex(index)

	job, err := fixture.run(context.Background(), "job-idx-1", "")
	require.NoError(t, err)

	require.Len(t, index.records, 1, "一次执行登记一行")
	rec := index.records[0]
	// 同源断言：索引与任务摘要读的是同一份 Close 结论，不能各算一套
	assert.Equal(t, "job-idx-1", rec.JobID)
	assert.Equal(t, job.Attempts, rec.Attempt)
	assert.Equal(t, string(fixture.profile.Kind), rec.Kind)
	assert.Equal(t, fixture.profile.Name, rec.Profile)
	assert.Equal(t, job.Exec.OutBytes, rec.Info.OutBytes)
	assert.Equal(t, job.Exec.ErrBytes, rec.Info.ErrBytes)
	assert.Equal(t, job.Exec.Truncated, rec.Info.Truncated)
	assert.Equal(t, core.ArtifactAvailable, job.Exec.Artifact)

	// 执行侧交给索引的是绝对路径：折成相对写法是库实现那一侧的事（本卡 §3.1）
	assert.True(t, filepath.IsAbs(rec.Info.OutPath), "got %q", rec.Info.OutPath)
	assert.Contains(t, rec.Info.OutPath, "job-idx-1")

	// 与磁盘上的实际大小对照：同源断言只证明索引与摘要一致，两者一起算错也照样一致，
	// 所以这里拿一次 os.Stat 作为第三方证据（本卡 DoD 第 5 条）。
	onDisk, err := os.Stat(rec.Info.OutPath)
	require.NoError(t, err)
	assert.Equal(t, onDisk.Size(), rec.Info.OutBytes, "索引里的 out_bytes 必须是文件真实大小")
}

func TestRunner_IndexFailureDoesNotAffectResult(t *testing.T) {
	// 这一条要看到索引写失败的那条 warn，而索引挂在产物存储上、日志走存储那份 logger，
	// 所以这里自建一套"存储与执行器共用一个日志缓冲"的现场，不用 fixture 的静默存储。
	var logs bytes.Buffer
	cfg := configWith(t.TempDir(), shellCommand(t, "echo hello"))
	normalized := cfg.Normalized()
	profiles := mustLoad(t, normalized)
	require.Len(t, profiles, 1)

	root := filepath.Join(t.TempDir(), "exec")
	store, err := NewArtifactStore(ArtifactOptions{Dir: root, MaxBytes: core.DefaultExecMaxOutputBytes},
		bufferLogger(&logs))
	require.NoError(t, err)
	index := &fakeIndex{root: root, recordErr: errors.New("index disk is full")}
	store.SetIndex(index)
	runner := NewRunner(profiles[0], store, normalized.Executors, bufferLogger(&logs))

	job := &core.Job{ID: "job-idx-fail", Name: profiles[0].HandlerKey(), Attempts: 1}
	err = runner.Handler()(context.Background(), job)
	require.NoError(t, err, "索引写不进去不该让任务失败")

	require.NotNil(t, job.Exec)
	assert.Equal(t, core.ArtifactAvailable, job.Exec.Artifact)
	assert.NotZero(t, job.Exec.OutBytes)
	assert.Empty(t, index.records, "写失败时不该留下一行")

	// 文件确实还在：索引缺一行，正文照样读得到
	data, _, readErr := store.Read("job-idx-fail", 1, "out", 0)
	require.NoError(t, readErr)
	assert.Equal(t, int64(len(data)), job.Exec.OutBytes)

	out := logs.String()
	assert.Contains(t, out, "executor output was not added to the artifact index")
	assert.Contains(t, out, "index disk is full")
}

func TestHTTPRunner_RecordsIndex(t *testing.T) {
	target := newCountingTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("pong"))
	}), false)
	command := httpGetCommand(target.url(), target.hostPort())
	// 测试服务在回环地址上，档位要显式放开私网判断（本机回环调试那条路）
	command.DenyPrivate = boolPtr(false)
	fixture := newHTTPFixture(t, command, nil)
	index := &fakeIndex{root: fixture.artifacts.Dir()}
	fixture.artifacts.SetIndex(index)

	job, err := fixture.run(context.Background(), "job-http-idx", "")
	require.NoError(t, err)

	require.Len(t, index.records, 1, "http 档位也要接索引")
	rec := index.records[0]
	assert.Equal(t, string(KindHTTP), rec.Kind, "第二个写入点单独取证，别只测进程路径")
	assert.Equal(t, fixture.profile.Name, rec.Profile)
	assert.Equal(t, job.Attempts, rec.Attempt)
	assert.NotZero(t, rec.Info.OutBytes)
	assert.Equal(t, job.Exec.OutBytes, rec.Info.OutBytes)
}

func TestPurgeExpired_DeletesIndexRowsAfterDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "exec")
	now := time.Now()
	store := newStoreWithClock(t, root, time.Hour, func() time.Time { return now })
	index := &fakeIndex{root: root}
	store.SetIndex(index)

	createArtifactDir(t, store, "job-old", 1)
	createArtifactDir(t, store, "job-new", 1)
	require.NoError(t, os.Chtimes(filepath.Join(root, "job-old"), now.Add(-2*time.Hour), now.Add(-2*time.Hour)))

	deleted, err := store.PurgeExpired()
	require.NoError(t, err)
	require.Equal(t, 1, deleted)

	require.Len(t, index.deleted, 1, "只清掉过期那一个任务的行")
	assert.Equal(t, "job-old", index.deleted[0].JobID)
	assert.False(t, index.deleted[0].DirPresent, "删行的时候目录必须已经删掉了")
}

// TestPurgeExpired_KeepsRowsWhenRemovalFails 守的是"先删目录、再删行"的反面：
// 目录没删掉时行必须留着。挡得住删除的平台才测得到这条，挡不住时如实跳过。
func TestPurgeExpired_KeepsRowsWhenRemovalFails(t *testing.T) {
	root := filepath.Join(t.TempDir(), "exec")
	now := time.Now()
	store := newStoreWithClock(t, root, time.Hour, func() time.Time { return now })
	index := &fakeIndex{root: root}
	store.SetIndex(index)

	dir := createArtifactDir(t, store, "job-stuck", 1)
	makeUndeletable(t, dir)
	require.NoError(t, os.Chtimes(dir, now.Add(-2*time.Hour), now.Add(-2*time.Hour)))

	deleted, err := store.PurgeExpired()
	if err == nil {
		t.Skip("this platform removed the directory anyway, so the failed-removal path cannot be observed")
	}
	assert.Zero(t, deleted)
	assert.Empty(t, index.deleted, "目录还在，行就不能删")
}

func TestPurgeOrphans_DeletesIndexRows(t *testing.T) {
	root := filepath.Join(t.TempDir(), "exec")
	store := newStoreWithClock(t, root, 0, time.Now)
	index := &fakeIndex{root: root}
	store.SetIndex(index)

	createArtifactDir(t, store, "job-live", 1)
	createArtifactDir(t, store, "job-orphan", 1)

	deleted, err := store.PurgeOrphans(func() (map[string]bool, error) {
		return map[string]bool{"job-live": true}, nil
	})
	require.NoError(t, err)
	require.Equal(t, 1, deleted)

	require.Len(t, index.deleted, 1)
	assert.Equal(t, "job-orphan", index.deleted[0].JobID)
	assert.False(t, index.deleted[0].DirPresent)
}

func TestPurge_IndexDeleteFailureOnlyLogs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "exec")
	var logs bytes.Buffer
	store, err := NewArtifactStore(ArtifactOptions{Dir: root, MaxBytes: core.DefaultExecMaxOutputBytes},
		bufferLogger(&logs))
	require.NoError(t, err)
	store.SetIndex(&fakeIndex{root: root, deleteErr: errors.New("index is locked")})

	createArtifactDir(t, store, "job-gone", 1)
	deleted, err := store.PurgeOrphans(func() (map[string]bool, error) {
		return map[string]bool{}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, 1, deleted, "索引删失败不改变本轮删除计数")

	out := logs.String()
	assert.Contains(t, out, "artifact index rows were not deleted")
	assert.Contains(t, out, "index is locked")
}

func TestSetIndex_NilIsSafe(t *testing.T) {
	// 不调 SetIndex：写产物、执行、清理都要照旧工作（本卡 DoD 第 1 条的 executor 侧）
	fixture := newRunnerFixture(t, shellCommand(t, "echo hello"), nil)

	job, err := fixture.run(context.Background(), "job-no-index", "")
	require.NoError(t, err)
	assert.Equal(t, core.ArtifactAvailable, job.Exec.Artifact)
	assert.Nil(t, fixture.artifacts.Index(), "没挂索引时读回来就是 nil")
	// 日志里不该出现索引相关的字（执行本身那条 INFO 与任务 ID 里的字样不算，那是既有内容）
	out := strings.ToLower(fixture.logs.String())
	assert.NotContains(t, out, "artifact index")
	assert.NotContains(t, out, "artifact index rows were not deleted")

	deleted, err := fixture.artifacts.PurgeOrphans(func() (map[string]bool, error) {
		return map[string]bool{}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, 1, deleted, "没挂索引也要照常清理目录")
}

// TestIndexAccessors_NilStoreIsSafe 覆盖访问器的 nil 安全：
// 产物存储没建出来的部署里，调用方拿到的必须是 nil 而不是 panic。
func TestIndexAccessors_NilStoreIsSafe(t *testing.T) {
	var store *ArtifactStore
	assert.Nil(t, store.Index())
	store.recordIndex(KindScript, "hello", ArtifactInfo{JobID: "job-a"})
	store.deleteIndexRows("job-a")
	store.reconcileIndex()
}

func TestStart_ReconcilesIndexAfterPurges(t *testing.T) {
	root := filepath.Join(t.TempDir(), "exec")
	var logs bytes.Buffer
	store, err := NewArtifactStore(ArtifactOptions{Dir: root, MaxBytes: core.DefaultExecMaxOutputBytes},
		bufferLogger(&logs))
	require.NoError(t, err)

	index := &fakeIndex{
		root:            root,
		reconcileMarked: 3,
		reconcileProbe:  []string{"job-here", "job-gone", "../escape"},
	}
	store.SetIndex(index)
	createArtifactDir(t, store, "job-here", 1)

	ctx, cancel := context.WithCancel(context.Background())
	done := store.Start(ctx, func() (map[string]bool, error) {
		return map[string]bool{"job-here": true}, nil
	})
	cancel()
	<-done

	assert.Equal(t, 1, index.reconcileCalls, "对账只在启动那一轮跑一次")
	// 目录在的答 yes；压根没建过的、以及 ID 不成目录名的都答 no
	assert.Equal(t, []string{"job-here:yes", "job-gone:no", "../escape:no"}, index.reconcileAnswers)
	assert.Contains(t, logs.String(), "artifact index rows marked as purged")
	assert.Contains(t, logs.String(), "count=3")
}

func TestStart_ReconcileFailureOnlyLogs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "exec")
	var logs bytes.Buffer
	store, err := NewArtifactStore(ArtifactOptions{Dir: root, MaxBytes: core.DefaultExecMaxOutputBytes},
		bufferLogger(&logs))
	require.NoError(t, err)
	store.SetIndex(&fakeIndex{root: root, reconcileErr: errors.New("index table is gone")})

	ctx, cancel := context.WithCancel(context.Background())
	done := store.Start(ctx, func() (map[string]bool, error) { return map[string]bool{}, nil })
	cancel()
	<-done

	assert.Contains(t, logs.String(), "artifact index reconciliation failed")
	assert.NotContains(t, logs.String(), "artifact index rows marked as purged")
}

// TestStart_WithoutReconcilerIsFine 覆盖只实现必需方法的那类索引：
// 对账是可选能力，实现没有它时启动扫描照常跑完，也不报错。
func TestStart_WithoutReconcilerIsFine(t *testing.T) {
	root := filepath.Join(t.TempDir(), "exec")
	var logs bytes.Buffer
	store, err := NewArtifactStore(ArtifactOptions{Dir: root, MaxBytes: core.DefaultExecMaxOutputBytes},
		bufferLogger(&logs))
	require.NoError(t, err)
	store.SetIndex(&plainIndex{})

	ctx, cancel := context.WithCancel(context.Background())
	done := store.Start(ctx, func() (map[string]bool, error) { return map[string]bool{}, nil })
	cancel()
	<-done

	assert.Empty(t, logs.String(), "没有对账能力时不该多出日志：启动这一轮与挂索引之前一致")
}

// plainIndex 只实现 ArtifactIndexer 的必需方法，用来证明对账接口确实是可选的。
type plainIndex struct{}

func (p *plainIndex) Record(IndexRecord) error           { return nil }
func (p *plainIndex) MarkPurged(string, int) error       { return nil }
func (p *plainIndex) MarkAllPurged(string) error         { return nil }
func (p *plainIndex) DeleteByJob(string) error           { return nil }
func (p *plainIndex) List(string) ([]IndexRecord, error) { return nil, nil }
func (p *plainIndex) Exists(string, int) (bool, error)   { return false, nil }

// raceIndex 是加了锁的索引替身：本卡 §9 的那条风险是两个 goroutine 同时触达索引
// （Record 在执行侧、DeleteByJob 在清理侧），所以这份替身自己必须线程安全，
// 否则 -race 抓到的是测试内部的问题而不是被测代码的。
type raceIndex struct {
	mu      sync.Mutex
	root    string
	records int
	deletes []deletedCall
}

func (r *raceIndex) Record(IndexRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records++
	return nil
}

func (r *raceIndex) MarkPurged(string, int) error { return nil }

func (r *raceIndex) MarkAllPurged(string) error { return nil }

func (r *raceIndex) DeleteByJob(jobID string) error {
	info, err := os.Stat(filepath.Join(r.root, jobID))
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deletes = append(r.deletes, deletedCall{JobID: jobID, DirPresent: err == nil && info.IsDir()})
	return nil
}

func (r *raceIndex) List(string) ([]IndexRecord, error) { return nil, nil }

func (r *raceIndex) Exists(string, int) (bool, error) { return false, nil }

// TestConcurrent_ExecuteWhilePurging 让"执行中同时跑一轮 TTL 清理"真的发生一次：
// TTL 短到清理侧随时能追上前一轮目录，两侧因此持续撞在一起。
// 这里要的是两件事——-race 干净，以及"先删目录、再删行"在并发下依然成立
// （每次 DeleteByJob 回看目录都必须已经没了）。
func TestConcurrent_ExecuteWhilePurging(t *testing.T) {
	root := filepath.Join(t.TempDir(), "exec")
	store := newStoreWithClock(t, root, time.Millisecond, time.Now)
	index := &raceIndex{root: root}
	store.SetIndex(index)

	cfg := configWith(t.TempDir(), shellCommand(t, "echo hello"))
	normalized := cfg.Normalized()
	profiles := mustLoad(t, normalized)
	require.Len(t, profiles, 1)
	runner := NewRunner(profiles[0], store, normalized.Executors, quietLogger())

	// 先放一个已经过期的目录，保证清理侧第一轮就有东西可删
	createArtifactDir(t, store, "job-victim", 1)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 15; i++ {
			job := &core.Job{ID: fmt.Sprintf("job-race-%d", i), Name: profiles[0].HandlerKey(), Attempts: 1}
			// 不要求成功：输出目录可能正被清理侧删掉，那正是本用例要制造的现场
			_ = runner.Handler()(context.Background(), job)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 30; i++ {
			_, _ = store.PurgeExpired()
		}
	}()
	wg.Wait()

	index.mu.Lock()
	defer index.mu.Unlock()
	assert.NotZero(t, index.records, "执行侧要至少登记成功一次")
	require.NotZero(t, index.deletes, "清理侧要至少删掉一个目录")
	for _, call := range index.deletes {
		assert.False(t, call.DirPresent, "删行时目录必须已经没了，并发也不例外：%s", call.JobID)
	}
}

// newStoreWithClock 造一个时间源可控的产物存储，用例不必 sleep 等 TTL 过期。
func newStoreWithClock(t *testing.T, root string, ttl time.Duration, now func() time.Time) *ArtifactStore {
	t.Helper()

	store, err := NewArtifactStore(ArtifactOptions{
		Dir:      root,
		MaxBytes: core.DefaultExecMaxOutputBytes,
		TTL:      ttl,
		Now:      now,
	}, quietLogger())
	require.NoError(t, err)
	return store
}

// createArtifactDir 建出一个任务目录并写一条输出，模拟"这个任务跑过"，返回该目录。
func createArtifactDir(t *testing.T, store *ArtifactStore, jobID string, attempt int) string {
	t.Helper()

	writer, err := store.Open(jobID, attempt)
	require.NoError(t, err)
	_, err = writer.Stdout().Write([]byte("line\n"))
	require.NoError(t, err)
	_, err = writer.Close()
	require.NoError(t, err)

	dir, err := store.jobDir(jobID)
	require.NoError(t, err)
	return dir
}

// makeUndeletable 尽力把目录变成删不掉的样子：Linux 去掉目录写权限，
// Windows 把里面的文件设成只读（Go 的 RemoveAll 在 Windows 上会自己重试可写性，
// 所以那条路径多半挡不住，由调用方跳过）。无论挡不挡住，收尾都要把权限还原，
// 否则 t.TempDir 自己清不掉。
func makeUndeletable(t *testing.T, dir string) {
	t.Helper()

	restore := func() {
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d == nil {
				return nil
			}
			return os.Chmod(path, 0o755)
		})
		_ = os.Chmod(dir, 0o755)
	}
	t.Cleanup(restore)

	if runtime.GOOS == "windows" {
		require.NoError(t, filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			return os.Chmod(path, 0o444)
		}))
		return
	}
	require.NoError(t, os.Chmod(dir, 0o500))
}
