package core

import (
	"context"
	"time"
)

// JobStatus 任务状态
type JobStatus int

const (
	StatusPending JobStatus = iota
	StatusRunning
	StatusSuccess
	StatusFailed
	StatusCancelled
)

// Job 延迟任务结构
type Job struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Payload   []byte    `json:"payload"`    // 任务数据
	TriggerAt time.Time `json:"trigger_at"` // 下次触发时间

	// 执行配置
	Handler Handler         `json:"-"` // 处理函数（不持久化）
	Ctx     context.Context `json:"-"` // 上下文（不持久化）

	// Cron 重复任务支持
	CronExpr string `json:"cron_expr,omitempty"` // Cron表达式，空表示一次性任务
	IsRepeat bool   `json:"is_repeat"`

	// 重试配置
	MaxRetries int           `json:"max_retries"`
	RetryCount int           `json:"retry_count"`
	RetryDelay time.Duration `json:"retry_delay"` // 基础退避时间

	// 状态
	Status    JobStatus `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// 元数据
	Attempts int `json:"attempts"` // 总尝试次数（含重试）
}

// Handler 任务处理函数
type Handler func(ctx context.Context, job *Job) error

// GetTriggerTime 实现 Item 接口
func (j *Job) GetTriggerTime() time.Time {
	return j.TriggerAt
}

// GetID 实现 Item 接口
func (j *Job) GetID() string {
	return j.ID
}

// CloneForRetry 创建重试副本
func (j *Job) CloneForRetry(nextTime time.Time) *Job {
	return &Job{
		ID:         j.ID + "_retry_" + time.Now().Format("20060102150405"),
		Name:       j.Name,
		Payload:    j.Payload,
		TriggerAt:  nextTime,
		Handler:    j.Handler,
		CronExpr:   j.CronExpr,
		IsRepeat:   j.IsRepeat,
		MaxRetries: j.MaxRetries,
		RetryCount: j.RetryCount + 1,
		RetryDelay: j.RetryDelay * 2, // 指数退避
		Status:     StatusPending,
		CreatedAt:  j.CreatedAt,
		UpdatedAt:  time.Now(),
	}
}

// JobSnapshot 用于持久化的任务快照（不含函数指针）
type JobSnapshot struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Payload    []byte    `json:"payload"`
	TriggerAt  time.Time `json:"trigger_at"`
	CronExpr   string    `json:"cron_expr"`
	IsRepeat   bool      `json:"is_repeat"`
	MaxRetries int       `json:"max_retries"`
	RetryCount int       `json:"retry_count"`
	RetryDelay int64     `json:"retry_delay"` // nanoseconds
	Status     int       `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	Attempts   int       `json:"attempts"`
}

// ToSnapshot 转换为可持久化格式
func (j *Job) ToSnapshot() JobSnapshot {
	return JobSnapshot{
		ID:         j.ID,
		Name:       j.Name,
		Payload:    j.Payload,
		TriggerAt:  j.TriggerAt,
		CronExpr:   j.CronExpr,
		IsRepeat:   j.IsRepeat,
		MaxRetries: j.MaxRetries,
		RetryCount: j.RetryCount,
		RetryDelay: int64(j.RetryDelay),
		Status:     int(j.Status),
		CreatedAt:  j.CreatedAt,
		UpdatedAt:  j.UpdatedAt,
		Attempts:   j.Attempts,
	}
}

// FromSnapshot 从快照恢复（需重新注册Handler）
func (j *Job) FromSnapshot(s JobSnapshot) {
	j.ID = s.ID
	j.Name = s.Name
	j.Payload = s.Payload
	j.TriggerAt = s.TriggerAt
	j.CronExpr = s.CronExpr
	j.IsRepeat = s.IsRepeat
	j.MaxRetries = s.MaxRetries
	j.RetryCount = s.RetryCount
	j.RetryDelay = time.Duration(s.RetryDelay)
	j.Status = JobStatus(s.Status)
	j.CreatedAt = s.CreatedAt
	j.UpdatedAt = s.UpdatedAt
	j.Attempts = s.Attempts
	j.Status = StatusPending // 恢复后重置为待处理
}
