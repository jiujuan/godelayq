package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jiujuan/godelayq/internal/core"
)

// handlers_attempts.go 提供逐次尝试账本的读端点（TASK-RE02）：
// GET /jobs/:id/attempts 回答"这条任务每次尝试分别怎么样"——事件流有丢弃与
// 保留上限（观测口径），账本与任务快照同库淘汰（审计口径）。

// attemptReader 是账本的读取窄接口，定义在消费方（api）；由 store/jobs.Store
// 隐式实现。JSON 部署不注入：端点返回 ledger:false 的降级形状，而不是 503——
// "后端没这能力"与"任务不存在"是两件事，客户端按 ledger 标志决定是否展示账本页。
type attemptReader interface {
	Attempts(jobID string, limit int) ([]core.Attempt, error)
}

// WithAttemptReader 注入账本读取方（TASK-RE02）。
func WithAttemptReader(r attemptReader) Option {
	return func(s *Server) { s.attempts = r }
}

// attemptsQueryLimit 是 ?limit= 的上界：账本行按任务裁剪过（attempts_keep_last），
// 单任务本来就不过几十条，上界只防"limit=999999"这种笔误把整表读进内存。
const attemptsQueryLimit = 100

// AttemptResponse 是账本一行的 API 形状（设计文档 §8.2）。
// 可判定字段用指针：null（未判定）与零值（退出码 0、不允许重试）分得开。
type AttemptResponse struct {
	Seq           int        `json:"seq"`
	Status        string     `json:"status"`
	TriggerSource string     `json:"trigger_source"`
	StartedAt     time.Time  `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	DurationMs    int64      `json:"duration_ms"`
	// Class 是失败类别文本（TASK-RE04 词表），成功尝试为空。
	Class      string `json:"class,omitempty"`
	Retryable  *bool  `json:"retryable,omitempty"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	Signal     string `json:"signal,omitempty"`
	HTTPStatus *int   `json:"http_status,omitempty"`
	Error      string `json:"error,omitempty"`
}

// AttemptsResponse GET /jobs/:id/attempts 的响应体。
// Ledger=false 表示这次部署没有账本能力（JSON 存储），items 恒为空数组。
type AttemptsResponse struct {
	JobID  string            `json:"job_id"`
	Ledger bool              `json:"ledger"`
	// Detail 是降级说明，只在 ledger=false 时出现。
	Detail string            `json:"detail,omitempty"`
	Items  []AttemptResponse `json:"items"`
}

// GetJobAttempts GET /api/v1/jobs/:id/attempts
//
// 任务不存在与任务没跑过在这里不区分：账本按任务 ID 查，空数组两种含义都有
// （任务详情页先查 /jobs/:id 再看账本，语义由它组织）。
func (s *Server) GetJobAttempts(c *gin.Context) {
	id := c.Param("id")

	limit := core.DefaultAttemptsKeepLast
	if raw := c.Query("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > attemptsQueryLimit {
			c.JSON(http.StatusBadRequest, ErrorResponse{
				Code:    http.StatusBadRequest,
				Message: "invalid limit",
				Details: "limit must be an integer between 1 and 100",
			})
			return
		}
		limit = parsed
	}

	// 账本能力缺失：200 + ledger:false + 空 items（降级语义见 attemptReader 注释）
	if s.attempts == nil {
		c.JSON(http.StatusOK, AttemptsResponse{
			JobID:  id,
			Ledger: false,
			Detail: "attempt ledger requires store.type=sqlite|postgres",
			Items:  []AttemptResponse{},
		})
		return
	}

	records, err := s.attempts.Attempts(id, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Code:    http.StatusInternalServerError,
			Message: "failed to query attempts",
			Details: err.Error(),
		})
		return
	}

	items := make([]AttemptResponse, 0, len(records))
	for _, rec := range records {
		items = append(items, attemptResponseOf(rec))
	}
	c.JSON(http.StatusOK, AttemptsResponse{JobID: id, Ledger: true, Items: items})
}

// attemptResponseOf 把账本词汇折成 API 形状：Set 标记折叠成指针（null = 未判定）。
func attemptResponseOf(rec core.Attempt) AttemptResponse {
	resp := AttemptResponse{
		Seq:           rec.Seq,
		Status:        rec.Status.String(),
		TriggerSource: rec.Trigger.String(),
		StartedAt:     rec.StartedAt,
		DurationMs:    rec.DurationMs,
		Class:         rec.Class,
		Signal:        rec.Signal,
		Error:         rec.Error,
	}
	if !rec.FinishedAt.IsZero() {
		finished := rec.FinishedAt
		resp.FinishedAt = &finished
	}
	if rec.RetryableSet {
		retryable := rec.Retryable
		resp.Retryable = &retryable
	}
	if rec.ExitCodeSet {
		exitCode := rec.ExitCode
		resp.ExitCode = &exitCode
	}
	if rec.HTTPStatusSet {
		status := rec.HTTPStatus
		resp.HTTPStatus = &status
	}
	return resp
}
