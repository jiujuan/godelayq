package observe

import "github.com/jiujuan/godelayq/internal/store/engine"

// 三张表的 DDL 与 docs/design/sqlite-observability-design.md §6 逐字一致。
// 表名、列名、索引名一经落地就是契约：S03/S05/S06 只读写，不再执行任何 DDL。
// 改动只能以新增迁移版本号的方式表达，改这里的 version 1 语句会让已存在的库文件对不上号。
//
// PostgreSQL 分支（UpPostgres）与 SQLite 逐列对照，差异只有三处，均为方言必需：
//   - 自增列：SQLite `INTEGER PRIMARY KEY AUTOINCREMENT` → PG `BIGINT GENERATED
//     ALWAYS AS IDENTITY`（写入代码省略 seq，bun 的 autoincrement tag 两边都成立）；
//   - 整数宽度：SQLite INTEGER 即 64 位，PG 的 INTEGER 是 32 位——时间戳（微秒/纳秒）
//     与字节数列全部升为 BIGINT，避免 2038 与溢出问题；
//   - 其余列与索引两方言逐字一致（布尔仍是 INTEGER 0/1、JSON 仍是 TEXT）。

// migrationTableName 是观测库记录已应用迁移版本的表名。表名带 observe_ 前缀：
// 将来若把任务快照也换成 SQLite（设计文档 §4.3），两张迁移表不能撞名
// （设计文档 §13 风险 6）。建表语句由 engine.DB.Migrate 按这个名字生成。
const migrationTableName = "observe_schema_migrations"

// migrations 是内置的迁移列表，必须按 version 升序排列。
// 本卡只有 version 1：建三张表与八个索引。
var migrations = []engine.Migration{
	{
		Version: 1,
		Up: []string{
			`CREATE TABLE IF NOT EXISTS job_events (
  seq      INTEGER PRIMARY KEY AUTOINCREMENT,
  ts_us    INTEGER NOT NULL,               -- core.Event.Timestamp，Unix 微秒
  type     TEXT    NOT NULL,               -- job.scheduled|started|completed|failed|cancelled|retrying|paused|resumed
  job_id   TEXT    NOT NULL,
  job_name TEXT,
  status   INTEGER NOT NULL,               -- core.JobStatus 的 int，与 JobSnapshot.Status 同一口径
  data     TEXT,                           -- core.Event.Data 原文（JSON），可空
  metadata TEXT                            -- core.Event.Metadata 序列化为 JSON
);`,
			`CREATE INDEX IF NOT EXISTS idx_events_job ON job_events(job_id, seq);`,
			`CREATE INDEX IF NOT EXISTS idx_events_ts ON job_events(ts_us);`,
			`CREATE INDEX IF NOT EXISTS idx_events_type ON job_events(type, ts_us);`,

			`CREATE TABLE IF NOT EXISTS artifact_index (
  job_id    TEXT NOT NULL,
  attempt   INTEGER NOT NULL,
  kind      TEXT NOT NULL,                 -- script|binary|http
  profile   TEXT,                          -- 档位名，不含 exec. 前缀
  out_rel   TEXT, err_rel  TEXT,           -- 相对 executors.output.dir；不存绝对路径
  out_bytes INTEGER NOT NULL DEFAULT 0,
  err_bytes INTEGER NOT NULL DEFAULT 0,
  truncated INTEGER NOT NULL DEFAULT 0,
  state     TEXT NOT NULL,                 -- available|purged
  created_at INTEGER NOT NULL,
  PRIMARY KEY (job_id, attempt)
);`,
			`CREATE INDEX IF NOT EXISTS idx_artifact_state ON artifact_index(state, created_at);`,

			`CREATE TABLE IF NOT EXISTS write_audit (
  seq       INTEGER PRIMARY KEY AUTOINCREMENT,
  ts_us     INTEGER NOT NULL,
  actor     TEXT,                          -- 账号名；静态凭据记 "machine"；未认证记空
  actor_kind TEXT NOT NULL,                -- user|machine|anonymous
  role      TEXT,                          -- viewer|operator|admin|ops|machine
  action    TEXT NOT NULL,                 -- 封闭集：job.create|job.cancel|job.pause|…（见 §9.5 映射表）
  method    TEXT NOT NULL,                 -- POST|PUT|DELETE
  route     TEXT NOT NULL,                 -- gin 路由模板，如 /api/v1/jobs/:id/pause；不存原始 URL 与查询串
  status    INTEGER NOT NULL,              -- HTTP 状态码（来自 c.Writer.Status()）
  latency_us INTEGER NOT NULL,
  verdict   TEXT NOT NULL,                 -- ok|denied|bad_request|not_found|conflict|error（按 status 派生）
  exec_verdict TEXT,                       -- 仅执行器任务：accepted|role_denied|profile_unavailable|payload_rejected|timeout_rejected
  exec_reason_code TEXT,                   -- 封闭枚举；不存 error 原文
  handler_key TEXT,                        -- 仅执行器任务：注册键 exec.<name>
  profile   TEXT,                          -- 仅执行器任务：档位名
  job_id    TEXT,                          -- 有归属时填；批量请求填请求级 ID 留空
  remote_ip TEXT,
  user_agent TEXT
);`,
			`CREATE INDEX IF NOT EXISTS idx_audit_ts ON write_audit(ts_us);`,
			`CREATE INDEX IF NOT EXISTS idx_audit_actor ON write_audit(actor, ts_us);`,
			`CREATE INDEX IF NOT EXISTS idx_audit_action ON write_audit(action, ts_us);`,
			`CREATE INDEX IF NOT EXISTS idx_audit_verdict ON write_audit(verdict, ts_us);`,
		},
		UpPostgres: []string{
			`CREATE TABLE IF NOT EXISTS job_events (
  seq      BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  ts_us    BIGINT  NOT NULL,               -- core.Event.Timestamp，Unix 微秒
  type     TEXT    NOT NULL,               -- job.scheduled|started|completed|failed|cancelled|retrying|paused|resumed
  job_id   TEXT    NOT NULL,
  job_name TEXT,
  status   INTEGER NOT NULL,               -- core.JobStatus 的 int，与 JobSnapshot.Status 同一口径
  data     TEXT,                           -- core.Event.Data 原文（JSON），可空
  metadata TEXT                            -- core.Event.Metadata 序列化为 JSON
);`,
			`CREATE INDEX IF NOT EXISTS idx_events_job ON job_events(job_id, seq);`,
			`CREATE INDEX IF NOT EXISTS idx_events_ts ON job_events(ts_us);`,
			`CREATE INDEX IF NOT EXISTS idx_events_type ON job_events(type, ts_us);`,

			`CREATE TABLE IF NOT EXISTS artifact_index (
  job_id    TEXT NOT NULL,
  attempt   INTEGER NOT NULL,
  kind      TEXT NOT NULL,                 -- script|binary|http
  profile   TEXT,                          -- 档位名，不含 exec. 前缀
  out_rel   TEXT, err_rel  TEXT,           -- 相对 executors.output.dir；不存绝对路径
  out_bytes BIGINT NOT NULL DEFAULT 0,
  err_bytes BIGINT NOT NULL DEFAULT 0,
  truncated INTEGER NOT NULL DEFAULT 0,
  state     TEXT NOT NULL,                 -- available|purged
  created_at BIGINT NOT NULL,
  PRIMARY KEY (job_id, attempt)
);`,
			`CREATE INDEX IF NOT EXISTS idx_artifact_state ON artifact_index(state, created_at);`,

			`CREATE TABLE IF NOT EXISTS write_audit (
  seq       BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  ts_us     BIGINT  NOT NULL,
  actor     TEXT,                          -- 账号名；静态凭据记 "machine"；未认证记空
  actor_kind TEXT NOT NULL,                -- user|machine|anonymous
  role      TEXT,                          -- viewer|operator|admin|ops|machine
  action    TEXT NOT NULL,                 -- 封闭集：job.create|job.cancel|job.pause|…（见 §9.5 映射表）
  method    TEXT NOT NULL,                 -- POST|PUT|DELETE
  route     TEXT NOT NULL,                 -- gin 路由模板，如 /api/v1/jobs/:id/pause；不存原始 URL 与查询串
  status    INTEGER NOT NULL,              -- HTTP 状态码（来自 c.Writer.Status()）
  latency_us BIGINT NOT NULL,
  verdict   TEXT NOT NULL,                 -- ok|denied|bad_request|not_found|conflict|error（按 status 派生）
  exec_verdict TEXT,                       -- 仅执行器任务：accepted|role_denied|profile_unavailable|payload_rejected|timeout_rejected
  exec_reason_code TEXT,                   -- 封闭枚举；不存 error 原文
  handler_key TEXT,                        -- 仅执行器任务：注册键 exec.<name>
  profile   TEXT,                          -- 仅执行器任务：档位名
  job_id    TEXT,                          -- 有归属时填；批量请求填请求级 ID 留空
  remote_ip TEXT,
  user_agent TEXT
);`,
			`CREATE INDEX IF NOT EXISTS idx_audit_ts ON write_audit(ts_us);`,
			`CREATE INDEX IF NOT EXISTS idx_audit_actor ON write_audit(actor, ts_us);`,
			`CREATE INDEX IF NOT EXISTS idx_audit_action ON write_audit(action, ts_us);`,
			`CREATE INDEX IF NOT EXISTS idx_audit_verdict ON write_audit(verdict, ts_us);`,
		},
	},
	// Version 2（TASK-RE07）：outbox 去重列。中继器按 at-least-once 投递，崩溃重放
	// 会重复发布同一事件；outbox_seq 是任务库出箱行的自增序号，部分唯一索引让
	// EventLog 的插入按"冲突忽略"去重。旧行全 NULL：NULL 互不冲突，直发模式
	// （JSON 部署 + 观测库）行为不变。两方言的 DDL 与冲突子句逐字一致——
	// 部分唯一索引的冲突目标语法两边同形，不设方言分支点。
	{
		Version: 2,
		Up: []string{
			`ALTER TABLE job_events ADD COLUMN outbox_seq INTEGER;`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_events_outbox ON job_events(outbox_seq) WHERE outbox_seq IS NOT NULL;`,
		},
	},
}

// currentSchemaVersion 读已应用的最高版本号；迁移表刚建好时返回 0。
const currentSchemaVersion = `SELECT COALESCE(MAX(version), 0) FROM observe_schema_migrations`
