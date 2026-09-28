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

	// Group 分组标签，可留空。不要求分组已在 /groups 注册（见 docs/design/web-console-design.md §5.3），
	// 取值受 core 的名称规则约束，因为它会出现在 URL 查询参数里。
	Group string `json:"group,omitempty" example:"nightly"`

	// 单次执行超时，如 "30s"；为空表示不限制（Handler 需检查 ctx 才能被中止）
	Timeout string `json:"timeout,omitempty" example:"30s"`

	// 重试策略
	MaxRetries int    `json:"max_retries,omitempty" example:"3"`   // 最大重试次数
	RetryDelay string `json:"retry_delay,omitempty" example:"30s"` // 重试间隔
}

// UpdateJobRequest 更新任务请求（仅支持修改未执行的任务）
type UpdateJobRequest struct {
	TriggerAt  *time.Time      `json:"trigger_at,omitempty"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	MaxRetries *int            `json:"max_retries,omitempty"`
	// Timeout 单次执行超时，如 "30s"；传 "0s" 可取消限制
	Timeout string `json:"timeout,omitempty"`
	// Group 分组标签。用指针区分"没传"与"传了空串"：
	// 后者是明确的取消分组动作，不能当成前者忽略掉。
	Group *string `json:"group,omitempty" example:"nightly"`
}

// BatchCreateJobsResponse POST /jobs/batch 的混合结果。
type BatchCreateJobsResponse struct {
	Succeeded int              `json:"succeeded" example:"2"`
	Failed    int              `json:"failed" example:"1"`
	Items     []JobResponse    `json:"items"`
	Errors    []BatchItemError `json:"errors"`
}

// BatchItemError 批量创建里单条失败的原因，Index 指向请求数组的下标。
type BatchItemError struct {
	Index   int    `json:"index" example:"1"`
	Code    int    `json:"code" example:"400"`
	Message string `json:"message" example:"unknown job type"`
	Details string `json:"details,omitempty" example:"job type 'nope' not registered"`
}

// JobResponse 任务响应
type JobResponse struct {
	ID        string          `json:"id" example:"0198a2e3-7d4f-7abc-9def-0123456789ab"`
	Name      string          `json:"name" example:"payment_check"`
	Status    string          `json:"status" example:"pending" enums:"pending,running,success,failed,cancelled,paused"`
	Group     string          `json:"group,omitempty" example:"nightly"`
	TriggerAt time.Time       `json:"trigger_at" format:"date-time"`
	Payload   json.RawMessage `json:"payload,omitempty"`

	// 执行信息
	RetryCount int    `json:"retry_count" example:"0"`
	MaxRetries int    `json:"max_retries" example:"3"`
	Timeout    string `json:"timeout,omitempty" example:"30s"` // 单次执行超时，空表示不限制
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
	Paused    int `json:"paused" example:"1"`     // 暂停中（含被强制暂停）
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

// LoginRequest 控制台登录请求
type LoginRequest struct {
	Username string `json:"username" binding:"required" example:"admin01"`
	Password string `json:"password" binding:"required" example:"correct horse battery staple"`
}

// RefreshRequest 用 refresh token 换新的访问令牌
type RefreshRequest struct {
	RefreshToken string `json:"refresh_token" binding:"required"`
}

// LogoutRequest 登出。带上 refresh_token 以便服务端一并吊销。
type LogoutRequest struct {
	RefreshToken string `json:"refresh_token,omitempty"`
}

// UserInfoResponse 当前身份，角色名与 core.Role.String 一致
type UserInfoResponse struct {
	Name string `json:"name" example:"admin01"`
	Role string `json:"role" example:"admin" enums:"viewer,operator,admin,ops"`
}

// TokenSessionResponse 一次登录或刷新的产物
type TokenSessionResponse struct {
	AccessToken  string           `json:"access_token"`
	RefreshToken string           `json:"refresh_token"`
	TokenType    string           `json:"token_type" example:"Bearer"`
	ExpiresAt    time.Time        `json:"expires_at" format:"date-time"`
	User         UserInfoResponse `json:"user"`
}

// WhoAmIResponse GET /auth/me：身份 + 当前令牌的过期时间
type WhoAmIResponse struct {
	UserInfoResponse
	ExpiresAt time.Time `json:"expires_at" format:"date-time"`
}

// WsTicketResponse 一次性实时通道票据
type WsTicketResponse struct {
	Ticket string `json:"ticket"`
	// ExpiresInSeconds 票据有效期，前端据此在过期前重新申领
	ExpiresInSeconds int64 `json:"expires_in_seconds" example:"5"`
}
