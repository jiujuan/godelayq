package api

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"godelayq/core"
	"github.com/gin-gonic/gin"
)

// Server HTTP API 服务器
type Server struct {
	scheduler *core.Scheduler
	store     core.Store
	registry  *JobRegistry
	engine    *gin.Engine
	port      string
	startTime time.Time

	// wsServer *core.WSServer // Commented out for now
}

// JobRegistry 任务处理器注册表（用于API创建的任务自动绑定Handler）
type JobRegistry struct {
	handlers map[string]core.Handler
	mu       sync.RWMutex
}

func NewJobRegistry() *JobRegistry {
	return &JobRegistry{
		handlers: make(map[string]core.Handler),
	}
}

func (r *JobRegistry) Register(name string, handler core.Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[name] = handler
}

func (r *JobRegistry) Get(name string) (core.Handler, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.handlers[name]
	return h, ok
}

func (r *JobRegistry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.handlers))
	for name := range r.handlers {
		names = append(names, name)
	}
	return names
}

// NewServer 创建API服务器
func NewServer(scheduler *core.Scheduler, store core.Store, port string) *Server {
	if port == "" {
		port = "8080"
	}

	s := &Server{
		scheduler: scheduler,
		store:     store,
		registry:  NewJobRegistry(),
		engine:    gin.New(),
		port:      port,
		startTime: time.Now(),

		// wsServer: core.NewWSServer(scheduler.GetEventBus()),
	}

	s.setupMiddleware()
	s.setupRoutes()

	return s
}

// RegisterJobHandler 注册任务处理器（需要在Start前调用）
func (s *Server) RegisterJobHandler(name string, handler core.Handler) {
	s.registry.Register(name, handler)
	// 同时注册到scheduler（用于从持久化恢复）
	s.scheduler.RegisterHandler(name, handler)
}

func (s *Server) setupMiddleware() {
	// 恢复中间件
	s.engine.Use(gin.Recovery())

	// 日志中间件（自定义格式）
	s.engine.Use(gin.LoggerWithFormatter(func(param gin.LogFormatterParams) string {
		return fmt.Sprintf("%s - [%s] \"%s %s %s %d %s \"%s\" %s\"\n",
			param.ClientIP,
			param.TimeStamp.Format(time.RFC1123),
			param.Method,
			param.Path,
			param.Request.Proto,
			param.StatusCode,
			param.Latency,
			param.Request.UserAgent(),
			param.ErrorMessage,
		)
	}))

	// CORS
	s.engine.Use(func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	})
}

func (s *Server) setupRoutes() {
	api := s.engine.Group("/api/v1")
	{
		// 任务管理
		jobs := api.Group("/jobs")
		{
			jobs.POST("", s.CreateJob)
			jobs.GET("", s.ListJobs)
			jobs.GET("/:id", s.GetJob)
			jobs.PUT("/:id", s.UpdateJob)
			jobs.DELETE("/:id", s.CancelJob)
			jobs.POST("/:id/cancel", s.CancelJob)
			jobs.POST("/:id/retry", s.RetryJob)
			jobs.POST("/batch", s.BatchCreateJobs)
		}

		// 统计与监控
		api.GET("/stats", s.GetStats)
		api.GET("/health", s.HealthCheck)

		// 获取支持的Job类型（用于前端展示）
		api.GET("/job-types", s.ListJobTypes)
	}

	// 404处理
	s.engine.NoRoute(func(c *gin.Context) {
		c.JSON(404, ErrorResponse{
			Code:    404,
			Message: "resource not found",
		})
	})

	// WebSocket 端点
	// s.engine.GET("/ws", s.wsServer.Handle)

	// SSE 备选方案（对于不支持WebSocket的客户端）
	s.engine.GET("/sse/events", s.handleSSE)
}

// Start 启动HTTP服务（非阻塞）
func (s *Server) Start() error {
	addr := fmt.Sprintf(":%s", s.port)

	// s.wsServer.Start() // 启动WebSocket管理

	// 使用http.Server支持优雅关闭
	srv := &http.Server{
		Addr:    addr,
		Handler: s.engine,
	}

	// 在后台启动
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Printf("HTTP server error: %v\n", err)
		}
	}()

	fmt.Printf("HTTP API server listening on http://localhost%s\n", addr)
	return nil
}

// Stop 优雅关闭
func (s *Server) Stop(ctx context.Context) error {
	srv := &http.Server{Addr: fmt.Sprintf(":%s", s.port)}
	return srv.Shutdown(ctx)
}

// 获取端口
func (s *Server) Port() string {
	return s.port
}
