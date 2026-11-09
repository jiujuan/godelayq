package jobs

import (
	"testing"

	"github.com/jiujuan/godelayq/internal/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// maxMigrationVersion 取迁移清单的最高版本号：schema_version 读数的期望值来源。
// 清单加版本时这条用例自动跟上，不需要手工维护期望数字。
func maxMigrationVersion() int {
	maxV := 0
	for _, m := range migrations {
		if m.Version > maxV {
			maxV = m.Version
		}
	}
	return maxV
}

// Store.DBRuntime（G4/Q05）：db 段四个读数的 SQLite 侧判据。
func TestStoreDBRuntime_SQLite(t *testing.T) {
	s := openSQLiteStore(t)

	rt, err := s.DBRuntime()
	require.NoError(t, err)

	assert.Equal(t, "sqlite", rt.Dialect)
	assert.Equal(t, maxMigrationVersion(), rt.SchemaVersion,
		"迁移全部应用后，读数该是清单的最高版本")
	assert.Equal(t, 1, rt.Pool.MaxOpen, "任务库固定单连接")
	assert.Equal(t, int64(0), rt.OutboxPending, "没写过的出箱表积压为 0")

	// 积压计数随 AppendOutbox 增长：读数与出箱表同源（pendingOutboxCount 共用）。
	require.NoError(t, s.AppendOutbox([]core.Event{
		{Type: core.EventJobStarted, JobID: "j1"},
		{Type: core.EventJobCompleted, JobID: "j1"},
	}))
	rt, err = s.DBRuntime()
	require.NoError(t, err)
	assert.Equal(t, int64(2), rt.OutboxPending)
}

// PG 侧同一组判据（未配 DSN 自然跳过）：方言名与版本读数在两方言上同语义。
func TestStoreDBRuntime_Postgres(t *testing.T) {
	s := openPostgresStore(t)

	rt, err := s.DBRuntime()
	require.NoError(t, err)

	assert.Equal(t, "postgres", rt.Dialect)
	assert.Equal(t, maxMigrationVersion(), rt.SchemaVersion)
	assert.Equal(t, int64(0), rt.OutboxPending)
}
