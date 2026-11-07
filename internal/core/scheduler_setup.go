// scheduler_setup.go 收纳装配期方法，均要求在 Start 之前调用：
// 并发/队列/事件预览/重试策略的 Set* 配置（运行期调用只 warn 并忽略），
// 以及 Handler 注册表（RegisterHandler/UnregisterHandler 与类别、查找查询）。
package core

import (
	"sort"
	"time"
)

// SetConcurrency 设置执行 worker 数量，需在 Start 之前调用。
// 传入非正数时回退到 DefaultConcurrency；Start 之后调用不生效并记录日志。
func (s *Scheduler) SetConcurrency(n int) {
	if n <= 0 {
		n = DefaultConcurrency
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		s.logger.Warn("SetConcurrency ignored: scheduler already running", "workers", n)
		return
	}
	s.concurrency = n
	// 与 concurrency 一起写：Workers 读数取自 targetWorkers（期望并发口径），
	// 装配期设的值也要在 Start 之前读得出来，否则设成 4 的调度器报 0。
	// 运行期真要改并发请走 ResizeWorkers——这里只写下取值，不起也不退任何协程。
	s.targetWorkers.Store(int32(n))
}

// SetQueueCapacity 设置执行队列容量，需在 Start 之前调用。
// 0 或负数表示与 worker 数相等；队列满时调度循环阻塞入队（背压）。
func (s *Scheduler) SetQueueCapacity(n int) {
	if n < 0 {
		n = 0
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		s.logger.Warn("SetQueueCapacity ignored: scheduler already running", "queue_capacity", n)
		return
	}
	s.queueCapacity = n
}

// SetExecConcurrency 设置执行器池的 worker 数量，需在 Start 之前调用；
// Start 之后调用不生效并记录日志。
//
// 与 SetConcurrency 的一条差别要说清：这里 0 不是"回退默认值"，而是"不建这个池"。
// 卡片 §3.3 要求 0 回退默认，但那条与 §3.7"enabled=false 时传 0（不建 exec 池）"直接冲突——
// 回退默认会让"关掉这个池"没有表达方式，而且没装执行器的进程不该凭空多出 4 个协程。
// 默认值 4 由配置层的 executors.concurrency 负责（Normalized 会补齐，显式写 0 会被配置校验拒绝），
// 本 setter 不重复一遍。负数同样按关闭处理。
func (s *Scheduler) SetExecConcurrency(n int) {
	if n < 0 {
		n = 0
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		s.logger.Warn("SetExecConcurrency ignored: scheduler already running", "exec_workers", n)
		return
	}
	s.execConcurrency = n
}

// SetExecQueueCapacity 设置执行器队列容量，需在 Start 之前调用。
// 0 或负数表示与 exec worker 数相等。
//
// 队列满时调度循环的行为与默认池不同：不阻塞，任务留在堆里等空位（见 scheduleLoop）。
func (s *Scheduler) SetExecQueueCapacity(n int) {
	if n < 0 {
		n = 0
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		s.logger.Warn("SetExecQueueCapacity ignored: scheduler already running", "exec_queue_capacity", n)
		return
	}
	s.execQueueCapacity = n
}

// SetEventPreviewLimit 设置完成/失败事件里输出预览的字节上限，需在 Start 之前调用。
// 取值来自 executors.output.inline_preview，由装配方传入；传入非正数时回退到
// DefaultExecInlinePreview，避免一次配置笔误让事件里连一行摘要都不剩。
//
// 只传一个字节数而不是传配置结构：core 不依赖执行器包，而事件是这里发的，
// 尺寸限制必须由发事件的一方执行，否则两条限制会各算各的。
func (s *Scheduler) SetEventPreviewLimit(n int) {
	if n <= 0 {
		n = DefaultExecInlinePreview
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		s.logger.Warn("SetEventPreviewLimit ignored: scheduler already running", "inline_preview", n)
		return
	}
	s.eventPreviewLimit = n
}

// SetRetryPolicy 运行期替换重试策略。传 nil 视为不改（保持现值）。
//
// 与上面那批 Set* 的分别：那些只在 Start 前有效、运行期 warn 并忽略，
// 本方法是专给重载链用的（配置热重载 R02/R06），运行期生效——
// 退避算法不绑通道也不绑协程，换掉它只影响下一次失败重试算出的时刻。
func (s *Scheduler) SetRetryPolicy(p RetryPolicy) {
	if p == nil {
		return
	}
	s.retryPolicy.Store(&p)
}

// RetryPolicyMaxDelay 返回当前策略的延迟上限，非 ExponentialBackoffRetry 时返回 0。
// 给 /admin/runtime 的重载读数与测试用：调用方不该为了看一个取值去断言接口类型。
func (s *Scheduler) RetryPolicyMaxDelay() time.Duration {
	loaded := s.retryPolicy.Load()
	if loaded == nil {
		return 0
	}
	backoff, ok := (*loaded).(*ExponentialBackoffRetry)
	if !ok {
		return 0
	}
	return backoff.MaxDelay
}

// RegisterHandler 注册任务类型对应的处理函数。
// 等价于 RegisterHandlerClass(jobType, handler, JobClassDefault)——这是兼容底线：
// 既有调用点、示例处理函数与测试都不需要知道"执行池"这个概念。
func (s *Scheduler) RegisterHandler(jobType string, handler Handler) {
	s.RegisterHandlerClass(jobType, handler, JobClassDefault)
}

// RegisterHandlerClass 注册处理函数并声明它的执行类别（TASK-E13）。
//
// 执行器档位用 JobClassExec：这类任务单次执行按分钟计，共享池会让普通任务的准时性失效。
// 类别在注册时定下、入堆时盖章到任务对象上，之后改注册不会影响已经在堆里的任务
// （docs/design/web-profile-design.md D8）——这一点与"谁来注册"无关：
// 注册既可能发生在启动阶段（装配 executors.commands），也可能发生在运行期
// （档位的在线管理，`executors.web_enabled` 打开后由 api 层触发）。
// 运行期的注册与摘除走的是下面这对方法，共用 s.mu，不需要新增锁。
func (s *Scheduler) RegisterHandlerClass(jobType string, handler Handler, class JobClass) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[jobType] = handler
	s.handlerClasses[jobType] = class
}

// UnregisterHandler 摘除一个注册键，返回它此前是否注册着。
//
// 两张表必须成对删：只删 handlers 会留下 handlerClasses 里的孤儿，HandlerClass
// 依旧回 (JobClassExec, true)，崩溃恢复守卫据此认为"这个键还注册着、属于执行池"，
// 把一条其实已经没有处理函数的 running 任务钉成 paused（判据见 cmd/server/main.go
// 的 pauseRunningExecOnRestore）。它本该在 executeJob 里找不到处理函数直接判失败，
// 就此变成永远等不到档位的僵尸任务。
//
// 它不停任何在跑的任务，也不动 cancelMap：中止执行属于 ForcePause（admin 档），
// 摘除注册键不该附带"替人中止一次已经发生的副作用"。
//
// 调用方负责顺序：先落盘再生效。反过来（先摘 handler 再写文件失败）会留下
// "档位文件里有这条、进程里跑不了"的半状态，重启之后自己就好了，
// 但那一刻接口与真实能力不一致。见 docs/design/web-profile-design.md §4 的 I2。
func (s *Scheduler) UnregisterHandler(jobType string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, ok := s.handlers[jobType]
	delete(s.handlers, jobType)
	delete(s.handlerClasses, jobType)
	return ok
}

// classOfKey 查注册表里声明的执行类别；没登记过的键按默认池处理（零值即 JobClassDefault）。
func (s *Scheduler) classOfKey(key string) JobClass {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.handlerClasses[key]
}

// HandlerClass 查某个注册键声明的执行类别，第二个返回值表示这个键有没有登记过。
//
// 给装配方用（TASK-E14 的崩溃守卫要按类别决定"结果未知的任务停不停住"）。
// 与内部用的 classOfKey 差在那条 bool 上：调用方必须能区分"注册过、属于普通池"
// 与"根本没注册过"。档位被删掉之后它的历史任务就落在后者，
// 这时候按"不知道"处理比按"普通任务"处理更诚实——恢复策略因此选择照常重排，
// 而那次重排在 executeJob 里会因找不到处理函数直接判失败，不会真的重复产生副作用。
func (s *Scheduler) HandlerClass(key string) (JobClass, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	class, ok := s.handlerClasses[key]
	return class, ok
}

// LookupHandler 按 Handler 键（Type 优先，回退 Name）查找已注册的处理函数。
// 注册表只有调度器这一份，HTTP 层校验与执行侧绑定共用它。
func (s *Scheduler) LookupHandler(key string) (Handler, bool) {
	return s.lookupHandler(key)
}

// HandlerNames 返回全部已注册的 Handler 键，按字典序排列，供接口稳定输出。
func (s *Scheduler) HandlerNames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	names := make([]string, 0, len(s.handlers))
	for name := range s.handlers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// lookupHandler 按任务键查找已注册的Handler
func (s *Scheduler) lookupHandler(key string) (Handler, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h, ok := s.handlers[key]
	return h, ok
}
