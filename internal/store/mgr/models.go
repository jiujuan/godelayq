package mgr

import (
	"context"

	"github.com/jiujuan/godelayq/internal/store/engine"
	"github.com/uptrace/bun"
)

// bgCtx 供无 ctx 入参的接口方法内部使用：core 的存储接口签名不带 ctx，
// 这里不发明新的传播方式，超时与取消由调用方（HTTP 层）在更外层控制。
var bgCtx = context.Background()

// 以下四个 bun 模型的表名、列名是冻结契约（schema.go），tag 全部显式给出，
// 不依赖 bun 的命名推断。时间戳一律 Unix 秒，与列注释一致。

// userRow 是 mgr_users 表的 bun 模型。
type userRow struct {
	bun.BaseModel `bun:"table:mgr_users"`

	Name           string `bun:"column:name,pk"`
	PasswordBcrypt string `bun:"column:password_bcrypt"`
	Role           string `bun:"column:role"`
	CreatedAt      int64  `bun:"column:created_at"`
	UpdatedAt      int64  `bun:"column:updated_at"`
}

// groupRow 是 mgr_groups 表的 bun 模型。
type groupRow struct {
	bun.BaseModel `bun:"table:mgr_groups"`

	Name        string `bun:"column:name,pk"`
	Description string `bun:"column:description"`
	Color       string `bun:"column:color"`
	CreatedAt   int64  `bun:"column:created_at"`
	UpdatedAt   int64  `bun:"column:updated_at"`
}

// profileRow 是 mgr_executor_profiles 表的 bun 模型：记录整体序列化进 record_json。
type profileRow struct {
	bun.BaseModel `bun:"table:mgr_executor_profiles"`

	Name       string `bun:"column:name,pk"`
	RecordJSON string `bun:"column:record_json"`
	UpdatedAt  int64  `bun:"column:updated_at"`
}

// metaRow 是 mgr_meta 表的 bun 模型：一次性导入标记的键值对。
type metaRow struct {
	bun.BaseModel `bun:"table:mgr_meta"`

	Key   string `bun:"column:key,pk"`
	Value string `bun:"column:value"`
}

// nameConflictClause 返回名表 upsert 的 ON CONFLICT 子句。
//
// 大小写不敏感唯一在两个方言上的表达不同：SQLite 靠 COLLATE NOCASE 主键拦截
// 大小写变体，冲突目标写 (name)；PostgreSQL（B05 起）没有该 collation，
// 用 lower(name) 表达式唯一索引，冲突目标也必须是同一表达式才能命中。
// 业务语义（"大小写变体视作同一实体"）不随方言变，这里收口成一处。
func nameConflictClause(d engine.Dialect) string {
	if d == engine.DialectPostgres {
		return "CONFLICT (lower(name)) DO UPDATE"
	}
	return "CONFLICT (name) DO UPDATE"
}
