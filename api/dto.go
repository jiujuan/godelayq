package api

import (
	"encoding/json"
	"time"
)

// CreateJobRequest 创建任务请求
type CreateJobRequest struct {
	// 任务标识
	Name string `json:"name" binding:"required" example:"payment_check"`

	// 执行时间配置（三选一）
	Delay     string     `json:"delay,omitempty" example:"10m"`               // 相对延迟，如 "10m", "1h30s"
	TriggerAt *time.Time `json:"trigger_at,omitempty" format:"date-time"`     // 绝对时间
	CronExpr  string     `json:"cron_expr,omitempty" example:"0 */5 * * * *"` // Cron表达式（秒级）

	// 任务内容
	Payload  json.RawMessage `json:"payload,omitempty" swaggertype:"object"` // 任务数据
	IsRepeat bool            `json:"is_repeat,omitempty"`                    // 是否重复执行（Cron任务）

	// 重试策略
	MaxRetries int    `json:"max_retries,omitempty" example:"3"`   // 最大重试次数
	RetryDelay string `json:"retry_delay,omitempty" example:"30s"` // 重试间隔
}

// UpdateJobRequest 更新任务请求（仅支持修改未执行的任务）
type UpdateJobRequest struct {
	TriggerAt  *time.Time      `json:"trigger_at,omitempty"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	MaxRetries *int            `json:"max_retries,omitempty"`
}

// JobResponse 任务响应
type JobResponse struct {
	ID        string          `json:"id" example:"job_1704182400_abc123"`
	Name      string          `json:"name" example:"payment_check"`
	Status    string          `json:"status" example:"pending" enums:"pending,running,success,failed,cancelled"`
	TriggerAt time.Time       `json:"trigger_at" format:"date-time"`
	Payload   json.RawMessage `json:"payload,omitempty"`

	// 执行信息
	RetryCount int    `json:"retry_count" example:"0"`
	MaxRetries int    `json:"max_retries" example:"3"`
	IsRepeat   bool   `json:"is_repeat" example:"false"`
	CronExpr   string `json:"cron_expr,omitempty"`

	// 时间戳
	CreatedAt time.Time `json:"created_at" format:"date-time"`
	UpdatedAt time.Time `json:"updated_at" format:"date-time"`

	// 计算字段
	NextRunIn string `json:"next_run_in,omitempty" example:"5m30s"` //  human readable
}

// ListJobsResponse 任务列表响应
type ListJobsResponse struct {
	Total int           `json:"total" example:"100"`
	Items []JobResponse `json:"items"`
}

// StatsResponse 统计信息
type StatsResponse struct {
	Pending   int `json:"pending" example:"10"`   // 等待中
	Running   int `json:"running" example:"2"`    // 执行中
	Completed int `json:"completed" example:"50"` // 已完成
	Failed    int `json:"failed" example:"3"`     // 失败

	// 堆信息
	HeapSize int `json:"heap_size" example:"10"`

	// 系统信息
	Uptime string `json:"uptime" example:"24h30m"`
}

// ErrorResponse 错误响应
type ErrorResponse struct {
	Code    int    `json:"code" example:"400"`
	Message string `json:"message" example:"invalid request parameters"`
	Details string `json:"details,omitempty"`
}
