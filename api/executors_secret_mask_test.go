package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
)

// 这一组是 TASK-E16 §3.3 的输出层用例：含 secret 参数的任务在读取接口里不显示明文、
// 结果端点按档位收严，以及掩码不改动存储这条限制本身。
// 未启用鉴权的 warn 与拒绝日志的档位字段同属 E16，写在 api/executors_submission_test.go。

const maskSecretValue = "pa55word-in-payload"

// maskServer 带两条档位：一条有 secret 参数，一条没有。
// 提交档位取 operator，好让四种身份都能走到掩码与判档这一段。
func maskServer(t *testing.T, logs *bytes.Buffer) *Server {
	t.Helper()

	return submissionServer(t, "operator", logs,
		submissionProfile("masked_hook", core.ExecutorArg{Name: "token", Required: true, Secret: true}),
		submissionProfile("open_hook", core.ExecutorArg{Name: "day"}),
	)
}

// seedExecutorJob 往存储里放一条执行器任务的快照，状态由调用方给。
// 重试与结果端点都要读已经跑过的任务，而测试里的调度器不启动，所以直接播种。
func seedExecutorJob(t *testing.T, srv *Server, jobID, name string, payload string, status core.JobStatus, meta *core.ExecMeta) {
	t.Helper()

	require.NoError(t, srv.store.Update(core.JobSnapshot{
		ID:        jobID,
		Name:      name,
		Payload:   []byte(payload),
		Attempts:  1,
		Status:    int(status),
		Exec:      meta,
		TriggerAt: time.Now(),
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}))
}

// TestJobResponses_MaskSecretArgs 覆盖 §5.6 要求的四处读取接口。
// 这四处都经 toJobResponse 这一个出口，所以断言的是"出口只有一处"这件事本身。
func TestJobResponses_MaskSecretArgs(t *testing.T) {
	srv := maskServer(t, nil)
	creds := identities(t, srv)
	admin := creds("admin")
	payload := `{"params":{"token":"` + maskSecretValue + `"}}`

	created := doJSON(t, srv, http.MethodPost, "/api/v1/jobs",
		`{"name":"exec.masked_hook","payload":`+payload+`}`, admin)
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var job JobResponse
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &job))

	assert.NotContains(t, created.Body.String(), maskSecretValue, "201 响应里没有明文")
	assert.Contains(t, created.Body.String(), `"token":"***"`)

	t.Run("GET /jobs 列表", func(t *testing.T) {
		listed := doGet(t, srv, "/api/v1/jobs?limit=50", admin)
		require.Equal(t, http.StatusOK, listed.Code)
		assert.NotContains(t, listed.Body.String(), maskSecretValue)
		assert.Contains(t, listed.Body.String(), `"token":"***"`)
	})

	t.Run("GET /jobs/:id 详情", func(t *testing.T) {
		fetched := doGet(t, srv, "/api/v1/jobs/"+job.ID, admin)
		require.Equal(t, http.StatusOK, fetched.Code)
		assert.NotContains(t, fetched.Body.String(), maskSecretValue)
	})

	t.Run("POST /jobs/:id/retry", func(t *testing.T) {
		seedExecutorJob(t, srv, "job-retry-mask", "exec.masked_hook", payload, core.StatusFailed, nil)

		retried := doJSON(t, srv, http.MethodPost, "/api/v1/jobs/job-retry-mask/retry", `{}`, admin)
		require.Equal(t, http.StatusOK, retried.Code, retried.Body.String())
		assert.NotContains(t, retried.Body.String(), maskSecretValue)
		assert.Contains(t, retried.Body.String(), `"token":"***"`)
	})

	t.Run("没有 secret 声明的档位原样给出", func(t *testing.T) {
		plain := doJSON(t, srv, http.MethodPost, "/api/v1/jobs",
			`{"name":"exec.open_hook","payload":{"params":{"day":"2026-09-30"}}}`, admin)
		require.Equal(t, http.StatusCreated, plain.Code, plain.Body.String())
		assert.Contains(t, plain.Body.String(), "2026-09-30")
	})

	t.Run("掩码不改动存储", func(t *testing.T) {
		// §3.3 第 3 条：设计文档 §8 不做加密，所以磁盘与快照里仍是原文。
		// 这条用例的意义在于"别把它当成加密"——哪天有人改成存储也掩码，这里会先失败。
		snapshots, err := srv.store.LoadAll()
		require.NoError(t, err)
		var found bool
		for _, snapshot := range snapshots {
			if snapshot.Name == "exec.masked_hook" && strings.Contains(string(snapshot.Payload), maskSecretValue) {
				found = true
			}
		}
		assert.True(t, found, "快照里的 payload 必须还是原文，执行侧要用的就是这份原文")
	})
}

// TestJobResponses_MaskSecretArgsInPreview 管的是另一路泄露：脚本或对端把参数值打印进输出，
// 那份尾部预览随摘要落盘、进事件、并从任务详情与列表接口出去（读取身份只需 viewer）。
// 执行侧写摘要时已按值掩过一遍（executor.Profile.MaskSecretText），这一组用例钉的是响应层：
// 在那之前落盘的快照里的预览也要掩，而且不改存储。
func TestJobResponses_MaskSecretArgsInPreview(t *testing.T) {
	srv := maskServer(t, nil)
	creds := identities(t, srv)
	admin := creds("admin")
	payload := `{"params":{"token":"` + maskSecretValue + `"}}`
	summary := &core.ExecMeta{
		Kind:       "http",
		Profile:    "masked_hook",
		HTTPStatus: 200,
		Preview:    `{"order": "` + maskSecretValue + `", "state": "accepted"}`,
		Artifact:   core.ArtifactPurged,
	}

	seedExecutorJob(t, srv, "job-preview-mask", "exec.masked_hook", payload, core.StatusSuccess, summary)
	seedExecutorJob(t, srv, "job-preview-open", "exec.open_hook",
		`{"params":{"day":"2026-09-30"}}`, core.StatusSuccess, &core.ExecMeta{
			Kind: "http", Profile: "open_hook", Preview: "day 2026-09-30 accepted", Artifact: core.ArtifactPurged,
		})

	fetched := doGet(t, srv, "/api/v1/jobs/job-preview-mask", admin)
	require.Equal(t, http.StatusOK, fetched.Code)
	assert.NotContains(t, fetched.Body.String(), maskSecretValue, "详情里的预览不留明文")
	assert.Contains(t, fetched.Body.String(), `{\"order\": \"***\"`, "掩码只替换取值，输出结构仍可读")

	listed := doGet(t, srv, "/api/v1/jobs?limit=50", admin)
	require.Equal(t, http.StatusOK, listed.Code)
	assert.NotContains(t, listed.Body.String(), maskSecretValue, "列表里的预览同样不留明文")

	open := doGet(t, srv, "/api/v1/jobs/job-preview-open", admin)
	require.Equal(t, http.StatusOK, open.Code)
	assert.Contains(t, open.Body.String(), "day 2026-09-30 accepted",
		"没有 secret 声明的档位，预览原样给出")

	snapshots, err := srv.store.LoadAll()
	require.NoError(t, err)
	for _, snapshot := range snapshots {
		if snapshot.ID == "job-preview-mask" {
			require.NotNil(t, snapshot.Exec)
			assert.Contains(t, snapshot.Exec.Preview, maskSecretValue,
				"存储里的摘要保持原样：响应层的掩码不回写")
		}
	}
}

// TestGetJobResult_StrictRoleForSecretProfiles 是 E07 预留的那个判档点：
// 含 secret 参数的档位，结果端点从 reader 升到 executors.required_role。
func TestGetJobResult_StrictRoleForSecretProfiles(t *testing.T) {
	artifacts := newArtifactStoreFor(t, 1<<20)
	sec := accountsSecurity(t)
	sec.Auth.Token = testToken
	srv := submissionServerWith(t, sec, "admin", newTestLogger(),
		[]core.ExecutorCommand{
			submissionProfile("masked_hook", core.ExecutorArg{Name: "token", Required: true, Secret: true}),
			submissionProfile("open_hook"),
		},
		WithArtifacts(artifacts),
	)
	creds := identities(t, srv)

	writeArtifact(t, artifacts, "job-secret", 1, "out", []byte("stdout echoed the value pa55word-in-payload"))
	seedExecutorJob(t, srv, "job-secret", "exec.masked_hook", `{}`, core.StatusSuccess,
		&core.ExecMeta{Kind: "http", Profile: "masked_hook", HTTPStatus: 200})
	seedExecutorJob(t, srv, "job-open", "exec.open_hook", `{}`, core.StatusSuccess,
		&core.ExecMeta{Kind: "http", Profile: "open_hook", HTTPStatus: 200})

	t.Run("operator 读含 secret 档位的输出被拒", func(t *testing.T) {
		recorder := doGet(t, srv, "/api/v1/jobs/job-secret/result", creds("operator"))
		require.Equal(t, http.StatusForbidden, recorder.Code, recorder.Body.String())
		assert.NotContains(t, recorder.Body.String(), "pa55word-in-payload",
			"403 的响应里不该先把正文带出去")
	})

	t.Run("admin 读得到正文并带一句说明", func(t *testing.T) {
		recorder := doGet(t, srv, "/api/v1/jobs/job-secret/result", creds("admin"))
		result := decodeJobResult(t, recorder)
		assert.Contains(t, result.Content, "pa55word-in-payload")
		assert.NotEmpty(t, result.RedactionNote, "§3.3 要求的 redaction_note")
		assert.Contains(t, result.RedactionNote, "secret")
	})

	t.Run("没有 secret 声明的档位照旧是 reader", func(t *testing.T) {
		recorder := doGet(t, srv, "/api/v1/jobs/job-open/result", creds("viewer"))
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		result := decodeJobResult(t, recorder)
		assert.Empty(t, result.RedactionNote)
	})

	t.Run("判档在参数解析之前", func(t *testing.T) {
		// 越权的连接问的是不存在的流：先给 403，不给 400。
		// 否则"这个任务有产物文件"这件事就从状态码里读出来了。
		recorder := doGet(t, srv, "/api/v1/jobs/job-secret/result?stream=nope",
			creds("operator"))
		assert.Equal(t, http.StatusForbidden, recorder.Code, recorder.Body.String())
	})
}

// TestEvents_NoSecretLeak 覆盖 §5.8：事件流里不出现 secret 参数值。
// 源头是 E08 的错误文本规则（值不进错误信息），这里验的是"到了接口这一层也没漏"。
func TestEvents_NoSecretLeak(t *testing.T) {
	srv := maskServer(t, nil)
	creds := identities(t, srv)
	admin := creds("admin")

	created := doJSON(t, srv, http.MethodPost, "/api/v1/jobs",
		`{"name":"exec.masked_hook","payload":{"params":{"token":"`+maskSecretValue+`"}}}`, admin)
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var job JobResponse
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &job))

	// 一条按错误文本落终态的任务：错误串是事件里唯一可能带内容的字段
	seedExecutorJob(t, srv, "job-failed-secret", "exec.masked_hook",
		`{"params":{"token":"`+maskSecretValue+`"}}`, core.StatusPending, nil)

	for _, target := range []string{"/api/v1/jobs/" + job.ID + "/events", "/api/v1/events",
		"/api/v1/jobs/job-failed-secret/events"} {
		recorder := doGet(t, srv, target, admin)
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		assert.NotContains(t, recorder.Body.String(), maskSecretValue, "事件端点 %s 泄出了参数值", target)
	}
}
