package core

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"
)

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

	// 任务注册表（用于从持久化恢复时绑定Handler）
	handlers map[string]Handler

	// 取消控制
	cancelMap map[string]context.CancelFunc

	eventBus *EventBus // 新增
}

// NewScheduler 创建调度器
func NewScheduler(store Store, retryPolicy RetryPolicy, eventBus *EventBus) *Scheduler {
	if retryPolicy == nil {
		retryPolicy = &ExponentialBackoffRetry{}
	}

	if eventBus == nil {
		eventBus = NewEventBus(100) // 默认事件总线
	}

	return &Scheduler{
		heap:        NewQuaternaryHeap(),
		store:       store,
		retryPolicy: retryPolicy,
		cronParser:  NewCronParser(),
		stopCh:      make(chan struct{}),
		newJobCh:    make(chan struct{}, 1),
		handlers:    make(map[string]Handler),
		cancelMap:   make(map[string]context.CancelFunc),
		eventBus:    eventBus,
	}
}

// RegisterHandler 注册任务类型对应的处理函数
func (s *Scheduler) RegisterHandler(jobType string, handler Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[jobType] = handler
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
			log.Printf("Failed to persist job %s: %v", job.ID, err)
		}
	}

	// 通知调度循环可能有更早的任务
	select {
	case s.newJobCh <- struct{}{}:
	default:
	}

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

// Cancel 取消任务
func (s *Scheduler) Cancel(jobID string) error {
	job := s.heap.Remove(jobID)
	if job != nil {
		j := job.(*Job)
		j.Status = StatusCancelled
		if s.store != nil {
			s.store.Delete(j.ID)
		}
		// 取消正在执行的上下文
		if cancel, ok := s.cancelMap[jobID]; ok {
			cancel()
		}

		// Cancel 中发布取消事件
		s.eventBus.Publish(Event{
			Type:      EventJobCancelled,
			JobID:     jobID,
			Status:    StatusCancelled,
			Timestamp: time.Now(),
		})

		return nil
	}
	return ErrJobNotFound
}

// Start 启动调度器
func (s *Scheduler) Start() {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	s.mu.Unlock()

	s.wg.Add(1)
	go s.scheduleLoop()
}

// Stop 停止调度器
func (s *Scheduler) Stop() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	s.running = false
	close(s.stopCh)
	s.mu.Unlock()

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
		waitTime := job.TriggerAt.Sub(now)

		if waitTime <= 0 {
			// 任务到期，弹出执行
			s.heap.PopItem()
			s.executeJob(job)
			continue
		}

		// 等待直到触发时间或新任务插入
		timer := time.NewTimer(waitTime)
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

// 执行任务
func (s *Scheduler) executeJob(job *Job) {
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

	// 创建可取消的上下文
	ctx, cancel := context.WithCancel(context.Background())
	s.cancelMap[job.ID] = cancel
	defer delete(s.cancelMap, job.ID)

	// 恢复Handler（如果是从持久化加载的）
	if job.Handler == nil {
		if h, ok := s.handlers[job.Name]; ok {
			job.Handler = h
		} else {
			log.Printf("No handler registered for job %s", job.Name)
			job.Status = StatusFailed
			return
		}
	}

	job.Status = StatusRunning
	job.Attempts++
	job.UpdatedAt = time.Now()

	// 异步执行避免阻塞调度循环
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		err := job.Handler(ctx, job)

		if err != nil {
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
				},
			})

			log.Printf("Job %s failed: %v", job.ID, err)
			s.handleFailure(job)
		} else {
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
		}
	}()
}

// 处理成功
func (s *Scheduler) handleSuccess(job *Job) {
	job.Status = StatusSuccess

	// 如果是重复任务，计算下次执行时间并重新入队
	if job.IsRepeat && job.CronExpr != "" {
		next, err := s.cronParser.Next(job.CronExpr, time.Now())
		if err == nil {
			newJob := &Job{
				ID:         job.ID, // 保持相同ID会覆盖旧数据
				Name:       job.Name,
				Payload:    job.Payload,
				TriggerAt:  next,
				Handler:    job.Handler,
				CronExpr:   job.CronExpr,
				IsRepeat:   true,
				MaxRetries: job.MaxRetries,
				Status:     StatusPending,
				CreatedAt:  job.CreatedAt,
				UpdatedAt:  time.Now(),
			}
			s.Schedule(newJob)
			return
		}
	}

	// 清理存储
	if s.store != nil {
		s.store.Delete(job.ID)
	}
}

// 处理失败与重试
func (s *Scheduler) handleFailure(job *Job) {
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

		log.Printf("Scheduling retry for job %s at %v", job.ID, nextTime)
		s.Schedule(retryJob)
	} else {
		job.Status = StatusFailed
		if s.store != nil {
			s.store.Update(job.ToSnapshot())
		}
	}
}

// GetEventBus 暴露 EventBus（用于WebSocket订阅）
func (s *Scheduler) GetEventBus() *EventBus {
	return s.eventBus
}

func generateID() string {
	return time.Now().Format("20060102150405") + "-" + randomString(8)
}

func randomString(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[time.Now().UnixNano()%int64(len(letters))]
	}
	return string(b)
}

// HeapLen 获取堆中任务数量（用于监控）
func (s *Scheduler) HeapLen() int {
	return s.heap.Len()
}
