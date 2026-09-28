package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"godelayq/core"
)

// RuntimeResponse GET /api/v1/admin/runtime
// 只读诊断：容量与占用，不含任务内容与凭据。ops 档专用（§5.4）。
type RuntimeResponse struct {
	// Uptime 进程启动至今的时长，与 /stats 同一口径
	Uptime string `json:"uptime" example:"24h30m"`
	// StartedAt 服务器构造时间，前端可格式化成绝对时间
	StartedAt time.Time `json:"started_at" format:"date-time"`
	// Scheduler 调度与执行侧的实时占用
	Scheduler core.RuntimeStats `json:"scheduler"`
	// EventHistory 事件内存缓冲的占用（详情页时间线的数据源）
	EventHistory EventHistoryStats `json:"event_history"`
	// SchedulingSuspendedNote 只在挂起时给一句话解释，避免运维误读成"服务卡死"
	SchedulingSuspendedNote string `json:"scheduling_suspended_note,omitempty"`
}

// GetRuntime GET /api/v1/admin/runtime
func (s *Server) GetRuntime(c *gin.Context) {
	stats := s.scheduler.RuntimeStats()

	resp := RuntimeResponse{
		Uptime:       time.Since(s.startTime).String(),
		StartedAt:    s.startTime,
		Scheduler:    stats,
		EventHistory: s.history.Stats(),
	}
	if stats.Suspended {
		resp.SchedulingSuspendedNote = "due jobs are not dispatched until unsuspend; restart clears it"
	}

	c.JSON(http.StatusOK, resp)
}

// SuspendScheduler POST /api/v1/admin/scheduler/suspend
// 挂起调度循环（维护窗口）：已在执行的任务照常跑完，堆与存储都不动。
func (s *Server) SuspendScheduler(c *gin.Context) {
	s.scheduler.Suspend()
	s.logger.Warn("scheduling suspended through the admin api")

	c.JSON(http.StatusOK, gin.H{"suspended": true})
}

// UnsuspendScheduler POST /api/v1/admin/scheduler/unsuspend
func (s *Server) UnsuspendScheduler(c *gin.Context) {
	s.scheduler.Unsuspend()
	s.logger.Info("scheduling resumed through the admin api")

	c.JSON(http.StatusOK, gin.H{"suspended": false})
}

// ClearEventHistory DELETE /api/v1/admin/events
// 清空事件内存缓冲：各详情页时间线从当前时刻重新开始，历史任务记录不受影响。
// 返回被清掉的条数，便于运维确认"确实清了"。
func (s *Server) ClearEventHistory(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"cleared": s.history.Clear()})
}
