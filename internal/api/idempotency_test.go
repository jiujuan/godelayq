package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/stretchr/testify/require"
)

// idempotency_test.go 是幂等提交的端到端测试（TASK-RE03）：套件的 JSON 存储
// 不实现登记表，用例里直接给 server 挂上进程内实现——降级形态本身就是被测路径之一。

// doJSONWithHeader 与 doJSON 同构，多带一个请求头并返回响应头
// （Idempotency-Replayed 的断言需要它）。
func (s *APITestSuite) doJSONWithHeader(method, path string, body any, headerKey, headerValue string, out any) (int, http.Header) {
	s.T().Helper()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(s.T(), err)
		reader = bytes.NewBuffer(raw)
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if headerKey != "" {
		req.Header.Set(headerKey, headerValue)
	}
	s.router.ServeHTTP(w, req)

	if out != nil && w.Body.Len() > 0 {
		require.NoError(s.T(), json.Unmarshal(w.Body.Bytes(), out), w.Body.String())
	}
	return w.Code, w.Header()
}

// withIdempotency 给套件的 server 挂上进程内登记表（TTL 1 小时，用例内不跨界）。
func (s *APITestSuite) withIdempotency() {
	s.server.idempotency = newMemoryIdempotencyStore(time.Hour)
}

// idempotencyBody 是一条可复用的创建请求（payload 可变，便于构造冲突）。
func idempotencyBody(payload string) CreateJobRequest {
	return CreateJobRequest{
		Name:    "payment_check",
		Delay:   "10m",
		Payload: json.RawMessage(payload),
	}
}

// TestIdempotency_ReplaySameKeySameContent：同键同内容重发 → 201 后 200，
// 同一任务、不产生第二条；响应头可区分新建与重放。
func (s *APITestSuite) TestIdempotency_ReplaySameKeySameContent() {
	s.registerPaymentHandler()
	s.withIdempotency()

	var first, second JobResponse
	code, _ := s.doJSONWithHeader("POST", "/api/v1/jobs", idempotencyBody(`{"a":1}`), "Idempotency-Key", "order-42", &first)
	require.Equal(s.T(), 201, code)

	code, header := s.doJSONWithHeader("POST", "/api/v1/jobs", idempotencyBody(`{"a":1}`), "Idempotency-Key", "order-42", &second)
	require.Equal(s.T(), 200, code)
	require.Equal(s.T(), first.ID, second.ID, "重放必须返回既有任务")
	require.Equal(s.T(), "true", header.Get("Idempotency-Replayed"))

	require.Equal(s.T(), 1, s.scheduler.HeapLen(), "重放不得创建第二个任务")
}

// TestIdempotency_ConflictSameKeyDifferentContent：同键异内容 → 409。
func (s *APITestSuite) TestIdempotency_ConflictSameKeyDifferentContent() {
	s.registerPaymentHandler()
	s.withIdempotency()

	var first JobResponse
	_, _ = s.doJSONWithHeader("POST", "/api/v1/jobs", idempotencyBody(`{"a":1}`), "Idempotency-Key", "order-42", &first)

	var resp ErrorResponse
	code, _ := s.doJSONWithHeader("POST", "/api/v1/jobs", idempotencyBody(`{"a":2}`), "Idempotency-Key", "order-42", &resp)
	require.Equal(s.T(), 409, code)
	require.Contains(s.T(), resp.Details, "order-42")
}

// TestIdempotency_DelayResubmitIsReplay：delay 类请求隔一段时间重发，解析后的
// 触发时刻不同，但指纹按时间声明原文计算——必须判重放而不是冲突。
func (s *APITestSuite) TestIdempotency_DelayResubmitIsReplay() {
	s.registerPaymentHandler()
	s.withIdempotency()

	var first JobResponse
	_, _ = s.doJSONWithHeader("POST", "/api/v1/jobs", idempotencyBody(`{"a":1}`), "Idempotency-Key", "order-43", &first)

	time.Sleep(10 * time.Millisecond)
	var second JobResponse
	code, _ := s.doJSONWithHeader("POST", "/api/v1/jobs", idempotencyBody(`{"a":1}`), "Idempotency-Key", "order-43", &second)
	require.Equal(s.T(), 200, code)
	require.Equal(s.T(), first.ID, second.ID)
}

// TestIdempotency_ClientJobID：job_id 兼作幂等键——同 ID 同内容重放 200，
// 异内容 409；指纹存在快照上，与登记表无关。
func (s *APITestSuite) TestIdempotency_ClientJobID() {
	s.registerPaymentHandler()
	s.withIdempotency()

	body := idempotencyBody(`{"a":1}`)
	body.JobID = "nightly-pay-1"

	var first JobResponse
	require.Equal(s.T(), 201, s.doJSON("POST", "/api/v1/jobs", body, &first))
	require.Equal(s.T(), "nightly-pay-1", first.ID)

	// 同 ID 同内容：无 Idempotency-Key 也重放（快照指纹比对）
	var second JobResponse
	require.Equal(s.T(), 200, s.doJSON("POST", "/api/v1/jobs", body, &second))
	require.Equal(s.T(), "nightly-pay-1", second.ID)

	// 同 ID 异内容（max_retries 变了）：409
	conflict := body
	conflict.MaxRetries = 5
	var resp ErrorResponse
	require.Equal(s.T(), 409, s.doJSON("POST", "/api/v1/jobs", conflict, &resp))

	// 同 ID 异 payload：409
	conflict2 := idempotencyBody(`{"a":999}`)
	conflict2.JobID = "nightly-pay-1"
	require.Equal(s.T(), 409, s.doJSON("POST", "/api/v1/jobs", conflict2, &resp))
}

// TestIdempotency_NoKeyOldBehavior：不带头、不带 job_id 的提交完全走旧行为（201）。
func (s *APITestSuite) TestIdempotency_NoKeyOldBehavior() {
	s.registerPaymentHandler()
	s.withIdempotency()

	var resp JobResponse
	require.Equal(s.T(), 201, s.doJSON("POST", "/api/v1/jobs", idempotencyBody(`{"a":1}`), &resp))
}

// TestIdempotency_BatchPerItem：批量逐条独立——同键同内容的两条都成功且同 ID。
func (s *APITestSuite) TestIdempotency_BatchPerItem() {
	s.registerPaymentHandler()
	s.withIdempotency()

	reqs := []CreateJobRequest{idempotencyBody(`{"a":1}`), idempotencyBody(`{"a":1}`)}
	var resp BatchCreateJobsResponse
	code, _ := s.doJSONWithHeader("POST", "/api/v1/jobs/batch", reqs, "Idempotency-Key", "order-44", &resp)
	require.Equal(s.T(), 207, code)
	require.Equal(s.T(), 2, resp.Succeeded)
	require.Len(s.T(), resp.Items, 2)
	require.Equal(s.T(), resp.Items[0].ID, resp.Items[1].ID, "同键重放返回同一任务")
}

// TestIdempotency_InvalidKeyAndJobID：头部与 job_id 的格式校验（400）。
func (s *APITestSuite) TestIdempotency_InvalidKeyAndJobID() {
	s.registerPaymentHandler()
	s.withIdempotency()

	longKey := make([]byte, 129)
	for i := range longKey {
		longKey[i] = 'k'
	}
	var resp ErrorResponse
	code, _ := s.doJSONWithHeader("POST", "/api/v1/jobs", idempotencyBody(`{}`), "Idempotency-Key", string(longKey), &resp)
	require.Equal(s.T(), 400, code)

	bad := idempotencyBody(`{}`)
	bad.JobID = "bad id with spaces"
	require.Equal(s.T(), 400, s.doJSON("POST", "/api/v1/jobs", bad, &resp))
}

// TestIdempotency_ServerGeneratedIDRegistered：job_id 缺省时重放走登记表，
// 返回的是服务端生成的 ID（UUIDv7 形态）。
func (s *APITestSuite) TestIdempotency_ServerGeneratedIDRegistered() {
	s.registerPaymentHandler()
	s.withIdempotency()

	var first, second JobResponse
	_, _ = s.doJSONWithHeader("POST", "/api/v1/jobs", idempotencyBody(`{"a":1}`), "Idempotency-Key", "order-45", &first)
	code, _ := s.doJSONWithHeader("POST", "/api/v1/jobs", idempotencyBody(`{"a":1}`), "Idempotency-Key", "order-45", &second)
	require.Equal(s.T(), 200, code)
	require.Equal(s.T(), first.ID, second.ID)
	require.Len(s.T(), first.ID, 36, "未指定 job_id 时必须由服务端生成 UUID")
}
