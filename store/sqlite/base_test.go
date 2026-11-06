package sqlite

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 本文件直接测 base.go 的机制，不经过任何业务包的 Open：
// 选项校验、PRAGMA 生效、迁移框架的幂等与失败回滚。观测层行为（建哪些表、
// Stats 读什么）由 db_test.go 与各业务文件自己的测试覆盖。

func TestOpenFile_RejectsBadOptions(t *testing.T) {
	t.Run("空路径", func(t *testing.T) {
		_, err := OpenFile(Options{Path: "   "}, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must not be empty")
	})

	t.Run("路径含DSN特殊字符", func(t *testing.T) {
		for _, bad := range []string{"a?b.sqlite", "a#b.sqlite"} {
			_, err := OpenFile(Options{Path: filepath.Join(t.TempDir(), bad)}, nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "must not contain")
		}
	})

	t.Run("非法synchronous", func(t *testing.T) {
		_, err := OpenFile(Options{
			Path:        filepath.Join(t.TempDir(), "x.sqlite"),
			Synchronous: "off",
		}, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "synchronous")
	})
}

func TestOpenFile_SetsPragmas(t *testing.T) {
	db, err := OpenFile(Options{Path: filepath.Join(t.TempDir(), "base.sqlite")}, nil)
	require.NoError(t, err)
	defer db.Close()

	// 未给 MaxOpenConns 时按 1 处理；未给 Synchronous 时用包内默认值。
	assert.Equal(t, 1, db.sqlDB.Stats().MaxOpenConnections)
	assert.Equal(t, DefaultSynchronous, db.Synchronous())
	assert.Equal(t, "wal", strings.ToLower(db.JournalMode()))
}

func TestMigrate_AppliesAndIsIdempotent(t *testing.T) {
	db, err := OpenFile(Options{Path: filepath.Join(t.TempDir(), "mig.sqlite")}, nil)
	require.NoError(t, err)
	defer db.Close()

	migs := []Migration{
		{Version: 1, Up: []string{`CREATE TABLE IF NOT EXISTS t1 (id INTEGER PRIMARY KEY)`}},
		{Version: 2, Up: []string{`INSERT INTO t1 (id) VALUES (1)`}},
	}
	require.NoError(t, db.Migrate("mig_schema_migrations", migs))

	var count int
	require.NoError(t, db.SQL().QueryRow(`SELECT COUNT(*) FROM t1`).Scan(&count))
	assert.Equal(t, 1, count)

	// 重跑同一份迁移必须无害：版本记录挡住重复执行，表与数据不动。
	require.NoError(t, db.Migrate("mig_schema_migrations", migs))
	require.NoError(t, db.SQL().QueryRow(`SELECT COUNT(*) FROM t1`).Scan(&count))
	assert.Equal(t, 1, count)

	// 只补跑更高版本，低版本不重复。
	require.NoError(t, db.Migrate("mig_schema_migrations", []Migration{
		{Version: 2, Up: []string{`INSERT INTO t1 (id) VALUES (999)`}},
		{Version: 3, Up: []string{`INSERT INTO t1 (id) VALUES (2)`}},
	}))
	require.NoError(t, db.SQL().QueryRow(`SELECT COUNT(*) FROM t1`).Scan(&count))
	assert.Equal(t, 2, count)
}

func TestMigrate_FailureDoesNotRecordVersion(t *testing.T) {
	db, err := OpenFile(Options{Path: filepath.Join(t.TempDir(), "fail.sqlite")}, nil)
	require.NoError(t, err)
	defer db.Close()

	broken := []Migration{
		{Version: 1, Up: []string{
			`CREATE TABLE IF NOT EXISTS t1 (id INTEGER PRIMARY KEY)`,
			`INSERT INTO no_such_table VALUES (1)`, // 第二条失败，整个版本回滚
		}},
	}
	err = db.Migrate("fail_schema_migrations", broken)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "apply migration 1")

	// 版本号没落下：修复后重跑同一版本要能成功（建表语句也重放，IF NOT EXISTS 保证无害）。
	var applied int
	require.NoError(t, db.SQL().QueryRow(
		`SELECT COALESCE(MAX(version), 0) FROM fail_schema_migrations`).Scan(&applied))
	assert.Equal(t, 0, applied)

	require.NoError(t, db.Migrate("fail_schema_migrations", []Migration{
		{Version: 1, Up: []string{`CREATE TABLE IF NOT EXISTS t1 (id INTEGER PRIMARY KEY)`}},
	}))
	require.NoError(t, db.SQL().QueryRow(`SELECT COUNT(*) FROM t1`).Scan(&applied))
}

func TestClose_IsIdempotentAndSQLStopsWorking(t *testing.T) {
	db, err := OpenFile(Options{Path: filepath.Join(t.TempDir(), "close.sqlite")}, nil)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	require.NoError(t, db.Close()) // 二次关闭返回第一次的结果，不报错

	// 关闭之后连接不可再用：这是装配顺序错误的兜底证明（见 SQL 的注释）。
	err = db.SQL().Ping()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "database is closed")
}

// TestOpenFile_FilePermissions 固定库文件 0640、父目录 0750（设计文档 §10.4）。
// Windows 上 NTFS 权限不由 mode bits 表达，os.Chmod 只影响只读位，
// 这条断言留给 Unix 与 CI（照 executor/artifact_test.go 的既有处理）。
func TestOpenFile_FilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上 NTFS 权限不由 mode bits 表达，这条断言在 Unix 与 CI 生效")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "base.sqlite")
	db, err := OpenFile(Options{Path: path}, nil)
	require.NoError(t, err)
	defer db.Close()

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, filePerm, info.Mode().Perm(), "database file mode")
	parent, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	assert.Equal(t, dirPerm, parent.Mode().Perm(), "parent dir mode")
}
