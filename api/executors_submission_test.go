package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
	"godelayq/executor"
)

// 这一组是 TASK-E16 的用例：谁能提交执行器任务、提交时判不判参数、含 secret 参数的任务
// 在读取接口里还不读得出明文。
//
// 档位一律用 http：探测结论恒为可用（E03 不访问网络），所以这些用例撞到的分支
// 只有本卡要判的那几条，不会先撞上"脚本没部署"那种不可用。

// submissionProfile 造一条不需要参数的 http 档位，占位名由调用方给。
func submissionProfile(name string, args ...core.ExecutorArg) core.ExecutorCommand {
	return core.ExecutorCommand{
		Name:         name,
		Kind:         "http",
		Method:       "GET",
		Body:         "none",
		URLTemplate:  "https://api.example.com/hooks/" + name,
		AllowedHosts: []string{"api.example.com"},
		Args:         args,
		Timeout:      30 * time.Second,
	}
}

// submissionServerWith 是完整形态：安全参数、日志器、档位列表与额外注入都由调用方给。
//
// 处理器按档位注册键一起注册：提交路径先查调度器的注册表再查档位，两者必须都在，
// 否则先撞到"类型未注册"那条 400，判不到本卡的分支。
func submissionServerWith(t *testing.T, sec Security, requiredRole string, logger *slog.Logger,
	commands []core.ExecutorCommand, opts ...Option) *Server {
	t.Helper()

	cfg := executorConfigFor(t.TempDir(), commands...)
	cfg.Executors.RequiredRole = requiredRole

	registry, err := executor.NewRegistry(cfg, logger)
	require.NoError(t, err)

	store, err := core.NewJSONFileStore(filepath.Join(t.TempDir(), "jobs.json"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	srv := NewServer(core.NewScheduler(store, nil, nil), store, "0", sec, logger,
		append([]Option{WithExecutorRegistry(registry)}, opts...)...)

	registerNopHandler(srv)
	nop := func(context.Context, *core.Job) error { return nil }
	for _, command := range commands {
		srv.RegisterJobHandler(executor.HandlerKeyPrefix+command.Name, nop)
	}
	return srv
}

// submissionLogger 给出写进缓冲的日志器；缓冲为 nil 时丢弃输出。
func submissionLogger(logs *bytes.Buffer) *slog.Logger {
	if logs == nil {
		return newTestLogger()
	}
	return slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// submissionServer 造一台带四个档位账号与机器凭据的服务器（矩阵与判定用例的常用形态）。
func submissionServer(t *testing.T, requiredRole string, logs *bytes.Buffer, commands ...core.ExecutorCommand) *Server {
	t.Helper()

	sec := accountsSecurity(t)
	sec.Auth.Token = testToken
	return submissionServerWith(t, sec, requiredRole, submissionLogger(logs), commands)
}

// submissionServerNoAuth 造一台未启用鉴权的服务器（§3.1 第 3 条那条 warn 的用例用）。
func submissionServerNoAuth(t *testing.T, logs *bytes.Buffer, commands ...core.ExecutorCommand) *Server {
	t.Helper()

	return submissionServerWith(t, Security{}, "operator", submissionLogger(logs), commands)
}

// identityHeader 取某个身份的凭据。machine 没有"账号"这一步，直接带静态 token。
//
// 控制台身份走 IssueSession 直接签发令牌：登录那一步是 bcrypt 验密，
// 它由 api/auth_test.go 的用例覆盖，而本卡的矩阵判的是"带着某个档位的合法令牌提交执行器任务会怎样"。
// 仍然走真实的签发路径（签名、过期、authStore 记录都与线上一致），只是不重复验密。
func identityHeader(t *testing.T, srv *Server, who string) http.Header {
	t.Helper()

	if who == "machine" {
		return http.Header{"Authorization": {"Bearer " + testToken}}
	}

	role := identityRole(t, who)
	// 主体名必须是配置里存在的账号：验签那一步会拿它回查当前账号与档位
	// （令牌写着 admin 但账号已被降权时要拒），所以这里不能自己编一个名字。
	session, err := srv.auth.IssueSession(Principal{Name: accountForIdentity(who), Role: role}, srv.tokens)
	require.NoError(t, err)
	return bearer(session.AccessToken)
}

// accountForIdentity 把身份名换成 accountsSecurity 里配的那个账号名。
func accountForIdentity(who string) string {
	switch who {
	case "viewer":
		return testViewerName
	case "operator":
		return testOperatorName
	case "admin":
		return testAdminName
	case "ops":
		return testOpsName
	default:
		return who
	}
}

// identities 给出一个"按身份名取凭据"的函数，登录结果在这次测试内复用。
//
// 存在的唯一理由是开销：每次登录都要过一次 bcrypt，带 -race 时一组权限用例里
// 三十次登录能占到半分钟，而那点信息（一个令牌）根本不需要重新算。
func identities(t *testing.T, srv *Server) func(who string) http.Header {
	t.Helper()

	cached := map[string]http.Header{}
	return func(who string) http.Header {
		if header, ok := cached[who]; ok {
			return header
		}
		header := identityHeader(t, srv, who)
		cached[who] = header
		return header
	}
}

// identityRole 把身份名换成档位，machine 单独列（core.ParseRole 故意不接受它，
// 因为它不是配置里能填的角色，只是凭据通道的身份）。
func identityRole(t *testing.T, who string) core.Role {
	t.Helper()

	if who == "machine" {
		return core.RoleMachine
	}
	role, ok := core.ParseRole(who)
	require.True(t, ok, "未知的身份 %q", who)
	return role
}

// TestCreateJob_ExecutorRequiredRole 是这张卡的核心产出：
// required_role 的三个取值 × 五种身份，共 15 组，结论只由 core.Role.AtLeast 决定
// （machine 与 operator 同级，所以默认 admin 档下脚本凭据提交不了）。
// docs/api.md 的权限矩阵引用这张表。
func TestCreateJob_ExecutorRequiredRole(t *testing.T) {
	for _, required := range []string{"operator", "admin", "ops"} {
		t.Run("required_"+required, func(t *testing.T) {
			srv := submissionServer(t, required, nil, submissionProfile("callback"))
			creds := identities(t, srv)
			minRole, ok := core.ParseRole(required)
			require.True(t, ok)

			for _, who := range []string{"viewer", "operator", "admin", "ops", "machine"} {
				t.Run(who, func(t *testing.T) {
					wantAllowed := identityRole(t, who).AtLeast(minRole)

					recorder := doJSON(t, srv, http.MethodPost, "/api/v1/jobs",
						`{"name":"exec.callback","payload":{}}`, creds(who))

					if wantAllowed {
						assert.Equal(t, http.StatusCreated, recorder.Code, recorder.Body.String())
						return
					}
					assert.Equal(t, http.StatusForbidden, recorder.Code, recorder.Body.String())

					// viewer 在路由层就被挡（建任务要 operator），
					// 只有走到提交档位判定那一层的身份才带得到 details。
					if who == "viewer" {
						return
					}
					var failure ErrorResponse
					require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &failure))
					assert.Equal(t, "insufficient role", failure.Message)
					assert.Contains(t, failure.Details, "executors.required_role",
						"拒绝要说出这道要求来自哪个配置项")
					assert.Contains(t, failure.Details, "exec.callback")
				})
			}
		})
	}
}

// TestBatchCreateJobs_MixedPermission 检查批量接口的逐条独立语义：
// 档位不够的那一条拿 403，同批的普通任务照常创建。
func TestBatchCreateJobs_MixedPermission(t *testing.T) {
	srv := submissionServer(t, "admin", nil, submissionProfile("callback"))
	creds := identities(t, srv)

	body := `[{"name":"payment_check"},{"name":"exec.callback","payload":{}},{"name":"payment_check"}]`
	recorder := doJSON(t, srv, http.MethodPost, "/api/v1/jobs/batch", body,
		creds("operator"))
	require.Equal(t, http.StatusMultiStatus, recorder.Code, recorder.Body.String())

	var resp BatchCreateJobsResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &resp))
	assert.Equal(t, 2, resp.Succeeded, "两条普通任务照常入队")
	assert.Equal(t, 1, resp.Failed)
	require.Len(t, resp.Errors, 1)
	assert.Equal(t, 1, resp.Errors[0].Index, "错误按下标指认那一条")
	assert.Equal(t, http.StatusForbidden, resp.Errors[0].Code)
	assert.Contains(t, resp.Errors[0].Details, "executors.required_role")
}

// TestCreateJob_ExecutorValidation 是 §3.2 的两条 400：参数非法与档位不可用要分得开。
func TestCreateJob_ExecutorValidation(t *testing.T) {
	unavailable := core.ExecutorCommand{
		Name:        "not_deployed",
		Kind:        "script",
		Runtime:     "node",
		Script:      "scripts/missing-on-purpose.mjs",
		Args:        []core.ExecutorArg{{Name: "day", Required: true}},
		ArgsRender:  []string{"--day={day}"},
		MaxParallel: 1,
	}

	var logs bytes.Buffer
	srv := submissionServer(t, "operator", &logs,
		submissionProfile("callback", core.ExecutorArg{Name: "day", Required: true, Pattern: `^\d{4}-\d{2}-\d{2}$`}),
		unavailable,
	)
	creds := identities(t, srv)
	srv.RegisterJobHandler("exec.not_deployed", func(context.Context, *core.Job) error { return nil })
	admin := creds("admin")

	t.Run("非法参数在提交期就被拒", func(t *testing.T) {
		recorder := doJSON(t, srv, http.MethodPost, "/api/v1/jobs",
			`{"name":"exec.callback","payload":{"params":{"day":"not-a-date"}}}`, admin)
		require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())

		var failure ErrorResponse
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &failure))
		assert.Equal(t, "invalid executor payload", failure.Message)
		assert.Contains(t, failure.Details, "params.day", "错误要指到具体参数")

		// 关键差别：这条任务没有入队，所以不会被执行一次再失败
		listed := doGet(t, srv, "/api/v1/jobs?limit=50", admin)
		assert.NotContains(t, listed.Body.String(), "exec.callback")
	})

	t.Run("payload 结构非法同样被拒", func(t *testing.T) {
		recorder := doJSON(t, srv, http.MethodPost, "/api/v1/jobs",
			`{"name":"exec.callback","payload":{"cmd":"rm -rf /"}}`, admin)
		require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
		assert.Contains(t, recorder.Body.String(), "payload key")
	})

	t.Run("档位不可用说的是另一件事", func(t *testing.T) {
		recorder := doJSON(t, srv, http.MethodPost, "/api/v1/jobs",
			`{"name":"exec.not_deployed","payload":{"args":{"day":"2026-09-30"}}}`, admin)
		require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())

		var failure ErrorResponse
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &failure))
		assert.Equal(t, "executor profile is not available on this server", failure.Message,
			"与\"类型未注册\"区分开：名字对，但这台机器现在跑不了")
		assert.NotEmpty(t, failure.Details, "details 要带探测原因")
	})

	t.Run("普通任务不受影响", func(t *testing.T) {
		recorder := doJSON(t, srv, http.MethodPost, "/api/v1/jobs",
			`{"name":"payment_check","payload":{"anything":"at all"}}`, admin)
		assert.Equal(t, http.StatusCreated, recorder.Code, recorder.Body.String())
	})

	t.Run("未知名字仍是未注册那条 400", func(t *testing.T) {
		recorder := doJSON(t, srv, http.MethodPost, "/api/v1/jobs",
			`{"name":"exec.no_such_profile"}`, admin)
		require.Equal(t, http.StatusBadRequest, recorder.Code)
		assert.Contains(t, recorder.Body.String(), "unknown job type")
	})
}

// TestCreateJob_TimeoutNormalized 落实 D5："不填超时不代表无限"。
// 任务详情显示的必须是真正会生效的那一个超时，与执行侧算出的同一个值。
func TestCreateJob_TimeoutNormalized(t *testing.T) {
	srv := submissionServer(t, "operator", nil, submissionProfile("callback"))
	creds := identities(t, srv)
	admin := creds("admin")

	t.Run("payload 不填超时用档位声明值", func(t *testing.T) {
		recorder := doJSON(t, srv, http.MethodPost, "/api/v1/jobs",
			`{"name":"exec.callback","payload":{}}`, admin)
		require.Equal(t, http.StatusCreated, recorder.Code, recorder.Body.String())

		var created JobResponse
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &created))
		assert.Equal(t, (30 * time.Second).String(), created.Timeout,
			"档位写了 30s，落盘与响应就得是 30s，不能是空")

		fetched := doGet(t, srv, "/api/v1/jobs/"+created.ID, admin)
		assert.Contains(t, fetched.Body.String(), `"timeout":"30s"`)
	})

	t.Run("payload 里的短超时被采纳", func(t *testing.T) {
		recorder := doJSON(t, srv, http.MethodPost, "/api/v1/jobs",
			`{"name":"exec.callback","payload":{"timeout":"5s"}}`, admin)
		require.Equal(t, http.StatusCreated, recorder.Code, recorder.Body.String())

		var created JobResponse
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &created))
		assert.Equal(t, (5 * time.Second).String(), created.Timeout)
	})

	t.Run("payload 超过档位上限被拒", func(t *testing.T) {
		recorder := doJSON(t, srv, http.MethodPost, "/api/v1/jobs",
			`{"name":"exec.callback","payload":{"timeout":"10m"}}`, admin)
		require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
		assert.Contains(t, recorder.Body.String(), "exceeds",
			"与 E08 执行期同一条规则：超过档位 timeout 就是拒绝，不是夹取")
	})

	t.Run("请求体顶层的 timeout 也受档位上限约束", func(t *testing.T) {
		recorder := doJSON(t, srv, http.MethodPost, "/api/v1/jobs",
			`{"name":"exec.callback","timeout":"2h","payload":{}}`, admin)
		require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())

		var failure ErrorResponse
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &failure))
		assert.Equal(t, "invalid timeout", failure.Message)
		assert.Contains(t, failure.Details, "profile")
	})
}

// TestUpdateJob_ExecutorGuards 覆盖 DoD 第六条：PUT 不能绕过提交档位与参数校验。
func TestUpdateJob_ExecutorGuards(t *testing.T) {
	srv := submissionServer(t, "admin", nil,
		submissionProfile("callback", core.ExecutorArg{Name: "day", Required: true, Pattern: `^\d{4}-\d{2}-\d{2}$`}))
	creds := identities(t, srv)
	admin := creds("admin")

	created := doJSON(t, srv, http.MethodPost, "/api/v1/jobs",
		`{"name":"exec.callback","payload":{"params":{"day":"2026-09-30"}}}`, admin)
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var job JobResponse
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &job))
	target := "/api/v1/jobs/" + job.ID

	t.Run("operator 改执行器任务的 payload 被档位判定拦住", func(t *testing.T) {
		recorder := doJSON(t, srv, http.MethodPut, target,
			`{"payload":{"params":{"day":"2026-10-01"}}}`, creds("operator"))
		assert.Equal(t, http.StatusForbidden, recorder.Code, recorder.Body.String(),
			"路由是 operator 档，但执行器任务的判定在处理器里，PUT 也一样要过")

		// 拒绝之后 payload 没被改动
		after := doGet(t, srv, target, admin)
		assert.Contains(t, after.Body.String(), "2026-09-30")
	})

	t.Run("非法 payload 被重校验", func(t *testing.T) {
		recorder := doJSON(t, srv, http.MethodPut, target,
			`{"payload":{"params":{"day":"yesterday"}}}`, admin)
		require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
		assert.Contains(t, recorder.Body.String(), "invalid executor payload")
	})

	t.Run("改名字被拒，改法向两边都拦", func(t *testing.T) {
		recorder := doJSON(t, srv, http.MethodPut, target,
			`{"name":"payment_check"}`, admin)
		require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
		assert.Contains(t, recorder.Body.String(), "job name cannot be changed",
			"换成普通任务名等于绕开提交档位判定")

		plain := doJSON(t, srv, http.MethodPost, "/api/v1/jobs", `{"name":"payment_check"}`, admin)
		var plainJob JobResponse
		require.NoError(t, json.Unmarshal(plain.Body.Bytes(), &plainJob))
		recorder = doJSON(t, srv, http.MethodPut, "/api/v1/jobs/"+plainJob.ID,
			`{"name":"exec.callback"}`, admin)
		assert.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
	})

	t.Run("传回相同名字按没传处理", func(t *testing.T) {
		recorder := doJSON(t, srv, http.MethodPut, target,
			`{"name":"exec.callback","payload":{"params":{"day":"2026-10-05"}}}`, admin)
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		assert.Contains(t, recorder.Body.String(), "2026-10-05")
	})

	t.Run("普通任务的 PUT 不受影响", func(t *testing.T) {
		plain := doJSON(t, srv, http.MethodPost, "/api/v1/jobs", `{"name":"payment_check"}`, admin)
		var plainJob JobResponse
		require.NoError(t, json.Unmarshal(plain.Body.Bytes(), &plainJob))

		recorder := doJSON(t, srv, http.MethodPut, "/api/v1/jobs/"+plainJob.ID,
			`{"payload":{"anything":true},"timeout":"2m"}`, admin)
		assert.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	})

	t.Run("判档与改名排在状态检查之前", func(t *testing.T) {
		// 已经跑完的执行器任务：档位不够的身份拿到的还是 403，
		// 而不是"这条任务已经结束了"的 409——那等于从状态码里读出任务的进度。
		seedExecutorJob(t, srv, "job-done-exec", "exec.callback",
			`{"params":{"day":"2026-09-30"}}`, core.StatusSuccess, nil)
		recorder := doJSON(t, srv, http.MethodPut, "/api/v1/jobs/job-done-exec",
			`{"payload":{"params":{"day":"2026-10-01"}}}`, creds("operator"))
		assert.Equal(t, http.StatusForbidden, recorder.Code, recorder.Body.String())

		// 已结束的非档位任务：换名字这条请求本身就不合法，拿 400 而不是 409
		seedExecutorJob(t, srv, "job-done-plain", "payment_check", "", core.StatusSuccess, nil)
		recorder = doJSON(t, srv, http.MethodPut, "/api/v1/jobs/job-done-plain",
			`{"name":"exec.callback"}`, admin)
		assert.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())

		// 没传 name 的普通改动照旧按状态回答；不存在的路径照旧 404
		recorder = doJSON(t, srv, http.MethodPut, "/api/v1/jobs/job-done-plain",
			`{"payload":{"a":1}}`, admin)
		assert.Equal(t, http.StatusConflict, recorder.Code, recorder.Body.String())
		recorder = doJSON(t, srv, http.MethodPut, "/api/v1/jobs/job-never-created",
			`{"payload":{"a":1}}`, admin)
		assert.Equal(t, http.StatusNotFound, recorder.Code, recorder.Body.String())
	})
}

// TestCreateJob_MachineTokenDeniedUnderDefaultRole 是配置注释里那句结论的现场：
// 默认 required_role: admin 时，静态凭据（machine，与 operator 同级）提交不了执行器任务。
func TestCreateJob_MachineTokenDeniedUnderDefaultRole(t *testing.T) {
	srv := submissionServer(t, "admin", nil, submissionProfile("callback"))

	denied := doJSON(t, srv, http.MethodPost, "/api/v1/jobs", `{"name":"exec.callback"}`,
		http.Header{"Authorization": {"Bearer " + testToken}})
	assert.Equal(t, http.StatusForbidden, denied.Code, denied.Body.String())

	// 同一份凭据提交普通任务照旧可以：本卡没有收紧普通写操作
	allowed := doJSON(t, srv, http.MethodPost, "/api/v1/jobs", `{"name":"payment_check"}`,
		http.Header{"Authorization": {"Bearer " + testToken}})
	assert.Equal(t, http.StatusCreated, allowed.Code, allowed.Body.String())
}

// TestAuthDisabled_WarnOnce 是 §3.1 第 3 条：未启用鉴权时提交照旧通过，
// 但要在日志里留下可见的痕迹，而每条任务都打一行会让其他日志看不到。
func TestAuthDisabled_WarnOnce(t *testing.T) {
	var logs bytes.Buffer
	srv := submissionServerNoAuth(t, &logs, submissionProfile("callback"))

	for range 5 {
		recorder := doJSON(t, srv, http.MethodPost, "/api/v1/jobs", `{"name":"exec.callback"}`, nil)
		require.Equal(t, http.StatusCreated, recorder.Code, recorder.Body.String())
	}

	warns := 0
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "executor job submitted while authentication is disabled") {
			warns++
		}
	}
	assert.Equal(t, 1, warns, "五次提交只留一条 warn，实际 %d 条", warns)
}

// TestLogAccessRejection_RoleFields 是 §3.4 第 2 条：拒绝日志要能看出"要什么、你是谁"。
func TestLogAccessRejection_RoleFields(t *testing.T) {
	var logs bytes.Buffer
	srv := submissionServer(t, "admin", &logs, submissionProfile("callback"))
	creds := identities(t, srv)

	// 提交档位判定拦下的一次
	doJSON(t, srv, http.MethodPost, "/api/v1/jobs", `{"name":"exec.callback"}`,
		creds("operator"))
	// 路由档位拦下的一次（viewer 连建任务都不行）
	doJSON(t, srv, http.MethodPost, "/api/v1/jobs", `{"name":"payment_check"}`,
		creds("viewer"))

	text := logs.String()
	assert.Contains(t, text, `msg="access denied"`)
	assert.Contains(t, text, "have=operator", "被拒身份要写出来")
	assert.Contains(t, text, "required=admin", "要求的档位要写出来")
	assert.Contains(t, text, "required=operator")
	assert.NotContains(t, text, "required=unknown")
}
