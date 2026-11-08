package pgtest

import (
	"context"
	"testing"

	"github.com/jiujuan/godelayq/internal/store/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
)

// TestPostgresEngineSmoke 是 PG 方言的冒烟用例：真实建库 → 打开 → 迁移（含 IDENTITY
// 自增与 lower(name) 表达式唯一索引两个方言分歧点）→ 写读往返 → 大小写变体 upsert
// → 拆库。它同时是 B05 风险清单第一项（ON CONFLICT (lower(name)) 表达式冲突目标）
// 的实证：这一条不过，mgr 包的 Save 语义在 PG 上就不成立。
func TestPostgresEngineSmoke(t *testing.T) {
	dsn := Database(t, "smoke")

	db, err := engine.Open(engine.Options{Dialect: engine.DialectPostgres, DSN: dsn}, nil)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// Path 必须是脱敏 DSN：密码不出现在任何日志与读数字段里。
	assert.NotContains(t, db.Path(), "admin123", "Path() must not leak the DSN password")
	assert.Equal(t, "n/a", db.JournalMode())

	migrations := []engine.Migration{
		{
			Version: 1,
			UpPostgres: []string{
				`CREATE TABLE IF NOT EXISTS smoke_rows (
  seq       BIGINT GENERATED ALWAYS AS IDENTITY,
  name      TEXT PRIMARY KEY,
  payload   TEXT NOT NULL
);`,
				`CREATE UNIQUE INDEX IF NOT EXISTS idx_smoke_rows_name_lower ON smoke_rows (lower(name));`,
			},
		},
	}
	require.NoError(t, db.Migrate("smoke_schema_migrations", migrations))

	type smokeRow struct {
		bun.BaseModel `bun:"table:smoke_rows"`
		Seq           int64  `bun:"column:seq,autoincrement"`
		Name          string `bun:"column:name,pk"`
		Payload       string `bun:"column:payload"`
	}

	// IDENTITY 自增列：INSERT 省略 seq，读回非零。
	row := &smokeRow{Name: "alpha", Payload: "p1"}
	_, err = db.NewInsert().Model(row).Exec(context.Background())
	require.NoError(t, err)
	assert.NotZero(t, row.Seq, "IDENTITY 列应在插入后回填")

	var got smokeRow
	require.NoError(t, db.NewSelect().Model(&got).Where("name = ?", "alpha").Scan(context.Background()))
	assert.Equal(t, "p1", got.Payload)

	// lower(name) 表达式冲突目标：大小写变体命中同一行（mgr Save 语义的根基）。
	upsert := &smokeRow{Name: "ALPHA", Payload: "p2"}
	_, err = db.NewInsert().Model(upsert).
		On("CONFLICT (lower(name)) DO UPDATE").
		Set("payload = EXCLUDED.payload").
		Exec(context.Background())
	require.NoError(t, err, "ON CONFLICT (lower(name)) 必须被 PG 接受")

	count, err := db.NewSelect().Model((*smokeRow)(nil)).Count(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(1), count, "变体 upsert 必须命中同一行")
}
