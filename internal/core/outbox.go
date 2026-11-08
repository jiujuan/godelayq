// outbox.go 定义事务性事件出口的能力接口（TASK-RE07，设计文档 §7.7）。
//
// 没有它时的世界：状态写入（store.Update）与事件发布（EventBus.Publish）是两个
// 独立动作，状态落了事件可能丢。有它时：persistAndEmit 把快照与事件交同一个
// 事务落库（job_outbox 表），中继器异步投递给 EventBus——订阅端（WS/SSE/
// observe.EventLog）完全不变，事件变成至少送达一次。
//
// 接口定义在 core（消费方是调度器），实现方 store/jobs 隐式实现；JSON 文件
// 存储不实现——调度器持有可 nil 引用，nil 即退回直发（现状），能力边界写进
// deployment 文档。RE01 把全部写点收口到 persistAndEmit/emit 正是为此。
package core

// EventOutbox 是事务性事件出口的能力接口。
//
// 实现方的两条纪律：
//   - UpdateWithEvents 必须在单个事务里落快照与出箱行（这是本接口存在的意义）；
//   - 事件经中继器投递，实现方与调用方都不得在写入成功后再自行 Publish
//    （双发禁令——调用侧的分派在 core/state.go，实现侧只写不发）。
type EventOutbox interface {
	// UpdateWithEvents 在同一事务里落任务快照与出箱事件。events 可为空
	//（纯状态写）；调用后调用方不得再 Publish 这些事件。
	UpdateWithEvents(snapshot JobSnapshot, events []Event) error
	// UpdateWithEventsAndAttempt 在同一事务里落任务快照、出箱事件与账本开始行
	//（W1/Q08）：running 转移的"落盘 + 记账"从此一次事务，三者同生共死——
	// 任何一步失败整体回滚，不留"状态落了、账本没开行"的半截现场。
	// attempt 的语义与 AttemptLedger.BeginAttempt 相同（running 行，冲突时按
	// (job_id, seq) 覆盖）。
	UpdateWithEventsAndAttempt(snapshot JobSnapshot, events []Event, attempt Attempt) error
	// AppendOutbox 只入箱不写快照（emit 通道用：事件先于/伴随状态变化但
	// 没有自己的快照写）。独立事务——事件与状态同事务的原子性只在
	// UpdateWithEvents 承诺，这里只承担"不丢"。
	AppendOutbox(events []Event) error
}

// WithEventOutbox 给调度器注入出箱能力（TASK-RE07）。实现方是任务库；
// JSON 部署不注入，persistAndEmit/emit 退回直发。
func WithEventOutbox(o EventOutbox) Option {
	return func(opts *componentOptions) { opts.outbox = o }
}
