package api

import (
	"fmt"

	"github.com/gin-gonic/gin"
	"godelayq/core"
)

// EventsResponse GET /jobs/:id/events 与 GET /events 共用的响应体。
// JobID 只在任务时间线里给出；全局流没有归属，留空。
type EventsResponse struct {
	JobID string       `json:"job_id,omitempty" example:"0198a2e3-7d4f-7abc-9def-0123456789ab"`
	Count int          `json:"count" example:"12"`
	Items []core.Event `json:"items"`
	// Note 明确告知这是内存缓冲：进程重启即清空，不是审计日志（§5.6）
	Note string `json:"note" example:"in-memory buffer, cleared on restart"`
}

// eventHistoryNote 是写进响应的时间线语义说明。
// 前端 UI 直接引用它，省得两处各写一句、日后不同步。
const eventHistoryNote = "in-memory buffer, cleared on restart"

// parseEventLimit 解析 ?limit=：非法或负值取全部，上限为窗口容量本身。
func parseEventLimit(raw string, capacity int) int {
	if raw == "" {
		return capacity
	}
	var limit int
	if _, err := fmt.Sscanf(raw, "%d", &limit); err != nil || limit <= 0 {
		return capacity
	}
	if limit > capacity {
		return capacity
	}
	return limit
}

// GetJobEvents GET /api/v1/jobs/:id/events
// 返回该任务的最近事件，按时间升序，供详情页时间线首屏。
func (s *Server) GetJobEvents(c *gin.Context) {
	jobID := c.Param("id")
	limit := parseEventLimit(c.Query("limit"), historyPerJobLimit)

	items := s.history.Events(jobID, limit)
	c.JSON(200, EventsResponse{
		JobID: jobID,
		Count: len(items),
		Items: items,
		Note:  eventHistoryNote,
	})
}

// ListRecentEvents GET /api/v1/events
// 全局最近事件，用于 Dashboard 首屏回灌：刷新页面不该让刚发生的事凭空消失。
func (s *Server) ListRecentEvents(c *gin.Context) {
	limit := parseEventLimit(c.Query("limit"), historyGlobalLimit)

	items := s.history.Recent(limit)
	c.JSON(200, EventsResponse{
		Count: len(items),
		Items: items,
		Note:  eventHistoryNote,
	})
}
