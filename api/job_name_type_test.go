package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
)

// 这一组是 TASK-N02 的用例：名称变回给人看的标签、类型决定跑什么，
// 而"只给 name"的旧写法在同一台服务器上仍然可用（设计文档 §D4 的兼容双写法）。
//
// 门禁那两条（TestExecutorGateUsesHandlerKeyNotName）是本卡最容易漏的一处：
// 名称与类型解耦后如果还按 Name 取档位，档位任务的 payload 校验会被整段绕过。

// nameTypeServer 造一台带档位账号的服务器，并补一个 email_send 类型：
// 用例需要"两种类型"才能证明 ?type= 筛的是类型而不是名称。
func nameTypeServer(t *testing.T, requiredRole string, commands ...core.ExecutorCommand) (*Server, func(string) http.Header) {
	t.Helper()

	srv := submissionServer(t, requiredRole, nil, commands...)
	srv.RegisterJobHandler("email_send", func(context.Context, *core.Job) error { return nil })
	return srv, identities(t, srv)
}

func decodeJob(t *testing.T, recorder *httptest.ResponseRecorder) JobResponse {
	t.Helper()

	var resp JobResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &resp), recorder.Body.String())
	return resp
}

func TestCreateJob_NameIsLabelAndTypeIsKey(t *testing.T) {
	srv, creds := nameTypeServer(t, "operator")
	admin := creds("admin")

	recorder := doJSON(t, srv, http.MethodPost, "/api/v1/jobs",
		`{"name":"每晚对账","type":"payment_check","payload":{"order_id":"123"}}`, admin)
	require.Equal(t, http.StatusCreated, recorder.Code, recorder.Body.String())

	resp := decodeJob(t, recorder)
	assert.Equal(t, "每晚对账", resp.Name, "名称是标签，原样留着")
	assert.Equal(t, "payment_check", resp.Type, "类型是查注册表用的那个键")
	assert.NotEmpty(t, resp.ID)

	// 读回同一条：类型必须存进快照，否则重启后按类型筛选与档位判定都会退回旧语义。
	detail := decodeJob(t, doGet(t, srv, "/api/v1/jobs/"+resp.ID, admin))
	assert.Equal(t, "payment_check", detail.Type)
	assert.Equal(t, "每晚对账", detail.Name)
}

func TestCreateJob_NameRuleAppliesOnlyToNewStyle(t *testing.T) {
	srv, creds := nameTypeServer(t, "operator")
	admin := creds("admin")

	// 带 type：名称套标签规则（core.ValidateJobName）。
	for _, body := range []string{
		`{"name":"a b","type":"payment_check"}`,
		`{"name":"   ","type":"payment_check"}`,
		`{"name":"payment_check","type":"payment_check"}`,
		`{"name":"对账_1","type":"payment_check"}`,
		`{"name":"对账-1","type":"payment_check"}`,
		`{"name":"` + strings.Repeat("汉", 65) + `","type":"payment_check"}`,
	} {
		recorder := doJSON(t, srv, http.MethodPost, "/api/v1/jobs", body, admin)
		require.Equal(t, http.StatusBadRequest, recorder.Code, "%s 应当被名称规则拒掉：%s", body, recorder.Body.String())

		var failure ErrorResponse
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &failure))
		assert.Equal(t, "invalid job name", failure.Message, "拒绝要说清是名称问题")
	}

	// 不带 type：旧写法，名称兼作注册键，因此不套标签规则。
	// payment_check 带下划线，如果在这里套了规则，既有调用方会全部拿到 400。
	recorder := doJSON(t, srv, http.MethodPost, "/api/v1/jobs",
		`{"name":"payment_check","payload":{"order_id":"1"}}`, admin)
	require.Equal(t, http.StatusCreated, recorder.Code, recorder.Body.String())
	assert.Empty(t, decodeJob(t, recorder).Type, "旧写法不写类型，回退规则交给 core.Job.HandlerKey")

	// 顺序：名称合法但类型没注册，报的是"类型未注册"而不是"名称不合法"。
	unknown := doJSON(t, srv, http.MethodPost, "/api/v1/jobs",
		`{"name":"每晚对账","type":"nope"}`, admin)
	require.Equal(t, http.StatusBadRequest, unknown.Code)
	assert.Contains(t, unknown.Body.String(), "unknown job type")
	assert.Contains(t, unknown.Body.String(), "nope", "details 要指到被查的那个键")
	assert.NotContains(t, unknown.Body.String(), "每晚对账", "标签不该出现在类型未注册的报错里")
}

func TestCreateJob_LegacyKeyMustNotBeBlank(t *testing.T) {
	srv, creds := nameTypeServer(t, "operator")
	admin := creds("admin")

	// type 是空白：按"没带 type"处理（判据是 trim 之后是否为空），
	// 于是走旧写法那条分支——名称不套标签规则，而是兼作注册键。
	recorder := doJSON(t, srv, http.MethodPost, "/api/v1/jobs",
		`{"name":"   ","type":"   "}`, admin)
	require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
	assert.Contains(t, recorder.Body.String(), "job type is required",
		"两个字段都给不出注册键时，报错说的是类型必填，而不是未注册或名称不合法")

	// name 是空白但没带 type：旧写法算不出键。
	blank := doJSON(t, srv, http.MethodPost, "/api/v1/jobs",
		`{"name":"   "}`, admin)
	require.Equal(t, http.StatusBadRequest, blank.Code, blank.Body.String())
	assert.Contains(t, blank.Body.String(), "job type is required",
		"两个字段都给不出注册键时，报错说的是类型必填，而不是未注册或名称不合法")
}

// TestExecutorGateUsesHandlerKeyNotName 是本卡的守门用例（设计文档 §10 风险 1）：
// 新写法建的档位任务，提交期判定必须照样发生。
// 把 api/handlers_executors.go 的 job.HandlerKey() 打回 job.Name，
// 下面第一条就会拿到 201 并入队——非法 payload 会被真的执行一次。
func TestExecutorGateUsesHandlerKeyNotName(t *testing.T) {
	srv, creds := nameTypeServer(t, "operator",
		submissionProfile("callback", core.ExecutorArg{
			Name: "day", Required: true, Pattern: `^\d{4}-\d{2}-\d{2}$`,
		}),
	)
	admin := creds("admin")

	t.Run("非法 payload 在提交期就被拒", func(t *testing.T) {
		recorder := doJSON(t, srv, http.MethodPost, "/api/v1/jobs",
			`{"name":"我的档位任务","type":"exec.callback","payload":{"params":{"day":"not-a-date"}}}`, admin)
		require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())

		var failure ErrorResponse
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &failure))
		assert.Equal(t, "invalid executor payload", failure.Message)
		assert.Contains(t, failure.Details, "params.day")

		// 关键是"没有入队"：入队了就会被执行一次再失败，副作用已经发生。
		listed := doGet(t, srv, "/api/v1/jobs?limit=50", admin)
		assert.NotContains(t, listed.Body.String(), "我的档位任务",
			"被判掉的档位任务不得出现在任务列表里")
	})

	t.Run("合法 payload 照常建成并带上类型", func(t *testing.T) {
		recorder := doJSON(t, srv, http.MethodPost, "/api/v1/jobs",
			`{"name":"我的档位任务","type":"exec.callback","payload":{"params":{"day":"2026-10-06"}}}`, admin)
		require.Equal(t, http.StatusCreated, recorder.Code, recorder.Body.String())

		resp := decodeJob(t, recorder)
		assert.Equal(t, "exec.callback", resp.Type)
		assert.Equal(t, "我的档位任务", resp.Name)
	})

	t.Run("身份档位判定也按类型走", func(t *testing.T) {
		// required_role=admin 的那台（本用例的 server 是 operator，所以另建一台）。
		strict, strictCreds := nameTypeServer(t, "admin",
			submissionProfile("callback", core.ExecutorArg{
				Name: "day", Required: true, Pattern: `^\d{4}-\d{2}-\d{2}$`,
			}),
		)
		recorder := doJSON(t, strict, http.MethodPost, "/api/v1/jobs",
			`{"name":"档位任务","type":"exec.callback","payload":{"params":{"day":"2026-10-06"}}}`,
			strictCreds("operator"))
		require.Equal(t, http.StatusForbidden, recorder.Code, recorder.Body.String())
		assert.Contains(t, recorder.Body.String(), "exec.callback",
			"拒绝要说清是哪个类型需要更高档位，而不是被标签名糊住")
	})

	t.Run("响应层的掩码按类型取档位", func(t *testing.T) {
		// payloadForResponse / execForResponse 用同一个键取档位：
		// 新写法建的含 secret 参数的任务，读列表时也要看到掩码。
		secretSrv, secretCreds := nameTypeServer(t, "operator", submissionProfile("hook",
			core.ExecutorArg{Name: "token", Required: true, Secret: true},
		))
		operator := secretCreds("operator")

		created := doJSON(t, secretSrv, http.MethodPost, "/api/v1/jobs",
			`{"name":"带凭据的任务","type":"exec.hook","payload":{"params":{"token":"s3cr3t"}}}`, operator)
		require.Equal(t, http.StatusCreated, created.Code, created.Body.String())

		listed := doGet(t, secretSrv, "/api/v1/jobs?limit=50", operator)
		assert.Contains(t, listed.Body.String(), "带凭据的任务", "任务确实用了新写法")
		assert.NotContains(t, listed.Body.String(), "s3cr3t",
			"按类型取到档位才能掩码：漏了解耦这里会漏出明文")
	})
}

func TestListJobs_FilterByTypeAndName(t *testing.T) {
	srv, creds := nameTypeServer(t, "operator")
	admin := creds("admin")

	// 同一个类型 payment_check 下的两种写法，加上另一个类型的一条。
	for _, body := range []string{
		`{"name":"payment_check","payload":{}}`,
		`{"name":"每晚对账","type":"payment_check","payload":{}}`,
		`{"name":"邮件通知","type":"email_send","payload":{}}`,
	} {
		recorder := doJSON(t, srv, http.MethodPost, "/api/v1/jobs", body, admin)
		require.Equal(t, http.StatusCreated, recorder.Code, "%s -> %s", body, recorder.Body.String())
	}

	t.Run("按类型筛命中两种写法", func(t *testing.T) {
		items := decodeList(t, doGet(t, srv, "/api/v1/jobs?type=payment_check&limit=50", admin))
		require.Len(t, items, 2, "旧写法（类型在名称里）与新写法都要命中")

		for _, item := range items {
			// 读侧口径：Type 为空表示旧写法，那时名称兼作类型（见 api/dto.go 的 JobResponse.Type）。
			key := item.Type
			if key == "" {
				key = item.Name
			}
			assert.Equal(t, "payment_check", key)
			assert.Contains(t, []string{"payment_check", "每晚对账"}, item.Name)
		}
	})

	t.Run("按名称筛只命中标签", func(t *testing.T) {
		items := decodeList(t, doGet(t, srv, "/api/v1/jobs?name=每晚对账&limit=50", admin))
		require.Len(t, items, 1)
		assert.Equal(t, "payment_check", items[0].Type)
	})

	t.Run("两个筛选可以叠加", func(t *testing.T) {
		items := decodeList(t, doGet(t, srv, "/api/v1/jobs?type=payment_check&name=payment_check&limit=50", admin))
		require.Len(t, items, 1, "只有旧写法那条同时满足两个条件")
		assert.Empty(t, items[0].Type)
	})

	t.Run("筛不存在的类型回空列表", func(t *testing.T) {
		items := decodeList(t, doGet(t, srv, "/api/v1/jobs?type=nope&limit=50", admin))
		assert.Empty(t, items)
	})
}

func TestUpdateJob_CannotSwitchType(t *testing.T) {
	srv, creds := nameTypeServer(t, "operator")
	admin := creds("admin")

	created := decodeJob(t, doJSON(t, srv, http.MethodPost, "/api/v1/jobs",
		`{"name":"每晚对账","type":"payment_check","payload":{}}`, admin))
	legacy := decodeJob(t, doJSON(t, srv, http.MethodPost, "/api/v1/jobs",
		`{"name":"payment_check","delay":"1h","payload":{}}`, admin))

	t.Run("换成别的类型被拒", func(t *testing.T) {
		recorder := doJSON(t, srv, http.MethodPut, "/api/v1/jobs/"+created.ID,
			`{"type":"email_send"}`, admin)
		require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())

		var failure ErrorResponse
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &failure))
		assert.Equal(t, "job type cannot be changed", failure.Message)
		assert.Contains(t, failure.Details, "payment_check")
		assert.Contains(t, failure.Details, "email_send")
	})

	t.Run("传相同类型按没传处理", func(t *testing.T) {
		recorder := doJSON(t, srv, http.MethodPut, "/api/v1/jobs/"+created.ID,
			`{"type":"payment_check","timeout":"20s"}`, admin)
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		assert.Equal(t, "20s", decodeJob(t, recorder).Timeout)
	})

	t.Run("旧写法的类型回退到名称", func(t *testing.T) {
		// 这条任务没写 type，类型就是 payment_check：传相同值应当放行。
		recorder := doJSON(t, srv, http.MethodPut, "/api/v1/jobs/"+legacy.ID,
			`{"type":"payment_check"}`, admin)
		assert.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

		switched := doJSON(t, srv, http.MethodPut, "/api/v1/jobs/"+legacy.ID,
			`{"type":"email_send"}`, admin)
		assert.Equal(t, http.StatusBadRequest, switched.Code, switched.Body.String())
	})

	t.Run("改名仍然被拒", func(t *testing.T) {
		recorder := doJSON(t, srv, http.MethodPut, "/api/v1/jobs/"+created.ID,
			`{"name":"另一个名字"}`, admin)
		require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
		assert.Contains(t, recorder.Body.String(), "job name cannot be changed")
	})
}

func TestBatchCreateJobs_MixedNameAndTypeStyles(t *testing.T) {
	srv, creds := nameTypeServer(t, "operator")
	admin := creds("admin")

	body := `[` +
		`{"name":"payment_check"},` +
		`{"name":"邮件通知","type":"email_send"},` +
		`{"name":"带 空格 的名称","type":"email_send"},` +
		`{"name":"缺类型","delay":"5m"}` +
		`]`
	recorder := doJSON(t, srv, http.MethodPost, "/api/v1/jobs/batch", body, admin)
	require.Equal(t, http.StatusMultiStatus, recorder.Code, recorder.Body.String())

	var resp BatchCreateJobsResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &resp))
	assert.Equal(t, 2, resp.Succeeded, "旧写法与新写法各一条照常创建")
	require.Len(t, resp.Errors, 2)

	assert.Equal(t, 2, resp.Errors[0].Index, "下标指向请求数组里的那一条")
	assert.Equal(t, http.StatusBadRequest, resp.Errors[0].Code)
	assert.Contains(t, resp.Errors[0].Details, "invalid job name", "第 3 条挂在名称规则上")

	assert.Equal(t, 3, resp.Errors[1].Index)
	assert.Equal(t, "unknown job type", resp.Errors[1].Message,
		`第 4 条没带 type，于是名称兼作键："缺类型"不是注册键`)

	listed := decodeList(t, doGet(t, srv, "/api/v1/jobs?limit=50", admin))
	require.Len(t, listed, 2)
	for _, item := range listed {
		assert.NotEqual(t, "带 空格 的名称", item.Name, "被判掉的不得入队")
	}
}

// decodeList 读 GET /jobs 的那一批响应，只关心 items。
func decodeList(t *testing.T, recorder *httptest.ResponseRecorder) []JobResponse {
	t.Helper()

	var resp struct {
		Items []JobResponse `json:"items"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &resp), recorder.Body.String())
	return resp.Items
}
