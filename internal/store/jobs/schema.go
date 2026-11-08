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

// migrations 是内置的迁移列表，必须按 version 升序排列。目前只有 version 1。
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
	},
}

// metaJobsImported 是一次性导入的标记：写入即生效，之后 store.path 指向的
// JSON 文件不再被读取。值是导入完成时刻的 Unix 秒的十进制文本，
// 与 mgr_meta 的三个标记同一用法，只为现场排障看"导没导过、什么时候导的"。
const metaJobsImported = "jobs_imported_from_file"
