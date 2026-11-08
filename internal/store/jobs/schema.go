package jobs

import "github.com/jiujuan/godelayq/internal/store/engine"

// 表的 DDL 与设计约定一致（mgrdb-design.md §6）。表名、列名一经落地就是契约：
// 改动只能以新增迁移版本号的方式表达，改 version 1 的语句会让已存在的库文件对不上号。
//
// snapshot_json 整存为唯一权威；id/name/status/updated_at 四列是冗余，只服务于
// 运维排查与留痕淘汰的轻查询。列化到此为止——接口上出现按状态查询的需求之前，
// 不提前做更多列。

// migrationTableName 是任务库记录已应用迁移版本的表名。jobs_ 前缀与管理库的
// mgr_schema_migrations、观测库的 observe_schema_migrations 互不撞车：
// 同一台机器上三库共存，各记各的版本。
const migrationTableName = "jobs_schema_migrations"

// migrations 是内置的迁移列表，必须按 version 升序排列。
//
// PostgreSQL 分支（UpPostgres）与 SQLite 的差异只有两类，方言必需：
// 整数宽度——PG 的 INTEGER 是 32 位会溢出，时间戳/字节数/序号列升为 BIGINT
//（updated_at、attempt 的 started_at/finished_at/duration_ms/seq）；其余列两方言逐字一致。
var migrations = []engine.Migration{
	{
		Version: 1,
		Up: []string{
			`CREATE TABLE IF NOT EXISTS jobs (
  id            TEXT PRIMARY KEY,
  name          TEXT NOT NULL,             -- 冗余列：现场排障用，不参与接口查询
  status        TEXT NOT NULL,             -- core.JobStatus 规范名（封闭词表）
  updated_at    INTEGER NOT NULL,          -- 写入时的 UnixNano，留痕淘汰的排序键
  snapshot_json TEXT NOT NULL              -- core.JobSnapshot 整体序列化，权威数据
);`,
			`CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);`,
			`CREATE INDEX IF NOT EXISTS idx_jobs_updated_at ON jobs(updated_at);`,
			`CREATE TABLE IF NOT EXISTS jobs_meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);`,
		},
		UpPostgres: []string{
			`CREATE TABLE IF NOT EXISTS jobs (
  id            TEXT PRIMARY KEY,
  name          TEXT NOT NULL,             -- 冗余列：现场排障用，不参与接口查询
  status        TEXT NOT NULL,             -- core.JobStatus 规范名（封闭词表）
  updated_at    BIGINT NOT NULL,           -- 写入时的 UnixNano，留痕淘汰的排序键
  snapshot_json TEXT NOT NULL              -- core.JobSnapshot 整体序列化，权威数据
);`,
			`CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);`,
			`CREATE INDEX IF NOT EXISTS idx_jobs_updated_at ON jobs(updated_at);`,
			`CREATE TABLE IF NOT EXISTS jobs_meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);`,
		},
	},
	// Version 2（TASK-RE02）：逐次尝试账本。列名避开 PG 保留字（trigger→trigger_source、
	// error→error_text）。可空语义：finished_at NULL = 在途（崩溃遗留，Restore 收口为
	// abandoned）；retryable/exit_code/http_status NULL = 未判定（0 与"没有退出码可言"分得开）。
	{
		Version: 2,
		Up: []string{
			`CREATE TABLE IF NOT EXISTS job_attempts (
  job_id         TEXT NOT NULL,
  seq            INTEGER NOT NULL,          -- 尝试序号，1 起递增，= Job.Attempts 自增后的值
  status         TEXT NOT NULL,             -- core.AttemptStatus 规范名（封闭词表）
  trigger_source TEXT NOT NULL,             -- core.AttemptTrigger 规范名（scheduled|retry|restore|misfire）
  started_at     INTEGER NOT NULL,          -- UnixNano
  finished_at    INTEGER,                   -- NULL = 在途
  duration_ms    INTEGER NOT NULL DEFAULT 0,
  class          TEXT,                      -- 失败类别文本（TASK-RE04），成功为 NULL
  retryable      INTEGER,                   -- 0/1；NULL = 未判定
  exit_code      INTEGER,
  signal         TEXT,
  http_status    INTEGER,
  error_text     TEXT,                      -- 脱敏后的错误文本（E08 口径）
  PRIMARY KEY (job_id, seq)
);`,
			`CREATE INDEX IF NOT EXISTS idx_attempts_job_started ON job_attempts(job_id, started_at);`,
			`CREATE INDEX IF NOT EXISTS idx_attempts_status ON job_attempts(status);`,
		},
		UpPostgres: []string{
			`CREATE TABLE IF NOT EXISTS job_attempts (
  job_id         TEXT NOT NULL,
  seq            BIGINT NOT NULL,           -- 尝试序号，1 起递增，= Job.Attempts 自增后的值
  status         TEXT NOT NULL,             -- core.AttemptStatus 规范名（封闭词表）
  trigger_source TEXT NOT NULL,             -- core.AttemptTrigger 规范名（scheduled|retry|restore|misfire）
  started_at     BIGINT NOT NULL,           -- UnixNano
  finished_at    BIGINT,                    -- NULL = 在途
  duration_ms    BIGINT NOT NULL DEFAULT 0,
  class          TEXT,                      -- 失败类别文本（TASK-RE04），成功为 NULL
  retryable      INTEGER,                   -- 0/1；NULL = 未判定
  exit_code      INTEGER,
  signal         TEXT,
  http_status    INTEGER,
  error_text     TEXT,                      -- 脱敏后的错误文本（E08 口径）
  PRIMARY KEY (job_id, seq)
);`,
			`CREATE INDEX IF NOT EXISTS idx_attempts_job_started ON job_attempts(job_id, started_at);`,
			`CREATE INDEX IF NOT EXISTS idx_attempts_status ON job_attempts(status);`,
		},
	},
	// Version 3（TASK-RE03）：幂等提交登记表。key 是 Idempotency-Key（缺省时
	// 客户端 job_id 兼作键）；fingerprint 是请求内容的 SHA256（口径见 api 层），
	// 键命中即比对它区分"重放"与"冲突"。TTL 淘汰由 api 层按 store.idempotency_ttl 驱动。
	{
		Version: 3,
		Up: []string{
			`CREATE TABLE IF NOT EXISTS job_idempotency (
  key         TEXT PRIMARY KEY,
  job_id      TEXT NOT NULL,
  fingerprint TEXT NOT NULL,
  created_at  INTEGER NOT NULL           -- UnixNano，TTL 淘汰的排序键
);`,
			`CREATE INDEX IF NOT EXISTS idx_idem_job ON job_idempotency(job_id);`,
			`CREATE INDEX IF NOT EXISTS idx_idem_created ON job_idempotency(created_at);`,
		},
		UpPostgres: []string{
			`CREATE TABLE IF NOT EXISTS job_idempotency (
  key         TEXT PRIMARY KEY,
  job_id      TEXT NOT NULL,
  fingerprint TEXT NOT NULL,
  created_at  BIGINT NOT NULL            -- UnixNano，TTL 淘汰的排序键
);`,
			`CREATE INDEX IF NOT EXISTS idx_idem_job ON job_idempotency(job_id);`,
			`CREATE INDEX IF NOT EXISTS idx_idem_created ON job_idempotency(created_at);`,
		},
	},
	// Version 4（TASK-RE07）：事务性事件出口。快照/账本状态变更与出箱行同事务落库，
	// OutboxRelay 异步投递给事件总线（at-least-once，读侧按 outbox_seq 去重）。
	// payload_json 是 core.Event 的序列化原文；OutboxSeq 由自增列在投递时回填进事件。
	{
		Version: 4,
		Up: []string{
			`CREATE TABLE IF NOT EXISTS job_outbox (
  seq           INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id        TEXT NOT NULL,
  type          TEXT NOT NULL,             -- core.EventType 原文
  payload_json  TEXT NOT NULL,             -- core.Event 序列化原文
  status        TEXT NOT NULL,             -- pending | dispatched
  created_at    INTEGER NOT NULL,          -- UnixNano
  dispatched_at INTEGER
);`,
			`CREATE INDEX IF NOT EXISTS idx_outbox_pending ON job_outbox(status, seq);`,
			`CREATE INDEX IF NOT EXISTS idx_outbox_job ON job_outbox(job_id, seq);`,
		},
		UpPostgres: []string{
			`CREATE TABLE IF NOT EXISTS job_outbox (
  seq           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  job_id        TEXT NOT NULL,
  type          TEXT NOT NULL,             -- core.EventType 原文
  payload_json  TEXT NOT NULL,             -- core.Event 序列化原文
  status        TEXT NOT NULL,             -- pending | dispatched
  created_at    BIGINT NOT NULL,           -- UnixNano
  dispatched_at BIGINT
);`,
			`CREATE INDEX IF NOT EXISTS idx_outbox_pending ON job_outbox(status, seq);`,
			`CREATE INDEX IF NOT EXISTS idx_outbox_job ON job_outbox(job_id, seq);`,
		},
	},
}

// metaJobsImported 是一次性导入的标记：写入即生效，之后 store.path 指向的
// JSON 文件不再被读取。值是导入完成时刻的 Unix 秒的十进制文本，
// 与 mgr_meta 的三个标记同一用法，只为现场排障看"导没导过、什么时候导的"。
const metaJobsImported = "jobs_imported_from_file"
