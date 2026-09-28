package api

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"godelayq/core"
)

// defaultListJobsLimit 是 GET /jobs 未显式给 limit 时返回的最大条数，
// maxListJobsLimit 是可请求的硬上限（limit 超过它会被截断而不是报错）。
const (
	defaultListJobsLimit = 50
	maxListJobsLimit     = 100
)

// maxBatchCreateSize 是 POST /jobs/batch 单请求允许的任务数。
const maxBatchCreateSize = 100

// validateJobGroup 校验任务上的分组标签。空串表示未分组，始终合法；
// 名称规则由 core 定义，因为分组名会进 URL 查询参数与列表页筛选。
func validateJobGroup(group string) *ErrorResponse {
	if group == "" {
		return nil
	}
	if err := core.ValidateGroupName(group); err != nil {
		return &ErrorResponse{Code: 400, Message: "invalid group name", Details: err.Error()}
	}
	return nil
}

// parseGroupFilter 区分 "?group="（含空值：精确筛未分组）与完全省略（不筛）。
// 两者用 c.Query 看不出来，会把"未分组"和"全部分组"混成一回事。
func parseGroupFilter(c *gin.Context) (string, bool) {
	value, present := c.GetQuery("group")
	if !present {
		return "", false
	}
	return value, true
}

// CreateJob 创建任务
func (s *Server) CreateJob(c *gin.Context) {
	var req CreateJobRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, ErrorResponse{
			Code:    400,
			Message: "invalid request body",
			Details: err.Error(),
		})
		return
	}

	job, failure := s.createJobFromRequest(req)
	if failure != nil {
		c.JSON(failure.Code, *failure)
		return
	}

	c.JSON(201, s.toJobResponse(job))
}

// createJobFromRequest 校验请求并把任务交给调度器，供单条与批量创建共用。
// 返回非 nil 的 ErrorResponse 表示失败，调用方决定如何呈现（400/500 或批量里的逐条错误）。
func (s *Server) createJobFromRequest(req CreateJobRequest) (*core.Job, *ErrorResponse) {
	// 计算触发时间
	triggerAt, err := s.calculateTriggerTime(req)
	if err != nil {
		return nil, &ErrorResponse{Code: 400, Message: "invalid time format", Details: err.Error()}
	}

	// 检查Handler是否存在（注册表在调度器，执行时按同一键回查）
	handler, ok := s.scheduler.LookupHandler(req.Name)
	if !ok {
		return nil, &ErrorResponse{
			Code:    400,
			Message: "unknown job type",
			Details: fmt.Sprintf("job type '%s' not registered", req.Name),
		}
	}

	if failure := validateJobGroup(req.Group); failure != nil {
		return nil, failure
	}

	// 解析重试延迟
	retryDelay := 1 * time.Minute
	if req.RetryDelay != "" {
		if d, err := time.ParseDuration(req.RetryDelay); err == nil {
			retryDelay = d
		}
	}

	// 解析执行超时（格式非法直接拒绝，不静默忽略）
	var timeout time.Duration
	if req.Timeout != "" {
		d, err := time.ParseDuration(req.Timeout)
		if err != nil {
			return nil, &ErrorResponse{Code: 400, Message: "invalid timeout format", Details: err.Error()}
		}
		timeout = d
	}

	// 创建任务
	job := &core.Job{
		Name:       req.Name,
		Payload:    []byte(req.Payload),
		TriggerAt:  triggerAt,
		Group:      req.Group,
		CronExpr:   req.CronExpr,
		IsRepeat:   req.IsRepeat,
		Timeout:    timeout,
		MaxRetries: req.MaxRetries,
		RetryDelay: retryDelay,
		Status:     core.StatusPending,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}

	// 绑定Handler（与执行侧回查用的是同一份注册表）
	job.Handler = handler

	// 添加到调度器
	if err := s.scheduler.Schedule(job); err != nil {
		return nil, &ErrorResponse{Code: 500, Message: "failed to schedule job", Details: err.Error()}
	}

	return job, nil
}

// ListJobs 获取任务列表
func (s *Server) ListJobs(c *gin.Context) {
	status := c.Query("status") // 状态名过滤，如 pending/running/success/failed/cancelled/paused
	name := c.Query("name")     // 名称过滤
	groupFilter, filterByGroup := parseGroupFilter(c)
	limit := parseListJobsLimit(c.Query("limit"))
	offset := parseListJobsOffset(c.Query("offset"))

	var wantStatus core.JobStatus
	filterByStatus := status != ""
	if filterByStatus {
		parsed, ok := core.ParseJobStatus(status)
		if !ok {
			c.JSON(400, ErrorResponse{
				Code:    400,
				Message: "invalid status filter",
				Details: "expected one of pending, running, success, failed, cancelled, paused",
			})
			return
		}
		wantStatus = parsed
	}

	// 从存储加载所有任务快照（含终态留痕）
	snapshots, err := s.store.LoadAll()
	if err != nil {
		c.JSON(500, ErrorResponse{
			Code:    500,
			Message: "failed to load jobs",
			Details: err.Error(),
		})
		return
	}

	matched := make([]core.JobSnapshot, 0, len(snapshots))
	for _, snap := range snapshots {
		if filterByStatus && core.JobStatus(snap.Status) != wantStatus {
			continue
		}
		if name != "" && snap.Name != name {
			continue
		}
		// 分组匹配忽略大小写：注册表的主键就是这个口径（core/group_store.go），
		// 两套大小写规则会让"看着是同一个组"筛出两种结果。
		if filterByGroup && !strings.EqualFold(snap.Group, groupFilter) {
			continue
		}
		matched = append(matched, snap)
	}

	// 存储是 map，遍历顺序随机；不排序的话带分页的每次请求结果都不同
	sort.Slice(matched, func(i, j int) bool {
		if !matched[i].UpdatedAt.Equal(matched[j].UpdatedAt) {
			return matched[i].UpdatedAt.After(matched[j].UpdatedAt)
		}
		return matched[i].ID < matched[j].ID
	})

	total := len(matched)
	if offset > total {
		offset = total
	}
	page := matched[offset:]
	if len(page) > limit {
		page = page[:limit]
	}

	items := make([]JobResponse, 0, len(page))
	for _, snap := range page {
		job := &core.Job{}
		job.FromSnapshot(snap)
		items = append(items, s.toJobResponse(job))
	}

	c.JSON(200, ListJobsResponse{
		Total: total,
		Items: items,
	})
}

// parseListJobsLimit 解析 limit：非法或非正值取默认值，超过上限则截断。
func parseListJobsLimit(raw string) int {
	if raw == "" {
		return defaultListJobsLimit
	}

	var limit int
	if _, err := fmt.Sscanf(raw, "%d", &limit); err != nil || limit <= 0 {
		return defaultListJobsLimit
	}
	if limit > maxListJobsLimit {
		return maxListJobsLimit
	}

	return limit
}

// parseListJobsOffset 解析 offset，非法或负值按 0 处理。
func parseListJobsOffset(raw string) int {
	var offset int
	if _, err := fmt.Sscanf(raw, "%d", &offset); err != nil || offset < 0 {
		return 0
	}

	return offset
}

// GetJob 获取单个任务详情
func (s *Server) GetJob(c *gin.Context) {
	id := c.Param("id")

	// 尝试从堆中查找（活跃任务）
	// 注：需要给QuaternaryHeap增加查询方法，或者从store加载
	// 这里简化为从store加载
	snapshots, _ := s.store.LoadAll()

	for _, snap := range snapshots {
		if snap.ID == id {
			job := &core.Job{}
			job.FromSnapshot(snap)
			c.JSON(200, s.toJobResponse(job))
			return
		}
	}

	c.JSON(404, ErrorResponse{
		Code:    404,
		Message: "job not found",
	})
}

// UpdateJob 更新任务（仅pending状态可更新）
func (s *Server) UpdateJob(c *gin.Context) {
	id := c.Param("id")

	var req UpdateJobRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, ErrorResponse{
			Code:    400,
			Message: "invalid request body",
		})
		return
	}

	var payload []byte
	if len(req.Payload) > 0 {
		payload = req.Payload
	}

	// 与 CreateJob 同口径：格式非法先拒绝，不静默忽略
	var timeout time.Duration
	if req.Timeout != "" {
		d, err := time.ParseDuration(req.Timeout)
		if err != nil {
			c.JSON(400, ErrorResponse{
				Code:    400,
				Message: "invalid timeout format",
				Details: err.Error(),
			})
			return
		}
		timeout = d
	}
	// 分组同样在这里校验：apply 里返回的错误会被当成 500，
	// 而"组名不合法"明明是调用方的问题
	if req.Group != nil {
		if failure := validateJobGroup(*req.Group); failure != nil {
			c.JSON(failure.Code, *failure)
			return
		}
	}

	// 原地更新：不再走 Cancel→Schedule，避免两步之间失败导致任务丢失
	job, err := s.scheduler.UpdatePending(id, func(j *core.Job) error {
		if req.TriggerAt != nil {
			j.TriggerAt = *req.TriggerAt
		}
		if payload != nil {
			j.Payload = payload
		}
		if req.MaxRetries != nil {
			j.MaxRetries = *req.MaxRetries
		}
		if req.Timeout != "" {
			j.Timeout = timeout
		}
		if req.Group != nil {
			j.Group = *req.Group
		}
		return nil
	})

	switch {
	case errors.Is(err, core.ErrJobNotFound):
		c.JSON(404, ErrorResponse{
			Code:    404,
			Message: "job not found",
		})
		return
	case errors.Is(err, core.ErrJobNotPending):
		c.JSON(409, ErrorResponse{
			Code:    409,
			Message: "job cannot be modified",
			Details: "only pending jobs can be updated",
		})
		return
	case err != nil:
		c.JSON(500, ErrorResponse{
			Code:    500,
			Message: "failed to update job",
			Details: err.Error(),
		})
		return
	}

	c.JSON(200, s.toJobResponse(job))
}

// CancelJob 取消任务
func (s *Server) CancelJob(c *gin.Context) {
	id := c.Param("id")

	if err := s.scheduler.Cancel(id); err != nil {
		if err == core.ErrJobNotFound {
			c.JSON(404, ErrorResponse{
				Code:    404,
				Message: "job not found or already executed",
			})
			return
		}
		c.JSON(500, ErrorResponse{
			Code:    500,
			Message: "failed to cancel job",
		})
		return
	}

	c.Status(204)
}

// RetryJob 手动重试失败任务
func (s *Server) RetryJob(c *gin.Context) {
	id := c.Param("id")

	// 查找失败的任务
	snapshots, _ := s.store.LoadAll()
	var target *core.JobSnapshot

	for _, snap := range snapshots {
		if snap.ID == id && core.JobStatus(snap.Status) == core.StatusFailed {
			target = &snap
			break
		}
	}

	if target == nil {
		c.JSON(404, ErrorResponse{
			Code:    404,
			Message: "failed job not found",
		})
		return
	}

	// 重置状态并重新调度
	job := &core.Job{}
	job.FromSnapshot(*target)
	job.Status = core.StatusPending
	job.RetryCount = 0
	job.TriggerAt = time.Now().Add(1 * time.Second) // 1秒后执行
	job.UpdatedAt = time.Now()

	if h, ok := s.scheduler.LookupHandler(job.HandlerKey()); ok {
		job.Handler = h
	}

	if err := s.scheduler.Schedule(job); err != nil {
		c.JSON(500, ErrorResponse{
			Code:    500,
			Message: "failed to retry job",
		})
		return
	}

	c.JSON(200, s.toJobResponse(job))
}

// GetStats 获取统计信息
func (s *Server) GetStats(c *gin.Context) {
	snapshots, _ := s.store.LoadAll()

	// 一次取齐调度器的实时值：Running/HeapSize/Suspended 分三次读会拿到三个不同瞬间，
	// "堆里还压着任务却显示未挂起"这种自相矛盾的组合，正是横幅最不该出现的时刻
	sched := s.scheduler.RuntimeStats()

	stats := StatsResponse{
		Uptime: time.Since(s.startTime).String(),
	}

	// Pending/Completed/Failed 来自存储快照：它们描述"已落盘的状态"，
	// 进程重启后依然可累计；Running 与 HeapSize 取实时值。
	for _, snap := range snapshots {
		switch core.JobStatus(snap.Status) {
		case core.StatusPending:
			stats.Pending++
		case core.StatusPaused:
			// 暂停单独计数：它既不在堆里也不算完成，混进 pending 会让
			// "我以为还有 10 个任务要跑"变成谎话
			stats.Paused++
		case core.StatusSuccess:
			stats.Completed++
		case core.StatusFailed:
			stats.Failed++
		}
	}

	stats.Running = sched.Running
	stats.HeapSize = sched.HeapSize
	stats.SchedulingSuspended = sched.Suspended

	c.JSON(200, stats)
}

// HealthCheck 健康检查
func (s *Server) HealthCheck(c *gin.Context) {
	c.JSON(200, gin.H{
		"status": "healthy",
		"time":   time.Now().Format(time.RFC3339),
	})
}

// ListJobTypes 获取支持的Job类型（来自调度器注册表，按字典序）
func (s *Server) ListJobTypes(c *gin.Context) {
	c.JSON(200, gin.H{
		"types": s.scheduler.HandlerNames(),
	})
}

// BatchCreateJobs POST /api/v1/jobs/batch
// 逐条独立处理：某条失败不影响其他条入队，失败原因按原始下标返回。
func (s *Server) BatchCreateJobs(c *gin.Context) {
	var reqs []CreateJobRequest
	if err := c.ShouldBindJSON(&reqs); err != nil {
		c.JSON(400, ErrorResponse{
			Code:    400,
			Message: "invalid batch format",
			Details: "expected a JSON array of job definitions: " + err.Error(),
		})
		return
	}

	if len(reqs) == 0 {
		c.JSON(400, ErrorResponse{
			Code:    400,
			Message: "empty batch",
			Details: "the request body must contain at least one job",
		})
		return
	}
	if len(reqs) > maxBatchCreateSize {
		c.JSON(400, ErrorResponse{
			Code:    400,
			Message: "batch too large",
			Details: fmt.Sprintf("got %d jobs, at most %d per request", len(reqs), maxBatchCreateSize),
		})
		return
	}

	items := make([]JobResponse, 0, len(reqs))
	failures := make([]BatchItemError, 0)

	for index, req := range reqs {
		job, failure := s.createJobFromRequest(req)
		if failure != nil {
			failures = append(failures, BatchItemError{
				Index:   index,
				Code:    failure.Code,
				Message: failure.Message,
				Details: failure.Details,
			})
			continue
		}
		items = append(items, s.toJobResponse(job))
	}

	// 混合结果用 207 表达；即使全部失败也是 207 + errors，调用方看 errors 定位
	c.JSON(207, BatchCreateJobsResponse{
		Succeeded: len(items),
		Failed:    len(failures),
		Items:     items,
		Errors:    failures,
	})
}

// 辅助方法

func (s *Server) calculateTriggerTime(req CreateJobRequest) (time.Time, error) {
	now := time.Now()

	// 优先级：Cron > TriggerAt > Delay > 立即执行
	if req.CronExpr != "" {
		// 计算下次执行时间（使用robfig/cron）
		parser := core.NewCronParser()
		next, err := parser.Next(req.CronExpr, now)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid cron expression: %w", err)
		}
		return next, nil
	}

	if req.TriggerAt != nil {
		if req.TriggerAt.Before(now) {
			return time.Time{}, fmt.Errorf("trigger time must be in the future")
		}
		return *req.TriggerAt, nil
	}

	if req.Delay != "" {
		d, err := time.ParseDuration(req.Delay)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid delay format: %w", err)
		}
		return now.Add(d), nil
	}

	// 默认立即执行（1秒后，避免当前时间戳问题）
	return now.Add(1 * time.Second), nil
}

func (s *Server) toJobResponse(job *core.Job) JobResponse {
	resp := JobResponse{
		ID:         job.ID,
		Name:       job.Name,
		Status:     job.Status.String(),
		Group:      job.Group,
		TriggerAt:  job.TriggerAt,
		Payload:    job.Payload,
		RetryCount: job.RetryCount,
		MaxRetries: job.MaxRetries,
		IsRepeat:   job.IsRepeat,
		CronExpr:   job.CronExpr,
		CreatedAt:  job.CreatedAt,
		UpdatedAt:  job.UpdatedAt,
	}

	if job.Timeout > 0 {
		resp.Timeout = job.Timeout.String()
	}

	// 计算剩余时间
	if job.Status == core.StatusPending {
		d := time.Until(job.TriggerAt)
		if d > 0 {
			resp.NextRunIn = d.Round(time.Second).String()
		} else {
			resp.NextRunIn = "imminent"
		}
	}

	return resp
}
