package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"godelayq/core"
)

// Server HTTP API 服务器
type Server struct {
	scheduler *core.Scheduler
	store     core.Store
	registry  *JobRegistry
	engine    *gin.Engine
	wsServer  *core.WSServer
	httpSrv   *http.Server
	port      string
	startTime time.Time

	// baseCtx 传给每个请求；Stop 取消它即可让 SSE 等长连接立即收尾
	baseCtx    context.Context
	baseCancel context.CancelFunc
	mu         sync.Mutex   // 保护 ln 与 shutdown
	ln         net.Listener // Start 成功后有效，ListenAddr 返回真实监听地址
	shutdown   bool
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

	baseCtx, baseCancel := context.WithCancel(context.Background())

	s := &Server{
		scheduler:  scheduler,
		store:      store,
		registry:   NewJobRegistry(),
		engine:     gin.New(),
		wsServer:   core.NewWSServer(scheduler.GetEventBus()),
		port:       port,
		startTime:  time.Now(),
		baseCtx:    baseCtx,
		baseCancel: baseCancel,
	}

	// 持有唯一的 http.Server 实例，Stop 才能真正关闭监听
	s.httpSrv = &http.Server{
		Addr:              fmt.Sprintf(":%s", s.port),
		Handler:           s.engine,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context {
			return s.baseCtx
		},
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
	s.engine.GET("/ws", s.wsServer.Handle)

	// SSE 备选方案（对于不支持WebSocket的客户端）
	s.engine.GET("/sse/events", s.handleSSE)
}

// Start 启动HTTP服务（非阻塞）。监听失败直接返回错误，便于上层回滚。
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.httpSrv.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s failed: %w", s.httpSrv.Addr, err)
	}

	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()

	go func() {
		if err := s.httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Printf("HTTP server error: %v\n", err)
		}
	}()

	fmt.Printf("HTTP API server listening on http://%s\n", s.ListenAddr())
	return nil
}

// ListenAddr 返回实际监听地址（端口为 0 时由系统分配），未启动时返回空串
func (s *Server) ListenAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Stop 优雅关闭：通知长连接退出、停止监听，最后关闭全部 WebSocket 客户端。
// 重复调用安全。
func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	if s.shutdown {
		s.mu.Unlock()
		return nil
	}
	s.shutdown = true
	s.mu.Unlock()

	// 请求上下文随即结束，SSE 等长连接处理器可立即返回
	s.baseCancel()

	err := s.httpSrv.Shutdown(ctx)
	s.wsServer.Stop()

	if err != nil {
		// 超时后强制关闭残留连接
		s.httpSrv.Close()
	}
	return err
}

// 获取端口
func (s *Server) Port() string {
	return s.port
}
