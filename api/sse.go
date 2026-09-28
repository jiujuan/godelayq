package api

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"
	"godelayq/core"
)

// publishedEventTypes 是实际会被广播的事件类型，用于校验过滤参数。
// heap.updated 虽然在 core 里定义了类型，但目前没有任何代码发布它，故不列入。
var publishedEventTypes = []core.EventType{
	core.EventJobScheduled,
	core.EventJobStarted,
	core.EventJobCompleted,
	core.EventJobFailed,
	core.EventJobCancelled,
	core.EventJobRetrying,
}

// handleSSE 以 Server-Sent Events 推送事件。
//
// 事件类型走 EventBus 的类型级订阅（服务端不再收全量再丢弃），
// job_types 因事件总线不感知任务属性，仍在本 handler 内过滤。
func (s *Server) handleSSE(c *gin.Context) {
	eventTypes, err := parseSSEEventTypes(c.QueryArray("event_types"))
	if err != nil {
		c.JSON(400, ErrorResponse{
			Code:    400,
			Message: "invalid event_types",
			Details: err.Error(),
		})
		return
	}
	jobTypes := c.QueryArray("job_types")

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")

	// 立即下发响应头：否则客户端要等到第一个事件才能拿到 header，
	// 期间 EventSource / http.Client.Do 都会一直阻塞。
	// 用 SSE 注释帧做首包，客户端解析时会自动忽略 ":" 开头的行。
	if _, err := fmt.Fprint(c.Writer, ": connected\n\n"); err != nil {
		return
	}
	c.Writer.Flush()

	eventBus := s.scheduler.GetEventBus()

	var subID string
	var eventCh <-chan core.Event
	if len(eventTypes) > 0 {
		subID, eventCh = eventBus.Subscribe(eventTypes...)
	} else {
		subID, eventCh = eventBus.SubscribeAll()
	}
	defer eventBus.Unsubscribe(subID)

	clientGone := c.Request.Context().Done()

	for {
		select {
		case <-clientGone:
			return
		case ev := <-eventCh:
			if !matchesJobTypes(ev, jobTypes) {
				continue
			}

			data, _ := json.Marshal(ev)
			if _, err := fmt.Fprintf(c.Writer, "data: %s\n\n", data); err != nil {
				return
			}
			c.Writer.Flush()
		}
	}
}

// parseSSEEventTypes 解析 event_types，支持重复参数与逗号分隔两种写法。
// 未知类型直接报错：订阅一个不存在的事件类型会永远收不到东西，静默比报错更难排查。
// 返回空切片表示不过滤类型。
func parseSSEEventTypes(raw []string) ([]core.EventType, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	requested := make(map[string]bool, len(raw))
	for _, item := range raw {
		for _, name := range strings.Split(item, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			requested[name] = true
		}
	}
	if len(requested) == 0 {
		return nil, nil
	}

	// 去重后按已知类型的固定顺序返回：同一通道在同一类型上重复注册会收到重复事件
	types := make([]core.EventType, 0, len(requested))
	known := make([]string, 0, len(publishedEventTypes))
	for _, candidate := range publishedEventTypes {
		name := string(candidate)
		known = append(known, name)
		if requested[name] {
			types = append(types, candidate)
		}
	}

	var unknown []string
	for name := range requested {
		if !containsName(types, name) {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("unknown event type(s): %s; supported: %s",
			strings.Join(unknown, ", "), strings.Join(known, ", "))
	}

	return types, nil
}

func containsName(types []core.EventType, name string) bool {
	for _, candidate := range types {
		if string(candidate) == name {
			return true
		}
	}

	return false
}

// matchesJobTypes 与 WebSocket 侧过滤保持一致：按事件的 JobName 匹配。
func matchesJobTypes(event core.Event, jobTypes []string) bool {
	if len(jobTypes) == 0 {
		return true
	}

	for _, jobType := range jobTypes {
		if jobType == event.JobName {
			return true
		}
	}

	return false
}
