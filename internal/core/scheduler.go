// scheduler.go 是调度器的入口文件：Scheduler 结构体与构造、执行类别（JobClass）、
// 哨兵错误、跨文件共享的小助手（findSnapshot/notifyNewJob/notifyExecSlot/generateID）
// 与只读的观测访问器（RuntimeStats 等）。其余方法按职责分布在同包四个文件：
// scheduler_setup.go（装配期配置与 Handler 注册表）、scheduler_job.go（任务控制）、
// scheduler_runtime.go（启动/停止/调度循环/崩溃恢复）、scheduler_exec.go（执行与收尾）。
package core

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// DefaultConcurrency 默认的并发执行 worker 数量，与 docs/deployment.md 中的
// workers 配置口径一致。
const DefaultConcurrency = 100

// JobClass 是任务的执行类别，决定它进哪一个执行池（TASK-E13）。
//
// 只有两类：普通任务与执行器任务。按档位再拆更多池不在本卡范围（卡片 §8）。
type JobClass int

const (
	// JobClassDefault 是共享执行池：既有部署里所有任务、以及代码注册的处理函数都在这一类。
	// 取零值是为了让"没盖章"的任务落在老池子里，而不是掉进一个不存在的池。
	JobClassDefault JobClass = iota

	// JobClassExec 是执行器档位（exec.*）：单次执行按分钟计、一次 cron 触发可能几十条到期，
	// 因此独占一套 worker 与队列，不再和普通任务抢名额。
	JobClassExec
)

// Scheduler 任务调度器
type Scheduler struct {
	heap  *QuaternaryHeap
	store Store
	// retryPolicy 用原子指针而不是普通接口字段：worker 协程在 handleFailure 里读它，
	// 重载链要在运行期写它（SetRetryPolicy），普通字段的读写并发会被 -race 抓住。
	// 指针指向的是"接口值本身"，因此读写都是整条策略一次换掉，不存在半新半旧的策略。
	retryPolicy atomic.Pointer[RetryPolicy]
	cronParser  CronParser

	// misfirePolicy/misfireGrace 是 cron 错过窗口的全局默认（TASK-RE06），
	// 构造后不变（配置归 ClassRestart）；任务级 MisfirePolicy 字段优先于全局默认。
	misfirePolicy string
	misfireGrace  time.Duration

	// 控制
	mu      sync.RWMutex
	running bool
	stopCh  chan struct{}
	wg      sync.WaitGroup

	// 信号通知有新任务加入（用于提前唤醒定时器）
	newJobCh chan struct{}
	// resumeCh 只用于唤醒挂起中的调度循环，与"有新任务"分开：
	// 复用 newJobCh 会让 suspend 期间的 Schedule 调用把它占满，
	// 恢复信号就可能在下次循环之前被当成普通唤醒消费掉。
	resumeCh chan struct{}

	// suspended 是调度总开关：true 时不弹出任何到期任务。
	// 进程内状态，Start 会清零——重启即解除，维护窗口不该跨重启生效。
	suspended atomic.Bool

	// 执行侧：有界队列 + 固定 worker 池，避免到期风暴时无限起协程
	//
	// concurrency 是"启动期那份快照"：Start 按它建 workCh、起 worker。运行期扩缩不写它，
	// 因为 queueCapacity<=0 时通道容量回退到的就是这一个值（见 Start），
	// 把它改成新的期望并发会让 RuntimeStats 的 QueueCapacity 报出通道并不具备的容量
	// （两个读数口径见 docs/design/config-reload-design.md §7.1）。
	// 运行期的期望并发另存在 targetWorkers。
	concurrency   int
	queueCapacity int // 0 表示与 concurrency 相等
	workCh        chan *Job

	// targetWorkers 是"此刻想要几个 worker"，供 RuntimeStats 的 Workers 读数与扩缩判定。
	// 用原子而不是并进 s.mu 保护的 concurrency：它是运行期的期望并发，与 concurrency
	// 那份启动期快照在扩缩之后会分叉（通道容量按老值建着，期望并发按新值跑着），
	// 两个概念各自一个字段，读数才不会把彼此说成对方。
	targetWorkers atomic.Int32

	// retireRequests 是"还有几个 worker 该退场"的计数。缩容只加这个计数，
	// 不主动通知任何协程：正在跑任务的 worker 跑完、回到循环开头时看到计数>0 就自己减一并 return。
	// 这条形状（而不是给每个 worker 一个身份序号）是有意的：
	// 序号方案在 Start→Stop→Start 之后要重新对齐编号（Start 会重建 stopCh 与 workCh，
	// 老协程全退、新协程从 0 开始），而计数方案只需 Start 清零一次。
	retireRequests atomic.Int32

	// 执行器池（TASK-E13）：与上面的默认池各自排队、各自起 worker。
	// execConcurrency 为 0 表示这个池不存在——没打开执行器的进程连通道和协程都不建，
	// 行为与本卡之前完全一致。
	execConcurrency   int
	execQueueCapacity int // 0 表示与 execConcurrency 相等
	execCh            chan *Job

	// execInFlight 已进入执行器池 Handler、尚未返回的任务数。
	// 与 inFlight 分开计数：合成一个数字就给不出"执行器池把普通任务挤在哪儿"的读数。
	execInFlight atomic.Int32

	// execSlotFreed 容量 1、只做了非阻塞发送：执行器 worker 取走一个任务就敲一下，
	// 叫醒正在等空位的调度循环。
	//
	// 两次释放只触发一次唤醒是允许的（信号是"有变化"而不是"变化次数"）：
	// 调度循环还有一条 500ms 的兜底超时兜住漏掉的唤醒，最坏情况只是延迟一次投递，
	// 不会死锁。别把那条兜底超时当多余代码删掉。
	execSlotFreed chan struct{}

	// eventPreviewLimit 是完成/失败事件里输出预览的字节上限（executors.output.inline_preview）。
	// 事件会广播给全部 WS/SSE 订阅者，并在 api 的内存缓冲里留下最近若干条
	//（启用观测层的事件库时还会被持久化，见 docs/design/sqlite-observability-design.md §6.1），
	// 因此事件只带结论，输出正文留在产物文件里。受 s.mu 保护：装配期写入，执行协程读取。
	eventPreviewLimit int

	// 任务注册表，按 HandlerKey（Type，回退 Name）绑定Handler（用于从持久化恢复）
	handlers map[string]Handler

	// handlerClasses 记录每个注册键声明的执行类别，与 handlers 同一把锁保护。
	// 分开两张表而不是让 Handler 自己带类别：类别是注册方声明的部署属性，
	// 处理函数本身不该知道自己在哪个池里跑。
	handlerClasses map[string]JobClass

	// 取消控制（与 handlers 一样受 s.mu 保护）
	cancelMap map[string]context.CancelFunc
	// forcedPause 记下"已请求强制暂停、等待执行收尾认领"的任务 ID。
	// 用集合而不是存指针：中止是异步的，标记由 handleInterrupted/handleSuccess/
	// handleFailure 三条收尾路径之一消费，谁先回来谁负责把任务停在 paused。
	forcedPause map[string]struct{}
	// cancelRequested 记下"已请求取消、等待执行收尾认领"的任务 ID（TASK-RE01）。
	// 与 forcedPause 同形同理由：取消是异步的，Cancel 调用侧只登记标记并取消上下文，
	// 落 cancelled 终态由收尾路径认领——事件对应"确实停住了"这个事实。
	// 认领顺序：parkForcedPause 的标记先于本标记检查（强制暂停优先于一切）；
	// 三条收尾路径都会消费它，Handler 不响应取消而正常收尾时也不泄漏。
	cancelRequested map[string]struct{}

	// inFlight 已进入 Handler 执行、尚未返回的任务数，供统计接口读取
	inFlight atomic.Int32

	// restoreGuard 是崩溃恢复的状态改判钩子（TASK-E14），受 s.mu 保护。
	// 放在调度器而不是调用方：判断"崩溃瞬间在跑的执行器任务要不要停住"要读注册表里的类别，
	// 而注册表只有调度器握着。
	restoreGuard RestoreGuard

	// ledger 是逐次尝试账本（TASK-RE02），构造后不再变更；nil 表示本次部署没有
	// 账本能力（JSON 存储）：begin/finish/abandon 全部跳过，降级在构造日志里说明一次。
	// begin/finish 只由持有任务的 worker 协程调用，abandon 只在启动期 Restore 里调用，
	// 不需要额外的锁。
	ledger AttemptLedger

	// attemptsKeepLast 每任务账本保留条数，收尾后裁剪用；仅 ledger 非 nil 时有意义。
	attemptsKeepLast int

	// outbox 是事务性事件出口（TASK-RE07），构造后不再变更；nil 表示本次部署
	// 没有出箱能力（JSON 存储）：persistAndEmit/emit 退回直发（Update+Publish，
	// 与 RE01 之前逐字节等价）。注入后事件经中继器投递，调度器不再直接 Publish。
	outbox EventOutbox

	eventBus *EventBus // 新增

	// logger 结构化日志器，构造后不再变更；未注入时为 slog.Default()
	logger *slog.Logger
}

// NewScheduler 创建调度器。可通过 WithLogger 注入日志器。
func NewScheduler(store Store, retryPolicy RetryPolicy, eventBus *EventBus, opts ...Option) *Scheduler {
	if retryPolicy == nil {
		retryPolicy = &ExponentialBackoffRetry{}
	}

	if eventBus == nil {
		eventBus = NewEventBus(100) // 默认事件总线
	}

	settings := newComponentOptions(opts...)

	s := &Scheduler{
		heap:           NewQuaternaryHeap(),
		store:          store,
		cronParser:     NewCronParser(),
		stopCh:         make(chan struct{}),
		newJobCh:       make(chan struct{}, 1),
		resumeCh:       make(chan struct{}, 1),
		concurrency:    DefaultConcurrency,
		execSlotFreed:  make(chan struct{}, 1),
		handlers:       make(map[string]Handler),
		handlerClasses: make(map[string]JobClass),
		cancelMap:      make(map[string]context.CancelFunc),
		forcedPause:    make(map[string]struct{}),
		// cancelRequested 与 forcedPause 同批初始化（见字段注释）。
		cancelRequested: make(map[string]struct{}),
		// 预览上限默认取配置默认值：不装配执行器的程序也一样，事件里的预览不会没头没尾。
		eventPreviewLimit: DefaultExecInlinePreview,
		eventBus:          eventBus,
		logger:            resolveLogger(settings.logger),
	}
	// 原子字段不能进结构体字面量，所以建好体之后一次性 Store。
	s.retryPolicy.Store(&retryPolicy)
	// targetWorkers 与 concurrency 成对初始化：构造完到 Start 之前，RuntimeStats 的
	// Workers 读的是前者，少这一行会让没显式设置过并发的调度器报出 0 而不是 DefaultConcurrency。
	s.targetWorkers.Store(int32(s.concurrency))

	// 账本能力（TASK-RE02）：注入即启用，nil 即降级。降级只在这里说一次——
	// 执行路径上每个尝试都记日志会把"没这能力"放大成噪音。
	s.ledger = settings.ledger
	if s.ledger != nil {
		s.attemptsKeepLast = settings.attemptsKeepLast
		if s.attemptsKeepLast <= 0 {
			s.attemptsKeepLast = DefaultAttemptsKeepLast
		}
	} else {
		s.logger.Info("attempt ledger not available; per-attempt records are disabled (audit relies on the event stream)")
	}

	// misfire 全局默认（TASK-RE06）：未注入用默认（fire_once + 60s 宽限），
	// 与"什么都不配 = 现状行为"对齐。
	s.misfirePolicy = settings.misfirePolicy
	if s.misfirePolicy == "" {
		s.misfirePolicy = DefaultMisfirePolicy
	}
	s.misfireGrace = settings.misfireGrace
	if s.misfireGrace <= 0 {
		s.misfireGrace = DefaultMisfireGracePeriod
	}

	// 出箱能力（TASK-RE07）：注入即启用，nil 即直发。两条通道的分派收口在
	// state.go 的 persistAndEmit/emit，调用方不感知。
	s.outbox = settings.outbox
	return s
}

// notifyExecSlot 非阻塞敲一次"执行器池腾出了空位"，用于叫醒在等空位的调度循环。
//
// 信号可能被合并（容量 1，两次释放只唤醒一次），这是允许的：
// 调度循环还有一条 500ms 的兜底超时会重新检查队列，最坏只是晚一点投递，不会卡死。
func (s *Scheduler) notifyExecSlot() {
	select {
	case s.execSlotFreed <- struct{}{}:
	default:
	}
}

// findSnapshot 按 ID 取存储中的快照。
//
// Store 提供 SnapshotReader 能力时直查（TASK-RE03，JSON 与任务库都实现）；
// 第三方 Store 没有这项能力时回退 LoadAll 线性扫描（既有行为，量级由
// history_limit 约束）。这条回退是有意的兼容层，不是待删代码。
func (s *Scheduler) findSnapshot(jobID string) (JobSnapshot, bool) {
	if s.store == nil {
		return JobSnapshot{}, false
	}
	if reader, ok := s.store.(SnapshotReader); ok {
		snap, found, err := reader.Get(jobID)
		if err != nil {
			s.logger.Error("failed to load a job snapshot", "job_id", jobID, "error", err)
			return JobSnapshot{}, false
		}
		return snap, found
	}

	snapshots, err := s.store.LoadAll()
	if err != nil {
		s.logger.Error("failed to load jobs while looking up a job", "job_id", jobID, "error", err)
		return JobSnapshot{}, false
	}
	for _, snap := range snapshots {
		if snap.ID == jobID {
			return snap, true
		}
	}
	return JobSnapshot{}, false
}

// notifyNewJob 唤醒调度循环重算等待时长；信号通道满时丢弃即可（已有待处理唤醒）。
func (s *Scheduler) notifyNewJob() {
	select {
	case s.newJobCh <- struct{}{}:
	default:
	}
}

// ErrJobNotPending 表示任务已不在待执行队列中（已被弹出执行、已结束或不存在），
// 因此无法原地更新。
var ErrJobNotPending = errors.New("job is not pending")

// ErrJobNotPaused 表示任务当前不处于暂停状态，无法恢复。
var ErrJobNotPaused = errors.New("job is not paused")

// RunningCount 返回正在执行（已进入 Handler）的任务数。
func (s *Scheduler) RunningCount() int {
	return int(s.inFlight.Load())
}

// GetEventBus 暴露 EventBus（用于WebSocket订阅）
func (s *Scheduler) GetEventBus() *EventBus {
	return s.eventBus
}

// generateID 生成任务/订阅/客户端的标识：UUIDv7，时间有序且随机位来自 crypto/rand。
// 旧实现是"秒级时间戳 + 8 位 math/rand 后缀"，同一秒提交的任务会因随机位碰撞得到
// 相同 ID，后一个直接覆盖前一个（任务静默丢失，无任何报错）。
func generateID() string {
	return uuid.Must(uuid.NewV7()).String()
}

// HeapLen 获取堆中任务数量（用于监控）
func (s *Scheduler) HeapLen() int {
	return s.heap.Len()
}

// RuntimeStats 是调度与执行侧的实时占用，供运维端点读取。
// 只有计数与开关状态，不含任务内容与凭据，因此 viewer 以外的角色才需要看到它。
//
// 两个池的读法要说清（TASK-E13 改了 Running 的含义范围）：
// Running 与 QueueLength 只算**普通池**，执行器池的对应值是 ExecRunning 与 ExecQueueLength。
// 接口侧 /stats 的 running 是两池之和（见 api 的 GetStats），
// 想定位"执行器把名额占到什么程度"就读 /admin/runtime 的拆分值。
type RuntimeStats struct {
	// Started 表示调度循环与 worker 池是否在跑（Stop 之后为 false）
	Started bool `json:"started"`
	// Workers 期望的普通任务执行池协程数：运行期扩缩立刻改这个读数，
	// 实跑协程要等任务重新流动才收敛——闲着的多余协程停在 select 上，
	// 既不检查退场名额也不会自己退出（温和缩容，见 ResizeWorkers）。
	Workers int `json:"workers"`
	// QueueCapacity 普通任务执行队列容量；未显式配置时与**启动期**的 worker 数相等。
	// 通道是 Start 那一刻建出来的，之后扩缩都不换通道，所以这一项不跟随 Workers。
	QueueCapacity int `json:"queue_capacity"`
	// QueueLength 普通任务里已入队但尚未被 worker 取走的数量
	QueueLength int `json:"queue_length"`
	// Running 普通任务里已进入 Handler、尚未返回的数量（不含执行器池，见结构体注释）
	Running int `json:"running"`
	// ExecWorkers 执行器池的协程数；0 表示这一次启动没建这个池
	ExecWorkers int `json:"exec_workers"`
	// ExecQueueCap 执行器队列容量；未显式配置时与 ExecWorkers 相等
	ExecQueueCap int `json:"exec_queue_capacity"`
	// ExecQueueLength 执行器任务里已入队、尚未被 worker 取走的数量
	ExecQueueLength int `json:"exec_queue_length"`
	// ExecRunning 执行器池里已进入 Handler、尚未返回的数量
	ExecRunning int `json:"exec_running"`
	// HeapSize 堆中待执行任务数（两个池共用一张堆，这里是总数）
	HeapSize int `json:"heap_size"`
	// Suspended 调度总开关是否处于挂起
	Suspended bool `json:"suspended"`
	// ForcePausePending 已请求强制暂停、尚待执行收尾认领的任务数
	ForcePausePending int `json:"force_pause_pending"`
}

// RuntimeStats 返回当前占用快照。队列长度与在跑数量是瞬时值，
// 读到的只是调用那一刻的状态，用于观察趋势而不是审计。
func (s *Scheduler) RuntimeStats() RuntimeStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	capacity := s.queueCapacity
	if capacity <= 0 {
		capacity = s.concurrency
	}

	execCapacity := s.execQueueCapacity
	if execCapacity <= 0 {
		execCapacity = s.execConcurrency
	}

	return RuntimeStats{
		Started: s.running,
		// Workers 读的是期望并发（targetWorkers），不是启动期那份 concurrency 快照：
		// 与下面 QueueCapacity 的口径分别见两个结构体字段的注释，扩缩之后两者可以不相等。
		Workers:           int(s.targetWorkers.Load()),
		QueueCapacity:     capacity,
		QueueLength:       len(s.workCh),
		Running:           int(s.inFlight.Load()),
		ExecWorkers:       s.execConcurrency,
		ExecQueueCap:      execCapacity,
		ExecQueueLength:   len(s.execCh),
		ExecRunning:       int(s.execInFlight.Load()),
		HeapSize:          s.heap.Len(),
		Suspended:         s.suspended.Load(),
		ForcePausePending: len(s.forcedPause),
	}
}
