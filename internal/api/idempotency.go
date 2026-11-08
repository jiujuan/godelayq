package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jiujuan/godelayq/internal/core"
)

// idempotency.go 是幂等提交的 api 侧实现（TASK-RE03，设计文档 §7.3）：
// 键（Idempotency-Key 头，缺省时客户端 job_id 兼作）命中即比对指纹——
// 同指纹是重放（返回既有任务），异指纹是冲突（409）。客户端 job_id 另有一条
// 独立于登记表的撞 ID 判定：按 SnapshotReader 直查既有快照比对指纹，
// 重启与"登记表没有的旧任务"都拦得住。

// idempotencyStore 是幂等键登记表的窄接口，定义在消费方（api）；由
// store/jobs.Store 隐式实现（库模式，持久）。JSON 部署注入进程内 map 实现
// （WithMemoryIdempotencyStore）——重启即失效，能力边界写进 deployment 文档。
type idempotencyStore interface {
	LookupIdempotency(key string) (jobID string, fingerprint string, found bool, err error)
	PutIdempotency(key string, jobID string, fingerprint string) error
	PruneExpiredIdempotency(ttl time.Duration) error
}

// WithIdempotencyStore 注入登记表与键保留时长。ttl <= 0 用 core.DefaultIdempotencyTTL。
func WithIdempotencyStore(r idempotencyStore, ttl time.Duration) Option {
	return func(s *Server) {
		s.idempotency = r
		s.idempotencyTTL = normalizeIdempotencyTTL(ttl)
	}
}

// WithMemoryIdempotencyStore 给 JSON 部署注入进程内登记表：幂等只在进程
// 生命周期内成立，重启后同一键会创建新任务（能力边界，接口文档与 deployment.md 说明）。
func WithMemoryIdempotencyStore(ttl time.Duration) Option {
	return WithIdempotencyStore(newMemoryIdempotencyStore(ttl), ttl)
}

func normalizeIdempotencyTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return core.DefaultIdempotencyTTL
	}
	return ttl
}

// memoryIdempotencyEntry 是进程内登记表的一行。
type memoryIdempotencyEntry struct {
	jobID       string
	fingerprint string
	createdAt   time.Time
}

// memoryIdempotencyStore 是 idempotencyStore 的进程内实现：惰性淘汰
// （Lookup 时顺手清过期行），PruneExpired 为 no-op——没有后台协程可驱动。
type memoryIdempotencyStore struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]memoryIdempotencyEntry
}

func newMemoryIdempotencyStore(ttl time.Duration) *memoryIdempotencyStore {
	return &memoryIdempotencyStore{
		ttl:     normalizeIdempotencyTTL(ttl),
		entries: make(map[string]memoryIdempotencyEntry),
	}
}

func (m *memoryIdempotencyStore) LookupIdempotency(key string) (string, string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[key]
	if !ok {
		return "", "", false, nil
	}
	if time.Since(entry.createdAt) > m.ttl {
		delete(m.entries, key)
		return "", "", false, nil
	}
	return entry.jobID, entry.fingerprint, true, nil
}

func (m *memoryIdempotencyStore) PutIdempotency(key string, jobID string, fingerprint string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.entries[key]; exists {
		// 先到先得，与任务库实现的 ON CONFLICT DO NOTHING 同一语义
		return nil
	}
	m.entries[key] = memoryIdempotencyEntry{jobID: jobID, fingerprint: fingerprint, createdAt: time.Now()}
	return nil
}

func (m *memoryIdempotencyStore) PruneExpiredIdempotency(time.Duration) error { return nil }

// idempotencyFingerprint 是指纹口径的唯一权威：结构体字段集决定"哪些变化算
// 不同内容"。增删 CreateJobRequest 字段时必须同步这里（编译器不拦，注释声明）。
//
// TimeSpec 用提交侧的时间声明原文（cron 表达式 / trigger_at 时刻 / delay 字符串），
// 不是解析后的绝对时刻：delay:10m 隔三秒重发的解析结果差 3 秒，指纹必须不变。
// 代价是同一逻辑内容的 delay:10m 与等效 trigger_at 写法判为不同内容——
// 按"字面不同的请求"对待，可预期性优先于语义等价（设计文档 §7.3 的修订口径）。
type idempotencyFingerprint struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// JobID 不进指纹：它是幂等键本身，不是内容（同键必同 ID，进指纹是循环）。
	Payload    json.RawMessage `json:"payload,omitempty"`
	CronExpr   string          `json:"cron_expr,omitempty"`
	IsRepeat   bool            `json:"is_repeat,omitempty"`
	TimeSpec   string          `json:"time_spec"`
	Group      string          `json:"group,omitempty"`
	Timeout    time.Duration   `json:"timeout"`
	MaxRetries int             `json:"max_retries"`
	RetryDelay time.Duration   `json:"retry_delay"`
}

// idempotencyTimeSpec 提取时间声明原文，优先级与 calculateTriggerTime 一致
// （cron > trigger_at > delay > 立即）。四档各自稳定：同请求重发同声明。
func idempotencyTimeSpec(req CreateJobRequest) string {
	switch {
	case req.CronExpr != "":
		return "cron:" + req.CronExpr
	case req.TriggerAt != nil:
		return "at:" + req.TriggerAt.UTC().Format(time.RFC3339Nano)
	case req.Delay != "":
		return "delay:" + req.Delay
	default:
		return "immediate"
	}
}

// fingerprintOfRequest 算一次创建请求的指纹（SHA256 hex）。
func fingerprintOfRequest(req CreateJobRequest, timeout time.Duration, retryDelay time.Duration) string {
	return fingerprintOf(idempotencyFingerprint{
		Name:       req.Name,
		Type:       req.Type,
		Payload:    req.Payload,
		CronExpr:   req.CronExpr,
		IsRepeat:   req.IsRepeat,
		TimeSpec:   idempotencyTimeSpec(req),
		Group:      req.Group,
		Timeout:    timeout,
		MaxRetries: req.MaxRetries,
		RetryDelay: retryDelay,
	})
}

func fingerprintOf(v idempotencyFingerprint) string {
	// 结构体字段序确定，json.Marshal 输出稳定；错误不可能发生（全是可编码标量），
	// 与既有事件序列化点同一忽略口径。
	data, _ := json.Marshal(v)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// validateIdempotencyKey 校验 Idempotency-Key 请求头：trim 后 1..128 字符。
// 空串返回 nil——没带头是合法路径（键缺省时用 job_id 兼作，都没有就没有幂等）。
func validateIdempotencyKey(raw string) (string, *ErrorResponse) {
	key := strings.TrimSpace(raw)
	if key == "" {
		return "", nil
	}
	if len(key) > 128 {
		return "", &ErrorResponse{
			Code:    400,
			Message: "invalid idempotency key",
			Details: "Idempotency-Key must be at most 128 characters",
		}
	}
	return key, nil
}

// validateJobIDField 校验客户端指定的任务 ID：[A-Za-z0-9_-]{1,64}。
// 比服务端生成的 UUIDv7 更宽（人可读的幂等键是合法用法），但不放行空白与
// URL 特殊字符——它会出现在 /jobs/:id 路径里。
func validateJobIDField(raw string) (string, *ErrorResponse) {
	id := strings.TrimSpace(raw)
	if id == "" {
		return "", nil
	}
	if len(id) > 64 {
		return "", &ErrorResponse{Code: 400, Message: "invalid job id", Details: "job_id must be at most 64 characters"}
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return "", &ErrorResponse{
				Code:    400,
				Message: "invalid job id",
				Details: fmt.Sprintf("job_id %q must match [A-Za-z0-9_-]{1,64}", id),
			}
		}
	}
	return id, nil
}
