// attempt.go 定义逐次尝试的账本词汇与能力接口（TASK-RE02）。
//
// Attempt 是执行账本的一行：一次尝试何时因何开始、如何结束。它与
// ExecMeta（最近一次执行的结论摘要，随任务快照落盘）分工明确——快照只回答
// "最后一次怎么样"，账本回答"每一次分别怎么样"。账本行比事件可靠：
// 事件有丢弃与保留上限，账本与任务快照同库、随终态留痕淘汰一起管理。
//
// 接口定义在 core（消费方是调度器与 api 的读端点），实现方 store/jobs
// 隐式实现；JSON 文件存储不实现——调度器持有可 nil 引用，nil 即降级
// （构造时记一条 info，说明逐次尝试不再落账本，审计依赖事件流）。
package core

import (
	"errors"
	"strings"
	"time"
)

// AttemptStatus 一次尝试的状态。封闭词表，与 job_attempts.status 列的取值一致。
type AttemptStatus int

const (
	// AttemptRunning 尝试已开始、尚未收尾。崩溃遗留的 running 行由 Restore 收口为 abandoned。
	AttemptRunning AttemptStatus = iota
	AttemptSuccess
	AttemptFailed
	// AttemptCancelled 尝试被外部中止后结束：用户取消、强制暂停、优雅关停打断。
	// 与 abandoned 的分界：cancelled 表示"我们主动停的、没有结论"；abandoned 表示
	// "进程没了、结局无从知晓"。
	AttemptCancelled
	// AttemptAbandoned 只由 Restore 写：崩溃前在跑的尝试，重启时确认它永远不会
	// 有结论了。重跑会开出新 seq，账本因此永远连续。
	AttemptAbandoned
)

// String 返回状态的规范名，与 job_attempts.status 列及 API 的取值一致。
func (s AttemptStatus) String() string {
	switch s {
	case AttemptRunning:
		return "running"
	case AttemptSuccess:
		return "success"
	case AttemptFailed:
		return "failed"
	case AttemptCancelled:
		return "cancelled"
	case AttemptAbandoned:
		return "abandoned"
	default:
		return "unknown"
	}
}

// ParseAttemptStatus 按规范名解析状态（大小写不敏感），未知名称返回 false。
func ParseAttemptStatus(name string) (AttemptStatus, bool) {
	for _, status := range []AttemptStatus{AttemptRunning, AttemptSuccess, AttemptFailed, AttemptCancelled, AttemptAbandoned} {
		if strings.EqualFold(status.String(), name) {
			return status, true
		}
	}
	return 0, false
}

// AttemptTrigger 一次尝试为什么发生。封闭词表，与 job_attempts.trigger_source 列一致。
type AttemptTrigger int

const (
	// AttemptTriggerScheduled 常规排期到期：新提交、cron 的下一轮、暂停后的恢复。
	// 零值即它：绝大多数尝试都是排期到期。
	AttemptTriggerScheduled AttemptTrigger = iota
	// AttemptTriggerRetry 失败后的重试（CloneForRetry 的新对象）。
	AttemptTriggerRetry
	// AttemptTriggerRestore 崩溃/重启后的补跑（Restore 重新入堆的快照）。
	AttemptTriggerRestore
	// AttemptTriggerMisfire 错过触发窗口后的补跑（TASK-RE06 引入时使用）。
	AttemptTriggerMisfire
)

// String 返回触发来源的规范名，与 job_attempts.trigger_source 列一致。
func (t AttemptTrigger) String() string {
	switch t {
	case AttemptTriggerScheduled:
		return "scheduled"
	case AttemptTriggerRetry:
		return "retry"
	case AttemptTriggerRestore:
		return "restore"
	case AttemptTriggerMisfire:
		return "misfire"
	default:
		return "unknown"
	}
}

// ParseAttemptTrigger 按规范名解析触发来源（大小写不敏感），未知名称返回 false。
func ParseAttemptTrigger(name string) (AttemptTrigger, bool) {
	for _, trigger := range []AttemptTrigger{AttemptTriggerScheduled, AttemptTriggerRetry, AttemptTriggerRestore, AttemptTriggerMisfire} {
		if strings.EqualFold(trigger.String(), name) {
			return trigger, true
		}
	}
	return 0, false
}

// Attempt 是账本的一行，字段与 job_attempts 表的列一一对应（DDL 见
// docs/design/reliable-execution-design.md §6.1）。
//
// 可空结论字段用成对的 Set 标记表达"未判定"：exit_code 0 与"没有退出码可言"
// 必须分得开（设计文档 §6.3），库列是 NULL，接口序列化为显式的 has_* 布尔。
type Attempt struct {
	JobID string
	// Seq 尝试序号：1 起递增，等于 Job.Attempts 自增后的值，(job_id, seq) 是主键。
	Seq    int
	Status AttemptStatus
	// Trigger 是这次尝试的触发来源（scheduled|retry|restore|misfire）。
	Trigger AttemptTrigger

	StartedAt  time.Time
	FinishedAt time.Time // 零值 = 在途（只有 running 行允许）
	DurationMs int64

	// 结论字段：失败时由执行器分类填充（TASK-RE04），成功时留空。
	Class         string
	Retryable     bool
	RetryableSet  bool
	ExitCode      int
	ExitCodeSet   bool
	Signal        string
	HTTPStatus    int
	HTTPStatusSet bool
	// Error 是脱敏后的错误文本（脱敏口径在执行器侧，E08 规则）。
	Error string
}

// attemptClassOf 定一次失败的类别文本（TASK-RE04 §6.4）。顺序即口径：
// 超时优先（普通 handler 的 DeadlineExceeded 未必实现 AttemptOutcome，
// 而执行器的 timeout 类经 errors.Is 也会落到这里），
// 其次执行器侧的接口实现，最后是普通 handler 的兜底 handler_error。
func attemptClassOf(err error, timedOut bool) string {
	if timedOut {
		return "timeout"
	}
	var outcome AttemptOutcome
	if errors.As(err, &outcome) {
		return outcome.AttemptClass()
	}
	return "handler_error"
}

// AttemptOutcome 是一次失败的分类明细（TASK-RE04）。
//
// core 不认识执行器的 ExitError——只按接口读，依赖方向保持 executor → core，
// 与 PermanentError（"要不要重试"）同一手法。本接口管"失败长什么样"：
// 类别文本、可重试性、退出码/信号/状态码与脱敏后的错误文本，落账本行与
// job.failed 事件的 metadata。普通处理函数的失败没有这个实现——
// 记 handler_error（或 timeout），只有错误文本可填。
type AttemptOutcome interface {
	// AttemptClass 返回失败类别文本（设计文档 §6.4 词表：invalid_submission/
	// profile_unavailable/not_allowed/timeout/permit_wait/exit_code/http_status/
	// address_refused/transport/interrupted）。
	AttemptClass() string
	// AttemptRetryable 返回"这次失败允许重试吗"；ok=false 表示未判定（账本列 NULL）。
	AttemptRetryable() (retryable bool, ok bool)
	// AttemptExitCode 返回进程退出码；ok=false 表示没有退出码可言
	//（非 exit_code 类别一律 false——HTTP 状态码冒充退出码是假信息）。
	AttemptExitCode() (code int, ok bool)
	// AttemptSignal 返回终止信号名，正常退出为空。
	AttemptSignal() string
	// AttemptHTTPStatus 返回响应状态码；ok=false 表示非 http 类别。
	AttemptHTTPStatus() (status int, ok bool)
	// AttemptErrorText 返回脱敏后的错误文本（秘密止步于子进程边界，E08 口径）。
	AttemptErrorText() string
}

// DefaultAttemptsKeepLast 是每任务账本保留条数的默认值（store.attempts_keep_last
// 的 0/缺省取值）。20 条足够覆盖"一次排期 + 数次重试"再留几轮历史；cron 任务
// 靠收尾裁剪滚动保新。
const DefaultAttemptsKeepLast = 20

// WithAttemptLedger 给调度器注入账本能力与每任务保留条数（TASK-RE02）。
// 实现方是任务库（store/jobs 隐式实现 AttemptLedger）；JSON 部署不注入，
// 调度器按 nil 降级。keepLast <= 0 用 DefaultAttemptsKeepLast。
func WithAttemptLedger(l AttemptLedger, keepLast int) Option {
	return func(o *componentOptions) {
		o.ledger = l
		if keepLast > 0 {
			o.attemptsKeepLast = keepLast
		} else {
			o.attemptsKeepLast = DefaultAttemptsKeepLast
		}
	}
}

// AttemptLedger 是逐次尝试账本的能力接口（TASK-RE02）。
//
// begin/finish 只由持有该任务的 worker 协程调用（与 Job.Exec 的写侧纪律一致）；
// AbandonAttempts 只由 Restore 在启动期调用（彼时没有并发执行）。
// 实现方应保证同 (job_id, seq) 的重复写入幂等（upsert），崩溃重放不会产生重复行。
type AttemptLedger interface {
	// BeginAttempt 记一次尝试的开始：status=running、trigger/started_at 以 rec 为准。
	BeginAttempt(jobID string, rec Attempt) error
	// FinishAttempt 收口一次尝试：status/结论字段以 rec 为准（FinishedAt/DurationMs
	// 由调度器填好再传入）。收口一个不存在的 seq 不算错误（upsert 语义）。
	FinishAttempt(jobID string, seq int, rec Attempt) error
	// AbandonAttempts 把 jobID 上 seq <= uptoSeq、仍处于 running 的尝试收口为
	// abandoned（finished_at = at，时长按 started_at 补算）。崩溃恢复的收口动作：
	// 无论改判成 replay 还是 pause，"那次尝试没跑完"都是事实。
	AbandonAttempts(jobID string, uptoSeq int, at time.Time) error
	// Attempts 按 seq 降序读某任务最近 limit 条（limit <= 0 由实现方兜底）。
	Attempts(jobID string, limit int) ([]Attempt, error)
	// PruneAttempts 把单个任务的账本裁到最近 keepLast 条（按 seq 保留最新的）。
	// 调度器在每次收尾后调用：主键 (job_id, seq) 前缀索引使代价为常数；
	// 全表清理不需要——停跑的任务天然只剩最近几次的行。
	PruneAttempts(jobID string, keepLast int) error
}
