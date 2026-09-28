package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	engine    *gin.Engine
	wsServer  *core.WSServer
	httpSrv   *http.Server
	port      string
	sec       Security
	startTime time.Time
	// logger 访问日志与服务器生命周期日志
	logger *slog.Logger

	// auth 是控制台账号的认证器；nil 表示只配了机器凭据或完全没配鉴权
	auth *Authenticator
	// tokens 承载登录态的服务端部分：refresh 表 / jti 拒绝表 / 一次性 ticket
	tokens *authStore
	// loginLimit 给登录端点做双维度（IP+账号）失败计数：
	// bcrypt 单次约 60-100ms，不限流的登录端点等于一个免费的重量级 DoS 开关
	loginLimit *loginLimiter
	// authErr 推迟到 Start 才报错：构造认证器要校验 bcrypt 哈希，失败必须让进程起不来，
	// 但 NewServer 的签名不带 error（调用方遍布测试），因此在监听前统一抛出。
	authErr error

	// groups 是分组注册表；nil 表示这次部署没装配分组存储，
	// /api/v1/groups 各端点据此返回 503（任务的 group 标签不受影响，
	// 它本来就存在任务快照里）。
	groups core.GroupStore
	// history 是事件总线的内存订阅者，为详情页时间线与 Dashboard 提供最近事件
	history *EventHistory

	// baseCtx 传给每个请求；Stop 取消它即可让 SSE 等长连接立即收尾
	baseCtx    context.Context
	baseCancel context.CancelFunc
	mu         sync.Mutex   // 保护 ln 与 shutdown
	ln         net.Listener // Start 成功后有效，ListenAddr 返回真实监听地址
	shutdown   bool
}

// Option 是 NewServer 的可选依赖，形态与 core.NewScheduler 的 opts 一致：
// 每加一个依赖就改一次位置参数列表，会连带惊动十几处测试调用。
type Option func(*Server)

// WithGroupStore 注入分组注册表。未注入时分组端点返回 503，
// 但任务上的 group 标签照常读写——两者是两份数据，不要混用。
func WithGroupStore(store core.GroupStore) Option {
	return func(s *Server) { s.groups = store }
}

// NewServer 创建API服务器。sec 为零值时不鉴权、接受任意跨域来源；
// logger 为 nil 时使用 slog.Default()。
func NewServer(scheduler *core.Scheduler, store core.Store, port string, sec Security, logger *slog.Logger, opts ...Option) *Server {
	if port == "" {
		port = "8080"
	}
	if logger == nil {
		logger = slog.Default()
	}

	baseCtx, baseCancel := context.WithCancel(context.Background())

	s := &Server{
		scheduler: scheduler,
		store:     store,
		engine:    gin.New(),
		wsServer: core.NewWSServer(scheduler.GetEventBus(), newWSUpgrader(),
			core.WithAllowedOrigins(sec.AllowOrigins...),
			core.WithLogger(logger)),
		port:       port,
		sec:        sec,
		startTime:  time.Now(),
		logger:     logger,
		tokens:     newAuthStore(),
		loginLimit: newLoginLimiter(),
		baseCtx:    baseCtx,
		baseCancel: baseCancel,
	}

	// 账号配置有问题（哈希格式、cost、缺密钥）必须挡住启动：
	// 否则鉴权看似开启，实际所有登录都失败，运维只在现场发现。
	auth, err := NewAuthenticator(sec.Auth)
	if err != nil {
		s.authErr = err
		logger.Error("authentication is configured but cannot be initialized", "error", err)
	}
	s.auth = auth
	if auth != nil {
		logger.Info("console accounts loaded", "count", len(auth.accountNames()))
	}

	for _, opt := range opts {
		opt(s)
	}
	// 事件历史跟着服务器起：订阅在构造时建立，服务器活着期间的第一个事件就不会漏。
	// 上限是常量（api/history.go），运维要看的只是"缓冲占用多少"，不配 knob。
	s.history = NewEventHistory(scheduler.GetEventBus())

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

// RegisterJobHandler 注册任务处理器（需要在Start前调用）。
// 注册表只在调度器那一份，HTTP 层只是转发入口。
func (s *Server) RegisterJobHandler(name string, handler core.Handler) {
	s.scheduler.RegisterHandler(name, handler)
}

func (s *Server) setupMiddleware() {
	// 恢复中间件（panic 连同堆栈写入 slog）
	s.engine.Use(recovery(s.logger))

	// 访问日志（同进程日志共用级别与格式）
	s.engine.Use(requestLogger(s.logger))

	// CORS（同时负责预检，故必须早于鉴权中间件）
	s.engine.Use(corsMiddleware(s.sec))

	// 鉴权覆盖全部端点，含 /ws 与 /sse/events；预检请求已由上面的 CORS 短路。
	// 未配置 token 时该中间件直接放行。
	s.engine.Use(s.authMiddleware())
}

func (s *Server) setupRoutes() {
	api := s.engine.Group("/api/v1")
	{
		// 认证：login/refresh 由 authMiddleware 的显式白名单放行（见 publicAuthEndpoints），
		// 其余都要带有效凭据
		auth := api.Group("/auth")
		{
			auth.POST("/login", s.Login)
			auth.POST("/refresh", s.Refresh)
			auth.POST("/logout", s.Logout)
			auth.GET("/me", s.Me)
			auth.POST("/ws-ticket", s.WsTicket)
		}

		// 只读端点：任何已认证身份都够用（machine 凭据也在内）
		reader := s.RequireRole(core.RoleViewer)
		// 任务写操作：operator 档及以上；machine 静态凭据的档位等同 operator
		operator := s.RequireRole(core.RoleOperator)

		// 任务管理：读放行宽，写要求 operator 档
		jobs := api.Group("/jobs")
		{
			jobs.GET("", reader, s.ListJobs)
			jobs.GET("/:id", reader, s.GetJob)
			jobs.POST("", operator, s.CreateJob)
			jobs.PUT("/:id", operator, s.UpdateJob)
			jobs.DELETE("/:id", operator, s.CancelJob)
			jobs.POST("/:id/cancel", operator, s.CancelJob)
			jobs.POST("/:id/retry", operator, s.RetryJob)
			jobs.POST("/batch", operator, s.BatchCreateJobs)

			// 生命周期：暂停/恢复归 operator，强制暂停会中止执行，只有 admin 以上
			jobs.POST("/:id/pause", operator, s.PauseJob)
			jobs.POST("/:id/resume", operator, s.ResumeJob)
			jobs.POST("/:id/force-pause", s.RequireRole(core.RoleAdmin), s.ForcePauseJob)
			jobs.GET("/:id/events", reader, s.GetJobEvents)
			jobs.POST("/batch-ops", operator, s.BatchJobOps)
		}

		// 全局最近事件：Dashboard 刷新后补历史用
		api.GET("/events", reader, s.ListRecentEvents)

		// 分组注册表。未 WithGroupStore 的部署直接 503，任务上的 group 标签不受影响。
		groups := api.Group("/groups", s.requireGroupStore())
		{
			groups.GET("", reader, s.ListGroups)
			groups.POST("", operator, s.CreateGroup)
			groups.PUT("/:name", operator, s.UpdateGroup)
			// 删除是唯一会牵动一批任务的操作（默认 detach 解除分组），
			// 因此档位比创建/改名更高（决策 D5）
			groups.DELETE("/:name", s.RequireRole(core.RoleAdmin), s.DeleteGroup)
		}

		// 运维端点：改的是整个进程的行为（调度总开关、缓冲清理）或暴露内部占用，
		// 只给 ops 档
		admin := api.Group("/admin", s.RequireRole(core.RoleOps))
		{
			admin.GET("/runtime", s.GetRuntime)
			admin.POST("/scheduler/suspend", s.SuspendScheduler)
			admin.POST("/scheduler/unsuspend", s.UnsuspendScheduler)
			admin.DELETE("/events", s.ClearEventHistory)
		}

		// 统计与监控。/health 继续保持"启用鉴权则需凭据"的历史契约
		// （docs/api.md 现有描述），控制台的登录页正是靠它的 401/200 判断鉴权是否开启。
		api.GET("/stats", reader, s.GetStats)
		api.GET("/health", reader, s.HealthCheck)

		// 获取支持的Job类型（用于前端展示）
		api.GET("/job-types", reader, s.ListJobTypes)
	}

	// 404处理
	s.engine.NoRoute(func(c *gin.Context) {
		c.JSON(404, ErrorResponse{
			Code:    404,
			Message: "resource not found",
		})
	})

	// 实时通道与 REST 同一套身份要求（viewer 档）：
	// 事件流里带着任务名与 payload 摘要，不该让未认证连接旁听。
	// 控制台走 ?ticket=，脚本沿用 ?token=（见 api/security.go 的通道说明）。
	reader := s.RequireRole(core.RoleViewer)

	// WebSocket 端点：core.WSServer 只认 http，升级请求由 gin 转发
	s.engine.GET("/ws", reader, func(c *gin.Context) {
		s.wsServer.Handle(c.Writer, c.Request)
	})

	// SSE 备选方案（对于不支持WebSocket的客户端）
	s.engine.GET("/sse/events", reader, s.handleSSE)
}

// Start 启动HTTP服务（非阻塞）。监听失败直接返回错误，便于上层回滚。
func (s *Server) Start() error {
	// 鉴权配置坏掉时绝不带着"半开"的权限体系上线
	if s.authErr != nil {
		return fmt.Errorf("authentication is misconfigured: %w", s.authErr)
	}

	ln, err := net.Listen("tcp", s.httpSrv.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s failed: %w", s.httpSrv.Addr, err)
	}

	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()

	go func() {
		if err := s.httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Error("http server error", "error", err)
		}
	}()

	s.logger.Info("http api server listening", "addr", s.ListenAddr())
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
	// 撤掉事件订阅：drain 协程随之退出，测试里服务器停掉后不会再碰内存缓冲
	s.history.Stop()

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
