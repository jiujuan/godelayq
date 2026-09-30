package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"godelayq/core"
)

// TestJobResponse_ExposesAttempts 是 TASK-E18 §3.2 第 3 条的数据前提：
// "第几次尝试"下拉的范围来自任务对象，而这个范围必须与结果端点认的可取值同一条，
// 否则下拉里就会出现点了必然报 400 的那一项。
func TestJobResponse_ExposesAttempts(t *testing.T) {
	srv := newSecurityServer(t, Security{})

	require.NoError(t, srv.store.Update(core.JobSnapshot{
		ID:       "attempts-third",
		Name:     "payment_check",
		Status:   int(core.StatusSuccess),
		Attempts: 3,
		Exec:     &core.ExecMeta{Kind: "script", Profile: "payment_check", DurationMs: 12},
	}))
	require.NoError(t, srv.store.Update(core.JobSnapshot{
		ID:        "attempts-none",
		Name:      "payment_check",
		Status:    int(core.StatusPending),
		TriggerAt: time.Now().Add(time.Hour),
	}))

	read := func(id string) JobResponse {
		t.Helper()

		recorder := doGet(t, srv, "/api/v1/jobs/"+id, nil)
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

		var job JobResponse
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &job), recorder.Body.String())
		return job
	}

	assert.Equal(t, 3, read("attempts-third").Attempts, "已经跑过三次就要读出三次")
	assert.Equal(t, 0, read("attempts-none").Attempts, "还没启动过任何一次执行就是 0")

	// 这个键要恒在：下拉按它决定有没有第二项可换，缺键与 0 对前端是两件事
	assert.Contains(t, string(mustBody(t, srv, "/api/v1/jobs/attempts-none")), `"attempts":0`)
}

func mustBody(t *testing.T, srv *Server, path string) []byte {
	t.Helper()

	recorder := doGet(t, srv, path, nil)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	return recorder.Body.Bytes()
}
