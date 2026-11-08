package api

import (
	"errors"
	"sync"
	"time"

	"github.com/stretchr/testify/require"
)

// idempotency_fault_test.go 是 TASK-RE08 的幂等故障注入：登记表故障时提交降级
// 放行（RE03 声明的口径）、键过期后允许重建。

// failingIdempotencyStore 是永远失败的登记表替身。
type failingIdempotencyStore struct{}

func (failingIdempotencyStore) LookupIdempotency(string) (string, string, bool, error) {
	return "", "", false, errors.New("injected registry failure")
}
func (failingIdempotencyStore) PutIdempotency(string, string, string) error {
	return errors.New("injected registry failure")
}
func (failingIdempotencyStore) PruneExpiredIdempotency(time.Duration) error {
	return errors.New("injected registry failure")
}

// TestIdempotency_RegistryFailureDegrades：登记表全故障——提交照常 201（幂等是
// 保护不是门闩），重发拿到的还是 201（重复建任务，降级口径的预期行为），不 5xx。
func (s *APITestSuite) TestIdempotency_RegistryFailureDegrades() {
	s.registerPaymentHandler()
	s.server.idempotency = failingIdempotencyStore{}
	s.server.idempotencyTTL = time.Hour

	var first, second JobResponse
	code, _ := s.doJSONWithHeader("POST", "/api/v1/jobs", idempotencyBody(`{"a":1}`), "Idempotency-Key", "fault-key", &first)
	require.Equal(s.T(), 201, code)

	code, _ = s.doJSONWithHeader("POST", "/api/v1/jobs", idempotencyBody(`{"a":1}`), "Idempotency-Key", "fault-key", &second)
	require.Equal(s.T(), 201, code, "登记表故障时重发放行为新建（降级）")
	require.NotEqual(s.T(), first.ID, second.ID, "降级窗口里重发会重复建任务——RE03 注释声明的口径")
}

// TestIdempotency_ExpiredKeyRecreates：键过 TTL 后重发允许新建（201），淘汰发生。
func (s *APITestSuite) TestIdempotency_ExpiredKeyRecreates() {
	s.registerPaymentHandler()
	// TTL 10ms 的进程内登记表：Lookup 惰性淘汰
	s.server.idempotency = newMemoryIdempotencyStore(10 * time.Millisecond)
	s.server.idempotencyTTL = 10 * time.Millisecond

	var first JobResponse
	code, _ := s.doJSONWithHeader("POST", "/api/v1/jobs", idempotencyBody(`{"a":1}`), "Idempotency-Key", "ttl-key", &first)
	require.Equal(s.T(), 201, code)

	time.Sleep(30 * time.Millisecond)
	var second JobResponse
	code, header := s.doJSONWithHeader("POST", "/api/v1/jobs", idempotencyBody(`{"a":1}`), "Idempotency-Key", "ttl-key", &second)
	require.Equal(s.T(), 201, code, "过期键重发允许新建")
	require.Empty(s.T(), header.Get("Idempotency-Replayed"))
	require.NotEqual(s.T(), first.ID, second.ID)
}

// countingIdempotencyStore 统计 PruneExpired 调用次数（淘汰随成功创建驱动）。
type countingIdempotencyStore struct {
	mu    sync.Mutex
	calls int
}

func (c *countingIdempotencyStore) LookupIdempotency(string) (string, string, bool, error) {
	return "", "", false, nil
}
func (c *countingIdempotencyStore) PutIdempotency(string, string, string) error { return nil }
func (c *countingIdempotencyStore) PruneExpiredIdempotency(time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return nil
}

// TestIdempotency_PruneDrivenByCreate：淘汰随每次成功创建驱动（无后台协程的口径）。
func (s *APITestSuite) TestIdempotency_PruneDrivenByCreate() {
	s.registerPaymentHandler()
	counter := &countingIdempotencyStore{}
	s.server.idempotency = counter
	s.server.idempotencyTTL = time.Hour

	code, _ := s.doJSONWithHeader("POST", "/api/v1/jobs", idempotencyBody(`{"a":1}`), "Idempotency-Key", "prune-key", &JobResponse{})
	require.Equal(s.T(), 201, code)
	counter.mu.Lock()
	calls := counter.calls
	counter.mu.Unlock()
	require.Equal(s.T(), 1, calls, "成功创建后应驱动一次淘汰")
}
