package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
	"godelayq/executor"
)

// newArtifactStoreFor 建一个指向临时目录的真实产物存储。
// 结果端点读的是文件，用假的读逻辑就测不到"读到的确实是那次尝试的那条流"。
func newArtifactStoreFor(t *testing.T, maxBytes int) *executor.ArtifactStore {
	t.Helper()

	store, err := executor.NewArtifactStore(executor.ArtifactOptions{
		Dir:      filepath.Join(t.TempDir(), "exec"),
		MaxBytes: maxBytes,
	}, newTestLogger())
	require.NoError(t, err)
	return store
}

// writeArtifact 真实写一次某条流的产物。
func writeArtifact(t *testing.T, store *executor.ArtifactStore, jobID string, attempt int, stream string, content []byte) {
	t.Helper()

	writer, err := store.Open(jobID, attempt)
	require.NoError(t, err)

	target := writer.Stdout()
	if stream == "err" {
		target = writer.Stderr()
	}
	_, err = target.Write(content)
	require.NoError(t, err)
	_, err = writer.Close()
	require.NoError(t, err)
}

// putSnapshot 往存储里放一条任务快照。meta 为 nil 表示"这不是执行器任务"。
func putSnapshot(t *testing.T, srv *Server, jobID string, attempts int, meta *core.ExecMeta) {
	t.Helper()

	require.NoError(t, srv.store.Update(core.JobSnapshot{
		ID:       jobID,
		Name:     "exec.demo",
		Attempts: attempts,
		Status:   int(core.StatusSuccess),
		Exec:     meta,
	}))
}

func decodeJobResult(t *testing.T, recorder *httptest.ResponseRecorder) JobResultResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	var resp JobResultResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &resp), recorder.Body.String())
	return resp
}

// countingStore 只多记一件事：写了多少次。
// 产物缺失的回写必须每个尝试最多一次，否则反复查询会把读路径变成写路径。
type countingStore struct {
	core.Store

	mu      sync.Mutex
	updates int
}

func (c *countingStore) Update(snapshot core.JobSnapshot) error {
	c.mu.Lock()
	c.updates++
	c.mu.Unlock()
	return c.Store.Update(snapshot)
}

func (c *countingStore) updateCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.updates
}

func TestGetJobResult_TailAndHead(t *testing.T) {
	artifacts := newArtifactStoreFor(t, 1<<20)
	srv := newSecurityServer(t, Security{}, WithArtifacts(artifacts))

	body := strings.Repeat("a", 10240) + strings.Repeat("b", 10240)
	writeArtifact(t, artifacts, "job-1", 2, "out", []byte(body))
	putSnapshot(t, srv, "job-1", 2, &core.ExecMeta{
		Kind: "script", Profile: "demo", ExitCode: 1,
		OutBytes: int64(len(body)), Artifact: core.ArtifactAvailable,
	})

	tail := decodeJobResult(t, doGet(t, srv, "/api/v1/jobs/job-1/result?max_bytes=1024&from=tail", nil))
	assert.Equal(t, "job-1", tail.JobID)
	assert.Equal(t, 2, tail.Attempt, "省略 attempt 时取最近一次已结束的尝试")
	assert.Equal(t, "out", tail.Stream)
	assert.True(t, tail.Found)
	assert.Equal(t, int64(len(body)), tail.SizeBytes, "size_bytes 是文件里的总量，与本次返回多少无关")
	assert.Equal(t, 1024, tail.ReturnedBytes)
	assert.True(t, tail.Truncated)
	assert.Equal(t, strings.Repeat("b", 1024), tail.Content)
	assert.Equal(t, core.ArtifactAvailable, tail.Meta.Artifact, "文件在，产物状态不该被改写")

	head := decodeJobResult(t, doGet(t, srv, "/api/v1/jobs/job-1/result?max_bytes=1024&from=head", nil))
	assert.Equal(t, strings.Repeat("a", 1024), head.Content)
	assert.True(t, head.Truncated)

	// 正好读满不算截断：调用方拿到的是全部内容
	full := decodeJobResult(t, doGet(t, srv, "/api/v1/jobs/job-1/result?max_bytes="+strconv.Itoa(len(body)), nil))
	assert.False(t, full.Truncated)
	assert.Equal(t, len(body), full.ReturnedBytes)
	assert.Equal(t, body, full.Content)

	// stderr 从没写过：文件在但内容为空，与"没有产物"是两回事
	empty := decodeJobResult(t, doGet(t, srv, "/api/v1/jobs/job-1/result?stream=err", nil))
	assert.True(t, empty.Found, "两个流在每次尝试时都会建出来，空的 stderr 也是存在的产物")
	assert.Empty(t, empty.Content)
	assert.False(t, empty.Truncated)
}

func TestGetJobResult_DefaultMaxBytesFollowsPreviewLimit(t *testing.T) {
	artifacts := newArtifactStoreFor(t, 1<<20)
	cfg := core.DefaultConfig()
	cfg.Executors.Enabled = true
	cfg.Executors.Workspace = t.TempDir()
	cfg.Executors.Output.InlinePreview = 64
	registry, err := executor.NewRegistry(cfg, newTestLogger())
	require.NoError(t, err)

	srv := newSecurityServer(t, Security{}, WithArtifacts(artifacts), WithExecutorRegistry(registry))
	writeArtifact(t, artifacts, "job-def", 1, "out", []byte(strings.Repeat("x", 4096)))
	putSnapshot(t, srv, "job-def", 1, &core.ExecMeta{Kind: "script", Profile: "demo"})

	resp := decodeJobResult(t, doGet(t, srv, "/api/v1/jobs/job-def/result", nil))
	assert.Equal(t, 64*resultDefaultReadFactor, resp.ReturnedBytes,
		"没带 max_bytes 时读预览上限的 4 倍")
	assert.True(t, resp.Truncated)
}

func TestGetJobResult_RejectsBadParameters(t *testing.T) {
	artifacts := newArtifactStoreFor(t, 1<<20)
	srv := newSecurityServer(t, Security{}, WithArtifacts(artifacts))

	writeArtifact(t, artifacts, "job-bad", 2, "out", []byte("some output"))
	putSnapshot(t, srv, "job-bad", 2, &core.ExecMeta{Kind: "script", Profile: "demo"})

	for query, wantMessage := range map[string]string{
		"stream=stdout":         "invalid stream",
		"from=middle":           "invalid from",
		"attempt=3":             "invalid attempt",
		"attempt=0&max_bytes=0": "invalid max_bytes",
		"attempt=abc":           "invalid attempt",
		"max_bytes=-1":          "invalid max_bytes",
		"max_bytes=9999999999":  "invalid max_bytes",
		"max_bytes=abc":         "invalid max_bytes",
		"stream=OUT":            "invalid stream",
	} {
		recorder := doGet(t, srv, "/api/v1/jobs/job-bad/result?"+query, nil)
		assert.Equal(t, http.StatusBadRequest, recorder.Code, query+" -> "+recorder.Body.String())

		var failure ErrorResponse
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &failure))
		assert.Equal(t, wantMessage, failure.Message, query)
		assert.NotEmpty(t, failure.Details, query+" 的 details 要说明允许的写法")
	}

	// attempt=0 与省略同义：读最近一次已结束的尝试，不是 400
	assert.Equal(t, http.StatusOK, doGet(t, srv, "/api/v1/jobs/job-bad/result?attempt=0", nil).Code)
}

func TestGetJobResult_UnknownJobAndPlainJob(t *testing.T) {
	artifacts := newArtifactStoreFor(t, 1<<20)
	srv := newSecurityServer(t, Security{}, WithArtifacts(artifacts))

	missing := doGet(t, srv, "/api/v1/jobs/no-such-job/result", nil)
	require.Equal(t, http.StatusNotFound, missing.Code)
	assert.Contains(t, missing.Body.String(), "job not found")

	// 非执行器任务：没有摘要也没有产物文件
	putSnapshot(t, srv, "plain-1", 1, nil)
	plain := doGet(t, srv, "/api/v1/jobs/plain-1/result", nil)
	require.Equal(t, http.StatusNotFound, plain.Code)

	var failure ErrorResponse
	require.NoError(t, json.Unmarshal(plain.Body.Bytes(), &failure))
	assert.Equal(t, "no execution result for this job", failure.Message)
}

// TestGetJobResult_PurgedArtifact 检查降级与回写：摘要留着、正文已经没了，
// 于是接口给 found=false，并把快照里的 available 改成 purged，只改一次。
func TestGetJobResult_PurgedArtifact(t *testing.T) {
	artifacts := newArtifactStoreFor(t, 1<<20)

	inner, err := core.NewJSONFileStore(filepath.Join(t.TempDir(), "jobs.json"))
	require.NoError(t, err)
	counted := &countingStore{Store: inner}
	t.Cleanup(func() { _ = inner.Close() })

	gin.SetMode(gin.TestMode)
	srv := NewServer(core.NewScheduler(counted, nil, nil), counted, "0", Security{}, newTestLogger(),
		WithArtifacts(artifacts))

	putSnapshot(t, srv, "job-gone", 3, &core.ExecMeta{
		Kind: "script", Profile: "demo", ExitCode: 0,
		Preview: "the only thing left", Artifact: core.ArtifactAvailable,
	})
	before := counted.updateCount()

	first := decodeJobResult(t, doGet(t, srv, "/api/v1/jobs/job-gone/result", nil))
	assert.False(t, first.Found)
	assert.Empty(t, first.Content)
	assert.Zero(t, first.SizeBytes)
	require.NotNil(t, first.Meta)
	assert.Equal(t, core.ArtifactPurged, first.Meta.Artifact)
	assert.Equal(t, "the only thing left", first.Meta.Preview, "正文没了，结论还是要给")
	assert.Equal(t, before+1, counted.updateCount(), "读路径上只有这一处写，且只写一次")

	// 第二次读不再回写：状态已经存下来了
	doGet(t, srv, "/api/v1/jobs/job-gone/result", nil)
	assert.Equal(t, before+1, counted.updateCount(), "已经标成 purged 就不该再写一遍")

	var reloaded []core.JobSnapshot
	reloaded, err = srv.store.LoadAll()
	require.NoError(t, err)
	for _, snapshot := range reloaded {
		if snapshot.ID == "job-gone" {
			require.NotNil(t, snapshot.Exec)
			assert.Equal(t, core.ArtifactPurged, snapshot.Exec.Artifact, "回写要落到存储里")
		}
	}
}

func TestGetJobResult_WithoutArtifactStore(t *testing.T) {
	srv := newSecurityServer(t, Security{})

	recorder := doGet(t, srv, "/api/v1/jobs/whatever/result", nil)
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code, recorder.Body.String())

	var failure ErrorResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &failure))
	assert.Contains(t, failure.Message, "not configured")
	assert.Contains(t, failure.Details, "api.WithArtifacts")
}

func TestGetJobResult_IsNotCacheable(t *testing.T) {
	artifacts := newArtifactStoreFor(t, 1<<20)
	srv := newSecurityServer(t, Security{}, WithArtifacts(artifacts))

	writeArtifact(t, artifacts, "job-cache", 1, "out", []byte("hello"))
	putSnapshot(t, srv, "job-cache", 1, &core.ExecMeta{Kind: "script", Profile: "demo"})

	recorder := doGet(t, srv, "/api/v1/jobs/job-cache/result", nil)
	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
	assert.True(t, strings.HasPrefix(recorder.Header().Get("Content-Type"), "application/json"),
		"结果正文一律按 JSON 给出，不做纯文本流式输出，实际 %q", recorder.Header().Get("Content-Type"))
}

func TestGetJobResult_RequiresAuth(t *testing.T) {
	artifacts := newArtifactStoreFor(t, 1<<20)
	srv := newSecurityServer(t, accountsSecurity(t), WithArtifacts(artifacts))

	writeArtifact(t, artifacts, "job-auth", 1, "out", []byte("hello"))
	putSnapshot(t, srv, "job-auth", 1, &core.ExecMeta{Kind: "script", Profile: "demo"})

	assert.Equal(t, http.StatusUnauthorized, doGet(t, srv, "/api/v1/jobs/job-auth/result", nil).Code)

	viewer := login(t, srv, testViewerName, testPassword)
	recorder := doGet(t, srv, "/api/v1/jobs/job-auth/result", bearer(viewer.AccessToken))
	assert.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
}

// executorConfigFor 搭一份可预测的档位配置：http 档位的探测结论恒为可用
// （探测不访问网络），脚本档位指向一个不存在的文件，因此恒为不可用。
func executorConfigFor(workspace string, commands ...core.ExecutorCommand) core.Config {
	cfg := core.DefaultConfig()
	cfg.Executors.Enabled = true
	cfg.Executors.Workspace = workspace
	cfg.Executors.Commands = commands
	return cfg
}

func TestListExecutors_ShowsProfilesSortedAndAvailability(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "exec-ws-hidden")
	registry, err := executor.NewRegistry(executorConfigFor(workspace,
		core.ExecutorCommand{
			Name:         "zeta_http",
			Kind:         "http",
			Method:       "GET",
			URLTemplate:  "https://api.example.com/reports/{day}",
			AllowedHosts: []string{"api.example.com"},
			Args:         []core.ExecutorArg{{Name: "day", Required: true, Pattern: `^\d{4}-\d{2}-\d{2}$`}},
			Timeout:      10 * time.Minute,
		},
		core.ExecutorCommand{
			Name:        "alpha_script",
			Kind:        "script",
			Runtime:     "node",
			Script:      "scripts/not-deployed.mjs",
			Args:        []core.ExecutorArg{{Name: "token", Secret: true}},
			ArgsRender:  []string{"--token={token}"},
			Env:         map[string]string{"API_TOKEN": "JWT-secret-should-not-appear"},
			EnvAllow:    []string{"TZ"},
			MaxParallel: 2,
		},
	), newTestLogger())
	require.NoError(t, err)

	srv := newSecurityServer(t, Security{}, WithExecutorRegistry(registry))
	recorder := doGet(t, srv, "/api/v1/executors", nil)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	var resp ListExecutorsResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &resp))
	assert.True(t, resp.Enabled)
	assert.Nil(t, resp.RequiredRole, "required_role 由 TASK-E16 给出真实取值，本卡固定为 null")
	require.Len(t, resp.Profiles, 2)

	// 按注册键字典序，而不是配置里的声明顺序
	assert.Equal(t, "exec.alpha_script", resp.Profiles[0].Key)
	assert.Equal(t, "exec.zeta_http", resp.Profiles[1].Key)

	script := resp.Profiles[0]
	assert.Equal(t, "alpha_script", script.Name)
	assert.Equal(t, "script", script.Kind)
	assert.False(t, script.RuntimeOK, "脚本文件没部署，探测结论要看得见")
	assert.NotEmpty(t, script.Reason)
	assert.Equal(t, 2, script.MaxParallel)
	assert.Equal(t, []string{"TZ"}, script.EnvAllow)
	require.Len(t, script.Args, 1)
	assert.Equal(t, "token", script.Args[0].Name)
	assert.True(t, script.Args[0].Secret)
	assert.False(t, script.Args[0].Required)
	assert.Empty(t, script.URL, "只有 http 档位给 URL")

	httpProfile := resp.Profiles[1]
	assert.True(t, httpProfile.RuntimeOK)
	assert.Empty(t, httpProfile.Reason)
	assert.Equal(t, "https://api.example.com/reports/{day}", httpProfile.URL, "模板原文，占位符不渲染")
	assert.Equal(t, "10m0s", httpProfile.Timeout)
	require.Len(t, httpProfile.Args, 1)
	assert.True(t, httpProfile.Args[0].Required)
	assert.Equal(t, `^\d{4}-\d{2}-\d{2}$`, httpProfile.Args[0].Pattern)

	// 凭据与目录结构都不该出现在响应里
	text := recorder.Body.String()
	assert.NotContains(t, text, "JWT-secret-should-not-appear", "env 的固定值是配置里的凭据")
	assert.NotContains(t, text, "exec-ws-hidden", "脚本绝对路径不外露，只给相对 workspace 的写法")
}

func TestListExecutors_DefaultStateIsNotAnError(t *testing.T) {
	// 开关关着的登记表
	closedRegistry, err := executor.NewRegistry(core.DefaultConfig(), newTestLogger())
	require.NoError(t, err)
	srv := newSecurityServer(t, Security{}, WithExecutorRegistry(closedRegistry))

	recorder := doGet(t, srv, "/api/v1/executors", nil)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	assert.JSONEq(t, `{"enabled":false,"required_role":null,"profiles":[]}`, recorder.Body.String())

	// 压根没注入登记表（测试里直接构造 Server 的形态）：同一个答案，同样不是 503
	plain := newSecurityServer(t, Security{})
	recorder = doGet(t, plain, "/api/v1/executors", nil)
	require.Equal(t, http.StatusOK, recorder.Code)
	assert.JSONEq(t, `{"enabled":false,"required_role":null,"profiles":[]}`, recorder.Body.String())
}

func TestListExecutors_RequiresAuth(t *testing.T) {
	registry, err := executor.NewRegistry(executorConfigFor(t.TempDir(),
		core.ExecutorCommand{
			Name:         "ping",
			Kind:         "http",
			Method:       "GET",
			URLTemplate:  "https://api.example.com/health",
			AllowedHosts: []string{"api.example.com"},
		},
	), newTestLogger())
	require.NoError(t, err)

	srv := newSecurityServer(t, accountsSecurity(t), WithExecutorRegistry(registry))
	assert.Equal(t, http.StatusUnauthorized, doGet(t, srv, "/api/v1/executors", nil).Code)

	viewer := login(t, srv, testViewerName, testPassword)
	recorder := doGet(t, srv, "/api/v1/executors", bearer(viewer.AccessToken))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	var resp ListExecutorsResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &resp))
	assert.True(t, resp.Enabled)
	assert.Len(t, resp.Profiles, 1)
}
