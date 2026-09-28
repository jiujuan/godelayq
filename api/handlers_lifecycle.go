package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"godelayq/core"
)

// 任务生命周期批量操作的动作名。与单条端点共用同一套调度器方法，
// 批量只是循环调用，不做事务（见 docs/design/web-console-design.md §5.5）。
const (
	jobOpCancel     = "cancel"
	jobOpPause      = "pause"
	jobOpForcePause = "force-pause"
	jobOpResume     = "resume"
	jobOpMove       = "move"
)

// maxBatchOpsSize 与批量创建同一量级：一次点击不该能提交半个任务表。
const maxBatchOpsSize = 100

// PauseJob POST /api/v1/jobs/:id/pause
// 暂停一个待执行任务：任务与历史都保留，只是不再排期（区别于 DELETE 的删除）。
func (s *Server) PauseJob(c *gin.Context) {
	job, err := s.scheduler.Pause(c.Param("id"))
	s.respondJobOp(c, jobOpPause, job, err)
}

// ForcePauseJob POST /api/v1/jobs/:id/force-pause
// 中止正在执行的一次尝试并停在 paused：不计失败、不消耗重试次数（admin 档）。
// 返回 200 表示"中止已发起"，任务真正停在 paused 由执行收尾完成。
func (s *Server) ForcePauseJob(c *gin.Context) {
	job, err := s.scheduler.ForcePause(c.Param("id"))
	s.respondJobOp(c, jobOpForcePause, job, err)
}

// ResumeJob POST /api/v1/jobs/:id/resume
// 恢复一个暂停的任务，沿用原 ID 重新排期。
func (s *Server) ResumeJob(c *gin.Context) {
	job, err := s.scheduler.Resume(c.Param("id"))
	s.respondJobOp(c, jobOpResume, job, err)
}

func (s *Server) respondJobOp(c *gin.Context, op string, job *core.Job, err error) {
	if err != nil {
		code, message := jobOpFailure(op, err)
		c.JSON(code, ErrorResponse{Code: code, Message: message})
		return
	}
	c.JSON(http.StatusOK, s.toJobResponse(job))
}

// notPendingHints 把"任务不在待执行队列"翻译成可操作下一步。
// 409 的文案决定了前端能不能直接把原因讲给用户，所以逐动作给出。
var notPendingHints = map[string]string{
	jobOpPause:      "job is running; force-pause interrupts the current attempt",
	jobOpForcePause: "job already finished or is being cancelled",
	jobOpResume:     "job is still winding down after a force-pause; retry once it settles",
	jobOpMove:       "job is running; its group changes when the attempt finishes",
}

// jobOpFailure 把调度器的哨兵错误映射成 HTTP 状态与说明。调用方保证 err 非空。
func jobOpFailure(op string, err error) (int, string) {
	switch {
	case errors.Is(err, core.ErrJobNotFound):
		return http.StatusNotFound, "job not found"
	case errors.Is(err, core.ErrJobNotPending):
		if hint, ok := notPendingHints[op]; ok {
			return http.StatusConflict, hint
		}
		return http.StatusConflict, "job is not pending"
	case errors.Is(err, core.ErrJobNotPaused):
		return http.StatusConflict, "job is not paused"
	default:
		return http.StatusInternalServerError, err.Error()
	}
}

// BatchJobOpsRequest POST /api/v1/jobs/batch-ops
type BatchJobOpsRequest struct {
	// Action 取 cancel|pause|force-pause|resume|move，整批一个动作
	Action string `json:"action" binding:"required" enums:"cancel,pause,force-pause,resume,move" example:"pause"`
	// IDs 任务 ID 列表，最多 100 条
	IDs []string `json:"ids" binding:"required" example:"0198a2e3-7d4f-7abc-9def-0123456789ab"`
	// Group 仅 action=move 使用。空串表示取消分组，因此它是必填字段而不是可省略
	Group *string `json:"group,omitempty" example:"nightly"`
}

// BatchOpsItemError 批量操作里单条失败的原因，ID 直接指向请求里的条目。
type BatchOpsItemError struct {
	ID      string `json:"id" example:"0198a2e3-7d4f-7abc-9def-0123456789ab"`
	Code    int    `json:"code" example:"409"`
	Message string `json:"message" example:"job is running; force-pause interrupts the current attempt"`
}

// BatchJobOpsResponse 与批量创建同形态：混合结果用 207 表达，逐条原因看 errors。
type BatchJobOpsResponse struct {
	Action    string              `json:"action" example:"pause"`
	Succeeded int                 `json:"succeeded" example:"2"`
	Failed    int                 `json:"failed" example:"1"`
	Items     []JobResponse       `json:"items,omitempty"`
	Errors    []BatchOpsItemError `json:"errors"`
}

// BatchJobOps POST /api/v1/jobs/batch-ops
// 逐条独立执行：某条失败不影响其他条，失败原因按任务 ID 返回。
func (s *Server) BatchJobOps(c *gin.Context) {
	var req BatchJobOpsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, ErrorResponse{Code: 400, Message: "invalid request body", Details: err.Error()})
		return
	}

	switch req.Action {
	case jobOpCancel, jobOpPause, jobOpForcePause, jobOpResume, jobOpMove:
	default:
		c.JSON(400, ErrorResponse{
			Code:    400,
			Message: "unsupported action",
			Details: fmt.Sprintf("got %q, expected cancel|pause|force-pause|resume|move", req.Action),
		})
		return
	}

	if len(req.IDs) == 0 {
		c.JSON(400, ErrorResponse{
			Code:    400,
			Message: "empty id list",
			Details: "the request body must name at least one job",
		})
		return
	}
	if len(req.IDs) > maxBatchOpsSize {
		c.JSON(400, ErrorResponse{
			Code:    400,
			Message: "batch too large",
			Details: fmt.Sprintf("got %d ids, at most %d per request", len(req.IDs), maxBatchOpsSize),
		})
		return
	}

	// 强制暂停整批要求 admin 以上：档位判断要看请求体，中间件做不到，
	// 因此在处理器里判一次。宁可拒绝整批也不要"批里混进几条越权动作"。
	if req.Action == jobOpForcePause && !s.allowRole(c, core.RoleAdmin) {
		c.JSON(http.StatusForbidden, ErrorResponse{
			Code:    http.StatusForbidden,
			Message: "insufficient role",
			Details: "force-pause requires admin or ops",
		})
		return
	}

	if req.Action == jobOpMove {
		if req.Group == nil {
			c.JSON(400, ErrorResponse{
				Code:    400,
				Message: "group is required",
				Details: `action=move needs "group"; send an empty string to detach`,
			})
			return
		}
		if failure := validateJobGroup(*req.Group); failure != nil {
			c.JSON(failure.Code, *failure)
			return
		}
	}

	items := make([]JobResponse, 0, len(req.IDs))
	failures := make([]BatchOpsItemError, 0)

	for _, jobID := range req.IDs {
		job, code, message := s.applyJobOp(req.Action, jobID, req.Group)
		if message != "" {
			failures = append(failures, BatchOpsItemError{ID: jobID, Code: code, Message: message})
			continue
		}
		if job != nil {
			items = append(items, s.toJobResponse(job))
		}
	}

	c.JSON(207, BatchJobOpsResponse{
		Action:    req.Action,
		Succeeded: len(req.IDs) - len(failures),
		Failed:    len(failures),
		Items:     items,
		Errors:    failures,
	})
}

// applyJobOp 执行单个动作。返回的 code/message 为空表示成功；
// job 为 nil 表示没有可返回的任务现状（目前只有 cancel：记录已被删除）。
func (s *Server) applyJobOp(action string, jobID string, group *string) (*core.Job, int, string) {
	var (
		job *core.Job
		err error
	)

	switch action {
	case jobOpCancel:
		err = s.scheduler.Cancel(jobID)
	case jobOpPause:
		job, err = s.scheduler.Pause(jobID)
	case jobOpForcePause:
		job, err = s.scheduler.ForcePause(jobID)
	case jobOpResume:
		job, err = s.scheduler.Resume(jobID)
	case jobOpMove:
		err = s.scheduler.SetGroup(jobID, *group)
		if err == nil {
			// SetGroup 不返回任务：改的是快照或堆内条目，回读一次给调用方确认
			job = s.jobByID(jobID)
		}
	default:
		err = fmt.Errorf("unsupported action %q", action)
	}

	if err != nil {
		code, message := jobOpFailure(action, err)
		return nil, code, message
	}
	return job, 0, ""
}

// jobByID 回读一个任务的当前快照；查不到返回 nil。
func (s *Server) jobByID(jobID string) *core.Job {
	snapshots, err := s.store.LoadAll()
	if err != nil {
		return nil
	}
	for _, snap := range snapshots {
		if snap.ID == jobID {
			job := &core.Job{}
			job.FromSnapshot(snap)
			return job
		}
	}
	return nil
}
