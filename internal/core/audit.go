package core

import "time"

// AuditEntry 是一行写操作台账，字段与 store/observe 的 write_audit 列一一对应。
// 类型放在 core 是共享词汇的惯例（与 Event、JobSnapshot 同层）：
// api 的中间件负责构造它，store/observe 的台账负责落盘与查询，
// 两边都只认这个形状，互相不 import。
//
// 这张表从构造上就没有请求体：结构里没有 payload 字段，也没有能装下它的位置。
// 后来人若想"顺手加一个 body 列"，请先回到设计文档 §10 第 1 条——参数取值曾经出现在
// 校验错误的文本里，而执行侧的既有规范是"档位名可以外泄，参数值不可以"。
type AuditEntry struct {
	Time      time.Time
	Actor     string
	ActorKind string
	Role      string
	Action    string
	Method    string
	Route     string
	Status    int
	Latency   time.Duration
	Verdict   string

	// 执行器补充列：只有走过提交期判定的请求才有值，普通任务提交一律为空。
	ExecVerdict    string
	ExecReasonCode string
	HandlerKey     string
	Profile        string
	JobID          string
	RemoteIP       string
	UserAgent      string
}

// AuditFilter 是台账查询的条件。零值字段表示不按该项过滤。
// Since/Until 是闭区间还是开区间由实现决定，见 store/observe 一侧的 Query 注释。
type AuditFilter struct {
	Actor   string
	Action  string
	Verdict string
	Since   *time.Time
	Until   *time.Time
	Limit   int
	Offset  int
}
