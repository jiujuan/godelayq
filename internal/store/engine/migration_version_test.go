package engine

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// MigrationVersion（G4/Q05）：版本号读数与 Migrate 共用同一张表、同一条语义。
func TestMigrationVersion_ReadsAppliedVersions(t *testing.T) {
	db, err := Open(Options{Path: filepath.Join(t.TempDir(), "ver.sqlite")}, nil)
	require.NoError(t, err)
	defer db.Close()

	// 迁移表由 Migrate 创建：读数先于建表是装配错误，报错而不是 0——
	// 0 会被运维读成"全新库"，表不存在读成 0 是在说谎。
	_, err = db.MigrationVersion("ver_schema_migrations")
	require.Error(t, err)

	migs := []Migration{
		{Version: 1, Up: []string{`CREATE TABLE IF NOT EXISTS t1 (id INTEGER PRIMARY KEY)`}},
		{Version: 4, Up: []string{`INSERT INTO t1 (id) VALUES (1)`}},
	}
	require.NoError(t, db.Migrate("ver_schema_migrations", migs))

	v, err := db.MigrationVersion("ver_schema_migrations")
	require.NoError(t, err)
	assert.Equal(t, 4, v)

	// 空表（迁移清单为空但表已建）读 0：COALESCE 的语义。
	require.NoError(t, db.Migrate("empty_schema_migrations", nil))
	v, err = db.MigrationVersion("empty_schema_migrations")
	require.NoError(t, err)
	assert.Equal(t, 0, v)
}

// Stats 透传连接池读数（G4/Q05）：机制读数归 engine，业务包折算自这里。
func TestDBStats_Passthrough(t *testing.T) {
	db, err := Open(Options{Path: filepath.Join(t.TempDir(), "stats.sqlite"), MaxOpenConns: 2}, nil)
	require.NoError(t, err)
	defer db.Close()

	st := db.Stats()
	assert.Equal(t, 2, st.MaxOpenConnections)
	// Open 时的连通性检查（Ping）已经建了一条连接，所以不是 0。
	assert.Equal(t, 1, st.OpenConnections)
}
