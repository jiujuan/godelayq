package api

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
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
	// Reload 是配置热重载最近一次的结论；整个字段缺省表示这台服务器没启用热重载。
	//
	// 指针 + omitempty 才有这个语义：值类型会给出 {"result":""} 一个空对象，
	// 那会被读成"启用过但从没重载过"，而这里想说的是一个都没开启。
	// 类型是本地那份而不是 core.ReloadState，理由见 api/reload_state.go 顶部（D-R0502）。
	Reload *ReloadStatus `json:"reload,omitempty"`
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
	// 读口没注入时整个 reload 键不出现（未启用热重载的部署）。
	// 这一层只保证一件事：**不主动把服务自己的配置值搬进读数**（用例见 reload_state_test.go 的
	// TestGetRuntime_LeaksNoConfigValues）。两段自由文本（error / watcher_error）由产出方组成、
	// 这里原样透出，其中的非凭据取值（例如操作者写错的时长、重名的档位）可能被回显——
	// 那道闸门在写文本的那一侧，不在这一行注释里（缺陷表 D-R0607）。
	if s.reloadState != nil {
		resp.Reload = reloadStatusOf(s.reloadState.State(), s.reloadEnabled)
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

// auditDefaultLimit 与 auditMaxLimit 是台账一次请求的行数缺省与上界。
// 这张表按写请求量线性增长，不设上界的 ?limit= 等于让一个运维误操作把整表读进内存。
const (
	auditDefaultLimit = 50
	auditMaxLimit     = 500
)

// auditActorLimit 是 ?actor= 的长度上界。账号名来自配置，本来就不会长到这一步，
// 这个上界挡的是拼错的查询与随意输入的长串。
const auditActorLimit = 128

// requireAudit 在没有台账读取方的部署里挡住查询端点。
//
// 与 requireArtifacts / requireIndex 同一取向：503 说的是"这台服务器没在记"，
// 而一份空列表说的是"没有符合条件的记录"——运维据此要做的事完全不同。
func (s *Server) requireAudit() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.auditRead == nil {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, ErrorResponse{
				Code:    http.StatusServiceUnavailable,
				Message: "write audit log is not configured",
				Details: "start the server with observability.enabled and observability.audit.enabled to enable /api/v1/admin/audit",
			})
			return
		}
		c.Next()
	}
}

// AuditItem 是一行台账在响应里的样子。字段与表列同名，latency 用微秒整数
// （time.Duration 序列化出来是纳秒整数，跨语言读的人看不出新旧单位）。
type AuditItem struct {
	Time           time.Time `json:"time"`
	Actor          string    `json:"actor"`
	ActorKind      string    `json:"actor_kind"`
	Role           string    `json:"role"`
	Action         string    `json:"action"`
	Method         string    `json:"method"`
	Route          string    `json:"route"`
	Status         int       `json:"status"`
	LatencyMicros  int64     `json:"latency_us"`
	Verdict        string    `json:"verdict"`
	ExecVerdict    string    `json:"exec_verdict,omitempty"`
	ExecReasonCode string    `json:"exec_reason_code,omitempty"`
	HandlerKey     string    `json:"handler_key,omitempty"`
	Profile        string    `json:"profile,omitempty"`
	JobID          string    `json:"job_id,omitempty"`
	RemoteIP       string    `json:"remote_ip,omitempty"`
	UserAgent      string    `json:"user_agent,omitempty"`
}

// AuditResponse GET /api/v1/admin/audit 的响应体。
//
// Total 是匹配过滤条件的总行数（不带 limit/offset），Count 是本次返回的行数，
// 两者不等就还有下一页——这与 GET /jobs 的 total 口径相同。
// 顺序是最新在前（表的 seq 降序），与事件端点的升序相反：台账的用法是"刚发生了什么"。
type AuditResponse struct {
	Count  int         `json:"count"`
	Total  int         `json:"total"`
	Limit  int         `json:"limit"`
	Offset int         `json:"offset"`
	Items  []AuditItem `json:"items"`
}

// GetAudit GET /api/v1/admin/audit
//
// 只读台账，不做清理：这张表的淘汰由 observability.audit.retention_count / retention_age
// 负责（与事件表同形），给它加一个清空端点会让"谁删了台账"这件事没有出处。
func (s *Server) GetAudit(c *gin.Context) {
	filter, ok := s.parseAuditFilter(c)
	if !ok {
		return
	}

	entries, total, err := s.auditRead.Query(filter)
	if err != nil {
		// 与事件端点、产物列表同一口径：读不出来就报错，不静默给一份看起来完整的空结果
		s.logger.Error("failed to read the write audit log", "error", err)
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Code:    http.StatusInternalServerError,
			Message: "failed to load audit records",
			Details: err.Error(),
		})
		return
	}

	items := make([]AuditItem, 0, len(entries))
	for _, entry := range entries {
		items = append(items, auditItemOf(entry))
	}
	c.JSON(http.StatusOK, AuditResponse{
		Count:  len(items),
		Total:  total,
		Limit:  filter.Limit,
		Offset: filter.Offset,
		Items:  items,
	})
}

// parseAuditFilter 解析并校验查询参数。返回 false 表示已经写出 400 响应。
//
// action 与 verdict 按封闭集校验，因此 ?action=../x 这类拼错或试探在进库之前就被挡下；
// 过滤条件本身全部是语句参数，不存在拼接进 SQL 的路径。
func (s *Server) parseAuditFilter(c *gin.Context) (AuditFilter, bool) {
	var filter AuditFilter

	limit := auditDefaultLimit
	if raw := c.Query("limit"); raw != "" {
		parsed, ok := s.parseAuditInt(c, "limit", raw)
		if !ok {
			return filter, false
		}
		if parsed == 0 {
			parsed = auditDefaultLimit
		}
		if parsed > auditMaxLimit {
			s.respondBadParam(c, "limit", raw, "the maximum is 500 rows per request")
			return filter, false
		}
		limit = parsed
	}
	filter.Limit = limit

	if raw := c.Query("offset"); raw != "" {
		parsed, ok := s.parseAuditInt(c, "offset", raw)
		if !ok {
			return filter, false
		}
		filter.Offset = parsed
	}

	if raw := c.Query("actor"); raw != "" {
		if len(raw) > auditActorLimit {
			s.respondBadParam(c, "actor", raw, fmt.Sprintf("at most %d characters", auditActorLimit))
			return filter, false
		}
		filter.Actor = raw
	}

	if raw := c.Query("action"); raw != "" {
		if !slices.Contains(AuditActions(), raw) {
			s.respondBadParam(c, "action", raw, "expected one of "+strings.Join(AuditActions(), ", "))
			return filter, false
		}
		filter.Action = raw
	}

	if raw := c.Query("verdict"); raw != "" {
		if !slices.Contains(AuditVerdicts, raw) {
			s.respondBadParam(c, "verdict", raw, "expected one of "+strings.Join(AuditVerdicts, ", "))
			return filter, false
		}
		filter.Verdict = raw
	}

	if raw := c.Query("since"); raw != "" {
		parsed, ok := s.parseAuditTime(c, "since", raw)
		if !ok {
			return filter, false
		}
		filter.Since = &parsed
	}

	if raw := c.Query("until"); raw != "" {
		parsed, ok := s.parseAuditTime(c, "until", raw)
		if !ok {
			return filter, false
		}
		filter.Until = &parsed
	}

	return filter, true
}

// parseAuditInt 解析非负整数参数；负数与非法写法都是 400，不静默纠正成 0。
//
// 400 在这里写而不是让调用方补：这一列参数有四个（limit/offset/since/until），
// 少补一处就是"返回 false 但没人写过响应"，而 gin 会把那变成 200 空体。
func (s *Server) parseAuditInt(c *gin.Context, name, raw string) (int, bool) {
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		s.respondBadParam(c, name, raw, "expected a non-negative integer")
		return 0, false
	}
	return value, true
}

// parseAuditTime 解析 RFC3339 时间。错误原文里带的是调用方自己写的那个值，
// 回显它不泄露服务端信息，而"哪个写法不对"正是运维需要的。
func (s *Server) parseAuditTime(c *gin.Context, name, raw string) (time.Time, bool) {
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		s.respondBadParam(c, name, raw, "expected RFC3339, e.g. 2026-10-01T12:00:00Z")
		return time.Time{}, false
	}
	return parsed, true
}

func auditItemOf(entry AuditEntry) AuditItem {
	return AuditItem{
		Time:           entry.Time,
		Actor:          entry.Actor,
		ActorKind:      entry.ActorKind,
		Role:           entry.Role,
		Action:         entry.Action,
		Method:         entry.Method,
		Route:          entry.Route,
		Status:         entry.Status,
		LatencyMicros:  entry.Latency.Microseconds(),
		Verdict:        entry.Verdict,
		ExecVerdict:    entry.ExecVerdict,
		ExecReasonCode: entry.ExecReasonCode,
		HandlerKey:     entry.HandlerKey,
		Profile:        entry.Profile,
		JobID:          entry.JobID,
		RemoteIP:       entry.RemoteIP,
		UserAgent:      entry.UserAgent,
	}
}
