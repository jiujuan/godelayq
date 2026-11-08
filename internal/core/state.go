// state.go 是任务状态机的唯一规则来源（TASK-RE01）：
// 转移表与校验（jobTransitions/canTransition），以及调度器的三个收口通道——
// transition（校验 + 赋值 + 落盘 + 发事件）、persistAndEmit（状态写 + 事件）、
// emit（不带状态写的事件）。所有状态变化必须经 transition，所有"状态写 + 事件"
// 必须经 persistAndEmit，绕行的裸 store.Update/Publish 在包内不允许存在。
//
// RE07 会把 outbox 能力接进 persistAndEmit 与 emit（库模式下同事务写入出箱表、
// 事件改由中继器投递），调用方不变——这就是先把写点收口到本文件的原因。
package core

import "time"

// jobTransitions 是合法的状态转移表：键为出发态，值为允许到达的态的封闭清单。
// 不在表里的转移一律非法；from == to 恒真（原地内容更新不算转移，如 UpdatePending/SetGroup）。
// 每条出边的现实来源见 docs/design/reliable-execution-design.md §6.5——改表先改文档，
// 两处说的是同一张表。
//
// 两条设计取向要写明，免得后人"顺手补全"：
//   - 终态（success/failed/cancelled）没有出边。重跑一条终态任务 = 按原内容重新 Schedule
//     出一条新排期，不是"终态复活"。
//   - 表刻意没有 pending→pending 之类的恒等条目：恒等由 canTransition 的 from==to 分支表达，
//     表里只放真正的状态变化，读表的人一眼看清"哪些变化是允许的"。
var jobTransitions = map[JobStatus][]JobStatus{
	StatusPending:   {StatusRunning, StatusPaused, StatusCancelled, StatusFailed},
	StatusRunning:   {StatusSuccess, StatusFailed, StatusPending, StatusPaused, StatusCancelled},
	StatusPaused:    {StatusPending, StatusCancelled},
	StatusSuccess:   {},
	StatusFailed:    {},
	StatusCancelled: {},
}

// canTransition 判断一次状态转移是否合法。
//
// pending→failed 只有惟一来源：派发期失败（注册表里没有 handler，任务从未开始执行
// 就注定终态，见 executeJob 的无 handler 分支）。它不进"重试"语义——没有执行过，
// 重试也无从谈起，而保持 pending 只会让一条坏任务永远占着堆。
func canTransition(from, to JobStatus) bool {
	if from == to {
		return true
	}
	for _, allowed := range jobTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// transition 校验并执行一次状态转移，随后经 persistAndEmit 落盘与发事件。
//
// from 取调用时刻 job.Status 的现值——调用方必须在本方法之前不改状态，
// 转移的"原状态"由本方法读取，这是校验有意义的前提。
//
// 非法转移：放弃状态写、保留原状态，事件照发。理由：事件描述的是收尾路径得出的结论
// （比如 handler 明确失败了），丢事件等于丢事实；而把非法状态写进存储等于账本说谎——
// 两害相权，宁可账本少一行。非法转移本身按构造不该发生（每条调用点都有测试钉住），
// 一旦出现必然是代码改动破坏了状态机，error 级日志是给人的警报。
//
// 返回 false 表示转移被拒绝，调用方照常返回（本方法已处理日志与事件）。
func (s *Scheduler) transition(job *Job, to JobStatus, events ...Event) bool {
	return s.transitionWithAttempt(job, to, nil, events...)
}

// transitionWithAttempt 是 transition 的账本扩展（W1/Q08）：状态转移与账本开始行
// 同事务落库（出箱模式）。executeJob 的 running 落盘与 beginAttempt 原本是两次
// 独立事务，合并后快照、出箱行、账本行同生共死。
//
// attempt 为 nil 时与本变体存在之前逐字节一致（persistAndEmitWithAttempt 内部分派）：
// JSON 部署没有账本，直发模式的行为不得因合并通道的存在而改变。
// 校验语义与 transition 共用（见上）：from 由本方法读取，非法转移放弃状态写、
// 事件照发。
func (s *Scheduler) transitionWithAttempt(job *Job, to JobStatus, attempt *Attempt, events ...Event) bool {
	from := job.Status
	if !canTransition(from, to) {
		s.logger.Error("illegal job status transition rejected; keeping current status",
			"job_id", job.ID, "from", from.String(), "to", to.String())
		for i := range events {
			s.emit(events[i])
		}
		return false
	}
	job.Status = to
	job.UpdatedAt = time.Now()
	s.persistAndEmitWithAttempt(job, attempt, events...)
	return true
}

// persistAndEmit 把"任务快照写 + 事件"收口到惟一通道。
//
// 两种模式按 outbox 能力分派（TASK-RE07）：
//   - 出箱可用（任务库部署）：UpdateWithEvents 一次事务落快照与出箱行，
//     事件改由中继器投递给 EventBus——本方法不再 Publish（双发禁令）。
//     WS/SSE 的事件可见延迟上界因此变为一个中继周期（store.outbox_flush_interval）。
//   - 无出箱（JSON 部署）：store.Update 后逐个 Publish，与既有行为逐字节等价。
//
// 落盘失败只记日志并返回错误：与既有容错口径一致——落盘失败不回滚内存状态，
// 重启靠 Restore 对齐；错误返回给需要传播的调用方（如 applyGroupToSnapshot 的 API 语义）。
// 直发模式下事件无论落盘成败都照发：订阅端看到的是"发生了什么"；
// 出箱模式下事件的投递由中继器接管，落盘成败决定出箱行是否存在。
//
// 本方法不做转移校验（那是 transition 的事）：调用它的都是"状态语义上已定"的场景
// ——首次排期（pending）与原地内容更新（identity）。
//
// 与 Schedule 的时序契约：持久化必须在入堆之前（任务一进堆就可能被 worker 取走并就地
// 改写 Status/Attempts/UpdatedAt，快照是这些字段的读取方），事件随落盘一起先于入堆发出——
// 订阅端看到的 scheduled 事件因此严格早于 started。
func (s *Scheduler) persistAndEmit(job *Job, events ...Event) error {
	if s.outbox != nil {
		if err := s.outbox.UpdateWithEvents(job.ToSnapshot(), events); err != nil {
			s.logger.Error("failed to persist job snapshot with outbox events",
				"job_id", job.ID, "status", job.Status.String(), "error", err)
			return err
		}
		return nil
	}

	var persistErr error
	if s.store != nil {
		persistErr = s.store.Update(job.ToSnapshot())
		if persistErr != nil {
			s.logger.Error("failed to persist job snapshot",
				"job_id", job.ID, "status", job.Status.String(), "error", persistErr)
		}
	}
	for i := range events {
		s.emit(events[i])
	}
	return persistErr
}

// persistAndEmitWithAttempt 是 persistAndEmit 的账本扩展（W1/Q08），三种能力组合：
//   - 出箱 + 账本：一次事务落快照、出箱行、账本行（UpdateWithEventsAndAttempt），
//     任何一步失败三者一起回滚——本变体存在的目的；
//   - 出箱、无账本（attempt nil）：交回 persistAndEmit，与既有通道一字不差；
//   - 无出箱（JSON 部署，账本必为 nil；第三方实现可能只给账本不给出箱——防御性
//     保序）：快照与事件走既有通道，账本行独立补写，顺序与改动前的
//     transition→beginAttempt 一致。
func (s *Scheduler) persistAndEmitWithAttempt(job *Job, attempt *Attempt, events ...Event) error {
	if attempt == nil {
		return s.persistAndEmit(job, events...)
	}
	if s.outbox == nil {
		persistErr := s.persistAndEmit(job, events...)
		s.writeBeginAttempt(*attempt)
		return persistErr
	}
	if err := s.outbox.UpdateWithEventsAndAttempt(job.ToSnapshot(), events, *attempt); err != nil {
		s.logger.Error("failed to persist job snapshot with outbox events and attempt",
			"job_id", job.ID, "status", job.Status.String(), "error", err)
		return err
	}
	return nil
}

// emit 是"不带状态写的事件"的惟一出口：不伴随快照变化的事件（job.resumed、
// job.retrying、job.started/completed/failed 等）从这里走。
// 出箱可用时事件先进出箱表（独立事务——AppendOutbox 只承担"不丢"，与状态
// 同事务的原子性由 UpdateWithEvents 承担），由中继器投递；否则直接 Publish。
func (s *Scheduler) emit(event Event) {
	if s.outbox != nil {
		if err := s.outbox.AppendOutbox([]Event{event}); err != nil {
			// 出箱失败：这条事件不会自愈（中继器看不到它），error 级留痕。
			// 状态一致性不受影响——出箱承载事件的"不丢"，不承载状态。
			s.logger.Error("failed to append outbox event",
				"job_id", event.JobID, "type", string(event.Type), "error", err)
			return
		}
		return
	}
	s.eventBus.Publish(event)
}
