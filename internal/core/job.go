package core

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"
)

// ExecPrefix 是执行器档位在调度器注册表里的键前缀：配置里 name: hello 的档位，
// 注册键是 exec.hello，任务里提交的类型名也是它。
//
// 定义在 core 而不是 executor：目录加载器（core/load.go）要按这个前缀拒绝任务文件（TASK-E17），
// 而 executor 依赖 core，常量放在 executor 会让 core 反向依赖执行器包。
const ExecPrefix = "exec."

// IsExecHandlerKey 判断一个任务类型名是不是执行器档位的注册键。
//
// 名字前后空白按去掉之后判断（与 formatToJob 对 name 的处理一致）：带空白的写法在注册表里
// 查不到东西，但按"是不是执行器任务"判断时不该因此被当成普通任务放过去。
// 只有前缀没有名字（就是 "exec." 本身）也算执行器键：这种名字说明配置写错了，
// 而"看起来像执行器键"的东西一律按执行器键处理更安全。
func IsExecHandlerKey(name string) bool {
	return strings.HasPrefix(strings.TrimSpace(name), ExecPrefix)
}

// JobStatus 任务状态
type JobStatus int

const (
	StatusPending JobStatus = iota
	StatusRunning
	StatusSuccess
	StatusFailed
	StatusCancelled
	// StatusPaused 只能追加在末尾：JobSnapshot.Status 以 int 落盘在既有的 jobs.json 里，
	// 插在中间会让历史快照的状态集体错位。
	StatusPaused
)

// String 返回状态的规范名，与 HTTP API 的 status 取值一致。
func (s JobStatus) String() string {
	switch s {
	case StatusPending:
		return "pending"
	case StatusRunning:
		return "running"
	case StatusSuccess:
		return "success"
	case StatusFailed:
		return "failed"
	case StatusCancelled:
		return "cancelled"
	case StatusPaused:
		return "paused"
	default:
		return "unknown"
	}
}

// ParseJobStatus 按规范名解析状态（大小写不敏感），未知名称返回 false。
func ParseJobStatus(name string) (JobStatus, bool) {
	for _, status := range []JobStatus{StatusPending, StatusRunning, StatusSuccess, StatusFailed, StatusCancelled, StatusPaused} {
		if strings.EqualFold(status.String(), name) {
			return status, true
		}
	}
	return 0, false
}

// IsTerminal 表示任务已结束：既不会被调度，也不应被崩溃恢复重新入队。
//
// paused 刻意不算终态：它还要被 Resume 唤醒，也不该被存储的终态留痕淘汰策略清掉。
func (s JobStatus) IsTerminal() bool {
	return s == StatusSuccess || s == StatusFailed || s == StatusCancelled
}

// ExecMeta.Artifact 的取值。空串表示"这次执行谈不上产物"（还没执行过，或执行器没落盘）。
const (
	// ArtifactAvailable 表示产物文件在磁盘上，正文可通过 GET /jobs/:id/result 读到。
	ArtifactAvailable = "available"
	// ArtifactPurged 表示摘要还在、文件已被清理（按 TTL 过期或判为孤儿），结果只剩结论。
	ArtifactPurged = "purged"
)

// ExecMeta 是一次执行的结论摘要，随任务快照落盘。
//
// 完整输出不在这里：JSON 存储每次合并落盘都是整文件重写，终态留痕默认上千条，
// 每条再带几十 KB 输出会把 jobs.json 推到百 MB 级。输出正文走产物文件，
// 依据见 docs/design/executor-design.md §6.4。
type ExecMeta struct {
	// Kind 是档位类型：script | binary | http
	Kind string `json:"kind"`
	// Profile 是档位名，不含 exec. 前缀
	Profile string `json:"profile"`
	// ExitCode 是进程退出码；http 档位与"没跑起来"的执行为 0
	ExitCode int `json:"exit_code,omitempty"`
	// Signal 是终止进程的信号名（如 SIGTERM），正常退出为空
	Signal string `json:"signal,omitempty"`
	// HTTPStatus 是 http 档位拿到的状态码，其它档位为 0
	HTTPStatus int `json:"http_status,omitempty"`
	// DurationMs 是单次执行耗时
	DurationMs int64 `json:"duration_ms"`
	// OutBytes / ErrBytes 是实际采集到的字节数（被 max_bytes 裁过的就是裁剪后的值，
	// 与产物文件里的内容长度一致；"还有更多没采到"由 Truncated 表达）
	OutBytes int64 `json:"out_bytes"`
	ErrBytes int64 `json:"err_bytes"`
	// Truncated 表示任一流的输出超过上限、后面的内容没有被采集
	Truncated bool `json:"truncated,omitempty"`
	// Permanent 表示这次失败不该重试（参数或脚本本身的问题），TASK-E12 使用
	Permanent bool `json:"permanent,omitempty"`
	// Preview 是输出尾部预览，长度上限由 executors.output.inline_preview 控制
	Preview string `json:"preview,omitempty"`
	// Artifact 是产物文件的状态：available | purged | 空（还没有产物可言）
	Artifact string `json:"artifact,omitempty"`
}

// TrimExecPreview 返回 text 尾部最多 limit 字节，起点落在字符边界上；limit <= 0 返回空串。
//
// 从尾部直接数 limit 字节会把一个多字节字符切成一半，接口与事件里就会多出替换字符，
// 所以切完再跳过开头的 continuation 字节，预览只会比 limit 短，不会更长。
// 执行侧、事件侧与接口侧共用这一份实现：各写一遍裁剪规则迟早算出不同长度的预览。
func TrimExecPreview(text string, limit int) string {
	if limit <= 0 {
		return ""
	}

	data := []byte(text)
	if len(data) <= limit {
		return text
	}

	tail := data[len(data)-limit:]
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	return string(tail)
}

// Job 延迟任务结构
type Job struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Type      string    `json:"type,omitempty"` // Handler 注册键；为空时回退到 Name
	Payload   []byte    `json:"payload"`        // 任务数据
	TriggerAt time.Time `json:"trigger_at"`     // 下次触发时间

	// Group 是分组名，空串表示未分组。它只是标签：不要求组已在 GroupStore 注册，
	// 删除分组也不会删除任务（见 api 层的 detach 策略）。
	Group string `json:"group,omitempty"`

	// 执行配置
	Handler Handler         `json:"-"` // 处理函数（不持久化）
	Ctx     context.Context `json:"-"` // 上下文（不持久化）

	// Exec 是最近一次执行的结果摘要；nil 表示"这不是执行器任务，或还没执行完"。
	// 不用空结构体表示没有结果，否则接口分不出这两种情况。
	// 只有持有本任务的那个执行协程可以写它（读侧走快照，见 ToSnapshot）；
	// 持久化用 JobSnapshot.Exec，所以这里不进 Job 的 JSON。
	Exec *ExecMeta `json:"-"`

	// Timeout 单次执行的超时时间，0 表示不限制。
	// Handler 必须检查 ctx，否则超时只能被观测、无法中断其执行。
	Timeout time.Duration `json:"timeout"`

	// class 是执行类别，由调度器在入堆之前按注册表盖章（Scheduler.RegisterHandlerClass）。
	// 不持久化也不进 JSON：恢复时重新查注册表，查不到就落回共享池，
	// 最坏结果是"这条任务没有专用名额"，不是"任务消失"。
	class JobClass

	// Cron 重复任务支持
	CronExpr string `json:"cron_expr,omitempty"` // Cron表达式，空表示一次性任务
	IsRepeat bool   `json:"is_repeat"`

	// 重试配置
	MaxRetries int           `json:"max_retries"`
	RetryCount int           `json:"retry_count"`
	RetryDelay time.Duration `json:"retry_delay"` // 基础退避时间

	// 状态
	Status    JobStatus `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// 元数据
	Attempts int `json:"attempts"` // 总尝试次数（含重试）
}

// Handler 任务处理函数
type Handler func(ctx context.Context, job *Job) error

// GetTriggerTime 实现 Item 接口
func (j *Job) GetTriggerTime() time.Time {
	return j.TriggerAt
}

// GetID 实现 Item 接口
func (j *Job) GetID() string {
	return j.ID
}

// HandlerKey 返回查找注册 Handler 的键：优先 Type，为空时回退 Name
func (j *Job) HandlerKey() string {
	if j.Type != "" {
		return j.Type
	}
	return j.Name
}

// CloneForRetry 创建重试副本。
// 保留原 ID 以维持任务链（查询/取消/存储清理按同一 ID）；
// 清除 Cron 属性，避免一次性任务失败重试后被误当作周期任务重新排期。
// 不带上一次的 Exec：副本代表一次新的执行，留着旧结论会让 GET /jobs/:id
// 在新一轮还没跑完时就显示上一次的退出码与输出预览。
func (j *Job) CloneForRetry(nextTime time.Time) *Job {
	return &Job{
		ID:         j.ID,
		Name:       j.Name,
		Type:       j.Type,
		Payload:    j.Payload,
		TriggerAt:  nextTime,
		Group:      j.Group,
		Handler:    j.Handler,
		Timeout:    j.Timeout,
		CronExpr:   "",
		IsRepeat:   false,
		MaxRetries: j.MaxRetries,
		RetryCount: j.RetryCount + 1,
		RetryDelay: j.RetryDelay * 2, // 指数退避
		Status:     StatusPending,
		CreatedAt:  j.CreatedAt,
		UpdatedAt:  time.Now(),
	}
}

// JobSnapshot 用于持久化的任务快照（不含函数指针）
type JobSnapshot struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Type       string    `json:"type,omitempty"`
	Payload    []byte    `json:"payload"`
	TriggerAt  time.Time `json:"trigger_at"`
	Group      string    `json:"group,omitempty"`
	CronExpr   string    `json:"cron_expr"`
	IsRepeat   bool      `json:"is_repeat"`
	Timeout    int64     `json:"timeout"` // nanoseconds
	MaxRetries int       `json:"max_retries"`
	RetryCount int       `json:"retry_count"`
	RetryDelay int64     `json:"retry_delay"` // nanoseconds
	Status     int       `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	Attempts   int       `json:"attempts"`

	// Exec 是执行结论摘要；老数据文件里没有这个键，解码后为 nil。
	// 用 omitempty：绝大多数任务不是执行器任务，不该在每条快照里都写出一个空 exec 键。
	Exec *ExecMeta `json:"exec,omitempty"`
}

// HandlerKey 返回这条快照对应的注册键：规则与 Job.HandlerKey 一致（Type 优先，回退 Name）。
//
// 钩子与恢复路径手上只有快照、没有 Job，规则却必须与入堆时那次查表完全相同，
// 否则同一个任务在"排期时算执行器任务、恢复时算普通任务"。
// 两份实现只差在接收者类型上，改一处要同时改另一处。
func (sn *JobSnapshot) HandlerKey() string {
	if sn.Type != "" {
		return sn.Type
	}
	return sn.Name
}

// ToSnapshot 转换为可持久化格式
func (j *Job) ToSnapshot() JobSnapshot {
	return JobSnapshot{
		ID:         j.ID,
		Name:       j.Name,
		Type:       j.Type,
		Payload:    j.Payload,
		TriggerAt:  j.TriggerAt,
		Group:      j.Group,
		CronExpr:   j.CronExpr,
		IsRepeat:   j.IsRepeat,
		Timeout:    int64(j.Timeout),
		MaxRetries: j.MaxRetries,
		RetryCount: j.RetryCount,
		RetryDelay: int64(j.RetryDelay),
		Status:     int(j.Status),
		CreatedAt:  j.CreatedAt,
		UpdatedAt:  j.UpdatedAt,
		Attempts:   j.Attempts,

		// Exec 复制的是指针，不复制对象：写出快照之后不要再改这个 ExecMeta，
		// 因为存储里的快照与任务对象指向同一份内容（事件发布读的也是它）。
		// 收尾顺序本来就是"先填摘要、再 Update(ToSnapshot())"，共享指针不会读到半成品。
		Exec: j.Exec,
	}
}

// FromSnapshot 从快照恢复（需重新注册Handler）。状态按快照原样还原：
// 崩溃恢复时由 Scheduler.Restore 显式把未完成任务复位为待处理。
func (j *Job) FromSnapshot(s JobSnapshot) {
	j.ID = s.ID
	j.Name = s.Name
	j.Type = s.Type
	j.Payload = s.Payload
	j.TriggerAt = s.TriggerAt
	j.Group = s.Group
	j.CronExpr = s.CronExpr
	j.IsRepeat = s.IsRepeat
	j.Timeout = time.Duration(s.Timeout)
	j.MaxRetries = s.MaxRetries
	j.RetryCount = s.RetryCount
	j.RetryDelay = time.Duration(s.RetryDelay)
	j.Status = JobStatus(s.Status)
	j.CreatedAt = s.CreatedAt
	j.UpdatedAt = s.UpdatedAt
	j.Attempts = s.Attempts
	j.Exec = s.Exec
}
