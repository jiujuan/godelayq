package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"godelayq/executor"
)

// AuditEntry 是一行写操作台账，字段与 store/sqlite 的 write_audit 列一一对应。
//
// 这张表从构造上就没有请求体：结构里没有 payload 字段，也没有能装下它的位置。
// 后来人若想"顺手加一个 body 列"，请先回到设计文档 §10 第 1 条——参数取值曾经出现在
// 校验错误的文本里，而执行侧的既有规范是"档位名可以外泄，参数值不可以"。
type AuditEntry struct {
	Time      time.Time
	Actor     string
	ActorKind string
	Role      string
	Action    string
	Method    string
	Route     string
	Status    int
	Latency   time.Duration
	Verdict   string

	// 执行器补充列：只有走过提交期判定的请求才有值，普通任务提交一律为空。
	ExecVerdict    string
	ExecReasonCode string
	HandlerKey     string
	Profile        string
	JobID          string
	RemoteIP       string
	UserAgent      string
}

// actor_kind 的三个取值。machine 与 anonymous 是从身份名与鉴权开关推出来的，
// 不是配置里能填的东西。
const (
	auditActorUser      = "user"
	auditActorMachine   = "machine"
	auditActorAnonymous = "anonymous"
)

// verdict 的封闭集：由 HTTP 状态码派生，不进表的内容（响应体、错误原文）一律不反映在这里。
const (
	auditVerdictOK         = "ok"
	auditVerdictBadRequest = "bad_request"
	auditVerdictDenied     = "denied"
	auditVerdictNotFound   = "not_found"
	auditVerdictConflict   = "conflict"
	auditVerdictPartial    = "partial"
	auditVerdictThrottled  = "throttled"
	auditVerdictError      = "error"
	auditVerdictOther      = "other"
)

// exec_verdict 的封闭集，与设计文档 §6.3 的列注释同一份取值。
const (
	auditExecAccepted           = "accepted"
	auditExecRoleDenied         = "role_denied"
	auditExecProfileUnavailable = "profile_unavailable"
	auditExecPayloadRejected    = "payload_rejected"
	auditExecTimeoutRejected    = "timeout_rejected"
)

// exec_reason 是"哪一道判定给出的这个结论"。它与 exec_verdict 今天基本一一对应，
// 只有一处例外带额外信息：身份不够时这里记的是要求达到的档位（operator|admin|ops），
// 于是"这台机器上周被谁按 required_role 拒掉"能一次查出来。
//
// 想把它做细（比如 payload 到底错在哪个键）就得解析 executor 的错误文本，
// 而那些文本里可能出现参数取值——设计文档 D7 禁止把它存进表，所以不做。
const (
	auditReasonPayload  = "payload_invalid"
	auditReasonProfile  = "profile_unavailable"
	auditReasonTimeout  = "timeout_beyond_profile"
	auditReasonAccepted = ""
)

// auditActionUnmatched 与 auditActionOther 是映射表没覆盖到时的两个落点：
// 前者说明这条路径压根没有路由，后者说明 setupRoutes 加了写路由而这张表没跟上。
const (
	auditActionUnmatched = "unmatched"
	auditActionOther     = "other"
)

// auditUserAgentLimit 截断 User-Agent 的长度。这一列是客户端自报的，
// 不给它上界等于让一个 curl -A 就能把台账行撑大。
const auditUserAgentLimit = 256

// auditActions 把"方法 + gin 路由模板"映射到封闭的动作词。
//
// 这张表与 api/server.go 的 setupRoutes 是两处，所以要靠 §6 第 4 条那条用例证明
// 漏配时落进 other 而不是静默丢行。键里带方法是因为 FullPath() 不含方法，
// 而 /jobs/:id 的 PUT 与 DELETE 是两个动作。
var auditActions = map[string]string{
	http.MethodPost + " /api/v1/auth/login":           "auth.login",
	http.MethodPost + " /api/v1/auth/refresh":         "auth.refresh",
	http.MethodPost + " /api/v1/auth/logout":          "auth.logout",
	http.MethodPost + " /api/v1/auth/ws-ticket":       "auth.ws_ticket",
	http.MethodPost + " /api/v1/jobs":                 "job.create",
	http.MethodPut + " /api/v1/jobs/:id":              "job.update",
	http.MethodDelete + " /api/v1/jobs/:id":           "job.cancel",
	http.MethodPost + " /api/v1/jobs/:id/cancel":      "job.cancel",
	http.MethodPost + " /api/v1/jobs/:id/retry":       "job.retry",
	http.MethodPost + " /api/v1/jobs/:id/pause":       "job.pause",
	http.MethodPost + " /api/v1/jobs/:id/resume":      "job.resume",
	http.MethodPost + " /api/v1/jobs/:id/force-pause": "job.force_pause",
	http.MethodPost + " /api/v1/jobs/batch":           "job.batch_create",
	http.MethodPost + " /api/v1/jobs/batch-ops":       "job.batch_op",
	http.MethodPost + " /api/v1/groups":               "group.create",
	http.MethodPut + " /api/v1/groups/:name":          "group.update",
	http.MethodDelete + " /api/v1/groups/:name":       "group.delete",
	// 档位的在线管理（TASK-W06）。这三个动作改的是"这台机器能执行什么"，
	// 比建任务更值得留痕；台账里每一次写请求都会落到上面某个动作词。
	http.MethodPost + " /api/v1/executors/profiles":         "executor.profile_create",
	http.MethodPut + " /api/v1/executors/profiles/:name":    "executor.profile_update",
	http.MethodDelete + " /api/v1/executors/profiles/:name": "executor.profile_delete",
	http.MethodPost + " /api/v1/admin/scheduler/suspend":    "admin.scheduler_suspend",
	http.MethodPost + " /api/v1/admin/scheduler/unsuspend":  "admin.scheduler_unsuspend",
	http.MethodDelete + " /api/v1/admin/events":             "admin.events_clear",
}

// AuditVerdicts 是 verdict 列的全部合法取值，供查询端点校验 ?verdict= 用。
// 导出的理由就是这个用途：映射与校验必须读同一份列表，否则加一个取值要改两处。
var AuditVerdicts = []string{
	auditVerdictOK, auditVerdictBadRequest, auditVerdictDenied, auditVerdictNotFound,
	auditVerdictConflict, auditVerdictPartial, auditVerdictThrottled, auditVerdictError,
	auditVerdictOther,
}

// AuditActions 是 action 列的全部合法取值（含两个兜底值），同样供查询端点校验。
func AuditActions() []string {
	values := make([]string, 0, len(auditActions)+2)
	seen := map[string]bool{}
	for _, action := range auditActions {
		if seen[action] {
			continue
		}
		seen[action] = true
		values = append(values, action)
	}
	return append(values, auditActionUnmatched, auditActionOther)
}

// auditExecutorInfo 是提交期判定留在 gin 上下文里的结论。
// Profile 为 nil 表示这次判定发生在认出档位之前（那种情况下也没有可登记的档位名）。
type auditExecutorInfo struct {
	Verdict    string
	ReasonCode string
	HandlerKey string
	Profile    string
}

const (
	auditExecutorKey = "godelayq.audit_executor"
	auditJobKey      = "godelayq.audit_job_id"
	auditBatchKey    = "godelayq.audit_batch"
)

// markAuditBatch 声明"这个请求是批量端点"，于是执行器结论与任务 ID 都不再往上下文里挂。
//
// 理由是设计文档 §6.3 那条口径：一行对应一个 HTTP 请求，而批量里的逐条结论各不相同。
// 让最后一条的结论落在这个请求的行上，读的人会把"这一条被拒了"看成"这次批量提交被拒了"，
// 而响应体里的 errors 才是真相。所以这里选择留空，不是漏填。
func markAuditBatch(c *gin.Context) {
	c.Set(auditBatchKey, true)
}

// stashAuditExecutor 把一个提交期结论挂到当前请求上，由审计中间件在 c.Next() 之后读出。
//
// 挂在这里而不是让中间件去猜状态码：判定与结论必须出自同一次执行，
// 而 403 也可能是路由档位挡的（那不该带执行器列）。
func stashAuditExecutor(c *gin.Context, verdict, reason string, profile *executor.Profile) {
	if c.GetBool(auditBatchKey) {
		return
	}
	info := auditExecutorInfo{Verdict: verdict, ReasonCode: reason}
	if profile != nil {
		info.HandlerKey = profile.HandlerKey()
		info.Profile = profile.Name
	}
	c.Set(auditExecutorKey, info)
}

// stashAuditJobID 在创建成功后记下任务 ID，让台账能说"这次提交产出了哪条任务"。
//
// 只有这一处填：路径参数里的 :id 是客户端给的任意文本，拿它填这一列等于给台账
// 开了一个无界的、由请求方决定的字段。
func stashAuditJobID(c *gin.Context, jobID string) {
	if c.GetBool(auditBatchKey) {
		return
	}
	c.Set(auditJobKey, jobID)
}

// AuditRecorder 是台账的写入能力。nil 表示未装配，此时中间件退回只记结构化日志。
//
// 与产物索引同一取向：这一行的写入失败不影响请求，台账缺行只意味着查不到，
// 而响应与访问日志仍然完整。
type AuditRecorder interface {
	Append(entry AuditEntry) error
}

// AuditReader 是台账的查询能力，供 GET /admin/audit 用。
// 返回的第二个值是匹配总数（不带 limit 的那一个），端点把它原样透出成分页依据。
type AuditReader interface {
	Query(f AuditFilter) ([]AuditEntry, int, error)
}

// AuditFilter 是台账查询的条件。零值字段表示不按该项过滤。
// Since/Until 是闭区间还是开区间由实现决定，见 store/sqlite 一侧的 Query 注释。
type AuditFilter struct {
	Actor   string
	Action  string
	Verdict string
	Since   *time.Time
	Until   *time.Time
	Limit   int
	Offset  int
}

// WithAuditLog 注入台账的写与读两面。传 nil 与不注入等价。
//
// 分成两个参数而不是一个接口：装配方可以只给写、不给读（未启用观测层的部署里
// 中间件照常记日志，而端点明确 503）。
func WithAuditLog(w AuditRecorder, r AuditReader) Option {
	return func(s *Server) {
		s.auditLog = w
		s.auditRead = r
	}
}

// auditMiddleware 把所有写操作登记成一行台账。注册在鉴权之后，因为身份由 authMiddleware 放进去。
//
// 一条需要注意的边界：authMiddleware 自己拒掉的请求（401）不经过这里——它在鉴权下游。
// 那些尝试仍然完整出现在访问日志里（api/logging.go 的 requestLogger 注册在鉴权之前），
// 所以"谁在试"这件事没有丢，只是不落这张表。
//
// 只审计写方法：GET 量大且没有权限变更含义（本卡 §8），实时通道（GET /ws、GET /sse/events）
// 因此根本不进这条路径，c.Writer.Status() 在长连接上语义不明确的顾虑也就不存在——
// 后来人不要为 WS 加特例。
func (s *Server) auditMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodPost, http.MethodPut, http.MethodDelete:
		default:
			c.Next()
			return
		}

		// 开始时间必须在 c.Next() 之前取；其余字段一律在之后读，
		// 因为 FullPath() 与状态码要到处理器跑完才是终值。
		started := time.Now()
		c.Next()

		s.recordAudit(s.buildAuditEntry(c, started))
	}
}

// buildAuditEntry 在请求处理结束后组装一行台账。
func (s *Server) buildAuditEntry(c *gin.Context, started time.Time) AuditEntry {
	route := c.FullPath()
	action, ok := auditActions[c.Request.Method+" "+route]
	if !ok {
		if route == "" {
			action = auditActionUnmatched
		} else {
			// 路由存在但映射表没跟上：宁可记成 other 并留一行 debug，
			// 也不要静默丢行——丢行在台账里与"没发生过"看起来一样。
			action = auditActionOther
			s.logger.Debug("write route has no audit action mapping",
				"method", c.Request.Method, "route", route)
		}
	}

	actor, kind, role := s.auditIdentity(c)
	entry := AuditEntry{
		Time:      started,
		Actor:     actor,
		ActorKind: kind,
		Role:      role,
		Action:    action,
		Method:    c.Request.Method,
		Route:     route,
		Status:    c.Writer.Status(),
		Latency:   time.Since(started),
		RemoteIP:  c.ClientIP(),
		UserAgent: truncateRunes(c.Request.UserAgent(), auditUserAgentLimit),
	}
	entry.Verdict = auditVerdict(entry.Status)

	if info, ok := c.Get(auditExecutorKey); ok {
		if exec, ok := info.(auditExecutorInfo); ok {
			entry.ExecVerdict = exec.Verdict
			entry.ExecReasonCode = exec.ReasonCode
			entry.HandlerKey = exec.HandlerKey
			entry.Profile = exec.Profile
		}
	}
	if value, ok := c.Get(auditJobKey); ok {
		if jobID, ok := value.(string); ok {
			entry.JobID = jobID
		}
	}
	return entry
}

// auditIdentity 给出台账用的身份三列。
//
// 未启用鉴权的部署里 actor 是空而不是 "anonymous"：那时 Principal 的名字是框架合成的，
// 记下它只会让台账看起来有一个并不存在的账号。档位照原样记（合成的是 ops），
// 因为"这次部署里任何人都等于 ops"本身就是运维要看的事实。
//
// role 记的是身份自己的档位名，machine 就写 machine——它的 operator 等效性由
// core.Role.rank 折算得出，而把折算值写进这一列会抹掉"这是静态凭据"这个信息，
// 只靠 actor_kind 一列区分反而更绕。
func (s *Server) auditIdentity(c *gin.Context) (actor, kind, role string) {
	principal, ok := PrincipalFrom(c)
	if !ok {
		return "", auditActorAnonymous, ""
	}
	role = principal.Role.String()

	switch {
	case !s.sec.authEnabled():
		return "", auditActorAnonymous, role
	case principal.Name == machinePrincipalName:
		return principal.Name, auditActorMachine, role
	case principal.Name == anonymousName:
		return "", auditActorAnonymous, role
	default:
		return principal.Name, auditActorUser, role
	}
}

// auditVerdict 把状态码折进封闭集。批量端点的 207 必须在 2xx 之前判，
// 否则混合结果会被读成成功。
func auditVerdict(status int) string {
	switch {
	case status == http.StatusMultiStatus:
		return auditVerdictPartial
	case status >= 200 && status < 300:
		return auditVerdictOK
	case status == http.StatusBadRequest:
		return auditVerdictBadRequest
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return auditVerdictDenied
	case status == http.StatusNotFound:
		return auditVerdictNotFound
	case status == http.StatusConflict:
		return auditVerdictConflict
	case status == http.StatusTooManyRequests:
		return auditVerdictThrottled
	case status >= 500:
		return auditVerdictError
	default:
		return auditVerdictOther
	}
}

// recordAudit 写台账，或在未装配时记那一行结构化日志。
//
// 未装配时记的这行与表同形（含 action/verdict/执行结论），而不是复用访问日志那行：
// requestLogger 已经为每个请求记了 method/path/status/who，它没有的是这张表的意义所在——
// 封闭集的动作与结论。所以这一行是台账的日志形态，不是它的重复。
func (s *Server) recordAudit(entry AuditEntry) {
	if s.auditLog == nil {
		s.logger.Info("write operation audited", auditLogAttrs(entry)...)
		return
	}
	if err := s.auditLog.Append(entry); err != nil {
		s.logger.Warn("write operation audit row was rejected",
			"action", entry.Action, "actor", entry.Actor, "error", err)
	}
}

// auditLogAttrs 把一行台账摊成 slog 属性。键名与表列同名，
// 这样"日志里的那行"与"库里的那行"能逐字对照。
func auditLogAttrs(entry AuditEntry) []any {
	attrs := []any{
		"who", entry.Actor, "kind", entry.ActorKind, "role", entry.Role,
		"action", entry.Action, "method", entry.Method, "route", entry.Route,
		"status", entry.Status, "verdict", entry.Verdict,
		"latency_ms", float64(entry.Latency.Microseconds()) / 1000.0,
		"client_ip", entry.RemoteIP,
	}
	if entry.ExecVerdict != "" {
		attrs = append(attrs, "exec_verdict", entry.ExecVerdict, "exec_reason", entry.ExecReasonCode)
	}
	if entry.HandlerKey != "" {
		attrs = append(attrs, "handler_key", entry.HandlerKey, "profile", entry.Profile)
	}
	if entry.JobID != "" {
		attrs = append(attrs, "job_id", entry.JobID)
	}
	return attrs
}

// truncateRunes 按字符而不是字节截断：User-Agent 里的非 ASCII 截在字节中间会留下坏字符。
func truncateRunes(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
