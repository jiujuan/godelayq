// scheduler_runtime.go 是调度器的引擎侧：启动与停止（Start/Stop）、
// worker 池与运行期扩缩（ResizeWorkers）、调度主循环与投递
// （scheduleLoop/dispatch/dispatchable）、挂起开关（Suspend/Unsuspend）、
// 以及崩溃恢复（Restore 与 RestoreGuard 改判钩子）。
package core

import (
	"context"
	"fmt"
	"time"
)

// Suspend 挂起调度：不再弹出任何到期任务，用于发布或维护窗口。
//
// 与 Stop 是两件事，不要混用：Stop 是优雅关停（worker 退出、被打断的任务以 pending
// 落盘等下次 Restore），Suspend 只让调度循环停止取任务——已在执行的任务照常跑完，
// 堆与存储都不改动。挂起期间 Schedule 仍然可用（任务照进堆，只是暂不触发），
// 恢复后一并生效。
//
// 状态是进程内的，重启后自动解除（见 Start）。
func (s *Scheduler) Suspend() {
	if s.suspended.CompareAndSwap(false, true) {
		s.logger.Warn("scheduling suspended; due jobs will not be dispatched")
	}
}

// Unsuspend 恢复调度，并唤醒正在等待的调度循环。
func (s *Scheduler) Unsuspend() {
	if s.suspended.CompareAndSwap(true, false) {
		s.logger.Info("scheduling resumed")
	}
	// 无论之前是否挂起都发一次唤醒：循环可能正卡在其它等待分支上，
	// 多一次空转只是重算堆顶等待时长。
	select {
	case s.resumeCh <- struct{}{}:
	default:
	}
	s.notifyNewJob()
}

// RestoreGuard 在 Restore 逐个处理快照时被调用，用来改判"崩溃瞬间的状态"。
//
// hold 为 true 表示这条任务不进堆，按 newStatus 落盘并留在存储里等人处理；
// 为 false 时 Restore 的行为与本钩子存在之前一字不差（复位为 pending 并重新排期）。
//
// 现在的用法只有一种（TASK-E14）：崩溃时状态仍是 running 的执行器任务停在 paused 上。
// 这类快照的含义是"进程已经起来过、结果未知"——被强杀的进程来不及写任何结论，
// 自动再跑一遍等于替用户决定"重复执行的后果可以接受"。
//
// 注意与优雅关闭的区别，别看名字猜：正常停服走的是 handleInterrupted，
// 它把被打断的任务落成 pending，因此下一次 Start 会照常重跑。
// 只有崩溃/强杀才会留下 running 快照，才会被这里停住。
type RestoreGuard func(snap JobSnapshot) (newStatus JobStatus, hold bool)

// RestoreReasonAfterCrash 是钩子改判时写进暂停事件的来源标记。
// 控制台的时间线与暂停提示按它区分"崩溃后停住"与"人手停住"，
// 少了这个来源，运维看到一批 paused 只会以为是系统故障。
const RestoreReasonAfterCrash = "restore_after_crash"

// SetRestoreGuard 安装崩溃恢复改判钩子，需在 Start 之前调用（Start 会立刻跑一次 Restore）。
// Start 之后调用不生效并记录日志——这时改判时机已经过去了。
// 传入 nil 表示撤掉钩子，恢复默认行为。
func (s *Scheduler) SetRestoreGuard(g RestoreGuard) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		s.logger.Warn("SetRestoreGuard ignored: scheduler already running")
		return
	}
	s.restoreGuard = g
}

// restoreGuardFn 在锁里取一次钩子，返回之后再调用它：
// 钩子自己可能回头查注册表（HandlerClass 要拿 s.mu），持锁调用会撞上自己。
func (s *Scheduler) restoreGuardFn() RestoreGuard {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.restoreGuard
}

// Restore 从持久化存储重建调度队列（崩溃/重启恢复）。
// 快照中仅 Pending/Running 状态的任务会被重新入队，状态重置为 Pending；
// 终态留痕与 paused 任务都不入队——暂停是人为决定，重启不该替用户取消它。
// Handler 在执行前按 HandlerKey 从注册表绑定。
//
// 装了 RestoreGuard 时，钩子可以拦住其中一部分任务不入队（见 RestoreGuard 与 TASK-E14）。
// 复位为 pending 这条默认规则只对"崩溃时还没开始执行"与"优雅关闭被打断"成立；
// 正常停服的重跑路径不经过这里的状态改判。
func (s *Scheduler) Restore() error {
	if s.store == nil {
		return nil
	}
	snapshots, err := s.store.LoadAll()
	if err != nil {
		return err
	}

	restored := 0
	held := 0
	for _, snap := range snapshots {
		// 存储现在同时保存终态留痕，恢复时只关心未完成的任务
		status := JobStatus(snap.Status)
		if status.IsTerminal() {
			continue
		}
		// paused 不是终态，但也不能被复活：重启不解除暂停，
		// 否则"暂停"在最常见的一次部署重启之后就悄悄失效了。
		if status == StatusPaused {
			continue
		}
		if s.heap.Get(snap.ID) != nil {
			continue
		}
		job := &Job{}
		job.FromSnapshot(snap)
		// 上次崩溃时处于 Running 的任务在此复位为待执行
		job.Status = StatusPending
		// 类别不在快照里，按注册表重新盖章：Start 之前处理函数已经注册完，
		// 恢复出来的执行器任务因此照样落在自己的池里。
		job.class = s.classOfKey(job.HandlerKey())

		// 改判发生在入堆之前：一旦进了堆就可能被 worker 取走，那时再停它要走取消。
		// LoadAll 给的是新切片，钩子期间没有持任何锁，因此它可以回头查注册表。
		if newStatus, hold := s.askRestoreGuard(snap); hold {
			s.holdRestored(job, newStatus)
			held++
			continue
		}

		// TriggerAt 已过期的任务直接入队，由调度循环立即补跑
		s.heap.PushItem(job)
		restored++
	}
	if restored > 0 {
		s.logger.Info("restored jobs from store", "count", restored)
	}
	if held > 0 {
		// 数量在这里给一次汇总：逐条的 job paused 日志在几十条崩溃现场里读不出规模，
		// 而 /stats 的 paused 计数混着人为暂停，看不出这一次重启新停了多少。
		s.logger.Info("paused executor jobs after crash", "count", held,
			"reason", RestoreReasonAfterCrash)
	}
	return nil
}

// askRestoreGuard 调用装配方装的改判钩子。
//
// 钩子 panic 时按"不改判"处理并记 error：Restore 正在逐个处理快照，
// 让一条钩子把整轮恢复打断，会让排在后面的任务既不重排也不改判——
// 那比多跑一次更难解释。
func (s *Scheduler) askRestoreGuard(snap JobSnapshot) (status JobStatus, hold bool) {
	guard := s.restoreGuardFn()
	if guard == nil {
		return 0, false
	}

	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("restore guard panicked, job keeps the default restore result",
				"job_id", snap.ID, "panic", r)
			status, hold = 0, false
		}
	}()

	return guard(snap)
}

// holdRestored 把改判后的任务落盘并发布事件，它不进堆。
//
// 落盘失败的后果是"下次启动再判一次"（存储里还是 running 快照），可接受，
// 但必须留下日志：这条任务本进程已经不执行了，存储与内存此刻不一致。
// 事件按 newStatus 分派：改判成 paused 才发 job.paused，
// 改判成别的状态时发暂停事件就是在说谎，那种用法将来要另配事件类型。
func (s *Scheduler) holdRestored(job *Job, newStatus JobStatus) {
	job.Status = newStatus
	job.UpdatedAt = time.Now()

	if s.store != nil {
		if err := s.store.Update(job.ToSnapshot()); err != nil {
			s.logger.Error("failed to persist the restored job status",
				"job_id", job.ID, "status", newStatus, "error", err)
		}
	}

	if newStatus != StatusPaused {
		s.logger.Warn("restored job held out of the queue without a matching event",
			"job_id", job.ID, "status", newStatus)
		return
	}

	s.eventBus.Publish(Event{
		Type:      EventJobPaused,
		JobID:     job.ID,
		JobName:   job.Name,
		Status:    StatusPaused,
		Timestamp: time.Now(),
		Metadata: map[string]interface{}{
			"reason":     RestoreReasonAfterCrash,
			"forced":     true,
			"trigger_at": job.TriggerAt,
			"attempts":   job.Attempts,
		},
	})
}

// ResizeWorkers 在运行期把普通池的 worker 数调整到 n（n>=1）。
//
// 与 SetConcurrency 的分工：SetConcurrency 只在 Start 之前有效、运行期 warn 并忽略
// （既有口径原样保留），ResizeWorkers 专给配置重载链用、运行期生效
// （docs/design/config-reload-design.md §6.1 把 scheduler.workers 钉在热更档）。
// 两者都改 targetWorkers，但只有本方法会真的起协程。
//
// 四条语义：
//   - 扩容：立刻为差额起 goroutine，新协程与启动期起的完全同构（同一个 worker 方法、
//     同一个 s.wg），因此 Stop 会等它们，一个都不能少算。
//   - 缩容：只写下目标值并记退场名额，不强杀、不取消在途任务；多出来的 worker
//     跑完手上那条任务、回到循环开头时退场。若之后一直没有任务流动，
//     读数（RuntimeStats.Workers）已经变小而实跑协程可能还多几个——这是温和缩容的代价，
//     不是缺陷，使用者文档（R07）里也要写清。
//   - 队列容量不变：通道换不得。默认池的投递方阻塞在 s.workCh <- job 上（见 dispatch），
//     换通道会让正在阻塞的那次发送永远等不到结果——发送的对象还挂在旧通道上。
//     worker 数与通道容量本来就是解耦的（容量只决定排队多少个，并发由协程数决定），
//     所以扩 worker 不碰通道就能生效。
//   - n <= 0 直接报错且不留任何痕迹，不回退到 DefaultConcurrency：重载链传进 0 说明
//     配置写错，静默补默认会把这条错配置跑下去。这条与 SetConcurrency 的
//     "非正数回退默认"方向相反是有意的——SetConcurrency 服务的是装配期
//     （那里 0 的常见含义是"这一节没写"，Normalized 已经补过一遍），
//     本方法服务的是运行期改配置（那里 0 只可能是笔误）。
//
// 调度器未启动时调用等价于 SetConcurrency(n)：只写下取值（含 concurrency，
// 下一次 Start 要按它建通道与协程），不起协程，返回 nil。
//
// 并发安全（与 Stop 的交错）：本方法全程在 s.mu 内判定 s.running 并记账，
// 而 Stop 在同一把锁内先置 running=false 再 close(stopCh)。于是只有两种交错：
//   - 本方法先拿到锁：它起了新协程并计入 wg；这些协程随后就会退出（select 的两个分支是随机的，
//     所以也可能先领到一条队列里等着的任务再退），Stop 的 wg.Wait() 仍然收敛。
//   - Stop 先释放锁：本方法看到 running==false，一个协程都不起。
//
// 不会出现"wg.Add 落在 wg.Wait 已经返回之后"那种违反：计数器归零必然发生在
// Stop 释放锁之后，而那时 running 已经是 false。wg.Add 在锁内、go 语句在锁外，
// 是为了不让持锁期间执行任何可能阻塞的动作；两者相隔一次解锁，
// 中间新协程即便先跑起来也只会在 select 上等通道，不影响这里的记账。
//
// 一个已知边界：上面两种交错的前提是"这一代的生命周期里没有第二次 Start"。
// 若 ResizeWorkers 放锁之后、go 语句执行之前，另一条协程做完了 Stop→Start，这批新协程领到的
// 还是上一代那条 workCh（队列是当参数传进 worker 的，卡片 §3.2 定死了签名），
// 而它们 select 的 s.stopCh 已经是新一代那个还开着的通道——于是卡在一条没人再投递的队列上，
// 把那一次 Stop 的 wg.Wait 拖住。生产进程里 `Scheduler.Start` 只有 `cmd/server` 一个调用点，
// 而 `examples/demo1`、`examples/demo2` 各是独立的 main、也都不接 `ResizeWorkers`，
// 所以今天哪个可执行文件都凑不出这个交错；重载链本身也是单线程逐项应用。
// 真要支持"关停与重启并发"，得把停止信号一起传进 worker。
// 已登记在卡片 §10.5。
func (s *Scheduler) ResizeWorkers(n int) error {
	if n <= 0 {
		return fmt.Errorf("workers must be positive, got %d", n)
	}

	s.mu.Lock()
	if !s.running {
		// 未启动时等价于 SetConcurrency：连 concurrency 一起写，下一次 Start 才按新值建通道起协程。
		// 代价是热更值不跨 Start→Stop→Start 保留（运行期那条分支不写 concurrency，见下面），
		// 生产进程 cmd/server 只 Start 一次，examples/* 那两个独立 main 也不接 ResizeWorkers，
		// 所以这条代价今天到不了；R06 若引入进程内重启要重放一次。
		s.concurrency = n
		s.targetWorkers.Store(int32(n))
		s.mu.Unlock()
		return nil
	}

	current := int(s.targetWorkers.Load())
	if n == current {
		// 幂等：重载链靠 Diff 保证只在真变了时调用，但重复调用不该多起协程或多记名额。
		s.mu.Unlock()
		return nil
	}

	// 只改期望并发，不碰 s.concurrency：那一份是启动期快照，queueCapacity<=0 时
	// 通道容量的回退值取的就是它，改了会让 RuntimeStats 报出通道并不具备的容量。
	s.targetWorkers.Store(int32(n))
	start := 0
	queue := s.workCh
	if n > current {
		// Add 在锁内：见上面"并发安全"——Stop 拿不到锁就无法在 Wait 之前看到这批新协程。
		start = n - current
		s.wg.Add(start)
	} else {
		// 缩容只记名额，不碰协程：名额由跑到循环开头的 worker 自己消费。
		// 不清既有余额也是有意的：余额始终等于"活着但超出目标的人数"，
		// 扩容补进来的协程会被这些名额逐一退掉，两边一起收敛到新目标值。
		s.retireRequests.Add(int32(current - n))
	}
	s.mu.Unlock()

	// go 语句在放锁之后：worker 第一件事是读原子计数、抢 s.stopCh 与队列，
	// 都不需要 s.mu，但也绝不该在持锁期间被启动。
	for i := 0; i < start; i++ {
		go s.worker(queue, false)
	}
	return nil
}

// Start 启动调度器。可重复调用：每次启动都会复位停止信号与执行队列。
func (s *Scheduler) Start() {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	// 复位停止信号，否则 Start→Stop→Start 的新协程会立刻撞上已关闭的通道
	s.stopCh = make(chan struct{})
	// 退场余额与停止信号同理：跨重启不留残留名额。上一代的 worker 已经全部退出
	// （Stop 等过 wg），留下的名额只会让这一代凭空少几个 worker。
	s.retireRequests.Store(0)
	// 调度总开关不跨重启：suspend 是给"这次发版/维护窗口"用的进程内意图，
	// 进程都换了，留着它只会让人对着一个不出任务的调度器猜原因。
	s.suspended.Store(false)
	workers := s.concurrency
	// 期望并发按这一次真正建出来的协程数计：未启动时 ResizeWorkers 写的是 concurrency，
	// 这里只是把两者对齐，运行期扩缩之后 concurrency 保持启动期口径不动。
	s.targetWorkers.Store(int32(workers))
	queueCap := s.queueCapacity
	if queueCap <= 0 {
		queueCap = workers
	}
	s.workCh = make(chan *Job, queueCap)

	// 执行器池：worker 数为 0 时连通道都不建（DoD 第 5 条）。
	// 通道置 nil 也是给投递判断用的信号——nil 通道永远"没有空位"，
	// 所以 execPoolEnabled 只要看通道是否非空。
	execWorkers := s.execConcurrency
	s.execCh = nil
	if execWorkers > 0 {
		execCap := s.execQueueCapacity
		if execCap <= 0 {
			execCap = execWorkers
		}
		s.execCh = make(chan *Job, execCap)
	}
	s.mu.Unlock()

	if err := s.Restore(); err != nil {
		s.logger.Error("failed to restore jobs on start", "error", err)
	}

	// wg 必须把两个池的 worker 都算进来：Stop 靠它等在途任务收尾，
	// 少算任何一个都会在执行器任务还在跑时就返回。
	s.wg.Add(1 + workers + execWorkers)
	go s.scheduleLoop()
	for i := 0; i < workers; i++ {
		go s.worker(s.workCh, false)
	}
	for i := 0; i < execWorkers; i++ {
		go s.worker(s.execCh, true)
	}
}

// worker 从指定队列取任务执行，队列容量与 worker 数量共同构成该池的并发上限。
//
// exec 为 true 表示服务的是执行器池：取走一个任务就敲一次 execSlotFreed——
// "队列里少了一个"正是调度循环在等的空位信号。
// 运行期扩缩的退场判定只作用于默认池，执行器池那一侧一个协程都不退。
func (s *Scheduler) worker(queue chan *Job, exec bool) {
	defer s.wg.Done()

	for {
		// 缩容的退场点：只在默认池生效。执行器池的 worker 数与"池存不存在"绑在
		// Start 的通道构造上（execCh 为 nil 即没有池），运行期扩缩另议，
		// 见 docs/design/config-reload-design.md §10 的 N1。
		// 位置在 select 之前：判到名额就退，退场的协程一条新任务都不会去领（放在执行之后，
		// 扩容时新起、又正好撞上余额的协程就会先跑一条再走）。
		// 跑完手上那条的 worker 回到循环开头时落的就是这里。
		if !exec && s.shouldRetire() {
			return
		}

		select {
		case <-s.stopCh:
			return
		case job := <-queue:
			if exec {
				s.notifyExecSlot()
			}
			s.executeJob(job)
		}
	}
}

// shouldRetire 消费一个退场名额；返回 true 表示当前这个 worker 该退出了。
// 余额为 0 时不做任何修改。计数只在缩容时增加、在退场与 Start 时清零，
// 因此不会出现"名额留到下一代 worker"的问题（Start 会清）。
//
// CAS 循环而不是 Add(-1)：两个同时到循环开头的协程可能把余额减成负数，
// 那条负余额会在下一次缩容时被"少退一个 worker"的方式偿还——读数与实跑就此长期错开。
func (s *Scheduler) shouldRetire() bool {
	for {
		pending := s.retireRequests.Load()
		if pending <= 0 {
			return false
		}
		if s.retireRequests.CompareAndSwap(pending, pending-1) {
			return true
		}
	}
}

// execPoolEnabled 判断这一次启动是否真的建了执行器池。
//
// 只在调度循环与 worker 协程里读：通道在 Start 的锁里赋值，Stop 会等这些协程全部退出，
// 因此不存在"读的时候被并发改掉"的窗口——与既有代码读 s.workCh 的方式一致。
func (s *Scheduler) execPoolEnabled() bool {
	return s.execCh != nil
}

// Stop 停止调度器：不再投递新任务，取消在途任务的上下文，并等待执行协程退出。
func (s *Scheduler) Stop() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	s.running = false
	close(s.stopCh)
	// 在途任务收到 ctx.Done 后返回，wg 才可能收敛；
	// 否则一个不检查上下文的处理器会把关停无限期挂住。
	cancels := make([]context.CancelFunc, 0, len(s.cancelMap))
	for _, cancel := range s.cancelMap {
		cancels = append(cancels, cancel)
	}
	s.mu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}

	s.wg.Wait()
}

// 调度主循环
func (s *Scheduler) scheduleLoop() {
	defer s.wg.Done()

	for {
		select {
		case <-s.stopCh:
			return
		default:
		}

		now := time.Now()

		// 挂起期间不弹出任何到期任务：堆、存储、执行中的任务都不动，
		// 到点的任务攒着，恢复后按原时间一并补跑。
		if s.suspended.Load() {
			select {
			case <-s.stopCh:
				return
			case <-s.resumeCh:
				continue
			}
		}

		// 原子地取出"已到期且所在池有空位"的任务（避免 Peek 与 Pop 之间被 Cancel 的竞态）。
		// 默认池的判定恒为可投递，因此这条与改动前的 PopIfDue 走的是同一条快路径；
		// 执行器队列满时绕开堆顶那一项，后面的普通任务照样按时投递（TASK-E13 的隔离点）。
		if item := s.heap.PopIfDueWhere(now, s.dispatchable); item != nil {
			if !s.dispatch(item.(*Job)) {
				return
			}
			continue
		}

		// 没有"到期且可投递"的任务。两种情况要分开等法：
		// 有到期任务但都被挡住（只有执行器队列满会走到这里）→ 等空位信号。
		// 绝不能继续往下按 TriggerAt 等：那些任务的触发时间已经过去，
		// 等一个已经过去的时刻等于零时长定时器忙等。
		if s.heap.HasDue(now) {
			// 这条 Debug 记录是"执行器队列已满、任务留在堆里等空位"的唯一现场痕迹：
			// 有它才能写出"没有忙等"的断言（第 5.1 第四条用例按出现次数判定），
			// 运维排障时也能看出到期任务是被哪个池挡住。
			s.logger.Debug("due job is waiting for an executor slot",
				"heap_size", s.heap.Len(), "exec_queue_capacity", cap(s.execCh))
			select {
			case <-s.stopCh:
				return
			case <-s.newJobCh:
				continue
			case <-s.execSlotFreed:
				continue
			case <-time.After(execBlockedRetry):
				// 兜底：唤醒信号可能被合并或错过，最坏情况晚投递 500ms，不会死锁
				continue
			}
		}

		item := s.heap.Peek()
		if item == nil {
			// 堆为空，等待新任务信号或超时检查
			select {
			case <-s.stopCh:
				return
			case <-s.newJobCh:
				continue
			case <-time.After(1 * time.Minute): // 定期唤醒检查
				continue
			}
		}

		job := item.(*Job)

		// 等待直到触发时间或新任务插入
		timer := time.NewTimer(job.TriggerAt.Sub(now))
		select {
		case <-s.stopCh:
			timer.Stop()
			return
		case <-s.newJobCh:
			timer.Stop()
			continue
		case <-timer.C:
			// 时间到，重新检查堆顶（可能被更新）
			continue
		}
	}
}

// dispatch 把到期任务投进它那个池的队列。返回值只表达一件事：调度循环要不要继续
// （false 表示收到停止信号）。投递失败的情况见 dispatchExec 里的说明。
func (s *Scheduler) dispatch(job *Job) bool {
	if job.class == JobClassExec && s.execPoolEnabled() {
		return s.dispatchExec(job)
	}

	// 默认池保持现状：队列满时阻塞等待空位，背压由 worker 池消化。
	// 这条不做预判是有意的——普通任务的"满则等"是 README 写明的既有行为。
	select {
	case <-s.stopCh:
		return false
	case s.workCh <- job:
		return true
	}
}

// dispatchExec 非阻塞投递到执行器队列。
//
// 调度循环在弹出之前已经用 dispatchable 确认过有空位，而且这个队列只有调度循环
// 一个生产者，所以 default 分支属于"按构造不该发生"的情形。真发生了也不能把任务丢掉：
// 塞回堆里并等一次空位信号再试——刚弹出的任务 TriggerAt 已经过去，
// 不等就会立刻再被弹出来，那才是卡片明确禁止的忙等。
func (s *Scheduler) dispatchExec(job *Job) bool {
	select {
	case <-s.stopCh:
		return false
	case s.execCh <- job:
		return true
	default:
	}

	s.logger.Error("executor queue was full after the dispatch pre-check; job put back into the heap",
		"job_id", job.ID, "queue_capacity", cap(s.execCh))
	s.heap.PushItem(job)
	s.notifyExecSlot()
	return true
}

// execBlockedRetry 是"有到期任务但执行器队列满"时的兜底唤醒间隔。
// 取值 500 毫秒的口径来自卡片 §3.4：够短，漏掉一次空位信号也只晚半秒；
// 够长，不至于在这条路径上变成轮询。
const execBlockedRetry = 500 * time.Millisecond

// dispatchable 判断这条任务现在能不能进它那个池。
//
// 这个函数会在堆的写锁里被调用，所以只读不需要加锁的东西：任务类别是入堆时盖的章，
// 队列空位用 channel 的 len/cap。绝不在这里查注册表——那会形成"先堆锁、后调度器锁"的
// 加锁顺序，而包内其它路径都是先调度器锁再碰堆，将来就会死锁。
func (s *Scheduler) dispatchable(item Item) bool {
	job, ok := item.(*Job)
	if !ok || job.class != JobClassExec || !s.execPoolEnabled() {
		// 没建执行器池时档位任务照旧进共享池，包括"满了阻塞"这条背压行为
		return true
	}
	return len(s.execCh) < cap(s.execCh)
}
