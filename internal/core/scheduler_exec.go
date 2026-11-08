// scheduler_exec.go 承载 worker 协程内的一次任务执行与收尾：
// executeJob 的执行流程（Handler 绑定、取消上下文、超时）、
// 事件数据组装（eventData/eventResult），以及按结果分派的收尾
// （handleSuccess/handleFailure/handleInterrupted/markFailed）。
package core

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// 执行任务（由 worker 协程调用，阻塞直到 Handler 返回）
func (s *Scheduler) executeJob(job *Job) {
	// 在途计数按池分开：统计接口要能说出"执行器池正在跑几个"，
	// 而既有的 running 字段不能因为新增一个池就读成别的含义。
	// 执行器任务落进默认池（没建池）时仍计入 running，与改动前一致。
	counter := &s.inFlight
	if job.class == JobClassExec && s.execPoolEnabled() {
		counter = &s.execInFlight
	}
	counter.Add(1)
	defer counter.Add(-1)

	// 发布开始事件（不带状态写，走 emit 收口；metadata.attempt 的口径 RE02 才修）
	s.emit(Event{
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
			// 派发期失败：任务从未开始执行就注定终态，pending→failed 是转移表里
			// 惟一的 pending→failed 边（TASK-RE01）。与现状一致：不写 running 快照、
			// 不递增 Attempts，直接落失败终态；失败事件随转移一起收口发出。
			errData, _ := json.Marshal(map[string]string{"error": "no handler registered"})
			s.transition(job, StatusFailed, Event{
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

	// 运行态落盘经 transition 收口（TASK-RE01）：pending→running。
	// Attempts 先自增再落盘（快照带本次尝试号）；崩溃后重启能从快照看出
	// 哪些任务当时在执行（由 Restore 复位为待执行），统计接口也不再依赖进程内状态。
	job.Attempts++
	s.transition(job, StatusRunning)

	err := job.Handler(execCtx, job)

	if err == nil {
		// 发布成功事件（不带状态写，走 emit 收口；终态落盘在 handleSuccess 的 transition 里）
		s.emit(Event{
			Type:      EventJobCompleted,
			JobID:     job.ID,
			JobName:   job.Name,
			Status:    StatusSuccess,
			Timestamp: time.Now(),
			Data:      s.eventData(job, nil),
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
	permanent := isPermanentFailure(err)

	// 失败事件的 metadata：retry_count/max_retries/timeout 是既有形状，
	// permanent 只在"重试没有意义"时加上（TASK-E12 §3.4）。
	// 只加真值不加假值是为了让既有事件的 JSON 形状一字不变；
	// api/history.go 与前端把这个键当透传字段，不解析。
	failureMetadata := map[string]interface{}{
		"retry_count": job.RetryCount,
		"max_retries": job.MaxRetries,
		"timeout":     timedOut,
	}
	if permanent {
		failureMetadata["permanent"] = true
	}

	// 发布失败事件（不带状态写，走 emit 收口；终态落盘在 handleFailure 的 transition 里）
	s.emit(Event{
		Type:      EventJobFailed,
		JobID:     job.ID,
		JobName:   job.Name,
		Status:    StatusFailed,
		Timestamp: time.Now(),
		Data:      s.eventData(job, err),
		Metadata:  failureMetadata,
	})

	if timedOut {
		s.logger.Error("job timed out", "job_id", job.ID, "timeout", job.Timeout, "error", err)
	} else {
		s.logger.Error("job failed", "job_id", job.ID, "error", err)
	}
	s.handleFailure(job, err)
}

// eventData 组装事件的附加数据：失败时带 error，有执行结论时带 result，
// 两者都没有则返回 nil（事件的 data 字段是 omitempty，于是整个键不出现在 JSON 里）。
// 只带 error 时的输出与改动前逐字节一致——既有测试断言的就是那份形状。
func (s *Scheduler) eventData(job *Job, failure error) json.RawMessage {
	fields := make(map[string]interface{}, 2)
	if failure != nil {
		fields["error"] = failure.Error()
	}
	if summary, ok := s.eventResult(job); ok {
		fields["result"] = summary
	}
	if len(fields) == 0 {
		return nil
	}

	// ExecMeta 的字段全是可以编码的标量，这里不会失败；忽略错误与既有事件发布处一致。
	data, _ := json.Marshal(fields)
	return data
}

// eventResult 取出要放进事件的执行结论摘要。第二个返回值为 false 表示这次执行没有结论：
// 不是执行器任务，或处理器没写 Job.Exec。
//
// 预览按 eventPreviewLimit 再裁一次：摘要落盘之后配置可能已经改小，而一条事件会推给
// 全部订阅者并在内存缓冲里留着。裁剪前先复制——同一个 ExecMeta 对象也被任务快照引用，
// 直接改它会改到接口读到的内容。
func (s *Scheduler) eventResult(job *Job) (ExecMeta, bool) {
	if job.Exec == nil {
		return ExecMeta{}, false
	}

	s.mu.RLock()
	limit := s.eventPreviewLimit
	s.mu.RUnlock()

	summary := *job.Exec
	if limit > 0 && len(summary.Preview) > limit {
		summary.Preview = TrimExecPreview(summary.Preview, limit)
	}
	return summary, true
}

// handleInterrupted 处理"执行被打断"：既不记为失败，也不消耗重试次数。
// 三条出路按优先级（TASK-RE01 的认领顺序锚定在 parkForcedPause）：
//  1. 强制暂停标记 → 停 paused（用户要求停住等人确认，优先于一切）；
//  2. 取消请求标记 → 落 cancelled 终态（Cancel 的收尾认领：事件与快照对应
//     "确实停住了"，Cancel 调用侧不再立即落盘）；
//  3. 调度器已停止 → 落回 pending，重启后照常重跑（优雅关停路径，至少一次语义，
//     副作用可能重复，Handler 需自行保证幂等）。
// 调度器还在跑、又没有任何标记：只有绕过 Cancel 直调 CancelRunning 才会走到这里，
// 维持既有口径——只记日志，快照保持 running，交给重启后的 Restore 处置。
func (s *Scheduler) handleInterrupted(job *Job) {
	// 强制暂停优先于关停恢复：否则"取消上下文"会被当成关停打断，
	// 任务被复位成 pending 重新排期，暂停请求当场失效。
	if s.parkForcedPause(job, "interrupted") {
		return
	}

	// 用户 Cancel 的收尾认领：running→cancelled（TASK-RE01）。
	if s.takeCancelRequested(job.ID) {
		s.transition(job, StatusCancelled, Event{
			Type:      EventJobCancelled,
			JobID:     job.ID,
			JobName:   job.Name,
			Status:    StatusCancelled,
			Timestamp: time.Now(),
			Metadata:  map[string]interface{}{"reason": "cancel"},
		})
		s.logger.Info("job cancelled", "job_id", job.ID)
		return
	}

	s.mu.RLock()
	running := s.running
	s.mu.RUnlock()

	if running {
		s.logger.Info("job execution cancelled", "job_id", job.ID)
		return
	}

	s.transition(job, StatusPending, Event{
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

	// 取消请求认领（TASK-RE01）：Handler 不响应取消而正常返回时，工作已经做完，
	// 结果为真就按成功落终态——已完成的事不能因为"取消来得晚"被说成 cancelled。
	// 标记必须消费掉，否则泄漏成永远待认领。
	if s.takeCancelRequested(job.ID) {
		s.logger.Info("cancel requested but the handler completed; recording success", "job_id", job.ID)
	}

	// 如果是重复任务，计算下次执行时间并重新入队。
	// 新一轮是同 ID 的新对象，经 Schedule 以 pending 落盘——上一轮 running→pending
	// 的转移因此不经 transition（新对象上无从取 from），前置事实是"本轮已成功"，
	// 例外已被 parkForcedPause 认领与 cancelRequested 消费兜住。
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

	// 终态留痕：completed 事件已在 executeJob 发布，这里只转移 + 落快照（无事件）。
	// 是否长期保留由存储保留策略决定。cron 分支计算失败也会落到这里，
	// 与改动前"落成功快照"的兜底一致。
	s.transition(job, StatusSuccess)
}

// 处理失败与重试。
//
// 四个分支的先后顺序就是判定口径（TASK-E12 §3.3 + TASK-RE01 的取消认领），改之前先读理由：
//  1. 强制暂停在最前：用户已经要求把这个任务停在 paused 上等技术员确认，
//     此时错误是永久还是可重试都不该改变结果——既不消耗重试次数，也不再排期。
//     parkForcedPause 认领中止标记的语义也依赖它先执行，后移会让暂停当场失效。
//  2. 取消请求其次（TASK-RE01）：Handler 不响应取消而自己报了错——按用户意图停住，
//     落 cancelled 终态，不再消耗重试次数、不再重新排期。失败结论已经在
//     executeJob 的 job.failed 事件里，时间线能看到完整经过。
//  3. 永久失败再次：参数写错、命令不存在这类问题，重跑只会把同一个错误再产生一遍，
//     还白占执行名额（拼错的解释器名配 max_retries: 5 就是五条同样无用的事件）。
//  4. 最后才是既有的"按 RetryCount 与 MaxRetries 决定重试还是落终态"。
//
// err 是处理函数返回的原始错误：判定要读它的 Permanent()，而 job 里没有它。
func (s *Scheduler) handleFailure(job *Job, err error) {
	// 同 handleSuccess：中止期间 Handler 自己报了错，也按用户意图停在 paused，
	// 不再消耗重试次数、不再重新排期。
	if s.parkForcedPause(job, "handler_failed_despite_cancel") {
		return
	}

	if s.takeCancelRequested(job.ID) {
		s.logger.Info("cancel requested and the handler failed; job stops as cancelled", "job_id", job.ID)
		s.transition(job, StatusCancelled, Event{
			Type:      EventJobCancelled,
			JobID:     job.ID,
			JobName:   job.Name,
			Status:    StatusCancelled,
			Timestamp: time.Now(),
			Metadata:  map[string]interface{}{"reason": "cancel"},
		})
		return
	}

	if isPermanentFailure(err) {
		s.markFailed(job)
		return
	}

	if job.RetryCount < job.MaxRetries {
		// 计算下次重试时间：只 Load 一次，算出的时刻同时用于发事件与排期，
		// 否则中间插进一次 SetRetryPolicy 就会让两条记录给出不同的重试时刻
		nextTime := (*s.retryPolicy.Load()).NextRetry(job)

		// 发布重试事件。它不带状态写（重试是"新对象重新 Schedule"），走 emit 收口；
		// 排期本身的落盘在下面 Schedule 的 persistAndEmit 里。
		s.emit(Event{
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
		s.markFailed(job)
	}
}

// markFailed 把任务落成失败终态并落盘。
// 重试耗尽与永久失败共用这一份收尾逻辑：状态仍是 StatusFailed，本卡不新增 JobStatus（§8）。
func (s *Scheduler) markFailed(job *Job) {
	// 收尾时任务状态是 running，running→failed 是合法边。
	s.transition(job, StatusFailed)
}
