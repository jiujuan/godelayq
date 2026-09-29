package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// DefaultConcurrency 默认的并发执行 worker 数量，与 docs/deployment.md 中的
// workers 配置口径一致。
const DefaultConcurrency = 100

// Scheduler 任务调度器
type Scheduler struct {
	heap        *QuaternaryHeap
	store       Store
	retryPolicy RetryPolicy
	cronParser  CronParser

	// 控制
	mu      sync.RWMutex
	running bool
	stopCh  chan struct{}
	wg      sync.WaitGroup

	// 信号通知有新任务加入（用于提前唤醒定时器）
	newJobCh chan struct{}
	// resumeCh 只用于唤醒挂起中的调度循环，与"有新任务"分开：
	// 复用 newJobCh 会让 suspend 期间的 Schedule 调用把它占满，
	// 恢复信号就可能在下次循环之前被当成普通唤醒消费掉。
	resumeCh chan struct{}

	// suspended 是调度总开关：true 时不弹出任何到期任务。
	// 进程内状态，Start 会清零——重启即解除，维护窗口不该跨重启生效。
	suspended atomic.Bool

	// 执行侧：有界队列 + 固定 worker 池，避免到期风暴时无限起协程
	concurrency   int
	queueCapacity int // 0 表示与 concurrency 相等
	workCh        chan *Job

	// 任务注册表，按 HandlerKey（Type，回退 Name）绑定Handler（用于从持久化恢复）
	handlers map[string]Handler

	// 取消控制（与 handlers 一样受 s.mu 保护）
	cancelMap map[string]context.CancelFunc
	// forcedPause 记下"已请求强制暂停、等待执行收尾认领"的任务 ID。
	// 用集合而不是存指针：中止是异步的，标记由 handleInterrupted/handleSuccess/
	// handleFailure 三条收尾路径之一消费，谁先回来谁负责把任务停在 paused。
	forcedPause map[string]struct{}

	// inFlight 已进入 Handler 执行、尚未返回的任务数，供统计接口读取
	inFlight atomic.Int32

	eventBus *EventBus // 新增

	// logger 结构化日志器，构造后不再变更；未注入时为 slog.Default()
	logger *slog.Logger
}

// NewScheduler 创建调度器。可通过 WithLogger 注入日志器。
func NewScheduler(store Store, retryPolicy RetryPolicy, eventBus *EventBus, opts ...Option) *Scheduler {
	if retryPolicy == nil {
		retryPolicy = &ExponentialBackoffRetry{}
	}

	if eventBus == nil {
		eventBus = NewEventBus(100) // 默认事件总线
	}

	settings := newComponentOptions(opts...)

	return &Scheduler{
		heap:        NewQuaternaryHeap(),
		store:       store,
		retryPolicy: retryPolicy,
		cronParser:  NewCronParser(),
		stopCh:      make(chan struct{}),
		newJobCh:    make(chan struct{}, 1),
		resumeCh:    make(chan struct{}, 1),
		concurrency: DefaultConcurrency,
		handlers:    make(map[string]Handler),
		cancelMap:   make(map[string]context.CancelFunc),
		forcedPause: make(map[string]struct{}),
		eventBus:    eventBus,
		logger:      resolveLogger(settings.logger),
	}
}

// SetConcurrency 设置执行 worker 数量，需在 Start 之前调用。
// 传入非正数时回退到 DefaultConcurrency；Start 之后调用不生效并记录日志。
func (s *Scheduler) SetConcurrency(n int) {
	if n <= 0 {
		n = DefaultConcurrency
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		s.logger.Warn("SetConcurrency ignored: scheduler already running", "workers", n)
		return
	}
	s.concurrency = n
}

// SetQueueCapacity 设置执行队列容量，需在 Start 之前调用。
// 0 或负数表示与 worker 数相等；队列满时调度循环阻塞入队（背压）。
func (s *Scheduler) SetQueueCapacity(n int) {
	if n < 0 {
		n = 0
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		s.logger.Warn("SetQueueCapacity ignored: scheduler already running", "queue_capacity", n)
		return
	}
	s.queueCapacity = n
}

// RegisterHandler 注册任务类型对应的处理函数
func (s *Scheduler) RegisterHandler(jobType string, handler Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[jobType] = handler
}

// LookupHandler 按 Handler 键（Type 优先，回退 Name）查找已注册的处理函数。
// 注册表只有调度器这一份，HTTP 层校验与执行侧绑定共用它。
func (s *Scheduler) LookupHandler(key string) (Handler, bool) {
	return s.lookupHandler(key)
}

// HandlerNames 返回全部已注册的 Handler 键，按字典序排列，供接口稳定输出。
func (s *Scheduler) HandlerNames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	names := make([]string, 0, len(s.handlers))
	for name := range s.handlers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// lookupHandler 按任务键查找已注册的Handler
func (s *Scheduler) lookupHandler(key string) (Handler, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h, ok := s.handlers[key]
	return h, ok
}

// Schedule 添加延迟任务
func (s *Scheduler) Schedule(job *Job) error {
	if job.ID == "" {
		job.ID = generateID()
	}
	if job.CreatedAt.IsZero() {
		job.CreatedAt = time.Now()
	}
	if job.Status == 0 {
		job.Status = StatusPending
	}

	// 如果是Cron任务，计算下次执行时间
	if job.IsRepeat && job.CronExpr != "" {
		next, err := s.cronParser.Next(job.CronExpr, time.Now())
		if err != nil {
			return err
		}
		job.TriggerAt = next
	}

	s.heap.PushItem(job)

	// 持久化
	if s.store != nil {
		if err := s.store.Save(job); err != nil {
			s.logger.Error("failed to persist job", "job_id", job.ID, "error", err)
		}
	}

	// 通知调度循环可能有更早的任务
	s.notifyNewJob()

	// 发布事件
	s.eventBus.Publish(Event{
		Type:      EventJobScheduled,
		JobID:     job.ID,
		JobName:   job.Name,
		Status:    StatusPending,
		Timestamp: time.Now(),
		Metadata: map[string]interface{}{
			"trigger_at": job.TriggerAt,
			"is_repeat":  job.IsRepeat,
		},
	})

	return nil
}

// CancelRunning 取消正在执行的任务上下文。
// 返回 true 表示找到并取消了执行中的任务。
func (s *Scheduler) CancelRunning(jobID string) bool {
	s.mu.Lock()
	cancel, ok := s.cancelMap[jobID]
	s.mu.Unlock()
	if !ok {
		return false
	}
	cancel()
	return true
}

// Cancel 取消任务：待执行任务从堆中移除，已出堆正在执行的任务取消其执行上下文。
// 两种情况都会清理存储，避免下次启动时被 Restore 重新入队。
func (s *Scheduler) Cancel(jobID string) error {
	cancelled := false

	if job := s.heap.Remove(jobID); job != nil {
		job.(*Job).Status = StatusCancelled
		cancelled = true
	}
	if s.CancelRunning(jobID) {
		cancelled = true
	}

	if !cancelled {
		// 暂停中的任务既不在堆里也不在执行中，但它确实存在且必须还能被删掉——
		// 否则一旦暂停就再也清理不掉，存储里只会长出删不掉的僵尸快照。
		if snap, ok := s.findSnapshot(jobID); ok && JobStatus(snap.Status) == StatusPaused {
			if s.store != nil {
				if err := s.store.Delete(jobID); err != nil {
					s.logger.Error("failed to delete paused job", "job_id", jobID, "error", err)
				}
			}
			s.eventBus.Publish(Event{
				Type:      EventJobCancelled,
				JobID:     jobID,
				JobName:   snap.Name,
				Status:    StatusCancelled,
				Timestamp: time.Now(),
				Metadata:  map[string]interface{}{"was_paused": true},
			})
			return nil
		}
		return ErrJobNotFound
	}

	if s.store != nil {
		if err := s.store.Delete(jobID); err != nil {
			s.logger.Error("failed to delete cancelled job", "job_id", jobID, "error", err)
		}
	}

	s.eventBus.Publish(Event{
		Type:      EventJobCancelled,
		JobID:     jobID,
		Status:    StatusCancelled,
		Timestamp: time.Now(),
	})
	return nil
}

// Pause 暂停一个待执行任务：把它从堆里取出，并以 paused 状态写回存储。
//
// 与 Cancel 的关键差别就在这里——Cancel 连存储一起删除（任务从此不存在），
// Pause 保留快照（任务只是不排期），因此 Resume 能按原 ID 把它唤回。
//
// 只对堆里的任务生效：正在执行的任务由 worker 持有，暂停它语义不明，
// 需要中止执行请走 ForcePause（admin 档）。对已暂停的任务重复调用是幂等的，
// 返回当前状态而不报错，让控制器的双击/重试不至于变成 409。
func (s *Scheduler) Pause(jobID string) (*Job, error) {
	item := s.heap.Remove(jobID)
	if item == nil {
		if s.isRunning(jobID) {
			return nil, ErrJobNotPending
		}
		if snap, ok := s.findSnapshot(jobID); ok && JobStatus(snap.Status) == StatusPaused {
			job := &Job{}
			job.FromSnapshot(snap)
			return job, nil
		}
		return nil, ErrJobNotFound
	}

	return s.pausePendingJob(item.(*Job), false)
}

// pausePendingJob 把一个刚从堆里摘下的任务落成 paused：落盘 + 唤醒循环 + 广播。
// forced 只影响事件里的标记位，状态迁移两条路径完全一致。
func (s *Scheduler) pausePendingJob(job *Job, forced bool) (*Job, error) {
	job.Status = StatusPaused
	job.UpdatedAt = time.Now()

	if s.store != nil {
		if err := s.store.Update(job.ToSnapshot()); err != nil {
			// 堆里已经取走了，落盘失败只可能让重启后的恢复口径不一致：
			// 记日志并继续，任务此刻确实处于暂停中。
			s.logger.Error("failed to persist paused job", "job_id", job.ID, "error", err)
		}
	}

	// 堆顶可能变了，唤醒调度循环重算等待时长
	s.notifyNewJob()

	s.eventBus.Publish(Event{
		Type:      EventJobPaused,
		JobID:     job.ID,
		JobName:   job.Name,
		Status:    StatusPaused,
		Timestamp: time.Now(),
		Metadata: map[string]interface{}{
			"trigger_at": job.TriggerAt,
			"forced":     forced,
		},
	})

	return job, nil
}

// Resume 恢复一个暂停的任务：从存储取回快照，重新排期并保留原 ID。
//
// 触发时间由 Schedule 统一处理：Cron 重复任务按表达式取下一个未来时点，
// 一次性任务的 TriggerAt 若已过期则立刻补跑——与崩溃恢复 Restore 的口径一致，
// 不额外制造"暂停期间到期的任务被吞掉"这种差异。
func (s *Scheduler) Resume(jobID string) (*Job, error) {
	snap, ok := s.findSnapshot(jobID)
	if !ok {
		return nil, ErrJobNotFound
	}
	if JobStatus(snap.Status) != StatusPaused {
		return nil, ErrJobNotPaused
	}
	// 强制暂停的落盘发生在取消上下文之前，Handler 可能还在收尾。此刻恢复会与
	// "即将把状态钉回 paused"的守卫打架，于是任务看起来自己又停了：直接拒绝，
	// 让调用方等它停稳（UI 的"暂停中"态就是为这段时间准备的）。
	if s.isRunning(jobID) {
		return nil, ErrJobNotPending
	}

	job := &Job{}
	job.FromSnapshot(snap)
	// Handler 不落盘，恢复时必须按 HandlerKey 重新绑定
	if handler, exists := s.lookupHandler(job.HandlerKey()); exists {
		job.Handler = handler
	}
	job.Status = StatusPending
	job.UpdatedAt = time.Now()

	if err := s.Schedule(job); err != nil {
		return nil, err
	}

	// Schedule 已广播 job.scheduled（它确实被重新排期了）；再补一条 resumed，
	// 让时间线能把"恢复"这个人为动作与自动重排区分开。
	s.eventBus.Publish(Event{
		Type:      EventJobResumed,
		JobID:     job.ID,
		JobName:   job.Name,
		Status:    StatusPending,
		Timestamp: time.Now(),
		Metadata: map[string]interface{}{
			"trigger_at": job.TriggerAt,
		},
	})

	return job, nil
}

// ForcePause 强制暂停一个正在执行的任务：中止当前 attempt，不计失败、不消耗重试，
// 停在 paused（限 admin/ops 档调用，授权在 api 层，core 不感知角色）。
//
// 与 Pause 的分工：Pause 只管还没出堆的任务；已经交给 worker 的任务只能靠取消上下文
// 让它自己停下来。因此这里只登记标记 + 取消 ctx，真正把状态落盘由执行收尾完成
// （见 parkForcedPause）——同一条 job.paused 事件对应"确实停住了"这个事实，
// UI 才能诚实地区分"暂停中"和"已暂停"。
func (s *Scheduler) ForcePause(jobID string) (*Job, error) {
	// 竞态下任务可能还在堆里没被弹出：那时取消上下文没有意义，直接从堆里摘掉，
	// 语义与普通暂停一致且更强（保证不会再执行一次）。
	if item := s.heap.Remove(jobID); item != nil {
		return s.pausePendingJob(item.(*Job), true)
	}

	s.mu.Lock()
	cancel, running := s.cancelMap[jobID]
	if running {
		s.forcedPause[jobID] = struct{}{}
	}
	s.mu.Unlock()

	if !running {
		if _, ok := s.findSnapshot(jobID); !ok {
			return nil, ErrJobNotFound
		}
		// 快照存在但既不在堆里也不在执行中：要么已暂停（交给 Pause 的幂等路径），
		// 要么已结束/正在收尾，两者都不能被"强制暂停"
		paused, err := s.Pause(jobID)
		if err == nil {
			return paused, nil
		}
		return nil, ErrJobNotPending
	}

	// 先把期望状态落盘，再取消：即便 Handler 完全不响应取消，
	// 收尾路径也会在它返回时把状态钉回 paused（见 handleSuccess 等处的守卫）。
	//
	// 只在真的取到快照时才改写它：纯内存部署（store 为 nil）没有可写的记录，
	// 而"store 非空却查不到"的极端竞态下用零值快照覆盖，会把任务内容抹平。
	job := &Job{ID: jobID, Status: StatusPaused, UpdatedAt: time.Now()}
	if snap, found := s.findSnapshot(jobID); found {
		snap.Status = int(StatusPaused)
		snap.UpdatedAt = job.UpdatedAt
		if s.store != nil {
			if err := s.store.Update(snap); err != nil {
				s.logger.Error("failed to persist force-paused job", "job_id", jobID, "error", err)
			}
		}
		job.FromSnapshot(snap) // 快照里的 status 已被置为 paused
	}

	cancel()

	return job, nil
}

// takeForcedPause 消费一个强制暂停标记（一次性），返回它是否存在。
func (s *Scheduler) takeForcedPause(jobID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.forcedPause[jobID]; !ok {
		return false
	}
	delete(s.forcedPause, jobID)
	return true
}

// parkForcedPause 由执行收尾调用：若该任务被请求过强制暂停，就把它钉在 paused 上
// 并广播 job.paused，返回 true 表示调用方应当放弃自己原有的收尾逻辑
// （不再记成功、不再计重试、不再重排 Cron 的下一轮）。
//
// 这是"不检查 ctx 的 Handler"唯一的兜底：本仓库一直承认这类处理器存在
// （见 executeJob 的超时注释），它可能压根没注意到上下文已被取消，
// 于是照常返回 nil 或 error。没有这层守卫，一次强制暂停会被随后的
// Cron 重排悄悄复活。
func (s *Scheduler) parkForcedPause(job *Job, note string) bool {
	if !s.takeForcedPause(job.ID) {
		return false
	}

	job.Status = StatusPaused
	job.UpdatedAt = time.Now()
	if s.store != nil {
		if err := s.store.Update(job.ToSnapshot()); err != nil {
			s.logger.Error("failed to persist force-paused job", "job_id", job.ID, "error", err)
		}
	}

	s.eventBus.Publish(Event{
		Type:      EventJobPaused,
		JobID:     job.ID,
		JobName:   job.Name,
		Status:    StatusPaused,
		Timestamp: time.Now(),
		Metadata: map[string]interface{}{
			"forced":     true,
			"note":       note,
			"retry_used": job.RetryCount,
			"attempts":   job.Attempts,
		},
	})
	s.logger.Info("job force-paused", "job_id", job.ID, "note", note)
	return true
}

// isRunning 判断任务是否已被 worker 取出、尚未返回。
func (s *Scheduler) isRunning(jobID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.cancelMap[jobID]
	return ok
}

// findSnapshot 按 ID 取存储中的快照。
//
// 线性扫描 LoadAll：与 hasStoredJob 同一代价，当前 history_limit 量级可接受。
// 等 Store 有 Get(jobID) 之后这两处一起收敛。
func (s *Scheduler) findSnapshot(jobID string) (JobSnapshot, bool) {
	if s.store == nil {
		return JobSnapshot{}, false
	}

	snapshots, err := s.store.LoadAll()
	if err != nil {
		s.logger.Error("failed to load jobs while looking up a job", "job_id", jobID, "error", err)
		return JobSnapshot{}, false
	}
	for _, snap := range snapshots {
		if snap.ID == jobID {
			return snap, true
		}
	}
	return JobSnapshot{}, false
}

// notifyNewJob 唤醒调度循环重算等待时长；信号通道满时丢弃即可（已有待处理唤醒）。
func (s *Scheduler) notifyNewJob() {
	select {
	case s.newJobCh <- struct{}{}:
	default:
	}
}

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

// ErrJobNotPending 表示任务已不在待执行队列中（已被弹出执行、已结束或不存在），
// 因此无法原地更新。
var ErrJobNotPending = errors.New("job is not pending")

// ErrJobNotPaused 表示任务当前不处于暂停状态，无法恢复。
var ErrJobNotPaused = errors.New("job is not paused")

// UpdatePending 原地修改一个待执行任务：apply 在任务的副本上生效，
// 只有堆内条目被成功替换后才落盘，避免"取消成功但重排失败"丢任务。
// apply 返回错误则不做任何改动；任务在检查后被调度弹出时返回 ErrJobNotPending。
func (s *Scheduler) UpdatePending(jobID string, apply func(*Job) error) (*Job, error) {
	item := s.heap.Get(jobID)
	if item == nil {
		// 堆里没有不等于任务不存在：正在执行或已结束的条目会返回 409 而非 404
		if s.hasStoredJob(jobID) {
			return nil, ErrJobNotPending
		}
		return nil, ErrJobNotFound
	}
	current := item.(*Job)

	// 复制后修改：失败或竞态时堆里的原条目不受影响
	updated := *current
	if current.Payload != nil {
		updated.Payload = append([]byte(nil), current.Payload...)
	}

	if apply != nil {
		if err := apply(&updated); err != nil {
			return nil, err
		}
	}
	updated.Status = StatusPending
	updated.UpdatedAt = time.Now()

	if !s.heap.Update(&updated) {
		// 取到条目之后被调度循环弹出了，交给执行侧而不是原地更新
		return nil, ErrJobNotPending
	}

	if s.store != nil {
		if err := s.store.Update(updated.ToSnapshot()); err != nil {
			s.logger.Error("failed to persist updated job", "job_id", updated.ID, "error", err)
		}
	}

	// 触发时间可能提前，唤醒调度循环重算等待时长
	s.notifyNewJob()

	return &updated, nil
}

// hasStoredJob 判断存储中是否仍有该任务的记录，用于区分"从未存在"与"已不可修改"。
func (s *Scheduler) hasStoredJob(jobID string) bool {
	_, ok := s.findSnapshot(jobID)
	return ok
}

// SetGroup 改挂一个任务的分组；group 为空串表示取消分组。
//
// 堆里的任务必须连堆内条目一起改：只写快照的话，内存里仍带着旧分组，
// 任务执行完收尾会把它写回去，分组改动跑一轮就失效。
// 暂停中的任务与终态留痕不在堆里，直接改快照；正在执行的任务由 worker 持有，
// 改法同上会被收尾覆盖，因此返回 ErrJobNotPending。
func (s *Scheduler) SetGroup(jobID string, group string) error {
	snap, ok := s.findSnapshot(jobID)
	if !ok {
		// 纯内存部署（store 为 nil）没有快照可读，但堆里确实可能有这个任务
		item := s.heap.Get(jobID)
		if item == nil {
			return ErrJobNotFound
		}
		snap = item.(*Job).ToSnapshot()
	}

	changed, err := s.applyGroupToSnapshot(snap, group)
	switch {
	case err != nil:
		return err
	case !changed && snap.Group != group:
		// 没改动也没报错：任务正在执行，或刚好在这次读取后被弹出
		return ErrJobNotPending
	default:
		return nil
	}
}

// RetagGroup 把所有挂在 fromGroup 上的任务改挂到 toGroup（空串表示取消分组），
// 返回改动的任务数。名称匹配忽略大小写，与 GroupStore 的主键口径一致。
//
// 非原子：中途出错时前面的已改、后面的未改，调用方重试即可（幂等）。
// 这与本仓库"尽力落盘 + 崩溃靠 Restore"的整体口径一致。
func (s *Scheduler) RetagGroup(fromGroup string, toGroup string) (int, error) {
	if s.store == nil || fromGroup == "" {
		return 0, nil
	}
	snapshots, err := s.store.LoadAll()
	if err != nil {
		return 0, fmt.Errorf("load jobs for retag failed: %w", err)
	}

	changed := 0
	for _, snap := range snapshots {
		if !strings.EqualFold(snap.Group, fromGroup) {
			continue
		}
		ok, err := s.applyGroupToSnapshot(snap, toGroup)
		if err != nil {
			s.logger.Error("failed to retag job", "job_id", snap.ID, "group", toGroup, "error", err)
			continue
		}
		if ok {
			changed++
		}
	}
	return changed, nil
}

// applyGroupToSnapshot 把一个任务的分组改成 toGroup，返回是否真的写动了。
// 调用方已经持有该任务的快照，因此不再回查存储。
func (s *Scheduler) applyGroupToSnapshot(snap JobSnapshot, toGroup string) (bool, error) {
	if item := s.heap.Get(snap.ID); item != nil {
		job := *(item.(*Job))
		if job.Group == toGroup {
			return false, nil
		}
		job.Group = toGroup
		job.UpdatedAt = time.Now()
		if !s.heap.Update(&job) {
			// 取到条目之后被调度循环弹出了，交给执行侧
			return false, nil
		}
		if s.store == nil {
			return true, nil
		}
		if err := s.store.Update(job.ToSnapshot()); err != nil {
			return false, err
		}
		return true, nil
	}

	// 正在执行的任务由 worker 持有内存副本，改快照会被它的收尾写回覆盖
	if JobStatus(snap.Status) == StatusRunning {
		return false, nil
	}
	if s.store == nil || snap.Group == toGroup {
		return false, nil
	}

	snap.Group = toGroup
	snap.UpdatedAt = time.Now()
	if err := s.store.Update(snap); err != nil {
		return false, err
	}
	return true, nil
}

// Restore 从持久化存储重建调度队列（崩溃/重启恢复）。
// 快照中仅 Pending/Running 状态的任务会被重新入队，状态重置为 Pending；
// 终态留痕与 paused 任务都不入队——暂停是人为决定，重启不该替用户取消它。
// Handler 在执行前按 HandlerKey 从注册表绑定。
func (s *Scheduler) Restore() error {
	if s.store == nil {
		return nil
	}
	snapshots, err := s.store.LoadAll()
	if err != nil {
		return err
	}

	restored := 0
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
		// TriggerAt 已过期的任务直接入队，由调度循环立即补跑
		s.heap.PushItem(job)
		restored++
	}
	if restored > 0 {
		s.logger.Info("restored jobs from store", "count", restored)
	}
	return nil
}

// RunningCount 返回正在执行（已进入 Handler）的任务数。
func (s *Scheduler) RunningCount() int {
	return int(s.inFlight.Load())
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
	// 调度总开关不跨重启：suspend 是给"这次发版/维护窗口"用的进程内意图，
	// 进程都换了，留着它只会让人对着一个不出任务的调度器猜原因。
	s.suspended.Store(false)
	workers := s.concurrency
	queueCap := s.queueCapacity
	if queueCap <= 0 {
		queueCap = workers
	}
	s.workCh = make(chan *Job, queueCap)
	s.mu.Unlock()

	if err := s.Restore(); err != nil {
		s.logger.Error("failed to restore jobs on start", "error", err)
	}

	s.wg.Add(1 + workers)
	go s.scheduleLoop()
	for i := 0; i < workers; i++ {
		go s.worker()
	}
}

// worker 从执行队列取任务执行，队列与 worker 数量共同构成并发上限
func (s *Scheduler) worker() {
	defer s.wg.Done()

	for {
		select {
		case <-s.stopCh:
			return
		case job := <-s.workCh:
			s.executeJob(job)
		}
	}
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

		// 原子地取出所有已到期任务（避免 Peek 与 Pop 之间被 Cancel 的竞态）
		if job := s.heap.PopIfDue(now); job != nil {
			if !s.dispatch(job.(*Job)) {
				return
			}
			continue
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

// dispatch 把到期任务投入执行队列。队列满时阻塞等待空位（背压由 worker 池消化），
// 关停信号到来时放弃投递并返回 false。
func (s *Scheduler) dispatch(job *Job) bool {
	select {
	case <-s.stopCh:
		return false
	case s.workCh <- job:
		return true
	}
}

// 执行任务（由 worker 协程调用，阻塞直到 Handler 返回）
func (s *Scheduler) executeJob(job *Job) {
	s.inFlight.Add(1)
	defer s.inFlight.Add(-1)

	// 发布开始事件
	s.eventBus.Publish(Event{
		Type:      EventJobStarted,
		JobID:     job.ID,
		JobName:   job.Name,
		Status:    StatusRunning,
		Timestamp: time.Now(),
		Metadata: map[string]interface{}{
			"attempt": job.Attempts,
		},
	})

	// 恢复/补齐Handler（Type 优先，回退 Name）；显式绑定的 Handler 优先
	if job.Handler == nil {
		key := job.HandlerKey()
		if h, ok := s.lookupHandler(key); ok {
			job.Handler = h
		}
		if job.Handler == nil {
			s.logger.Error("no handler registered for job", "job_id", job.ID, "handler_key", key)
			job.Status = StatusFailed
			job.UpdatedAt = time.Now()
			if s.store != nil {
				s.store.Update(job.ToSnapshot())
			}
			errData, _ := json.Marshal(map[string]string{"error": "no handler registered"})
			s.eventBus.Publish(Event{
				Type:      EventJobFailed,
				JobID:     job.ID,
				JobName:   job.Name,
				Status:    StatusFailed,
				Timestamp: time.Now(),
				Data:      errData,
			})
			return
		}
	}

	// 创建可取消的上下文；条目生命周期与实际执行一致，
	// 否则 Cancel 在任务真正运行前就查不到取消函数。
	cancelCtx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancelMap[job.ID] = cancel
	// 与 Stop 的取消快照互斥：要么 Stop 之后能看到这条登记并取消它，
	// 要么这里已察觉关停、自行取消，避免在关停竞态中启动无人取消的任务。
	stopping := false
	select {
	case <-s.stopCh:
		stopping = true
	default:
	}
	s.mu.Unlock()
	if stopping {
		cancel()
	}
	defer func() {
		s.mu.Lock()
		delete(s.cancelMap, job.ID)
		s.mu.Unlock()
		cancel()
	}()

	// 单次执行超时：Handler 需检查 ctx 才能被按时中止；
	// 不检查 ctx 的处理器仍会占住 worker 名额，超时只能作为失败被观测。
	execCtx := cancelCtx
	if job.Timeout > 0 {
		withTimeout, cancelTimeout := context.WithTimeout(cancelCtx, job.Timeout)
		defer cancelTimeout()
		execCtx = withTimeout
	}

	job.Status = StatusRunning
	job.Attempts++
	job.UpdatedAt = time.Now()

	// 落盘运行态：崩溃后重启能看出哪些任务当时在执行（由 Restore 复位为待执行），
	// 统计接口也不再依赖进程内状态。写入会被合并落盘吸收，不增加每次一写。
	if s.store != nil {
		if err := s.store.Update(job.ToSnapshot()); err != nil {
			s.logger.Error("failed to persist running job", "job_id", job.ID, "error", err)
		}
	}

	err := job.Handler(execCtx, job)

	if err == nil {
		// 发布成功事件
		s.eventBus.Publish(Event{
			Type:      EventJobCompleted,
			JobID:     job.ID,
			JobName:   job.Name,
			Status:    StatusSuccess,
			Timestamp: time.Now(),
			Metadata: map[string]interface{}{
				"duration_ms": time.Since(job.UpdatedAt).Milliseconds(),
			},
		})

		s.handleSuccess(job)
		return
	}

	// 父上下文被取消：用户 Cancel 或关停打断，不属于任务自身失败，不消耗重试次数
	if errors.Is(err, context.Canceled) || errors.Is(cancelCtx.Err(), context.Canceled) {
		s.handleInterrupted(job)
		return
	}

	timedOut := errors.Is(err, context.DeadlineExceeded) || errors.Is(execCtx.Err(), context.DeadlineExceeded)

	// 发布失败事件
	errData, _ := json.Marshal(map[string]string{"error": err.Error()})
	s.eventBus.Publish(Event{
		Type:      EventJobFailed,
		JobID:     job.ID,
		JobName:   job.Name,
		Status:    StatusFailed,
		Timestamp: time.Now(),
		Data:      errData,
		Metadata: map[string]interface{}{
			"retry_count": job.RetryCount,
			"max_retries": job.MaxRetries,
			"timeout":     timedOut,
		},
	})

	if timedOut {
		s.logger.Error("job timed out", "job_id", job.ID, "timeout", job.Timeout, "error", err)
	} else {
		s.logger.Error("job failed", "job_id", job.ID, "error", err)
	}
	s.handleFailure(job)
}

// handleInterrupted 处理"执行被打断"：既不记为失败，也不消耗重试次数。
// 用户主动取消时，事件与存储清理由 Cancel 完成；关停打断时把任务保持为
// Pending 落盘，交由下次 Start 的 Restore 重新入队（至少一次语义，
// 副作用可能重复，Handler 需自行保证幂等）。
func (s *Scheduler) handleInterrupted(job *Job) {
	// 强制暂停优先于关停恢复：否则"取消上下文"会被当成关停打断，
	// 任务被复位成 pending 重新排期，暂停请求当场失效。
	if s.parkForcedPause(job, "interrupted") {
		return
	}

	s.mu.RLock()
	running := s.running
	s.mu.RUnlock()

	if running {
		s.logger.Info("job execution cancelled", "job_id", job.ID)
		return
	}

	job.Status = StatusPending
	job.UpdatedAt = time.Now()
	if s.store != nil {
		if err := s.store.Update(job.ToSnapshot()); err != nil {
			s.logger.Error("failed to persist interrupted job", "job_id", job.ID, "error", err)
		}
	}

	s.eventBus.Publish(Event{
		Type:      EventJobCancelled,
		JobID:     job.ID,
		JobName:   job.Name,
		Status:    StatusPending,
		Timestamp: time.Now(),
		Metadata:  map[string]interface{}{"reason": "shutdown"},
	})
	s.logger.Info("job interrupted by shutdown, kept pending for recovery", "job_id", job.ID)
}

// 处理成功
func (s *Scheduler) handleSuccess(job *Job) {
	// 被请求过强制暂停的任务即便"成功返回"也要停在 paused：
	// 不检查 ctx 的 Handler 会走到这里，若不拦下来，Cron 的下一轮会被悄悄排上，
	// 暂停看起来像只生效了几秒钟。
	if s.parkForcedPause(job, "handler_completed_despite_cancel") {
		return
	}

	job.Status = StatusSuccess
	job.UpdatedAt = time.Now()

	// 如果是重复任务，计算下次执行时间并重新入队
	if job.IsRepeat && job.CronExpr != "" {
		next, err := s.cronParser.Next(job.CronExpr, time.Now())
		if err == nil {
			newJob := &Job{
				ID:         job.ID, // 保持相同ID会覆盖旧数据
				Name:       job.Name,
				Type:       job.Type,
				Payload:    job.Payload,
				Group:      job.Group, // 重排是同一逻辑任务的下一轮，丢分组等于凭空换组
				TriggerAt:  next,
				Handler:    job.Handler,
				CronExpr:   job.CronExpr,
				IsRepeat:   true,
				MaxRetries: job.MaxRetries,
				Timeout:    job.Timeout,
				Status:     StatusPending,
				CreatedAt:  job.CreatedAt,
				UpdatedAt:  time.Now(),
				// 不写 Exec：下一轮是一次新的执行，带上一次的退出码会让详情页在跑完之前显示旧结论。
				// 这里是显式字段列表，漏写即是置 nil，与 CloneForRetry 的口径一致。
			}
			s.Schedule(newJob)
			return
		}
		s.logger.Error("failed to compute next run for cron job", "job_id", job.ID, "cron_expr", job.CronExpr, "error", err)
	}

	// 终态留痕：写入成功快照，是否长期保留由存储的保留策略决定
	if s.store != nil {
		if err := s.store.Update(job.ToSnapshot()); err != nil {
			s.logger.Error("failed to persist completed job", "job_id", job.ID, "error", err)
		}
	}
}

// 处理失败与重试
func (s *Scheduler) handleFailure(job *Job) {
	// 同 handleSuccess：中止期间 Handler 自己报了错，也按用户意图停在 paused，
	// 不再消耗重试次数、不再重新排期。
	if s.parkForcedPause(job, "handler_failed_despite_cancel") {
		return
	}

	if job.RetryCount < job.MaxRetries {
		// 计算下次重试时间
		nextTime := s.retryPolicy.NextRetry(job)

		// 发布重试事件
		s.eventBus.Publish(Event{
			Type:      EventJobRetrying,
			JobID:     job.ID,
			JobName:   job.Name,
			Status:    StatusPending,
			Timestamp: time.Now(),
			Metadata: map[string]interface{}{
				"next_retry_at": nextTime,
				"retry_count":   job.RetryCount + 1,
			},
		})

		retryJob := job.CloneForRetry(nextTime)

		s.logger.Warn("scheduling retry", "job_id", job.ID, "next_time", nextTime)
		s.Schedule(retryJob)
	} else {
		job.Status = StatusFailed
		job.UpdatedAt = time.Now()
		if s.store != nil {
			if err := s.store.Update(job.ToSnapshot()); err != nil {
				s.logger.Error("failed to persist failed job", "job_id", job.ID, "error", err)
			}
		}
	}
}

// GetEventBus 暴露 EventBus（用于WebSocket订阅）
func (s *Scheduler) GetEventBus() *EventBus {
	return s.eventBus
}

// generateID 生成任务/订阅/客户端的标识：UUIDv7，时间有序且随机位来自 crypto/rand。
// 旧实现是"秒级时间戳 + 8 位 math/rand 后缀"，同一秒提交的任务会因随机位碰撞得到
// 相同 ID，后一个直接覆盖前一个（任务静默丢失，无任何报错）。
func generateID() string {
	return uuid.Must(uuid.NewV7()).String()
}

// HeapLen 获取堆中任务数量（用于监控）
func (s *Scheduler) HeapLen() int {
	return s.heap.Len()
}

// RuntimeStats 是调度与执行侧的实时占用，供运维端点读取。
// 只有计数与开关状态，不含任务内容与凭据，因此 viewer 以外的角色才需要看到它。
type RuntimeStats struct {
	// Started 表示调度循环与 worker 池是否在跑（Stop 之后为 false）
	Started bool `json:"started"`
	// Workers 配置的 worker 数量
	Workers int `json:"workers"`
	// QueueCapacity 执行队列容量；未显式配置时与 Workers 相等
	QueueCapacity int `json:"queue_capacity"`
	// QueueLength 已入队但尚未被 worker 取走的任务数
	QueueLength int `json:"queue_length"`
	// Running 已进入 Handler、尚未返回的任务数
	Running int `json:"running"`
	// HeapSize 堆中待执行任务数
	HeapSize int `json:"heap_size"`
	// Suspended 调度总开关是否处于挂起
	Suspended bool `json:"suspended"`
	// ForcePausePending 已请求强制暂停、尚待执行收尾认领的任务数
	ForcePausePending int `json:"force_pause_pending"`
}

// RuntimeStats 返回当前占用快照。队列长度与在跑数量是瞬时值，
// 读到的只是调用那一刻的状态，用于观察趋势而不是审计。
func (s *Scheduler) RuntimeStats() RuntimeStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	capacity := s.queueCapacity
	if capacity <= 0 {
		capacity = s.concurrency
	}

	return RuntimeStats{
		Started:           s.running,
		Workers:           s.concurrency,
		QueueCapacity:     capacity,
		QueueLength:       len(s.workCh),
		Running:           int(s.inFlight.Load()),
		HeapSize:          s.heap.Len(),
		Suspended:         s.suspended.Load(),
		ForcePausePending: len(s.forcedPause),
	}
}
