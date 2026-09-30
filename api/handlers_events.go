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
	// Note 说明这批事件从哪儿来、能看到多久：装配了事件库是 eventStoreNote，
	// 没装配是 eventHistoryNote。两个端点共用同一个结构，所以也共用这一对常量。
	Note string `json:"note" example:"in-memory buffer, cleared on restart"`
}

// 两个 Note 常量。分岔的依据是这次部署装没装配事件库（api.WithEventLog 有没有传实现），
// 与请求参数无关：同一份配置下所有请求拿到的句子都一样。
// 前端 UI 直接引用它们，省得两处各写一句、日后不同步（web/src/api/events.ts）。
const (
	// eventHistoryNote 是内存缓冲那句，S04 之前唯一的取值，原句不动。
	eventHistoryNote = "in-memory buffer, cleared on restart"
	// eventStoreNote 说的是库路径唯一会与直觉不同的地方：最新一批还没到 flush 周期时
	// 读不到，延迟上界就是一个 flush_interval（默认 200ms）。
	// 实时增量本来就由 WebSocket 负责，这里不承诺"刚刚"。
	eventStoreNote = "persisted event store; newest entry may lag by the write flush interval"
)

// eventsQueryLimit 是库路径上 parseEventLimit 的上界。
// 内存路径的上界是窗口容量本身（每任务 100、全局 500），那里"上限"与"留存"是同一件事；
// 库里能留 20 万条，一次响应给多少就成了另一个问题，所以另起一个常量而不是复用。
// 不新增配置项（本仓库惯例：未生效的选项不进配置）：它是单次响应的体积界限，不是策略旋钮。
const eventsQueryLimit = 1000

// parseEventLimit 解析 ?limit=：非法或负值取全部，上限由调用方给出的 capacity 决定。
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

// eventList 把"没有事件"归一成空列表：响应里的 items 必须是 []，不能是 null。
//
// 内存路径天然满足（api/history.go 的 tail 总是新建切片），库路径取决于读取方，
// 所以归一放在端点这一侧——前端两处都按数组处理这个字段。
func eventList(items []core.Event) []core.Event {
	if items == nil {
		return []core.Event{}
	}
	return items
}

// GetJobEvents GET /api/v1/jobs/:id/events
// 返回该任务的最近事件，按时间升序，供详情页时间线首屏。
//
// 装配了事件库就读库，否则读内存缓冲；两条路径不给同一份数据做合并，
// 也不在库读失败时偷偷退回内存（后者会让人拿着一份缺历史的列表做判断）。
func (s *Server) GetJobEvents(c *gin.Context) {
	jobID := c.Param("id")

	if s.events != nil {
		items, err := s.events.Events(jobID, parseEventLimit(c.Query("limit"), eventsQueryLimit))
		if err != nil {
			// 与 ListJobs 读存储失败同一口径：报错而不是给一份兜底数据
			c.JSON(500, ErrorResponse{
				Code:    500,
				Message: "failed to load job events",
				Details: err.Error(),
			})
			return
		}
		c.JSON(200, EventsResponse{
			JobID: jobID,
			Count: len(items),
			Items: eventList(items),
			Note:  eventStoreNote,
		})
		return
	}

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
	if s.events != nil {
		items, err := s.events.Recent(parseEventLimit(c.Query("limit"), eventsQueryLimit))
		if err != nil {
			c.JSON(500, ErrorResponse{
				Code:    500,
				Message: "failed to load recent events",
				Details: err.Error(),
			})
			return
		}
		c.JSON(200, EventsResponse{
			Count: len(items),
			Items: eventList(items),
			Note:  eventStoreNote,
		})
		return
	}

	limit := parseEventLimit(c.Query("limit"), historyGlobalLimit)
	items := s.history.Recent(limit)
	c.JSON(200, EventsResponse{
		Count: len(items),
		Items: items,
		Note:  eventHistoryNote,
	})
}
