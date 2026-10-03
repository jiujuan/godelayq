package api

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"godelayq/core"
	"godelayq/executor"
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
	// profileStore 是档位文件（executors.profiles_path）的读写口；profiles 是
	// "文件 → 登记表 → 调度器"的同步器。两个字段成对注入，缺任何一个都意味着
	// 写端点没法守住"落盘先于生效"的顺序，因此一起判、一起 503（见 requireExecutorProfiles）。
	// 未注入是默认状态：executors.web_enabled 默认 false。
	profileStore executorProfileStore
	profiles     executorProfileApplier
	// executors 是执行器档位登记表；nil 表示这次部署没装配执行器（开关关闭，
	// 或测试直接构造 Server）。读它的是 GET /executors 与结果端点的预览上限，
	// 未注入时不报错：登记表在开关关闭时也是非 nil 的空表，"没装配"是默认状态。
	executors *executor.Registry
	// executorAuthWarn 守住"未启用鉴权时提交执行器任务"那条 warn 只记一次（TASK-E16 §3.1.3）。
	// 它是每个 Server 一份而不是包级变量：测试里一批 Server 各自提交，
	// 包级 Once 会让第二台以后的部署一条都不记，那条用例就成了碰运气。
	executorAuthWarn sync.Once
	// artifacts 是执行输出的文件存储；nil 表示这次部署没有产物文件可读
	// （executors.enabled=false，或测试直接构造 Server）。
	// GET /jobs/:id/result 缺它时返回 503，因为"读不到正文"与"正文是空的"必须区分开。
	artifacts *executor.ArtifactStore
	// history 是事件总线的内存订阅者，为详情页时间线与 Dashboard 提供最近事件
	history *EventHistory
	// events 是事件时间线的持久化读取方；nil 表示这次部署没装配事件库，
	// 两个事件端点退回 history（即 S03 之前的行为）。
	// 它定义在消费方而不是由 store/sqlite 传一个具体类型进来：api 包不许碰驱动。
	events eventReader
	// auditLog 是写操作台账的写入方；nil 表示这次部署没装配观测库（或关了审计子开关），
	// 中间件退回只记一行结构化日志。它与 events 不同——那一个是读路径，这一个两端都用。
	auditLog AuditRecorder
	// auditRead 是台账的查询方，只给 GET /admin/audit 用。
	// 它与 auditLog 分成两个字段而不是一个接口：装配方可以只给写不给读。
	auditRead AuditReader
	// console 是嵌入的前端产物根；nil 表示这次部署只提供 API（开发形态）。
	// 它同时决定鉴权中间件是否豁免静态资源——登录页本身也是产物的一部分。
	console fs.FS
	// reloadState 是配置热重载最近一次结论的读口；nil 表示这台服务器没启用热重载，
	// /admin/runtime 于是连 reload 这个键都不给（见 RuntimeResponse.Reload）。
	// 注入方是 cmd/server 的那个读口句柄，它自己转发给 core.ConfigWatcher。
	// 这里只有读面：热重载只由文件变化触发，本卡不新增任何写端点。
	reloadState ReloadStateReader
	// reloadEnabled 是**进程配置**里的 reload.enabled，与 reloadState 一起注入。
	// 它单独存一份而不是取 State().Enabled：读数要说的是"这台进程的配置开关是什么"，
	// 而"开关开着、监听器没建起来"那种部署的 State() 里那位是假的（R05 的 D-R0503）。
	reloadEnabled bool

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

// WithExecutorProfileStore 注入档位文件的读写口，与 WithExecutorProfileApplier 成对使用：
// 三个写端点需要两者齐备（store 落盘、applier 生效），少给一个就是 503（设计文档 §6.6）。
// 单独注入它没有意义——写路径的每一端都要同时在场。
func WithExecutorProfileStore(store executorProfileStore) Option {
	return func(s *Server) { s.profileStore = store }
}

// WithExecutorProfileApplier 注入"把档位文件重新生效"的那一步（*executor.Applier）。
// 未注入时 /api/v1/executors/profiles 各端点返回 503，读端点 GET /executors 不受影响。
func WithExecutorProfileApplier(applier executorProfileApplier) Option {
	return func(s *Server) { s.profiles = applier }
}

// WithExecutorRegistry 注入执行器档位登记表。传 nil 与不注入等价：
// 登记表在开关关闭时也是非 nil 的空表，所以"没装配"只有测试直接构造 Server 那一种情况。
// 读取方是 GET /executors 与结果端点的预览上限，两者都把 nil 当默认状态处理，不返回 503。
func WithExecutorRegistry(reg *executor.Registry) Option {
	return func(s *Server) { s.executors = reg }
}

// WithArtifacts 注入执行输出的文件存储。传 nil 与不注入等价：
// /jobs/:id/result 会明确返回 503，而不是回一份"看起来是空输出"的结果。
func WithArtifacts(store *executor.ArtifactStore) Option {
	return func(s *Server) { s.artifacts = store }
}

// eventReader 是事件时间线的持久化读取方，由 store/sqlite.EventLog 实现。
// 两个方法都按写入顺序升序返回；nil 表示这次部署没装配事件库，
// GET /jobs/:id/events 与 GET /events 退回内存缓冲（api.EventHistory）。
//
// 接口定义在这里而不是拿具体类型：与 groups、artifacts 一样按能力声明，装配方负责给实现，
// api 包因此不需要知道事件库用的是哪种驱动。
type eventReader interface {
	Events(jobID string, limit int) ([]core.Event, error)
	Recent(limit int) ([]core.Event, error)
}

// WithEventLog 注入持久化的事件读取方。传 nil 与不注入等价，两者都走内存缓冲。
//
// 注入之后两个端点只读库、不与内存合并（设计文档 D5）：批量写入的可见性延迟上界是一个
// flush_interval，而跨来源去重与定序的复杂度要用它换。响应里的 note 会跟着换成
// "取自持久化事件库"那句，读的人知道自己在看什么。
func WithEventLog(r eventReader) Option {
	return func(s *Server) { s.events = r }
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

	// 是否带控制台只在这一处可见：没有这个日志，"我是不是编了个不带前端的二进制"
	// 只能靠访问 / 看是 404 还是页面来猜。
	if s.console != nil {
		logger.Info("embedded console enabled", "mount", "/")
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

	// 审计中间件在鉴权之后：身份是 authMiddleware 用 c.Set 放进去的，早于它就取不到 who。
	// 代价是它自己拒掉的 401 不经过这里（那些尝试仍在访问日志里，见 api/audit.go 的注释）。
	s.engine.Use(s.auditMiddleware())
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
			// 产物列表读的是索引表里的元信息，不含输出正文，所以档位与事件端点同为 reader；
			// 正文端点 GetJobResult 的门槛更严（带 secret 参数的档位会把读取门槛提到提交档位）。
			jobs.GET("/:id/artifacts", reader, s.requireIndex(), s.ListJobArtifacts)
			// 这三条写端点的路由档位是 operator，但 exec.* 任务要更高的档位：
			// 那层判定在请求体解析之后才能做（要看任务名是不是档位），所以落在处理器里，
			// 位置见 api/handlers_executors.go 的 gateExecutorSubmission（TASK-E16 §3.1）。
			// 只读 setupRoutes 就断言"建任务只要 operator"是不完整的结论。
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

			// 执行输出正文。判档在先、依赖检查在后：未认证的连接不该从状态码里
			// 读出"这次部署装没装产物存储"。
			// reader 是下限：档位声明了 secret 参数时，处理器会把它升到 executors.required_role
			// （TASK-E16 §3.3 第 2 条，判档位置见 GetJobResult 里的 resultGuard）。
			jobs.GET("/:id/result", reader, s.requireArtifacts(), s.GetJobResult)
		}

		// 执行器档位列表。未注入登记表时它回 {"enabled":false,"profiles":[]}：
		// 执行器默认关闭，那是默认状态而不是错误状态，所以这里没有 503 守卫。
		api.GET("/executors", reader, s.ListExecutors)

		// 档位的在线管理（查增改删）。两道门槛叠在一起：
		//   - 路由分组上的 requireExecutorProfiles 管"这台让不让在线改"；
		//   - RequireRole(ops) 管"这个身份够不够"——比删组（admin）更高一档，
		//     因为它改的是"这台机器能执行什么"（设计文档 D10）。
		// 提交 exec.* 任务的门槛是另一件事（executors.required_role，全局一份），两条判定互不替代。
		profiles := api.Group("/executors/profiles", s.requireExecutorProfiles())
		{
			profiles.GET("/:name", s.RequireRole(core.RoleOps), s.GetExecutorProfile)
			profiles.POST("", s.RequireRole(core.RoleOps), s.CreateExecutorProfile)
			profiles.PUT("/:name", s.RequireRole(core.RoleOps), s.UpdateExecutorProfile)
			profiles.DELETE("/:name", s.RequireRole(core.RoleOps), s.DeleteExecutorProfile)
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
			// 台账的读端点。档位沿用本组的 ops（行内含账号名与拒绝原因，
			// 比运行诊断更敏感），没装配观测库时由 requireAudit 明确回 503。
			admin.GET("/audit", s.requireAudit(), s.GetAudit)
		}

		// 统计与监控。/health 继续保持"启用鉴权则需凭据"的历史契约
		// （docs/api.md 现有描述），控制台的登录页正是靠它的 401/200 判断鉴权是否开启。
		api.GET("/stats", reader, s.GetStats)
		api.GET("/health", reader, s.HealthCheck)

		// 获取支持的Job类型（用于前端展示）
		api.GET("/job-types", reader, s.ListJobTypes)
	}

	// 404 处理。嵌入了前端产物时先给 SPA 一次兜底的机会（SPA 深链与静态资源
	// 都没有注册路由，全部落在这里），判定条件与鉴权豁免共用 consoleRequest。
	s.engine.NoRoute(func(c *gin.Context) {
		if s.consoleRequest(c) {
			s.serveConsole(c)
			return
		}
		c.JSON(http.StatusNotFound, ErrorResponse{
			Code:    http.StatusNotFound,
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
