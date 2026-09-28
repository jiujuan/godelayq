package core

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// retryPtr 构造 *int 以区分"未写 max_retries"与"显式写 0"
func retryPtr(n int) *int { return &n }

func newFormatLoader(t *testing.T) *DirectoryLoader {
	t.Helper()

	loader, err := NewDirectoryLoader(NewScheduler(nil, nil, nil), LoaderOptions{Dir: t.TempDir()})
	require.NoError(t, err)
	return loader
}

func TestDirectoryLoader_FormatToJob_MaxRetriesSemantics(t *testing.T) {
	loader := newFormatLoader(t)

	// 未写：沿用历史默认 3
	job, err := loader.formatToJob(&FileJobFormat{Name: "no-field"})
	require.NoError(t, err)
	assert.Equal(t, DefaultLoaderMaxRetries, job.MaxRetries)

	// 显式 0：不重试，不能被偷偷抬回默认值
	job, err = loader.formatToJob(&FileJobFormat{Name: "zero", MaxRetries: retryPtr(0)})
	require.NoError(t, err)
	assert.Equal(t, 0, job.MaxRetries)

	// 显式正数照抄
	job, err = loader.formatToJob(&FileJobFormat{Name: "two", MaxRetries: retryPtr(2)})
	require.NoError(t, err)
	assert.Equal(t, 2, job.MaxRetries)

	// 负数无意义，直接拒绝
	_, err = loader.formatToJob(&FileJobFormat{Name: "neg", MaxRetries: retryPtr(-1)})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "max_retries must not be negative")
}

// name 是 Handler 查找键，留空的任务永远执行不了
func TestDirectoryLoader_FormatToJob_RequiresName(t *testing.T) {
	loader := newFormatLoader(t)

	for _, name := range []string{"", "   "} {
		_, err := loader.formatToJob(&FileJobFormat{Name: name, Delay: "1m"})
		require.Error(t, err, "name=%q", name)
		assert.Contains(t, err.Error(), "name is required")
	}

	_, err := loader.formatToJob(&FileJobFormat{Name: "ok", Delay: "1m"})
	require.NoError(t, err)
}

// 老文件里的 tags/description 已成无人消费的死字段，但解码必须继续容忍未知键
func TestDirectoryLoader_LoadFile_IgnoresUnknownFields(t *testing.T) {
	dir := t.TempDir()
	store, err := NewJSONFileStore(filepath.Join(t.TempDir(), "jobs.json"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	loader, err := NewDirectoryLoader(NewScheduler(store, nil, nil), LoaderOptions{
		Dir:            dir,
		PostLoadAction: KeepAfterLoad,
	})
	require.NoError(t, err)

	path := filepath.Join(dir, "legacy.json")
	require.NoError(t, os.WriteFile(path, []byte(`{
		"id":"legacy-1","name":"payment_check","delay":"5m",
		"tags":["a","b"],"description":"旧文件里没人读的字段"
	}`), 0o644))

	require.NoError(t, loader.ScanAndLoad())

	snapshots, err := store.LoadAll()
	require.NoError(t, err)
	require.Len(t, snapshots, 1)
	assert.Equal(t, "legacy-1", snapshots[0].ID)
}

// 校验失败的文件走 ErrorDir 留档，且必须在日志里看得见
func TestDirectoryLoader_ScanAndLoad_StashesInvalidFile(t *testing.T) {
	dir := t.TempDir()
	errorDir := filepath.Join(dir, "errors")

	logs := &bytes.Buffer{}
	store, err := NewJSONFileStore(filepath.Join(t.TempDir(), "jobs.json"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	loader, err := NewDirectoryLoader(NewScheduler(store, nil, nil), LoaderOptions{
		Dir:            dir,
		ErrorDir:       errorDir,
		PostLoadAction: KeepAfterLoad,
		Logger:         slog.New(slog.NewTextHandler(logs, nil)),
	})
	require.NoError(t, err)

	// 空 name：校验阶段就失败
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bad.json"),
		[]byte(`{"id":"bad-1","delay":"1m"}`), 0o644))
	// 合法文件不受影响
	require.NoError(t, os.WriteFile(filepath.Join(dir, "good.json"),
		[]byte(`{"id":"good-1","name":"payment_check","delay":"1h"}`), 0o644))

	require.NoError(t, loader.ScanAndLoad())

	snapshots, err := store.LoadAll()
	require.NoError(t, err)
	require.Len(t, snapshots, 1)
	assert.Equal(t, "good-1", snapshots[0].ID)

	stashed, err := os.ReadFile(filepath.Join(errorDir, "bad.json.error"))
	require.NoError(t, err, "非法文件应被复制到 ErrorDir 留档")
	assert.Contains(t, string(stashed), "name is required")

	assert.Contains(t, logs.String(), "failed to load job file")
	assert.Contains(t, logs.String(), "path="+filepath.Join(dir, "bad.json"))
}

// ErrorDir 不可用时必须报错，而不是静默丢弃失败文件
func TestDirectoryLoader_HandleErrorFile_ReportsFailures(t *testing.T) {
	dir := t.TempDir()

	// 用一个普通文件占住 ErrorDir，MkdirAll 必然失败
	blocked := filepath.Join(dir, "blocked")
	require.NoError(t, os.WriteFile(blocked, []byte("not a dir"), 0o644))

	loader, err := NewDirectoryLoader(NewScheduler(nil, nil, nil), LoaderOptions{Dir: dir, ErrorDir: blocked})
	require.NoError(t, err)

	src := filepath.Join(dir, "job.json")
	require.NoError(t, os.WriteFile(src, []byte("garbage"), 0o644))

	err = loader.handleErrorFile(src, json.Unmarshal([]byte("garbage"), &FileJobFormat{}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "create error dir")
}

// 未配置 ErrorDir 时什么都不做：原始错误由调用方记录
func TestDirectoryLoader_HandleErrorFile_WithoutErrorDir(t *testing.T) {
	dir := t.TempDir()

	loader, err := NewDirectoryLoader(NewScheduler(nil, nil, nil), LoaderOptions{Dir: dir})
	require.NoError(t, err)

	src := filepath.Join(dir, "job.json")
	require.NoError(t, os.WriteFile(src, []byte("garbage"), 0o644))

	assert.NoError(t, loader.handleErrorFile(src, os.ErrInvalid))
	assert.FileExists(t, src, "未配置 ErrorDir 不应动原文件")
}
