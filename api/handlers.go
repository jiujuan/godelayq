package api

import (
	"fmt"
	"time"

	"godelayq/core"
	"github.com/gin-gonic/gin"
)

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

	// 计算触发时间
	triggerAt, err := s.calculateTriggerTime(req)
	if err != nil {
		c.JSON(400, ErrorResponse{
			Code:    400,
			Message: "invalid time format",
			Details: err.Error(),
		})
		return
	}

	// 检查Handler是否存在（仅用于验证，实际执行时从registry获取）
	if _, ok := s.registry.Get(req.Name); !ok {
		c.JSON(400, ErrorResponse{
			Code:    400,
			Message: "unknown job type",
			Details: fmt.Sprintf("job type '%s' not registered", req.Name),
		})
		return
	}

	// 解析重试延迟
	retryDelay := 1 * time.Minute
	if req.RetryDelay != "" {
		if d, err := time.ParseDuration(req.RetryDelay); err == nil {
			retryDelay = d
		}
	}

	// 创建任务
	job := &core.Job{
		Name:       req.Name,
		Payload:    []byte(req.Payload),
		TriggerAt:  triggerAt,
		CronExpr:   req.CronExpr,
		IsRepeat:   req.IsRepeat,
		MaxRetries: req.MaxRetries,
		RetryDelay: retryDelay,
		Status:     core.StatusPending,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}

	// 绑定Handler（从registry获取）
	if h, ok := s.registry.Get(req.Name); ok {
		job.Handler = h
	}

	// 添加到调度器
	if err := s.scheduler.Schedule(job); err != nil {
		c.JSON(500, ErrorResponse{
			Code:    500,
			Message: "failed to schedule job",
			Details: err.Error(),
		})
		return
	}

	c.JSON(201, s.toJobResponse(job))
}

// ListJobs 获取任务列表
func (s *Server) ListJobs(c *gin.Context) {
	status := c.Query("status") // 状态过滤
	name := c.Query("name")     // 名称过滤
	limit := 50                 // 默认分页
	if l := c.Query("limit"); l != "" {
		fmt.Sscanf(l, "%d", &limit)
	}

	// 从存储加载所有任务快照
	snapshots, err := s.store.LoadAll()
	if err != nil {
		c.JSON(500, ErrorResponse{
			Code:    500,
			Message: "failed to load jobs",
			Details: err.Error(),
		})
		return
	}

	items := make([]JobResponse, 0)
	for _, snap := range snapshots {
		// 过滤
		if status != "" && fmt.Sprintf("%d", snap.Status) != status {
			continue
		}
		if name != "" && snap.Name != name {
			continue
		}

		// 转换为响应格式
		job := &core.Job{}
		job.FromSnapshot(snap)
		items = append(items, s.toJobResponse(job))

		if len(items) >= limit {
			break
		}
	}

	c.JSON(200, ListJobsResponse{
		Total: len(items),
		Items: items,
	})
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

	// 获取现有任务
	snapshots, _ := s.store.LoadAll()
	var targetSnap core.JobSnapshot
	found := false

	for _, snap := range snapshots {
		if snap.ID == id {
			targetSnap = snap
			found = true
			break
		}
	}

	if !found {
		c.JSON(404, ErrorResponse{
			Code:    404,
			Message: "job not found",
		})
		return
	}

	// 检查状态
	if core.JobStatus(targetSnap.Status) != core.StatusPending {
		c.JSON(409, ErrorResponse{
			Code:    409,
			Message: "job cannot be modified",
			Details: "only pending jobs can be updated",
		})
		return
	}

	// 更新字段
	if req.TriggerAt != nil {
		targetSnap.TriggerAt = *req.TriggerAt
	}
	if req.Payload != nil {
		targetSnap.Payload = req.Payload
	}
	if req.MaxRetries != nil {
		targetSnap.MaxRetries = *req.MaxRetries
	}
	targetSnap.UpdatedAt = time.Now()

	// 删除旧任务并重新调度（因为堆需要重新排序）
	// 这里假设scheduler有Update方法，或者先Cancel再Schedule
	if err := s.scheduler.Cancel(id); err != nil {
		// 可能已经在执行，忽略错误
	}

	// 转换为Job并重新调度
	job := &core.Job{}
	job.FromSnapshot(targetSnap)
	if h, ok := s.registry.Get(job.Name); ok {
		job.Handler = h
	}

	if err := s.scheduler.Schedule(job); err != nil {
		c.JSON(500, ErrorResponse{
			Code:    500,
			Message: "failed to reschedule job",
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

	if h, ok := s.registry.Get(job.Name); ok {
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

	stats := StatsResponse{
		Uptime: time.Since(s.startTime).String(),
	}

	for _, snap := range snapshots {
		switch core.JobStatus(snap.Status) {
		case core.StatusPending:
			stats.Pending++
		case core.StatusRunning:
			stats.Running++
		case core.StatusSuccess:
			stats.Completed++
		case core.StatusFailed:
			stats.Failed++
		}
	}

	// 获取堆大小（活跃任务数）
	// 注意：需要在scheduler中暴露HeapLen方法
	// stats.HeapSize = s.scheduler.HeapLen()

	c.JSON(200, stats)
}

// HealthCheck 健康检查
func (s *Server) HealthCheck(c *gin.Context) {
	c.JSON(200, gin.H{
		"status": "healthy",
		"time":   time.Now().Format(time.RFC3339),
	})
}

// ListJobTypes 获取支持的Job类型
func (s *Server) ListJobTypes(c *gin.Context) {
	c.JSON(200, gin.H{
		"types": s.registry.List(),
	})
}

// POST /api/v1/jobs/batch
func (s *Server) BatchCreateJobs(c *gin.Context) {
	var reqs []CreateJobRequest
	if err := c.ShouldBindJSON(&reqs); err != nil {
		c.JSON(400, ErrorResponse{Code: 400, Message: "invalid batch format"})
		return
	}

	results := make([]JobResponse, 0, len(reqs))
	errors := make([]string, 0)

	for _, _ = range reqs {
		// TODO: 复用单个创建逻辑...
		// 记录成功和失败
	}

	c.JSON(207, gin.H{
		"succeeded": len(results),
		"failed":    len(errors),
		"items":     results,
		"errors":    errors,
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
	status := "unknown"
	switch job.Status {
	case core.StatusPending:
		status = "pending"
	case core.StatusRunning:
		status = "running"
	case core.StatusSuccess:
		status = "success"
	case core.StatusFailed:
		status = "failed"
	case core.StatusCancelled:
		status = "cancelled"
	}

	resp := JobResponse{
		ID:         job.ID,
		Name:       job.Name,
		Status:     status,
		TriggerAt:  job.TriggerAt,
		Payload:    job.Payload,
		RetryCount: job.RetryCount,
		MaxRetries: job.MaxRetries,
		IsRepeat:   job.IsRepeat,
		CronExpr:   job.CronExpr,
		CreatedAt:  job.CreatedAt,
		UpdatedAt:  job.UpdatedAt,
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
