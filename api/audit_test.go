package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
)

// auditCapture 是台账读写两面的替身：中间件用例只看自己被怎么调用，
// 真库的写入与查询在 store/sqlite 有用例。
type auditCapture struct {
	entries []AuditEntry
	err     error

	// 查询面的返回值与收到的过滤条件
	queryItems  []AuditEntry
	queryTotal  int
	queryErr    error
	queryFilter AuditFilter
	queryCalls  int
}

func (a *auditCapture) Append(entry AuditEntry) error {
	if a.err != nil {
		return a.err
	}
	a.entries = append(a.entries, entry)
	return nil
}

func (a *auditCapture) Query(f AuditFilter) ([]AuditEntry, int, error) {
	a.queryCalls++
	a.queryFilter = f
	if a.queryErr != nil {
		return nil, 0, a.queryErr
	}
	return a.queryItems, a.queryTotal, nil
}

// only 取出唯一一行；不是恰好一行就让用例停在这里，
// 因为"多记一行"与"少记一行"在本卡同样是缺陷（一行对应一个 HTTP 请求）。
func (a *auditCapture) only(t *testing.T) AuditEntry {
	t.Helper()

	require.Len(t, a.entries, 1, "台账应当只有一行：%+v", a.entries)
	return a.entries[0]
}

func (a *auditCapture) reset() { a.entries = nil }

// auditServer 造一台带账号鉴权与台账替身的服务器，并注册那条空处理器。
// 末尾的 opts 透传给 newSecurityServer：需要再挂一个可选依赖的用例（例如 /admin/runtime
// 的 reload 读口）不必重抄这里那三行装配。
func auditServer(t *testing.T, capture *auditCapture, opts ...Option) *Server {
	t.Helper()

	sec := accountsSecurity(t)
	sec.Auth.Token = testToken
	srv := newSecurityServer(t, sec, append([]Option{WithAuditLog(capture, capture)}, opts...)...)
	registerNopHandler(srv)
	return srv
}

// loginHeader 登录取一个账号的凭据，然后把台账清空一次。
//
// 需要清是因为登录本身就是一行（auth.login 是设计文档 §10 第 2 条点名要记的端点），
// 而用例接着要断言的是那次写操作——留着它，"只有一行"的断言就变成在测登录。
func loginHeader(t *testing.T, srv *Server, capture *auditCapture, who string) http.Header {
	t.Helper()

	header := bearer(login(t, srv, who, testPassword).AccessToken)
	capture.reset()
	return header
}

// opsHeader 拿一个 ops 档的凭据：本卡的用例大多要能写任务又能读台账。
func opsHeader(t *testing.T, srv *Server, capture *auditCapture) http.Header {
	t.Helper()

	return loginHeader(t, srv, capture, testOpsName)
}

// fieldStrings 把一行台账的所有字符串字段摊平，供"整行不含某个词"的负向断言用。
//
// 用反射而不是逐字段列一遍：新增列时这里会自动跟上，
// 而手工列表迟早漏掉一个，漏掉的那个正好是能泄露参数值的那一个。
func fieldStrings(entry AuditEntry) []string {
	var out []string
	value := reflect.ValueOf(entry)
	for i := 0; i < value.NumField(); i++ {
		field := value.Field(i)
		if field.Kind() == reflect.String {
			out = append(out, field.String())
		}
	}
	return out
}

// assertRowHasNo 断言这一行的任何字符串列都不含 needle。
func assertRowHasNo(t *testing.T, entry AuditEntry, needle string) {
	t.Helper()

	for _, value := range fieldStrings(entry) {
		assert.NotContains(t, value, needle)
	}
}

// TestAuditMiddleware_OnlyWrites 是这条路径的入口条件：读操作不进台账。
// GET 量大且没有权限变更含义（本卡 §8），实时通道也不在这里。
func TestAuditMiddleware_OnlyWrites(t *testing.T) {
	capture := &auditCapture{}
	srv := auditServer(t, capture)
	header := opsHeader(t, srv, capture)

	doGet(t, srv, "/api/v1/jobs", header)
	doGet(t, srv, "/api/v1/health", header)
	doGet(t, srv, "/api/v1/executors", header)
	doGet(t, srv, "/api/v1/admin/runtime", header)
	assert.Empty(t, capture.entries, "GET 一个都不该记")

	// 同一台服务器上做一次写：证明上面那个空结论不是"根本没挂中间件"
	recorder := doJSON(t, srv, http.MethodDelete, "/api/v1/jobs/no-such-job", "", header)
	require.Equal(t, http.StatusNotFound, recorder.Code)
	require.Len(t, capture.entries, 1)
	assert.Equal(t, "job.cancel", capture.entries[0].Action)
}

// TestAuditMiddleware_StatusFieldsAreFinal 守住"字段都在 c.Next() 之后读"：
// 状态码要 201（处理器的终值），而不是中间件层的 200 零值。
func TestAuditMiddleware_StatusFieldsAreFinal(t *testing.T) {
	capture := &auditCapture{}
	srv := auditServer(t, capture)
	header := opsHeader(t, srv, capture)

	recorder := doJSON(t, srv, http.MethodPost, "/api/v1/jobs",
		`{"name":"payment_check","delay":"1h","payload":{"order_id":"A-1"}}`, header)
	require.Equal(t, http.StatusCreated, recorder.Code, recorder.Body.String())

	entry := capture.only(t)
	assert.Equal(t, http.StatusCreated, entry.Status)
	assert.Equal(t, "job.create", entry.Action)
	assert.Equal(t, "/api/v1/jobs", entry.Route)
	assert.Equal(t, http.MethodPost, entry.Method)
	assert.Equal(t, auditVerdictOK, entry.Verdict)
	// 延迟只判"不为负"：本机的单调时钟刻度比一次进程内请求还粗（实测同一个刻度内
	// time.Since 会给 0），所以"> 0"这种断言在这台机器上是随机的。
	// 计算方式本身由 TestBuildAuditEntry_LatencyFromStart 用给定的起点确定地证。
	assert.GreaterOrEqual(t, entry.Latency, time.Duration(0))
	assert.False(t, entry.Time.IsZero())
	assert.NotEmpty(t, entry.JobID, "创建成功时台账要能说产出的是哪条任务")

	var created JobResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &created))
	assert.Equal(t, created.ID, entry.JobID, "记的是响应里那一条")

	// 参数取值不进台账：payload 里那个 order_id 是请求方给的
	assertRowHasNo(t, entry, "A-1")
}

// TestBuildAuditEntry_LatencyFromStart 用给定的起点确定地测一次延迟计算：
// 中间件里那句 time.Since(started) 只有把起点交给它才判得准，不依赖本机时钟刻度。
func TestBuildAuditEntry_LatencyFromStart(t *testing.T) {
	capture := &auditCapture{}
	srv := auditServer(t, capture)

	engine := gin.New()
	var built AuditEntry
	started := time.Now().Add(-25 * time.Millisecond)
	engine.GET("/probe", func(c *gin.Context) {
		built = srv.buildAuditEntry(c, started)
		c.Status(http.StatusCreated)
	})
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/probe", nil))
	require.Equal(t, http.StatusCreated, recorder.Code)

	assert.Greater(t, built.Latency, 20*time.Millisecond)
	assert.Less(t, built.Latency, 500*time.Millisecond)
	assert.True(t, built.Time.Equal(started), "行上的时间就是请求到达的那一刻")
}

// TestAuditMiddleware_RoleFields 是"中间件必须注册在鉴权之后"那条风险的证据：
// 顺序错了的话三种身份的 actor 全是空，而单测若不带凭据发请求就发现不了。
func TestAuditMiddleware_RoleFields(t *testing.T) {
	t.Run("account", func(t *testing.T) {
		capture := &auditCapture{}
		srv := auditServer(t, capture)

		doJSON(t, srv, http.MethodPost, "/api/v1/jobs", `{"name":"payment_check","delay":"1h"}`,
			loginHeader(t, srv, capture, testOperatorName))

		entry := capture.only(t)
		assert.Equal(t, testOperatorName, entry.Actor)
		assert.Equal(t, auditActorUser, entry.ActorKind)
		assert.Equal(t, "operator", entry.Role)
	})

	t.Run("machine", func(t *testing.T) {
		capture := &auditCapture{}
		srv := auditServer(t, capture)

		doJSON(t, srv, http.MethodPost, "/api/v1/jobs", `{"name":"payment_check","delay":"1h"}`,
			bearer(testToken))

		entry := capture.only(t)
		assert.Equal(t, machinePrincipalName, entry.Actor)
		assert.Equal(t, auditActorMachine, entry.ActorKind)
		// 记 machine 而不是它折算出来的 operator：把折算值写进这一列会抹掉"这是静态凭据"，
		// 而等效关系由 core.Role.rank 一处定义，读的人拿 actor_kind 也能对上。
		assert.Equal(t, "machine", entry.Role)
		assert.True(t, core.RoleMachine.AtLeast(core.RoleOperator), "折算等效性仍然成立")
	})

	t.Run("no_auth", func(t *testing.T) {
		capture := &auditCapture{}
		srv := newSecurityServer(t, Security{}, WithAuditLog(capture, capture))
		registerNopHandler(srv)

		recorder := doJSON(t, srv, http.MethodPost, "/api/v1/jobs", `{"name":"payment_check","delay":"1h"}`, nil)
		require.Equal(t, http.StatusCreated, recorder.Code)

		entry := capture.only(t)
		assert.Empty(t, entry.Actor, "未启用鉴权时不编一个账号名出来")
		assert.Equal(t, auditActorAnonymous, entry.ActorKind)
		assert.Equal(t, "ops", entry.Role, "这类部署里任何人都等于 ops，这本身是运维要看的事实")
	})

	t.Run("unauthenticated_reaches_the_gate", func(t *testing.T) {
		// 未认证的连接由鉴权层挡下（401），它发生在审计中间件上游，因此不进台账：
		// 这条断言守住的是"我们知道少了哪一类行"，而不是让它悄悄少。
		capture := &auditCapture{}
		srv := auditServer(t, capture)

		recorder := doJSON(t, srv, http.MethodPost, "/api/v1/jobs", `{"name":"payment_check"}`, nil)
		require.Equal(t, http.StatusUnauthorized, recorder.Code)
		assert.Empty(t, capture.entries, "401 只在访问日志里，不落这张表")
	})
}

// TestAuditMiddleware_RouteTemplateNotRawURL 是本卡最重要的正向断言之一：
// 存模板而不是原始 URL，于是任务 ID 与查询串都进不了表。
func TestAuditMiddleware_RouteTemplateNotRawURL(t *testing.T) {
	capture := &auditCapture{}
	srv := auditServer(t, capture)
	header := opsHeader(t, srv, capture)

	created := doJSON(t, srv, http.MethodPost, "/api/v1/jobs",
		`{"name":"payment_check","delay":"1h"}`, header)
	require.Equal(t, http.StatusCreated, created.Code)
	var job JobResponse
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &job))

	capture.entries = nil
	recorder := doJSON(t, srv, http.MethodPost,
		"/api/v1/jobs/"+job.ID+"/pause?token=secret-value&note=canary-note", "", header)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	entry := capture.only(t)
	assert.Equal(t, "/api/v1/jobs/:id/pause", entry.Route)
	assert.Equal(t, "job.pause", entry.Action)
	assertRowHasNo(t, entry, "secret-value")
	assertRowHasNo(t, entry, "canary-note")
	// ID 只出现在 job_id 之外的列里？这里没填 job_id（暂停路径不 stash），
	// 而任务 ID 是服务端生成的合法值，不是请求方任意文本，所以不属本条的禁止项。
	assert.Empty(t, entry.JobID)
}

// TestAuditMiddleware_UnmatchedPath 覆盖没有路由匹配的写请求：
// action 落到 unmatched，且不把请求路径抄进任何列。
func TestAuditMiddleware_UnmatchedPath(t *testing.T) {
	capture := &auditCapture{}
	srv := auditServer(t, capture)

	recorder := doJSON(t, srv, http.MethodPost, "/api/v1/nope", `{"x":1}`, opsHeader(t, srv, capture))
	require.Equal(t, http.StatusNotFound, recorder.Code)

	entry := capture.only(t)
	assert.Equal(t, auditActionUnmatched, entry.Action)
	assert.Empty(t, entry.Route, "没有模板时留空，不用原始 URL 顶上去")
	assert.Equal(t, auditVerdictNotFound, entry.Verdict)
	assertRowHasNo(t, entry, "nope")
}

// TestAuditMiddleware_MappedActionCoversWriteRoutes 是本卡 §6 第 4 条的机器对照：
// 映射表必须覆盖 setupRoutes 里的每一条写路由，漏一条就当场失败。
//
// 光靠"漏配落到 other + 记 debug"那条兜底不够：兜底只在运行时看得见，
// 而这条用例把同一个判断挪到了测试期，改路由的人不必等到冒烟。
func TestAuditMiddleware_MappedActionCoversWriteRoutes(t *testing.T) {
	capture := &auditCapture{}
	srv := auditServer(t, capture)

	routes := map[string]bool{}
	for _, entry := range srv.engine.Routes() {
		switch entry.Method {
		case http.MethodPost, http.MethodPut, http.MethodDelete:
			routes[entry.Method+" "+entry.Path] = true
		}
	}

	var missing []string
	for key := range routes {
		if _, ok := auditActions[key]; !ok {
			missing = append(missing, key)
		}
	}
	assert.Empty(t, missing, "写路由没有登记进审计映射表：%v", missing)
	assert.NotEmpty(t, routes, "路由表读出来是空的，说明这条用例没在检查任何东西")

	// 反向也判一次：映射表里写了而路由表没有的键，是删掉端点后留下的僵尸条目
	for key := range auditActions {
		assert.Contains(t, routes, key, "映射表里的 %s 已经没有对应路由", key)
	}
}

// TestAuditMiddleware_UnmappedRouteFallsBackToOther 覆盖兜底分支：
// 有人新增写路由而忘了登记映射时，行仍然要留下，并带一条 debug 说明哪里不同步。
func TestAuditMiddleware_UnmappedRouteFallsBackToOther(t *testing.T) {
	var logs bytes.Buffer
	capture := &auditCapture{}
	srv := auditLogServer(t, &logs, WithAuditLog(capture, capture))
	srv.engine.POST("/api/v1/audit-unmapped", func(c *gin.Context) { c.Status(http.StatusCreated) })

	require.Equal(t, http.StatusCreated,
		doJSON(t, srv, http.MethodPost, "/api/v1/audit-unmapped", `{}`, nil).Code)

	entry := capture.only(t)
	assert.Equal(t, auditActionOther, entry.Action)
	assert.Equal(t, "/api/v1/audit-unmapped", entry.Route)
	assert.Contains(t, logs.String(), "no audit action mapping")
}

// TestAuditMiddleware_BatchIsOneRow 钉住"一行对应一个 HTTP 请求"：
// 批量提交 5 条只记 1 行，且批量里的逐条执行结论与任务 ID 都不填。
func TestAuditMiddleware_BatchIsOneRow(t *testing.T) {
	capture := &auditCapture{}
	srv := auditServer(t, capture)

	body := `[{"name":"payment_check","delay":"1h"},{"name":"payment_check","delay":"2h"},` +
		`{"name":"payment_check","delay":"3h"},{"name":"no_such_type"},{"name":"payment_check","delay":"5h"}]`
	recorder := doJSON(t, srv, http.MethodPost, "/api/v1/jobs/batch", body, opsHeader(t, srv, capture))
	// 混合结果的 207：处理器对全成功与有失败都回 207，所以 verdict 一律是 partial
	require.Equal(t, http.StatusMultiStatus, recorder.Code, recorder.Body.String())

	entry := capture.only(t)
	assert.Equal(t, "job.batch_create", entry.Action)
	assert.Equal(t, auditVerdictPartial, entry.Verdict)
	assert.Empty(t, entry.JobID, "批量里的哪一条都不该被当成这一行的归属")
	assert.Empty(t, entry.ExecVerdict)

	// 全成功的批量同样是一行
	capture.entries = nil
	recorder = doJSON(t, srv, http.MethodPost, "/api/v1/jobs/batch",
		`[{"name":"payment_check","delay":"6h"}]`, opsHeader(t, srv, capture))
	require.Equal(t, http.StatusMultiStatus, recorder.Code)
	require.Len(t, capture.entries, 1)
	assert.Equal(t, auditVerdictPartial, capture.entries[0].Verdict,
		"207 不能因为是全成功就被读成 ok——批量端点的成功在响应体的 created 里")
}

// TestAuditVerdict_CoversStatuses 是状态码到封闭集的对照表，含两条容易写错的：
// 207 必须先于 2xx 判，429 不在设计文档列举的六项里但登录限流真的会给。
func TestAuditVerdict_CoversStatuses(t *testing.T) {
	cases := map[int]string{
		http.StatusOK:                  auditVerdictOK,
		http.StatusCreated:             auditVerdictOK,
		http.StatusNoContent:           auditVerdictOK,
		http.StatusMultiStatus:         auditVerdictPartial,
		http.StatusBadRequest:          auditVerdictBadRequest,
		http.StatusUnauthorized:        auditVerdictDenied,
		http.StatusForbidden:           auditVerdictDenied,
		http.StatusNotFound:            auditVerdictNotFound,
		http.StatusConflict:            auditVerdictConflict,
		http.StatusTooManyRequests:     auditVerdictThrottled,
		http.StatusInternalServerError: auditVerdictError,
		http.StatusBadGateway:          auditVerdictError,
		http.StatusTeapot:              auditVerdictOther,
	}
	for status, want := range cases {
		assert.Equal(t, want, auditVerdict(status), "status %d", status)
	}
}

// auditLogServer 造一台把日志写进缓冲的服务器（未启用鉴权，因此任何人都能写）。
// 台账用例里有几条判的就是"未注入 recorder 时那一行长什么样"，那些必须看真日志。
func auditLogServer(t *testing.T, logs *bytes.Buffer, opts ...Option) *Server {
	t.Helper()

	store, err := core.NewJSONFileStore(filepath.Join(t.TempDir(), "jobs.json"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	srv := NewServer(core.NewScheduler(store, nil, nil), store, "0", Security{},
		slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})), opts...)
	registerNopHandler(srv)
	return srv
}

// TestAuditMiddleware_WithoutRecorderLogsLine 覆盖未装配台账的那条路径：
// 记一行与表同形的结构化日志，不 panic、不影响响应。
func TestAuditMiddleware_WithoutRecorderLogsLine(t *testing.T) {
	var logs bytes.Buffer
	srv := auditLogServer(t, &logs)

	recorder := doJSON(t, srv, http.MethodPost, "/api/v1/jobs",
		`{"name":"payment_check","delay":"1h"}`, nil)
	require.Equal(t, http.StatusCreated, recorder.Code)

	out := logs.String()
	assert.Contains(t, out, "write operation audited")
	assert.Contains(t, out, "action=job.create")
	assert.Contains(t, out, "verdict=ok")
	assert.Contains(t, out, "route=/api/v1/jobs")
	// 没有执行器结论时不该出现那两个键：日志里"没有这个字段"与"字段是空值"是不同的事
	assert.NotContains(t, out, "exec_verdict")
}

// TestAuditMiddleware_RecorderErrorIgnored 守住台账不影响请求：
// 写入器报错时响应照旧，只是多一条 warn。
func TestAuditMiddleware_RecorderErrorIgnored(t *testing.T) {
	var logs bytes.Buffer
	capture := &auditCapture{err: errors.New("audit table is locked")}
	srv := auditLogServer(t, &logs, WithAuditLog(capture, capture))

	recorder := doJSON(t, srv, http.MethodPost, "/api/v1/jobs",
		`{"name":"payment_check","delay":"1h"}`, nil)
	require.Equal(t, http.StatusCreated, recorder.Code, "写入失败不该改响应")
	assert.Empty(t, capture.entries)

	out := logs.String()
	assert.Contains(t, out, "audit row was rejected")
	assert.Contains(t, out, "audit table is locked")
}

// TestAuditMiddleware_ConflictAndDeniedRows 把 403 与 409 两类拒绝各取一行：
// 路由档位挡下的 403 不该带执行器列（它不是提交期判定）。
func TestAuditMiddleware_ConflictAndDeniedRows(t *testing.T) {
	capture := &auditCapture{}
	srv := auditServer(t, capture)

	viewer := loginHeader(t, srv, capture, testViewerName)
	recorder := doJSON(t, srv, http.MethodPost, "/api/v1/jobs", `{"name":"payment_check"}`, viewer)
	require.Equal(t, http.StatusForbidden, recorder.Code)

	entry := capture.only(t)
	assert.Equal(t, auditVerdictDenied, entry.Verdict)
	assert.Equal(t, "job.create", entry.Action)
	assert.Empty(t, entry.ExecVerdict, "路由档位的 403 不是执行器提交结论")
	assert.Equal(t, testViewerName, entry.Actor)
}

// executorAuditServer 把执行器提交用例需要的档位登记表与台账替身接在一起。
// 鉴权参数沿用 submissionServer 的那份（四个账号 + 静态 token），身份由 identityHeader 取。
func executorAuditServer(t *testing.T, requiredRole string, capture *auditCapture,
	commands ...core.ExecutorCommand) *Server {
	t.Helper()

	sec := accountsSecurity(t)
	sec.Auth.Token = testToken
	return submissionServerWith(t, sec, requiredRole, submissionLogger(nil), commands,
		WithAuditLog(capture, capture))
}

// TestAuditExecutorInfo_ReasonCodes 表驱动走过提交期判定的五个出口。
// 每个出口只多了一行 stash（判定顺序与逻辑一字未改），所以这里同时是那条改动的验收。
func TestAuditExecutorInfo_ReasonCodes(t *testing.T) {
	callback := submissionProfile("callback", core.ExecutorArg{
		Name: "day", Required: true, Pattern: `^\d{4}-\d{2}-\d{2}$`,
	})
	unavailable := core.ExecutorCommand{
		Name: "not_deployed", Kind: "script", Runtime: "node",
		Script: "scripts/missing-on-purpose.mjs", MaxParallel: 1,
	}

	cases := []struct {
		name        string
		who         string
		target      string
		body        string
		execVerdict string
		reason      string
		status      int
		profile     string
	}{
		{
			name: "role denied", who: "operator", target: "/api/v1/jobs",
			body:        `{"name":"exec.callback","payload":{"params":{"day":"2026-09-30"}}}`,
			execVerdict: auditExecRoleDenied, reason: "admin",
			status: http.StatusForbidden, profile: "callback",
		},
		{
			name: "profile unavailable", who: "admin", target: "/api/v1/jobs",
			body:        `{"name":"exec.not_deployed","payload":{"args":{}}}`,
			execVerdict: auditExecProfileUnavailable, reason: auditReasonProfile,
			status: http.StatusBadRequest, profile: "not_deployed",
		},
		{
			name: "timeout rejected", who: "admin", target: "/api/v1/jobs",
			body:        `{"name":"exec.callback","timeout":"60s"}`,
			execVerdict: auditExecTimeoutRejected, reason: auditReasonTimeout,
			status: http.StatusBadRequest, profile: "callback",
		},
		{
			name: "payload rejected", who: "admin", target: "/api/v1/jobs",
			body:        `{"name":"exec.callback","payload":{"params":{"day":"not-a-date"}}}`,
			execVerdict: auditExecPayloadRejected, reason: auditReasonPayload,
			status: http.StatusBadRequest, profile: "callback",
		},
		{
			name: "accepted", who: "admin", target: "/api/v1/jobs",
			body:        `{"name":"exec.callback","payload":{"params":{"day":"2026-09-30"}}}`,
			execVerdict: auditExecAccepted, reason: auditReasonAccepted,
			status: http.StatusCreated, profile: "callback",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capture := &auditCapture{}
			srv := executorAuditServer(t, "admin", capture, callback, unavailable)
			srv.RegisterJobHandler("exec.callback", func(ctx context.Context, j *core.Job) error { return nil })
			srv.RegisterJobHandler("exec.not_deployed", func(ctx context.Context, j *core.Job) error { return nil })

			header := identityHeader(t, srv, tc.who)
			recorder := doJSON(t, srv, http.MethodPost, tc.target, tc.body, header)
			require.Equal(t, tc.status, recorder.Code, recorder.Body.String())

			entry := capture.only(t)
			assert.Equal(t, tc.execVerdict, entry.ExecVerdict)
			assert.Equal(t, tc.reason, entry.ExecReasonCode)
			assert.Equal(t, "exec."+tc.profile, entry.HandlerKey)
			assert.Equal(t, tc.profile, entry.Profile)
			assert.Equal(t, "job.create", entry.Action)

			// 结论与状态码必须互相说得上话，否则台账里会出现"accepted 但 403"这种行
			switch tc.execVerdict {
			case auditExecAccepted:
				assert.Equal(t, auditVerdictOK, entry.Verdict)
			case auditExecRoleDenied:
				assert.Equal(t, auditVerdictDenied, entry.Verdict)
			default:
				assert.Equal(t, auditVerdictBadRequest, entry.Verdict)
			}
		})
	}
}

// TestAuditExecutorInfo_NoArgValues 是本卡最重要的负向断言：
// 含 secret 参数的档位被拒时，请求方写进去的那个可搜索取值不能出现在表里任何一列。
// 错误原文（含参数名甚至可能含取值）继续只进响应与 slog，见设计文档 D7。
func TestAuditExecutorInfo_NoArgValues(t *testing.T) {
	secretProfile := core.ExecutorCommand{
		Name: "show_token", Kind: "http", Method: "GET", Body: "none",
		URLTemplate:  "https://api.example.com/hooks/show_token",
		AllowedHosts: []string{"api.example.com"},
		Args: []core.ExecutorArg{{
			Name: "token", Required: true, Secret: true, Pattern: `^[a-z]+$`,
		}},
		Timeout: 30 * time.Second,
	}

	const canary = "CANARY-SUPER-SECRET-VALUE"
	capture := &auditCapture{}
	srv := executorAuditServer(t, "operator", capture, secretProfile)
	srv.RegisterJobHandler("exec.show_token", func(ctx context.Context, j *core.Job) error { return nil })

	recorder := doJSON(t, srv, http.MethodPost, "/api/v1/jobs",
		`{"name":"exec.show_token","payload":{"params":{"token":"`+canary+`"}}}`,
		identityHeader(t, srv, "admin"))
	// 取值不合档位声明的 pattern，提交期就被拒
	require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
	assert.Contains(t, recorder.Body.String(), "invalid executor payload",
		"响应体里带错误说明是既有行为，本卡改的是表那一侧")

	entry := capture.only(t)
	assert.Equal(t, auditExecPayloadRejected, entry.ExecVerdict)
	assertRowHasNo(t, entry, canary)

	// 同一行的响应体之外也没有别处泄漏：整行序列化出来搜一遍
	encoded, err := json.Marshal(entry)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), canary)

	// 顺带确认这个 canary 也进不了台账日志的形态（未注入 recorder 时走的那条路径）
	var logs bytes.Buffer
	logSrv := submissionServerWith(t, Security{}, "operator",
		slog.New(slog.NewTextHandler(&logs, nil)), []core.ExecutorCommand{secretProfile})
	logSrv.RegisterJobHandler("exec.show_token", func(ctx context.Context, j *core.Job) error { return nil })
	doJSON(t, logSrv, http.MethodPost, "/api/v1/jobs",
		`{"name":"exec.show_token","payload":{"params":{"token":"`+canary+`"}}}`, nil)
	line := logs.String()
	assert.Contains(t, line, "write operation audited")
	assert.NotContains(t, line, canary)
	// 而 payload 本身也没被记进那行日志的任何一个键
	assert.False(t, strings.Contains(line, "payload="), "台账日志不该有 payload 字段")
}

// TestGetAudit_NoReader503 与产物列表、产物存储那两条守卫同口径：
// 没装配台账的部署要说"没在记"，不要给一份看着完整的空结果。
func TestGetAudit_NoReader503(t *testing.T) {
	srv := newSecurityServer(t, Security{})

	recorder := doGet(t, srv, "/api/v1/admin/audit", nil)
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)

	var failure ErrorResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &failure))
	assert.Equal(t, "write audit log is not configured", failure.Message)
	assert.Contains(t, failure.Details, "observability.audit.enabled")
}

// TestGetAudit_RecordsAndPaging 走一次正常查询：响应形状、顺序说明与 total 口径。
func TestGetAudit_RecordsAndPaging(t *testing.T) {
	first := AuditEntry{
		Time: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), Actor: testAdminName,
		ActorKind: auditActorUser, Role: "admin", Action: "job.create", Method: http.MethodPost,
		Route: "/api/v1/jobs", Status: http.StatusCreated, Latency: 1200 * time.Microsecond,
		Verdict: auditVerdictOK, JobID: "0198a2e3",
	}
	second := first
	second.Action = "group.delete"
	second.Verdict = auditVerdictDenied
	second.ExecVerdict = auditExecRoleDenied
	second.ExecReasonCode = "admin"

	capture := &auditCapture{queryItems: []AuditEntry{first, second}, queryTotal: 41}
	srv := auditServer(t, capture)

	recorder := doGet(t, srv, "/api/v1/admin/audit?limit=2&offset=4", opsHeader(t, srv, capture))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	var resp AuditResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &resp))
	assert.Equal(t, 2, resp.Count)
	assert.Equal(t, 41, resp.Total, "total 是不带分页的匹配数")
	assert.Equal(t, 2, resp.Limit)
	assert.Equal(t, 4, resp.Offset)
	require.Len(t, resp.Items, 2)
	assert.Equal(t, "job.create", resp.Items[0].Action)
	assert.Equal(t, int64(1200), resp.Items[0].LatencyMicros)
	assert.Equal(t, "0198a2e3", resp.Items[0].JobID)
	assert.Equal(t, "admin", resp.Items[1].ExecReasonCode)

	// 响应体里没有路径、没有 payload、没有 error 原文
	body := recorder.Body.String()
	for _, forbidden := range []string{"payload", "details", "content", "user_agent_note"} {
		assert.NotContains(t, body, forbidden)
	}

	assert.Equal(t, 1, capture.queryCalls)
}

// TestGetAudit_EmptyIsListNotNull 与事件、产物列表同一口径。
func TestGetAudit_EmptyIsListNotNull(t *testing.T) {
	capture := &auditCapture{}
	srv := auditServer(t, capture)

	recorder := doGet(t, srv, "/api/v1/admin/audit", opsHeader(t, srv, capture))
	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Contains(t, recorder.Body.String(), `"items":[]`)
}

// TestGetAudit_FilterValidation 是 §5 第 14 条：非法取值一律 400，且响应里不带 SQL 片段。
func TestGetAudit_FilterValidation(t *testing.T) {
	cases := []struct {
		name   string
		target string
		needle string
	}{
		{"path style action", "/api/v1/admin/audit?action=../x", "invalid action"},
		{"unknown verdict", "/api/v1/admin/audit?verdict=whatever", "invalid verdict"},
		{"bad since", "/api/v1/admin/audit?since=2026-10-01", "invalid since"},
		{"bad until", "/api/v1/admin/audit?until=now", "invalid until"},
		{"negative limit", "/api/v1/admin/audit?limit=-5", "invalid limit"},
		{"oversized limit", "/api/v1/admin/audit?limit=5000", "invalid limit"},
		{"non numeric offset", "/api/v1/admin/audit?offset=abc", "invalid offset"},
		{"oversized actor", "/api/v1/admin/audit?actor=" + strings.Repeat("a", 300), "invalid actor"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capture := &auditCapture{}
			srv := auditServer(t, capture)

			recorder := doGet(t, srv, tc.target, opsHeader(t, srv, capture))
			require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
			assert.Contains(t, recorder.Body.String(), tc.needle)
			assert.NotContains(t, recorder.Body.String(), "SELECT", "错误信息不该把库里的形态说出来")
			assert.Zero(t, capture.queryCalls, "校验不过就不该去问库")
		})
	}

	// 合法取值要能被接受，并且原样进到过滤条件里
	capture := &auditCapture{}
	srv := auditServer(t, capture)
	recorder := doGet(t, srv,
		"/api/v1/admin/audit?action=job.create&verdict=denied&actor="+testAdminName+
			"&since=2026-10-01T00:00:00Z&until=2026-10-02T00%3A00%3A00Z&limit=10&offset=5",
		opsHeader(t, srv, capture))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	filter := capture.queryFilter
	assert.Equal(t, "job.create", filter.Action)
	assert.Equal(t, auditVerdictDenied, filter.Verdict)
	assert.Equal(t, testAdminName, filter.Actor)
	assert.Equal(t, 10, filter.Limit)
	assert.Equal(t, 5, filter.Offset)
	require.NotNil(t, filter.Since)
	require.NotNil(t, filter.Until)
	assert.Equal(t, 24*time.Hour, filter.Until.Sub(*filter.Since))
}

// TestGetAudit_ReadErrorIs500 与事件端点、产物列表同一口径：读不出来就报错。
func TestGetAudit_ReadErrorIs500(t *testing.T) {
	capture := &auditCapture{queryErr: errors.New("no such table: write_audit")}
	srv := auditServer(t, capture)

	recorder := doGet(t, srv, "/api/v1/admin/audit", opsHeader(t, srv, capture))
	require.Equal(t, http.StatusInternalServerError, recorder.Code)

	var failure ErrorResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &failure))
	assert.Equal(t, "failed to load audit records", failure.Message)
	assert.Contains(t, failure.Details, "write_audit")
}

// TestGetAudit_RoleOps 钉住档位：台账含账号名与拒绝原因，只给 ops。
func TestGetAudit_RoleOps(t *testing.T) {
	capture := &auditCapture{}
	srv := auditServer(t, capture)

	assert.Equal(t, http.StatusUnauthorized, doGet(t, srv, "/api/v1/admin/audit", nil).Code)
	assert.Equal(t, http.StatusForbidden,
		doGet(t, srv, "/api/v1/admin/audit", bearer(testToken)).Code,
		"machine 折算 operator，低于 ops")
	for _, who := range []string{testViewerName, testOperatorName, testAdminName} {
		assert.Equal(t, http.StatusForbidden,
			doGet(t, srv, "/api/v1/admin/audit", loginHeader(t, srv, capture, who)).Code,
			"%s 不该读台账", who)
	}
	assert.Equal(t, http.StatusOK, doGet(t, srv, "/api/v1/admin/audit", opsHeader(t, srv, capture)).Code)
}
