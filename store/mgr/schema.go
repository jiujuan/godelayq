package mgr

import "godelayq/store/sqlite"

// 表的 DDL 与设计约定一致。表名、列名一经落地就是契约：改动只能以新增迁移
// 版本号的方式表达，改 version 1 的语句会让已存在的库文件对不上号。
// 唯一性一律 COLLATE NOCASE：与 JSON 存储及配置期查重"大小写不敏感"的口径一致，
// 登录名、分组名、档位名在大小写变体上视作同一个实体。

// migrationTableName 是管理库记录已应用迁移版本的表名。mgr_ 前缀与观测库的
// observe_schema_migrations 互不撞车（同一台机器上两库共存，各记各的版本）。
const migrationTableName = "mgr_schema_migrations"

// migrations 是内置的迁移列表，必须按 version 升序排列。目前只有 version 1。
var migrations = []sqlite.Migration{
	{
		Version: 1,
		Up: []string{
			`CREATE TABLE IF NOT EXISTS mgr_users (
  name            TEXT PRIMARY KEY COLLATE NOCASE,
  password_bcrypt TEXT NOT NULL,
  role            TEXT NOT NULL CHECK (role IN ('viewer','operator','admin','ops')),
  created_at      INTEGER NOT NULL,           -- 写入时的 Unix 秒
  updated_at      INTEGER NOT NULL
);`,
			`CREATE TABLE IF NOT EXISTS mgr_groups (
  name        TEXT PRIMARY KEY COLLATE NOCASE,
  description TEXT NOT NULL DEFAULT '',
  color       TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL
);`,
			`CREATE TABLE IF NOT EXISTS mgr_executor_profiles (
  name        TEXT PRIMARY KEY COLLATE NOCASE,
  record_json TEXT NOT NULL,                  -- core.ExecutorProfileRecord 整体序列化：
                                              -- 字段随 ExecutorCommand 演进，整存最稳，
                                              -- 与 JSON 文件格式同构，迁移零转换
  updated_at  INTEGER NOT NULL
);`,
			`CREATE TABLE IF NOT EXISTS mgr_meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);`,
		},
	},
}

// mgr_meta 的键。一次性导入的三个标记：写入即生效，之后对应的来源不再被读取。
// 值是导入完成时刻的 Unix 秒的十进制文本，只为现场排障看"导没导过、什么时候导的"。
const (
	metaUsersImported    = "users_imported_from_config"
	metaGroupsImported   = "groups_imported_from_file"
	metaProfilesImported = "profiles_imported_from_file"
)
