package core

import (
	"context"
	"strings"
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

// String 返回状态的规范名，与 HTTP API 的 status 取值一致。
func (s JobStatus) String() string {
	switch s {
	case StatusPending:
		return "pending"
	case StatusRunning:
		return "running"
	case StatusSuccess:
		return "success"
	case StatusFailed:
		return "failed"
	case StatusCancelled:
		return "cancelled"
	default:
		return "unknown"
	}
}

// ParseJobStatus 按规范名解析状态（大小写不敏感），未知名称返回 false。
func ParseJobStatus(name string) (JobStatus, bool) {
	for _, status := range []JobStatus{StatusPending, StatusRunning, StatusSuccess, StatusFailed, StatusCancelled} {
		if strings.EqualFold(status.String(), name) {
			return status, true
		}
	}
	return 0, false
}

// IsTerminal 表示任务已结束：既不会被调度，也不应被崩溃恢复重新入队。
func (s JobStatus) IsTerminal() bool {
	return s == StatusSuccess || s == StatusFailed || s == StatusCancelled
}

// Job 延迟任务结构
type Job struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Type      string    `json:"type,omitempty"` // Handler 注册键；为空时回退到 Name
	Payload   []byte    `json:"payload"`        // 任务数据
	TriggerAt time.Time `json:"trigger_at"`     // 下次触发时间

	// 执行配置
	Handler Handler         `json:"-"` // 处理函数（不持久化）
	Ctx     context.Context `json:"-"` // 上下文（不持久化）

	// Timeout 单次执行的超时时间，0 表示不限制。
	// Handler 必须检查 ctx，否则超时只能被观测、无法中断其执行。
	Timeout time.Duration `json:"timeout"`

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

// HandlerKey 返回查找注册 Handler 的键：优先 Type，为空时回退 Name
func (j *Job) HandlerKey() string {
	if j.Type != "" {
		return j.Type
	}
	return j.Name
}

// CloneForRetry 创建重试副本。
// 保留原 ID 以维持任务链（查询/取消/存储清理按同一 ID）；
// 清除 Cron 属性，避免一次性任务失败重试后被误当作周期任务重新排期。
func (j *Job) CloneForRetry(nextTime time.Time) *Job {
	return &Job{
		ID:         j.ID,
		Name:       j.Name,
		Type:       j.Type,
		Payload:    j.Payload,
		TriggerAt:  nextTime,
		Handler:    j.Handler,
		Timeout:    j.Timeout,
		CronExpr:   "",
		IsRepeat:   false,
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
	Type       string    `json:"type,omitempty"`
	Payload    []byte    `json:"payload"`
	TriggerAt  time.Time `json:"trigger_at"`
	CronExpr   string    `json:"cron_expr"`
	IsRepeat   bool      `json:"is_repeat"`
	Timeout    int64     `json:"timeout"` // nanoseconds
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
		Type:       j.Type,
		Payload:    j.Payload,
		TriggerAt:  j.TriggerAt,
		CronExpr:   j.CronExpr,
		IsRepeat:   j.IsRepeat,
		Timeout:    int64(j.Timeout),
		MaxRetries: j.MaxRetries,
		RetryCount: j.RetryCount,
		RetryDelay: int64(j.RetryDelay),
		Status:     int(j.Status),
		CreatedAt:  j.CreatedAt,
		UpdatedAt:  j.UpdatedAt,
		Attempts:   j.Attempts,
	}
}

// FromSnapshot 从快照恢复（需重新注册Handler）。状态按快照原样还原：
// 崩溃恢复时由 Scheduler.Restore 显式把未完成任务复位为待处理。
func (j *Job) FromSnapshot(s JobSnapshot) {
	j.ID = s.ID
	j.Name = s.Name
	j.Type = s.Type
	j.Payload = s.Payload
	j.TriggerAt = s.TriggerAt
	j.CronExpr = s.CronExpr
	j.IsRepeat = s.IsRepeat
	j.Timeout = time.Duration(s.Timeout)
	j.MaxRetries = s.MaxRetries
	j.RetryCount = s.RetryCount
	j.RetryDelay = time.Duration(s.RetryDelay)
	j.Status = JobStatus(s.Status)
	j.CreatedAt = s.CreatedAt
	j.UpdatedAt = s.UpdatedAt
	j.Attempts = s.Attempts
}
