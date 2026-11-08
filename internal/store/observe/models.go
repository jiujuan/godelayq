package observe

import (
	"context"
	"database/sql"

	"github.com/uptrace/bun"
)

// bgCtx 供无 ctx 入参的接口方法内部使用：api/executor 侧的消费方接口签名不带
// ctx，这里不发明新的传播方式，超时与取消由调用方在更外层控制。
var bgCtx = context.Background()

// 以下三个 bun 模型的表名、列名是冻结契约（schema.go），tag 全部显式给出，
// 不依赖 bun 的命名推断。seq 由数据库自增列给出，代码不算：同一次执行会在同一
// 毫秒内连发多条记录，只有 seq 能定出因果（设计文档 §6）。tag 上的 autoincrement
// 让 INSERT 省略该列——SQLite 落到 AUTOINCREMENT，PG 落到 IDENTITY（B05）。

// eventRecord 是 job_events 的一行，字段与列一一对应。
// data 与 metadata 用 sql.NullString：两列都可空，空与空字符串在库里是两回事。
type eventRecord struct {
	bun.BaseModel `bun:"table:job_events"`

	Seq         int64          `bun:"column:seq,autoincrement"`
	TimestampUS int64          `bun:"column:ts_us"`
	Type        string         `bun:"column:type"`
	JobID       string         `bun:"column:job_id"`
	JobName     string         `bun:"column:job_name"`
	Status      int            `bun:"column:status"`
	Data        sql.NullString `bun:"column:data"`
	Metadata    sql.NullString `bun:"column:metadata"`
	// OutboxSeq 是出箱序号（TASK-RE07 迁移 v2）：中继器投递的事件带值，
	// 直发事件（JSON 部署）为 NULL。部分唯一索引 + 冲突忽略实现读侧去重。
	OutboxSeq sql.NullInt64 `bun:"column:outbox_seq"`
}

// auditRecord 是 write_audit 的一行。可空列用 sql.NullString：
// "没有执行器结论"与"执行器结论是空串"在库里必须是两回事，
// 否则普通任务提交看起来像一次被拒的执行器提交。
type auditRecord struct {
	bun.BaseModel `bun:"table:write_audit"`

	Seq         int64          `bun:"column:seq,autoincrement"`
	TimestampUS int64          `bun:"column:ts_us"`
	Actor       string         `bun:"column:actor"`
	ActorKind   string         `bun:"column:actor_kind"`
	Role        string         `bun:"column:role"`
	Action      string         `bun:"column:action"`
	Method      string         `bun:"column:method"`
	Route       string         `bun:"column:route"`
	Status      int            `bun:"column:status"`
	LatencyUS   int64          `bun:"column:latency_us"`
	Verdict     string         `bun:"column:verdict"`

	ExecVerdict sql.NullString `bun:"column:exec_verdict"`
	ExecReason  sql.NullString `bun:"column:exec_reason_code"`
	HandlerKey  sql.NullString `bun:"column:handler_key"`
	Profile     sql.NullString `bun:"column:profile"`
	JobID       sql.NullString `bun:"column:job_id"`
	RemoteIP    sql.NullString `bun:"column:remote_ip"`
	UserAgent   sql.NullString `bun:"column:user_agent"`
}

// artifactRow 是 artifact_index 的一行。(job_id, attempt) 是复合主键：
// 一个任务一次尝试一行，冲突即整行覆盖。
type artifactRow struct {
	bun.BaseModel `bun:"table:artifact_index"`

	JobID      string `bun:"column:job_id,pk"`
	Attempt    int    `bun:"column:attempt,pk"`
	Kind       string `bun:"column:kind"`
	Profile    string `bun:"column:profile"`
	OutRel     string `bun:"column:out_rel"`
	ErrRel     string `bun:"column:err_rel"`
	OutBytes   int64  `bun:"column:out_bytes"`
	ErrBytes   int64  `bun:"column:err_bytes"`
	Truncated  int    `bun:"column:truncated"`
	State      string `bun:"column:state"`
	CreatedSec int64  `bun:"column:created_at"`
}
