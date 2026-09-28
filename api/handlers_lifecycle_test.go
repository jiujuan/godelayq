package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
)

func (s *APITestSuite) TestPauseAndResumeOverHTTP() {
	s.registerPaymentHandler()

	var created JobResponse
	require.Equal(s.T(), 201, s.doJSON("POST", "/api/v1/jobs",
		CreateJobRequest{Name: "payment_check", Delay: "2h", Group: "nightly"}, &created))

	var paused JobResponse
	require.Equal(s.T(), 200, s.doJSON("POST", "/api/v1/jobs/"+created.ID+"/pause", nil, &paused))
	assert.Equal(s.T(), "paused", paused.Status)
	assert.Equal(s.T(), "nightly", paused.Group, "暂停不能把任务从分组视图里弄丢")
	assert.Equal(s.T(), 0, s.scheduler.HeapLen())
	assert.Empty(s.T(), paused.NextRunIn, "暂停的任务没有下一次触发")

	// 双击/重试不该变成 409：Pause 对已暂停任务幂等
	require.Equal(s.T(), 200, s.doJSON("POST", "/api/v1/jobs/"+created.ID+"/pause", nil, &paused))

	var resumed JobResponse
	require.Equal(s.T(), 200, s.doJSON("POST", "/api/v1/jobs/"+created.ID+"/resume", nil, &resumed))
	assert.Equal(s.T(), created.ID, resumed.ID, "恢复要沿用原 ID")
	assert.Equal(s.T(), "pending", resumed.Status)
	assert.Equal(s.T(), 1, s.scheduler.HeapLen())

	var conflict ErrorResponse
	code := s.doJSON("POST", "/api/v1/jobs/"+created.ID+"/resume", nil, &conflict)
	assert.Equal(s.T(), 409, code)
	assert.Equal(s.T(), "job is not paused", conflict.Message)

	var missing ErrorResponse
	assert.Equal(s.T(), 404, s.doJSON("POST", "/api/v1/jobs/nope/pause", nil, &missing))
}

// TestPauseRunningJobPointsAtForcePause 验证 409 的文案真的指向下一步动作：
// 普通暂停管不了执行中的任务，这是 operator 与 admin 的分界。
func (s *APITestSuite) TestPauseRunningJobPointsAtForcePause() {
	release := make(chan struct{})
	started := make(chan struct{})

	s.server.RegisterJobHandler("slow", func(ctx context.Context, job *core.Job) error {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return ctx.Err()
	})

	var created JobResponse
	require.Equal(s.T(), 201, s.doJSON("POST", "/api/v1/jobs",
		CreateJobRequest{Name: "slow", Delay: "0s"}, &created))

	s.scheduler.Start()
	defer func() {
		close(release)
		s.scheduler.Stop()
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		s.T().Fatal("任务没有开始执行")
	}

	var conflict ErrorResponse
	code := s.doJSON("POST", "/api/v1/jobs/"+created.ID+"/pause", nil, &conflict)
	assert.Equal(s.T(), 409, code)
	assert.Contains(s.T(), conflict.Message, "force-pause")

	// 强制暂停：中止当前尝试并停在 paused，不计失败
	var forced JobResponse
	require.Equal(s.T(), 200, s.doJSON("POST", "/api/v1/jobs/"+created.ID+"/force-pause", nil, &forced))

	// 收尾是异步的：轮询到停在 paused 为止（在测试协程里轮询，
	// 交给 require.Eventually 会让断言跑在非测试 goroutine 上）
	deadline := time.Now().Add(2 * time.Second)
	var reread JobResponse
	for time.Now().Before(deadline) {
		reread = JobResponse{}
		require.Equal(s.T(), 200, s.doJSON("GET", "/api/v1/jobs/"+created.ID, nil, &reread))
		if reread.Status == "paused" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.Equal(s.T(), "paused", reread.Status, "收尾后任务必须停在 paused")

	var stats StatsResponse
	require.Equal(s.T(), 200, s.doJSON("GET", "/api/v1/stats", nil, &stats))
	assert.Equal(s.T(), 1, stats.Paused)
	assert.Zero(s.T(), stats.Failed, "强制暂停不计失败")
}

func (s *APITestSuite) TestBatchJobOpsPausesMovesAndReportsFailures() {
	s.registerPaymentHandler()

	createdIDs := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		var created JobResponse
		require.Equal(s.T(), 201, s.doJSON("POST", "/api/v1/jobs",
			CreateJobRequest{Name: "payment_check", Delay: "2h", Group: "nightly"}, &created))
		createdIDs = append(createdIDs, created.ID)
	}

	var pauseResult BatchJobOpsResponse
	code := s.doJSON("POST", "/api/v1/jobs/batch-ops", BatchJobOpsRequest{
		Action: jobOpPause,
		IDs:    []string{createdIDs[0], "does-not-exist", createdIDs[1]},
	}, &pauseResult)
	require.Equal(s.T(), 207, code)
	assert.Equal(s.T(), 2, pauseResult.Succeeded)
	assert.Equal(s.T(), 1, pauseResult.Failed)
	require.Len(s.T(), pauseResult.Errors, 1)
	assert.Equal(s.T(), "does-not-exist", pauseResult.Errors[0].ID)
	assert.Equal(s.T(), http.StatusNotFound, pauseResult.Errors[0].Code)
	assert.Len(s.T(), pauseResult.Items, 2)
	for _, item := range pauseResult.Items {
		assert.Equal(s.T(), "paused", item.Status)
	}

	// 移组对暂停中的任务同样有效：它改的是快照，不需要任务回到堆里
	moved := "weekend"
	var moveResult BatchJobOpsResponse
	require.Equal(s.T(), 207, s.doJSON("POST", "/api/v1/jobs/batch-ops", BatchJobOpsRequest{
		Action: jobOpMove,
		IDs:    createdIDs,
		Group:  &moved,
	}, &moveResult))
	require.Len(s.T(), moveResult.Items, 3)
	for _, item := range moveResult.Items {
		assert.Equal(s.T(), "weekend", item.Group)
	}

	// 只恢复刚才暂停的那两条：第三条一直是 pending，恢复它会得到 409
	var resumeResult BatchJobOpsResponse
	require.Equal(s.T(), 207, s.doJSON("POST", "/api/v1/jobs/batch-ops", BatchJobOpsRequest{
		Action: jobOpResume,
		IDs:    []string{createdIDs[0], createdIDs[1], createdIDs[2]},
	}, &resumeResult))
	assert.Equal(s.T(), 2, resumeResult.Succeeded)
	assert.Equal(s.T(), 1, resumeResult.Failed)
	assert.Equal(s.T(), http.StatusConflict, resumeResult.Errors[0].Code)
	assert.Equal(s.T(), 3, s.scheduler.HeapLen())

	var cancelResult BatchJobOpsResponse
	require.Equal(s.T(), 207, s.doJSON("POST", "/api/v1/jobs/batch-ops", BatchJobOpsRequest{
		Action: jobOpCancel,
		IDs:    createdIDs,
	}, &cancelResult))
	assert.Equal(s.T(), 3, cancelResult.Succeeded)
	assert.Empty(s.T(), cancelResult.Items, "取消后已经没有任务现状可返回")
	assert.Equal(s.T(), 0, s.scheduler.HeapLen())
}

func (s *APITestSuite) TestBatchJobOpsRejectsMalformedRequests() {
	s.registerPaymentHandler()

	var resp ErrorResponse

	require.Equal(s.T(), 400, s.doJSON("POST", "/api/v1/jobs/batch-ops",
		BatchJobOpsRequest{Action: "nuke", IDs: []string{"x"}}, &resp))
	assert.Equal(s.T(), "unsupported action", resp.Message)

	assert.Equal(s.T(), 400, s.doJSON("POST", "/api/v1/jobs/batch-ops",
		BatchJobOpsRequest{Action: jobOpPause, IDs: []string{}}, &resp))

	assert.Equal(s.T(), 400, s.doJSON("POST", "/api/v1/jobs/batch-ops",
		BatchJobOpsRequest{Action: jobOpMove, IDs: []string{"x"}}, &resp),
		"move 不带 group 与带空 group 是两回事，必须显式表态")

	oversized := make([]string, maxBatchOpsSize+1)
	for i := range oversized {
		oversized[i] = fmt.Sprintf("id-%d", i)
	}
	assert.Equal(s.T(), 400, s.doJSON("POST", "/api/v1/jobs/batch-ops",
		BatchJobOpsRequest{Action: jobOpPause, IDs: oversized}, &resp))

	// 组名规则与创建任务一致
	var created JobResponse
	require.Equal(s.T(), 201, s.doJSON("POST", "/api/v1/jobs",
		CreateJobRequest{Name: "payment_check", Delay: "2h"}, &created))
	assert.Equal(s.T(), 400, s.doJSON("POST", "/api/v1/jobs/batch-ops",
		BatchJobOpsRequest{Action: jobOpMove, IDs: []string{created.ID}, Group: ptrString("bad name")}, &resp))
	assert.Equal(s.T(), "invalid group name", resp.Message)
}

// force-pause 是唯一由 admin 档把关的生命周期动作；batch-ops 的档位要看请求体，
// 所以这里既测单条端点也测批量端点。
func TestLifecycleEndpointsGateRoles(t *testing.T) {
	srv := newSecurityServer(t, accountsSecurity(t))
	registerNopHandler(srv)

	// 直接经调度器放一个待执行任务：权限测试关心的是状态码，不是创建链路
	require.NoError(t, srv.scheduler.Schedule(&core.Job{
		ID: "lc-gate", Name: "payment_check", TriggerAt: time.Now().Add(2 * time.Hour),
	}))
	jobID := "lc-gate"

	operator := login(t, srv, testOperatorName, testPassword)
	viewer := login(t, srv, testViewerName, testPassword)
	forcePausePath := "/api/v1/jobs/" + jobID + "/force-pause"

	// viewer 连暂停都不能做
	assert.Equal(t, http.StatusForbidden,
		doJSON(t, srv, http.MethodPost, "/api/v1/jobs/"+jobID+"/pause", "", bearer(viewer.AccessToken)).Code)

	// operator 能暂停/恢复，但不能强制暂停
	assert.Equal(t, http.StatusOK,
		doJSON(t, srv, http.MethodPost, "/api/v1/jobs/"+jobID+"/pause", "", bearer(operator.AccessToken)).Code)
	assert.Equal(t, http.StatusOK,
		doJSON(t, srv, http.MethodPost, "/api/v1/jobs/"+jobID+"/resume", "", bearer(operator.AccessToken)).Code)
	assert.Equal(t, http.StatusForbidden,
		doJSON(t, srv, http.MethodPost, forcePausePath, "", bearer(operator.AccessToken)).Code)

	// 批量里混进 force-pause 时整批拒绝，而不是"批里几条越权动作偷偷执行"
	batchBody, _ := json.Marshal(BatchJobOpsRequest{Action: jobOpForcePause, IDs: []string{jobID}})
	assert.Equal(t, http.StatusForbidden,
		doJSON(t, srv, http.MethodPost, "/api/v1/jobs/batch-ops", string(batchBody), bearer(operator.AccessToken)).Code)
	assert.Equal(t, 1, srv.scheduler.HeapLen(), "被拒绝的批量操作不该留下半个改动")

	admin := login(t, srv, testAdminName, testPassword)
	assert.Equal(t, http.StatusOK,
		doJSON(t, srv, http.MethodPost, forcePausePath, "", bearer(admin.AccessToken)).Code)

	recorder := doJSON(t, srv, http.MethodPost, "/api/v1/jobs/batch-ops", string(batchBody), bearer(admin.AccessToken))
	require.Equal(t, 207, recorder.Code)
}

// 静态机器凭据的档位等同 operator：能暂停，不能强制暂停。
func TestMachineTokenCannotForcePause(t *testing.T) {
	sec := accountsSecurity(t)
	sec.Auth.Token = testToken
	srv := newSecurityServer(t, sec)
	registerNopHandler(srv)

	require.NoError(t, srv.scheduler.Schedule(&core.Job{
		ID: "lc-machine", Name: "payment_check", TriggerAt: time.Now().Add(2 * time.Hour),
	}))
	jobID := "lc-machine"

	assert.Equal(t, http.StatusOK,
		doJSON(t, srv, http.MethodPost, "/api/v1/jobs/"+jobID+"/pause", "", bearer(testToken)).Code)
	assert.Equal(t, http.StatusForbidden,
		doJSON(t, srv, http.MethodPost, "/api/v1/jobs/"+jobID+"/force-pause", "", bearer(testToken)).Code)
}
