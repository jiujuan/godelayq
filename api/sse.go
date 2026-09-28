package api

import (
	"encoding/json"
	"fmt"

	"godelayq/core"
	"github.com/gin-gonic/gin"
)

func (s *Server) handleSSE(c *gin.Context) {
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("Access-Control-Allow-Origin", "*")

	// 立即下发响应头：否则客户端要等到第一个事件才能拿到 header，
	// 期间 EventSource / http.Client.Do 都会一直阻塞。
	// 用 SSE 注释帧做首包，客户端解析时会自动忽略 ":"开头的行。
	if _, err := fmt.Fprint(c.Writer, ": connected\n\n"); err != nil {
		return
	}
	c.Writer.Flush()

	// 获取客户端过滤参数
	jobTypes := c.QueryArray("job_types")
	eventTypes := c.QueryArray("event_types")

	// 订阅事件
	subID, eventCh := s.scheduler.GetEventBus().SubscribeAll()
	defer s.scheduler.GetEventBus().Unsubscribe(subID)

	clientGone := c.Request.Context().Done()

	for {
		select {
		case <-clientGone:
			return
		case ev := <-eventCh:
			// 过滤
			if !matchSSEFilter(ev, jobTypes, eventTypes) {
				continue
			}

			data, _ := json.Marshal(ev)
			fmt.Fprintf(c.Writer, "data: %s\n\n", data)
			c.Writer.Flush()
		}
	}
}

func matchSSEFilter(ev core.Event, jobTypes, eventTypes []string) bool {
	// 实现过滤逻辑...
	return true
}
