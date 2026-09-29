package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bufferLogger 把日志写进缓冲区，供断言 warn / info 行。
func bufferLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, nil))
}

// artifactStoreFor 建一个把产物放在临时目录里的存储；MaxBytes 由调用方给，
// 便于构造截断。日志丢掉，需要断言日志的用例自己替换 store.logger。
func artifactStoreFor(t *testing.T, maxBytes int, ttl time.Duration, now func() time.Time) *ArtifactStore {
	t.Helper()

	store, err := NewArtifactStore(ArtifactOptions{
		Dir:      filepath.Join(t.TempDir(), "exec"),
		MaxBytes: maxBytes,
		TTL:      ttl,
		Now:      now,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	return store
}

func TestNewArtifactStore_CreatesRootDir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "nested", "exec")

	store, err := NewArtifactStore(ArtifactOptions{Dir: root, MaxBytes: 4096}, nil)
	require.NoError(t, err)

	info, err := os.Stat(store.Dir())
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	assert.Equal(t, root, store.Dir(), "相对路径应被解析成绝对路径，供接口与日志一致引用")
	assert.Equal(t, 4096, store.MaxBytes())
	assert.Equal(t, time.Duration(0), store.TTL(), "没配 TTL 就是不按时间清理")
}

func TestNewArtifactStore_RejectsBadOptions(t *testing.T) {
	t.Run("空目录", func(t *testing.T) {
		_, err := NewArtifactStore(ArtifactOptions{Dir: "   ", MaxBytes: 4096}, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "output dir must not be empty")
	})

	t.Run("负 TTL", func(t *testing.T) {
		_, err := NewArtifactStore(ArtifactOptions{Dir: t.TempDir(), MaxBytes: 4096, TTL: -time.Minute}, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "negative ttl")
	})
}

// TestNewArtifactStore_MaxBytesFloor 钉住第 6 条功能：配成 0 会让所有输出丢失，
// 所以抬到下限并留下一条 warn，而不是照 0 执行。
func TestNewArtifactStore_MaxBytesFloor(t *testing.T) {
	var logs bytes.Buffer

	store, err := NewArtifactStore(ArtifactOptions{
		Dir:      filepath.Join(t.TempDir(), "exec"),
		MaxBytes: 0,
	}, bufferLogger(&logs))
	require.NoError(t, err)

	assert.Equal(t, minArtifactMaxBytes, store.MaxBytes())
	output := logs.String()
	assert.Contains(t, output, "level=WARN")
	assert.Contains(t, output, "artifact max_bytes raised to minimum")
	assert.Contains(t, output, "configured=0")
	assert.Contains(t, output, fmt.Sprintf("effective=%d", minArtifactMaxBytes))
}

func TestArtifactWriter_RespectsMaxBytes(t *testing.T) {
	const limit = 1024
	store := artifactStoreFor(t, limit, 0, nil)

	writer, err := store.Open("job-cap", 1)
	require.NoError(t, err)

	chunk := bytes.Repeat([]byte("x"), 256)
	for i := 0; i < limit/256*3; i++ { // 写 3 倍上限
		n, err := writer.Stdout().Write(chunk)
		require.NoError(t, err)
		// 满额返回是 io.Writer 契约的要求：少返回会让 io.Copy 报 ErrShortWrite 并中断执行
		assert.Equal(t, len(chunk), n, "第 %d 次写入必须返回完整长度", i)
	}

	info, err := writer.Close()
	require.NoError(t, err)

	out, truncated, err := store.Read("job-cap", 1, "out", 0)
	require.NoError(t, err)
	assert.False(t, truncated, "Read 不限长度时不该报截断")
	assert.Len(t, out, limit, "文件实际大小必须正好是上限")

	assert.True(t, info.Truncated)
	assert.Equal(t, int64(limit), info.OutBytes)
	assert.Zero(t, info.ErrBytes)
	assert.Equal(t, writer.OutPath(), info.OutPath)
	assert.Equal(t, writer.ErrPath(), info.ErrPath)
}

// TestArtifactWriter_SmallWritesPastLimit 覆盖"截断发生在某次写入的中间"这种切分方式：
// 一次写入跨在上限边界上时，剩下的名额要落盘、差额记进 dropped，且仍然满额返回。
func TestArtifactWriter_SmallWritesPastLimit(t *testing.T) {
	store := artifactStoreFor(t, minArtifactMaxBytes, 0, nil)
	const limit = minArtifactMaxBytes

	writer, err := store.Open("job-boundary", 0)
	require.NoError(t, err)

	head := bytes.Repeat([]byte("a"), limit-24)
	n, err := writer.Stdout().Write(head)
	require.NoError(t, err)
	assert.Equal(t, len(head), n)

	// 还剩 24 字节名额，这一笔 30 字节里有 6 字节要被挡掉
	n, err = writer.Stdout().Write(bytes.Repeat([]byte("b"), 30))
	require.NoError(t, err)
	assert.Equal(t, 30, n)

	info, err := writer.Close()
	require.NoError(t, err)
	assert.Equal(t, int64(limit), info.OutBytes)
	assert.True(t, info.Truncated)

	out, _, err := store.Read("job-boundary", 0, "out", 0)
	require.NoError(t, err)
	assert.Equal(t, append(head, bytes.Repeat([]byte("b"), 24)...), out)

	// 已经到上限之后的写入同样满额返回，io.Copy 不会因为"少写了"而报 ErrShortWrite
	n, err = writer.Stdout().Write([]byte("more"))
	require.NoError(t, err)
	assert.Equal(t, 4, n)
}

func TestArtifactWriter_PerAttemptFiles(t *testing.T) {
	store := artifactStoreFor(t, 4096, 0, nil)

	// 三次尝试共用一个任务 ID（CloneForRetry 保留 ID），各自的文件必须互不覆盖
	for attempt, content := range map[int]string{1: "first run", 2: "second run", 3: "third run"} {
		writer, err := store.Open("job-retry", attempt)
		require.NoError(t, err)
		_, err = writer.Stdout().Write([]byte(content))
		require.NoError(t, err)
		_, err = writer.Close()
		require.NoError(t, err)
	}

	for attempt, content := range map[int]string{1: "first run", 2: "second run", 3: "third run"} {
		data, _, err := store.Read("job-retry", attempt, "out", 0)
		require.NoError(t, err)
		assert.Equal(t, content, string(data), "attempt %d 的输出被别的尝试覆盖了", attempt)
	}

	dir := filepath.Join(store.Dir(), "job-retry")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	assert.ElementsMatch(t,
		[]string{"a1.out", "a1.err", "a2.out", "a2.err", "a3.out", "a3.err"}, names)
}

// TestOpen_RejectsBadJobID 是本卡最重要的输入检查：产物目录名由调用方给，
// 一旦允许分隔符或 `..`，路径拼接就成了任意位置读写。
func TestOpen_RejectsBadJobID(t *testing.T) {
	store := artifactStoreFor(t, 4096, 0, nil)
	rootBefore := listTree(t, store.Dir())

	for _, jobID := range []string{
		"",
		"../escape",
		"a/b",
		`a\b`,
		"/abs/path",
		"C:\\Windows\\Temp",
		"job.name",
		"job name",
		"job\nname",
		"..",
		".hidden",
		strings.Repeat("j", 129),
	} {
		writer, err := store.Open(jobID, 1)
		require.Error(t, err, "job id %q 必须被拒绝", jobID)
		assert.Nil(t, writer)
		assert.Contains(t, err.Error(), "job id")
	}

	assert.Equal(t, rootBefore, listTree(t, store.Dir()), "被拒绝的 ID 不能留下任何文件")

	// 读写入口用同一套校验
	_, _, err := store.Read("../escape", 1, "out", 0)
	require.Error(t, err)
	_, _, err = store.Tail(`..\x`, 1, "err", 10)
	require.Error(t, err)
	assert.False(t, store.Exists("../escape", 1))
	require.Error(t, store.Remove("../escape"))
}

func TestRead_And_Tail(t *testing.T) {
	store := artifactStoreFor(t, 1<<20, 0, nil)

	writer, err := store.Open("job-size", 1)
	require.NoError(t, err)
	body := bytes.Repeat([]byte("0123456789"), 1024) // 10KB，头部是 "0123456789"、尾部是同一串
	_, err = writer.Stdout().Write(body)
	require.NoError(t, err)
	_, err = writer.Stderr().Write([]byte("boom"))
	require.NoError(t, err)
	_, err = writer.Close()
	require.NoError(t, err)

	t.Run("Read 从头部保留", func(t *testing.T) {
		data, truncated, err := store.Read("job-size", 1, "out", 1024)
		require.NoError(t, err)
		assert.True(t, truncated)
		assert.Len(t, data, 1024)
		assert.True(t, bytes.HasPrefix(data, []byte("0123456789")))
	})

	t.Run("Read 正好等于上限不算截断", func(t *testing.T) {
		data, truncated, err := store.Read("job-size", 1, "err", 4)
		require.NoError(t, err)
		assert.False(t, truncated)
		assert.Equal(t, "boom", string(data))
	})

	t.Run("Tail 从末尾取", func(t *testing.T) {
		data, truncated, err := store.Tail("job-size", 1, "out", 1024)
		require.NoError(t, err)
		assert.True(t, truncated)
		assert.Len(t, data, 1024)
		assert.True(t, bytes.HasSuffix(data, []byte("0123456789")))
	})

	t.Run("Tail 超过文件长度返回全文", func(t *testing.T) {
		data, truncated, err := store.Tail("job-size", 1, "out", 1<<20)
		require.NoError(t, err)
		assert.False(t, truncated)
		assert.Equal(t, body, data)
	})

	t.Run("Tail 只要结论", func(t *testing.T) {
		data, truncated, err := store.Tail("job-size", 1, "out", 0)
		require.NoError(t, err)
		assert.Empty(t, data)
		assert.True(t, truncated, "文件非空时 n=0 应报截断")
	})

	t.Run("Exists 认 stdout 不认缺失", func(t *testing.T) {
		assert.True(t, store.Exists("job-size", 1))
		assert.False(t, store.Exists("job-size", 2), "没跑过的 attempt 不该被认为有产物")
		assert.False(t, store.Exists("job-absent", 1))
	})
}

func TestRead_UnknownStream(t *testing.T) {
	store := artifactStoreFor(t, 4096, 0, nil)

	writer, err := store.Open("job-stream", 1)
	require.NoError(t, err)
	_, err = writer.Stdout().Write([]byte("data"))
	require.NoError(t, err)
	_, err = writer.Close()
	require.NoError(t, err)

	for _, stream := range []string{"meta", "stdout", "OUT", "", "out ", "log"} {
		_, _, err := store.Read("job-stream", 1, stream, 0)
		require.Error(t, err, "stream=%q 必须被拒绝", stream)
		assert.Contains(t, err.Error(), "unknown stream")

		_, _, err = store.Tail("job-stream", 1, stream, 10)
		require.Error(t, err, "stream=%q 必须被拒绝", stream)
	}
}

func TestRead_MissingFileIsSentinel(t *testing.T) {
	store := artifactStoreFor(t, 4096, 0, nil)

	_, _, err := store.Read("job-none", 1, "out", 0)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrArtifactMissing, "接口层要能据此区分 404 与 500")

	_, _, err = store.Tail("job-none", 1, "err", 10)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrArtifactMissing)
}

func TestPurgeExpired(t *testing.T) {
	now := time.Now()
	store := artifactStoreFor(t, 4096, 7*24*time.Hour, func() time.Time { return now })

	fresh := filepath.Join(store.Dir(), "job-fresh")
	stale := filepath.Join(store.Dir(), "job-stale")
	require.NoError(t, os.MkdirAll(fresh, artifactDirPerm))
	require.NoError(t, os.MkdirAll(stale, artifactDirPerm))
	// 目录 mtime 在写入文件时会被刷新，所以先建目录再改时间
	require.NoError(t, os.Chtimes(stale, now.Add(-8*24*time.Hour), now.Add(-8*24*time.Hour)))

	deleted, err := store.PurgeExpired()
	require.NoError(t, err)
	assert.Equal(t, 1, deleted)
	assert.DirExists(t, fresh)
	assert.NoDirExists(t, stale)
}

func TestPurgeExpired_DisabledWithoutTTL(t *testing.T) {
	store := artifactStoreFor(t, 4096, 0, nil)
	require.NoError(t, os.MkdirAll(filepath.Join(store.Dir(), "job-old"), artifactDirPerm))

	deleted, err := store.PurgeExpired()
	require.NoError(t, err)
	assert.Zero(t, deleted, "TTL=0 表示不按时间清理")
	assert.DirExists(t, filepath.Join(store.Dir(), "job-old"))
}

func TestPurgeOrphans_LiveFuncError(t *testing.T) {
	store := artifactStoreFor(t, 4096, 0, nil)
	keep := filepath.Join(store.Dir(), "job-keep")
	require.NoError(t, os.MkdirAll(keep, artifactDirPerm))

	var logs bytes.Buffer
	store.logger = bufferLogger(&logs)

	deleted, err := store.PurgeOrphans(func() (map[string]bool, error) {
		return nil, errors.New("store unavailable")
	})
	require.NoError(t, err, "存储读不出来时只记日志，不返回错误、不删文件")
	assert.Zero(t, deleted)
	assert.DirExists(t, keep)

	output := logs.String()
	assert.Contains(t, output, "artifact orphan scan skipped because the live job set is unavailable")
	assert.Contains(t, output, "store unavailable")

	// 没有 live 提供方便无法判断存活：同样报错而不是"全都删"
	_, err = store.PurgeOrphans(nil)
	require.Error(t, err)
	assert.DirExists(t, keep)
}

func TestPurgeOrphans_RemovesMissingJobs(t *testing.T) {
	store := artifactStoreFor(t, 4096, 0, nil)
	for _, jobID := range []string{"job-live", "job-cancelled", "job-trimmed"} {
		require.NoError(t, os.MkdirAll(filepath.Join(store.Dir(), jobID), artifactDirPerm))
	}

	deleted, err := store.PurgeOrphans(func() (map[string]bool, error) {
		return map[string]bool{"job-live": true}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, 2, deleted)
	assert.DirExists(t, filepath.Join(store.Dir(), "job-live"))
	assert.NoDirExists(t, filepath.Join(store.Dir(), "job-cancelled"))
	assert.NoDirExists(t, filepath.Join(store.Dir(), "job-trimmed"))
}

// TestJobDirs_IgnoresUnusableNames 保护手工放进产物根目录的别人的目录：
// 名字不像任务 ID 的条目既不报错也不被删。
func TestJobDirs_IgnoresUnusableNames(t *testing.T) {
	now := time.Now()
	store := artifactStoreFor(t, 4096, time.Hour, func() time.Time { return now })

	neighbor := filepath.Join(store.Dir(), "someone.elses.data")
	require.NoError(t, os.MkdirAll(neighbor, artifactDirPerm))
	require.NoError(t, os.Chtimes(neighbor, now.Add(-24*time.Hour), now.Add(-24*time.Hour)))
	require.NoError(t, os.WriteFile(filepath.Join(store.Dir(), "loose.txt"), []byte("x"), artifactFilePerm))

	var logs bytes.Buffer
	store.logger = bufferLogger(&logs)

	deleted, err := store.PurgeExpired()
	require.NoError(t, err)
	assert.Zero(t, deleted)
	assert.DirExists(t, neighbor)

	deleted, err = store.PurgeOrphans(func() (map[string]bool, error) { return map[string]bool{}, nil })
	require.NoError(t, err)
	assert.Zero(t, deleted)
	assert.DirExists(t, neighbor)
	assert.Contains(t, logs.String(), "leaving it alone")
}

func TestWriteMeta_Atomic(t *testing.T) {
	store := artifactStoreFor(t, 4096, 0, nil)

	writer, err := store.Open("job-meta", 2)
	require.NoError(t, err)

	first := ArtifactInfo{JobID: "job-meta", Attempt: 2, OutBytes: 11}
	require.NoError(t, writer.WriteMeta(first))

	dir := filepath.Join(store.Dir(), "job-meta")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, entry := range entries {
		assert.False(t, strings.HasSuffix(entry.Name(), ".tmp"),
			"meta 写完不该留下临时文件：%s", entry.Name())
	}

	content, err := os.ReadFile(filepath.Join(dir, metaFileName(2)))
	require.NoError(t, err)
	assert.Contains(t, string(content), `"attempt": 2`)

	// 再写一次是覆盖，同样不留临时文件
	require.NoError(t, writer.WriteMeta(ArtifactInfo{JobID: "job-meta", Attempt: 2, OutBytes: 22}))
	entries, err = os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 3, "两个流 + 一份 meta")

	content, err = os.ReadFile(filepath.Join(dir, metaFileName(2)))
	require.NoError(t, err)
	assert.Contains(t, string(content), `"out_bytes": 22`)

	// 无法编码的对象要报错而不是写出一份空文件
	require.Error(t, writer.WriteMeta(func() {}))
	_, err = writer.Close()
	require.NoError(t, err)
}

// TestStart_StopsWithContext 覆盖后台清理的两端：启动那一轮同步删掉孤儿、
// ctx 取消后协程退出（用返回的通道断言，不靠 sleep 猜时长）。
func TestStart_StopsWithContext(t *testing.T) {
	store := artifactStoreFor(t, 4096, 0, nil)
	keep := filepath.Join(store.Dir(), "job-live")
	orphan := filepath.Join(store.Dir(), "job-gone")
	require.NoError(t, os.MkdirAll(keep, artifactDirPerm))
	require.NoError(t, os.MkdirAll(orphan, artifactDirPerm))

	ctx, cancel := context.WithCancel(context.Background())
	liveCalls := 0
	done := store.Start(ctx, func() (map[string]bool, error) {
		liveCalls++
		return map[string]bool{"job-live": true}, nil
	})

	// 第一轮在 Start 返回之前就跑完了，所以这里可以直接断言删除结果
	assert.Equal(t, 1, liveCalls, "启动扫描必须问过存活任务集合")
	assert.DirExists(t, keep)
	assert.NoDirExists(t, orphan)

	cancel()
	waitClosed(t, done, "the cleaner must exit when the context is cancelled")
}

// TestArtifactWriter_IOCopyContract 直接跑 io.Copy——exec.Cmd 的输出复制用的就是它。
// 截断之后 io.Copy 必须"把整个源复制完且不报错"：限制写入器一旦少返回字节，
// io.Copy 会以 io.ErrShortWrite 中断，E09 的执行就会因为"输出太长"被砍掉。
func TestArtifactWriter_IOCopyContract(t *testing.T) {
	store := artifactStoreFor(t, minArtifactMaxBytes, 0, nil)
	writer, err := store.Open("job-iocopy", 1)
	require.NoError(t, err)

	source := bytes.NewReader(bytes.Repeat([]byte("z"), minArtifactMaxBytes*4))
	copied, err := io.Copy(writer.Stdout(), source)
	require.NoError(t, err)
	assert.Equal(t, int64(minArtifactMaxBytes*4), copied, "io.Copy 必须认为整段输出都写掉了")

	info, err := writer.Close()
	require.NoError(t, err)
	assert.Equal(t, int64(minArtifactMaxBytes), info.OutBytes)
	assert.True(t, info.Truncated)

	onDisk, truncated, err := store.Read("job-iocopy", 1, "out", 0)
	require.NoError(t, err)
	assert.False(t, truncated)
	assert.Len(t, onDisk, minArtifactMaxBytes)
}

func TestRemove(t *testing.T) {
	store := artifactStoreFor(t, 4096, 0, nil)

	writer, err := store.Open("job-remove", 1)
	require.NoError(t, err)
	_, err = writer.Stdout().Write([]byte("data"))
	require.NoError(t, err)
	_, err = writer.Close()
	require.NoError(t, err)
	require.NoError(t, writer.WriteMeta(map[string]string{"ok": "yes"}))

	require.NoError(t, store.Remove("job-remove"))
	assert.NoDirExists(t, filepath.Join(store.Dir(), "job-remove"))

	// 没产物时删除是空操作，不报错
	require.NoError(t, store.Remove("job-never-ran"))
}

// TestArtifactWriter_DoubleClose 让收尾幂等：E09 的执行路径可能在 defer 里再关一次。
func TestArtifactWriter_DoubleClose(t *testing.T) {
	store := artifactStoreFor(t, 4096, 0, nil)

	writer, err := store.Open("job-close", 1)
	require.NoError(t, err)
	_, err = writer.Stdout().Write([]byte("abc"))
	require.NoError(t, err)

	first, err := writer.Close()
	require.NoError(t, err)
	assert.Equal(t, int64(3), first.OutBytes)

	second, err := writer.Close()
	require.NoError(t, err)
	assert.Equal(t, first, second, "重复 Close 必须给出同一份结论")
}

// TestArtifactStore_Permissions 断言权限位不放开给其它用户。
// Windows 上 Go 的 chmod 只实现只读位，0750 与 0640 都会退化，所以这里跳过并写明原因。
func TestArtifactStore_Permissions(t *testing.T) {
	if runtimeIsWindows() {
		t.Skip("Windows 上 NTFS 权限不由 mode bits 表达，os.Chmod 只影响只读位；这条断言留给 Unix 与 CI")
	}

	store := artifactStoreFor(t, 4096, 0, nil)
	writer, err := store.Open("job-perm", 1)
	require.NoError(t, err)
	require.NoError(t, writer.WriteMeta(map[string]string{"k": "v"}))
	_, err = writer.Close()
	require.NoError(t, err)

	dirInfo, err := os.Stat(filepath.Join(store.Dir(), "job-perm"))
	require.NoError(t, err)
	assert.Equal(t, artifactDirPerm, dirInfo.Mode().Perm())

	for _, name := range []string{"a1.out", "a1.err", metaFileName(1)} {
		info, err := os.Stat(filepath.Join(store.Dir(), "job-perm", name))
		require.NoError(t, err)
		assert.Equal(t, artifactFilePerm, info.Mode().Perm(), "%s 的权限位", name)
	}
}

// runtimeIsWindows 用一个不依赖 build tag 的判断，让同一份测试文件在两个平台都能编译。
func runtimeIsWindows() bool { return os.PathSeparator == '\\' }

func waitClosed(t *testing.T, ch <-chan struct{}, reason string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal(reason)
	}
}

// listTree 返回目录树的相对路径集合，用来断言"被拒绝的输入没留下文件"。
func listTree(t *testing.T, root string) []string {
	t.Helper()

	var items []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		items = append(items, relative)
		return nil
	})
	require.NoError(t, err)
	return items
}
