// scheduler_job.go 承载对任务的控制动作：排期与取消（Schedule/Cancel）、
// 暂停族（Pause/Resume/ForcePause 及强制暂停的收尾认领）、
// 原地更新（UpdatePending）与分组改挂（SetGroup/RetagGroup/PauseByHandlerKey）。
package core

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

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

	// 执行类别在入堆之前盖章：投递判断要在堆的锁里读它，那时不该再回头查注册表。
	// 注册表里没有这个键时落在 JobClassDefault，与改动前的行为一致。
	job.class = s.classOfKey(job.HandlerKey())

	// 持久化必须在入堆之前：任务一进堆就可能被 worker 取走，worker 会就地改写
	// Status/Attempts/UpdatedAt（executeJob 开头那三行），而快照是这些字段的读取方。
	// 反过来先入堆再落盘，等于允许"执行中的任务被同一个指针读一次状态"，
	// -race 下会在 Resume→Schedule 这条路径上判为数据竞争（同一指针，两个协程）。
	// 落盘与 job.scheduled 事件一并走 persistAndEmit 收口（TASK-RE01）；JSON 存储下
	// Save(job) 与 Update(ToSnapshot) 等价，收口后 RE07 才能把两者放进同一事务。
	// 落盘失败只记日志、照常入堆，与之前的容错口径一致。
	//
	// 这里不经 transition：入堆的都是新排期（pending，含 cron 重排的同 ID 新对象），
	// "上一轮 running→pending"的转移在新对象上无从取 from，语义说明见 handleSuccess。
	if err := s.persistAndEmit(job, Event{
		Type:      EventJobScheduled,
		JobID:     job.ID,
		JobName:   job.Name,
		Status:    StatusPending,
		Timestamp: time.Now(),
		Metadata: map[string]interface{}{
			"trigger_at": job.TriggerAt,
			"is_repeat":  job.IsRepeat,
		},
	}); err != nil {
		s.logger.Error("failed to persist job", "job_id", job.ID, "error", err)
	}

	s.heap.PushItem(job)

	// 通知调度循环可能有更早的任务
	s.notifyNewJob()

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

// Cancel 取消任务（TASK-RE01 起落 cancelled 终态，不再删除快照）。三条路径：
//
//   - 堆内：摘出后转移 pending→cancelled，终态快照落盘；
//   - 执行中：登记 cancelRequested 并取消上下文，由 handleInterrupted 收尾认领
//     （转移 running→cancelled）。job.cancelled 事件因此晚于本调用返回——它对应
//     "确实停住了"这个事实，与 ForcePause 的认领语义同一取向；
//   - 暂停中：快照转移 paused→cancelled。
//
// 三条路径都保留终态留痕（受 store.history_limit/TTL 淘汰策略管理，Restore 跳过终态
// 不会复活）：可审计要求"这条任务存在过且被取消"在存储里可查。
// 行为变化见 docs/design/reliable-execution-design.md §11.3：取消后 GET /jobs/:id
// 返回 cancelled 记录而非 404。
//
// 已知边界（与改动前同窗，未扩大）：任务刚被调度循环弹出、还没登记进 cancelMap 的
// 瞬间，堆内与执行中两条都查不到，快照也还是 pending——按不存在处理返回
// ErrJobNotFound。要收紧这个窗口需要"弹出中"的中间态，收益撑不起复杂度。
func (s *Scheduler) Cancel(jobID string) error {
	if item := s.heap.Remove(jobID); item != nil {
		job := item.(*Job)
		s.transition(job, StatusCancelled, Event{
			Type:      EventJobCancelled,
			JobID:     jobID,
			JobName:   job.Name,
			Status:    StatusCancelled,
			Timestamp: time.Now(),
		})
		return nil
	}

	// 执行中：登记必须在取消之前——反过来 Handler 可能在登记前就返回了，
	// 收尾认领不到标记，取消请求就此丢失。
	s.mu.Lock()
	cancel, running := s.cancelMap[jobID]
	if running {
		s.cancelRequested[jobID] = struct{}{}
	}
	s.mu.Unlock()
	if running {
		cancel()
		return nil
	}

	// 暂停中的任务既不在堆里也不在执行中，但它确实存在且必须还能被取消——
	// 否则一旦暂停就再也清理不掉，存储里只会长出删不掉的僵尸快照。
	if snap, ok := s.findSnapshot(jobID); ok && JobStatus(snap.Status) == StatusPaused {
		job := &Job{}
		job.FromSnapshot(snap)
		s.transition(job, StatusCancelled, Event{
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

// pausePendingJob 把一个刚从堆里摘下的任务落成 paused：转移 + 落盘 + 唤醒循环 + 广播。
// forced 只影响事件里的标记位，状态迁移两条路径完全一致。
// 入参来自堆（或 ForcePause 的堆内分支），状态必为 pending，pending→paused 是合法边。
func (s *Scheduler) pausePendingJob(job *Job, forced bool) (*Job, error) {
	s.transition(job, StatusPaused, Event{
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

	// 堆顶可能变了，唤醒调度循环重算等待时长
	s.notifyNewJob()

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
	// 让时间线能把"恢复"这个人为动作与自动重排区分开。这是不带状态写的事件，
	// 走 emit 收口（TASK-RE01）。
	s.emit(Event{
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
	// 落盘走 transition 收口（TASK-RE01），不发事件——job.paused 由收尾的
	// parkForcedPause 统一发，这里先发一条等于说"已经停稳了"，与事实不符。
	//
	// 只在真的取到快照时才改写它：纯内存部署（store 为 nil）没有可写的记录，
	// 而"store 非空却查不到"的极端竞态下用零值快照覆盖，会把任务内容抹平。
	var job *Job
	if snap, found := s.findSnapshot(jobID); found {
		job = &Job{}
		job.FromSnapshot(snap)
		// 崩溃快照/执行中的任务此刻状态是 running，running→paused 是合法边。
		s.transition(job, StatusPaused)
	} else {
		job = &Job{ID: jobID, Status: StatusPaused, UpdatedAt: time.Now()}
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

// takeCancelRequested 消费一个取消请求标记（一次性），返回它是否存在（TASK-RE01）。
// 与 takeForcedPause 同形：标记由执行收尾路径认领，谁先回来谁负责落终态。
// 三条收尾路径（handleInterrupted/handleSuccess/handleFailure）都必须消费它——
// Handler 不响应取消而照常收尾时，标记不能泄漏成"永远待认领"。
func (s *Scheduler) takeCancelRequested(jobID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.cancelRequested[jobID]; !ok {
		return false
	}
	delete(s.cancelRequested, jobID)
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
//
// 认领顺序的锚点：本函数先于取消标记检查执行（各收尾路径的第一行），
// "强制暂停优先于一切"——用户已经要求停住等技术员确认，同一时刻到达的
// 取消请求不得改写结果。
func (s *Scheduler) parkForcedPause(job *Job, note string) bool {
	if !s.takeForcedPause(job.ID) {
		return false
	}

	// 收尾时任务状态是 running，running→paused 是合法边。
	s.transition(job, StatusPaused, Event{
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

	// 内容更新不改状态（pending→pending 的 identity），不经 transition；
	// 落盘走 persistAndEmit 收口（TASK-RE01）。落盘失败维持既有口径：
	// 只记日志，接口照常返回成功——堆里已是新内容，重启后由 Restore 对齐差异。
	_ = s.persistAndEmit(&updated)

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
		// 状态不变的纯内容更新，经 persistAndEmit 收口（TASK-RE01）；
		// 错误照旧向上传播（SetGroup 的 API 语义依赖它）。
		if err := s.persistAndEmit(&job); err != nil {
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
	// 非堆内路径改的是快照（暂停中/终态留痕），同样走收口；终态→终态的 identity
	// 不经 transition 校验（分组改挂与状态机无关）。
	job := &Job{}
	job.FromSnapshot(snap)
	if err := s.persistAndEmit(job); err != nil {
		return false, err
	}
	return true, nil
}

// PauseByHandlerKey 把某个任务类型下所有待执行的任务置为 paused，返回受影响的条数。
//
// 形状抄 RetagGroup：遍历存储快照 → 按键匹配 → 逐条处置 → 返回条数；
// 非原子、幂等，中途单条失败记日志后继续，调用方重试即可。
// 差别有两处：匹配的是 HandlerKey()（分组是用户标签，这里是注册键），
// 且它只处理待执行的那批——正在执行的任务一条都不动。
//
// 不动运行中任务是刻意的（docs/design/web-profile-design.md D6）：删除档位不该附带
// "替用户中止一次已经发生的副作用"，中止走 ForcePause（admin 档）。
// 因此返回值里不含它们，调用方不能把条数当成"这个类型的任务都停了"。
//
// 逐条处置复用 Pause，因而与页面单个暂停同一条路径：从堆里摘出、落成 paused、
// 唤醒调度循环、发 EventJobPaused。Pause 的两个哨兵错误在这里都是正常分支——
// ErrJobNotPending 表示那条正在执行，ErrJobNotFound 表示它已不在堆里也不在暂停名单上
// （通常是刚被调度循环弹出进入执行，或已经被别的调用送走），两者都不计入条数、
// 也不记错误日志。
//
// 只认存储：store 为 nil（纯内存部署）返回 (0, nil)，与 RetagGroup 同一条。
// 档位在线管理本来就要求落盘（web_enabled 的写入侧全部先写文件），
// 没有存储时这个动作没有可遍历的对象，也就没有意义。
func (s *Scheduler) PauseByHandlerKey(handlerKey string) (int, error) {
	if s.store == nil || strings.TrimSpace(handlerKey) == "" {
		return 0, nil
	}
	snapshots, err := s.store.LoadAll()
	if err != nil {
		return 0, fmt.Errorf("load jobs for handler key pause failed: %w", err)
	}

	paused := 0
	for _, snap := range snapshots {
		if snap.HandlerKey() != handlerKey {
			continue
		}
		// paused 与终态先跳过，重复调用因此返回 0；running 不在这里拦，
		// 交给 Pause 按当前堆与 worker 的实际状态判定。
		switch JobStatus(snap.Status) {
		case StatusPaused, StatusSuccess, StatusFailed, StatusCancelled:
			continue
		}

		if _, err := s.Pause(snap.ID); err != nil {
			if errors.Is(err, ErrJobNotPending) || errors.Is(err, ErrJobNotFound) {
				continue
			}
			s.logger.Error("failed to pause job for handler key",
				"job_id", snap.ID, "handler_key", handlerKey, "error", err)
			continue
		}
		paused++
	}
	return paused, nil
}
