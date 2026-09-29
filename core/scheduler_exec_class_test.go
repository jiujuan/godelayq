package core

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// 这一组是 TASK-E13 的池隔离用例。单独成文件而不是塞进 scheduler_concurrency_test.go：
// 那边验的是"一个池的容量与背压"，这里验的是"两个池并存时的行为"，
// 用例需要一套自己的计数辅助函数，混在一起会让两边的阅读负担都翻倍。

// poolCounters 记录两个池各自的在跑数量与峰值，以及跑完的总数。
type poolCounters struct {
	mu          sync.Mutex
	plainActive int
	execActive  int
	plainPeak   int
	execPeak    int
	finished    int
}

func (c *poolCounters) active() (plain int, exec int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.plainActive, c.execActive
}

func (c *poolCounters) peaks() (plain int, exec int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.plainPeak, c.execPeak
}

func (c *poolCounters) done() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.finished
}

// blockedHandler 造一个卡在闸门上、并把在跑数量记进计数器的处理函数。
// gate 关闭之前它不返回，因此"同时在跑几个"是可以被观察到的。
func blockedHandler(c *poolCounters, gate chan struct{}, execJob bool) Handler {
	return func(ctx context.Context, job *Job) error {
		c.mu.Lock()
		if execJob {
			c.execActive++
			if c.execActive > c.execPeak {
				c.execPeak = c.execActive
			}
		} else {
			c.plainActive++
			if c.plainActive > c.plainPeak {
				c.plainPeak = c.plainActive
			}
		}
		c.mu.Unlock()

		<-gate

		c.mu.Lock()
		if execJob {
			c.execActive--
		} else {
			c.plainActive--
		}
		c.finished++
		c.mu.Unlock()
		return nil
	}
}

// waitActive 等两个池同时各占满自己的名额。
// 本卡的判定点就在这上面：只有一边跑起来等于没测到隔离。
func waitActive(t *testing.T, counters *poolCounters, plainWant, execWant int, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		plain, exec := counters.active()
		if plain >= plainWant && exec >= execWant {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	plain, exec := counters.active()
	t.Fatalf("两个池没有同时占满自己的名额（普通在跑 %d 期望 %d，执行器在跑 %d 期望 %d）",
		plain, plainWant, exec, execWant)
}

// openGate 把闸门只开一次，供 defer 收尾用。
func openGate(gate chan struct{}) func() {
	var once sync.Once
	return func() { once.Do(func() { close(gate) }) }
}

// TestExecClass_UsesOwnPool 是 §5.1 第一条：普通池并发 2、执行器池并发 1，
// 两边同时各占满自己的名额，互不阻塞。
func TestExecClass_UsesOwnPool(t *testing.T) {
	const (
		plainWorkers = 2
		execWorkers  = 1
		plainTotal   = 3
		execTotal    = 3
	)

	counters := &poolCounters{}
	gate := make(chan struct{})
	unblock := openGate(gate)
	defer unblock()

	scheduler := NewScheduler(nil, nil, nil)
	scheduler.SetConcurrency(plainWorkers)
	scheduler.SetExecConcurrency(execWorkers)
	scheduler.RegisterHandler("plain_work", blockedHandler(counters, gate, false))
	scheduler.RegisterHandlerClass("exec_work", blockedHandler(counters, gate, true), JobClassExec)

	now := time.Now()
	for i := 0; i < plainTotal; i++ {
		if err := scheduler.Schedule(&Job{ID: "plain-" + strconv.Itoa(i), Name: "plain_work", TriggerAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < execTotal; i++ {
		if err := scheduler.Schedule(&Job{ID: "exec-" + strconv.Itoa(i), Name: "exec_work", TriggerAt: now}); err != nil {
			t.Fatal(err)
		}
	}

	scheduler.Start()
	defer scheduler.Stop()

	waitActive(t, counters, plainWorkers, execWorkers, 5*time.Second)

	unblock()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && counters.done() < plainTotal+execTotal {
		time.Sleep(10 * time.Millisecond)
	}
	if got := counters.done(); got != plainTotal+execTotal {
		t.Fatalf("六个任务都要跑完，实际 %d", got)
	}

	plainPeak, execPeak := counters.peaks()
	if plainPeak > plainWorkers {
		t.Errorf("普通池峰值超过 worker 数：%d > %d", plainPeak, plainWorkers)
	}
	if execPeak > execWorkers {
		t.Errorf("执行器池峰值超过 worker 数：%d > %d", execPeak, execWorkers)
	}
	if execPeak < execWorkers {
		t.Errorf("执行器池没占满自己的名额，峰值 %d", execPeak)
	}
}

// TestExecClass_DefaultKeepsBlockingBackpressure 是 §5.1 第二条（兼容底线）：
// 执行器池关闭时不建通道也不起协程，档位任务照旧走共享池的"满则阻塞"。
func TestExecClass_DefaultKeepsBlockingBackpressure(t *testing.T) {
	const total = 3

	counters := &poolCounters{}
	gate := make(chan struct{})
	unblock := openGate(gate)
	defer unblock()

	scheduler := NewScheduler(nil, nil, nil)
	scheduler.SetConcurrency(1)
	scheduler.SetExecConcurrency(0)
	scheduler.RegisterHandlerClass("exec_work", blockedHandler(counters, gate, true), JobClassExec)

	scheduler.Start()
	if scheduler.execCh != nil {
		t.Error("SetExecConcurrency(0) 之后不应创建执行器队列")
	}
	defer scheduler.Stop()

	now := time.Now()
	for i := 0; i < total; i++ {
		if err := scheduler.Schedule(&Job{ID: "exec-only-" + strconv.Itoa(i), Name: "exec_work", TriggerAt: now}); err != nil {
			t.Fatal(err)
		}
	}

	// 共享池只有一个 worker：档位任务照旧在计数器的执行器那一栏里出现（计数只看处理函数本身，
	// 不看它落在哪个池），这里等的就是"有一个在跑、且只有 1 个在跑"。
	waitActive(t, counters, 0, 1, 5*time.Second)

	unblock()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && counters.done() < total {
		time.Sleep(10 * time.Millisecond)
	}
	if got := counters.done(); got != total {
		t.Errorf("共享池路径上的档位任务要全部跑完，实际 %d", got)
	}
}

// TestExecClass_FullQueueKeepsJobInHeap 是 §5.1 第三条，也是 DoD 第一条那条"必须有用例直接证明"：
// 执行器队列满时任务留在堆里、状态仍是 pending，同时到期的普通任务照常被执行。
func TestExecClass_FullQueueKeepsJobInHeap(t *testing.T) {
	const execTotal = 5

	counters := &poolCounters{}
	gate := make(chan struct{})
	unblock := openGate(gate)
	defer unblock()

	scheduler := NewScheduler(nil, nil, nil)
	scheduler.SetConcurrency(2)
	scheduler.SetExecConcurrency(1)
	scheduler.SetExecQueueCapacity(1)
	scheduler.RegisterHandlerClass("exec_work", blockedHandler(counters, gate, true), JobClassExec)

	plainDone := make(chan struct{}, 1)
	scheduler.RegisterHandler("plain_late", func(ctx context.Context, job *Job) error {
		plainDone <- struct{}{}
		return nil
	})

	now := time.Now()
	for i := 0; i < execTotal; i++ {
		if err := scheduler.Schedule(&Job{ID: "exec-full-" + strconv.Itoa(i), Name: "exec_work", TriggerAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	// 普通任务比执行器任务晚一毫秒到期：共享池时它排在被挡住的任务后面拿不到名额。
	if err := scheduler.Schedule(&Job{ID: "plain-late", Name: "plain_late", TriggerAt: now.Add(time.Millisecond)}); err != nil {
		t.Fatal(err)
	}

	scheduler.Start()
	defer scheduler.Stop()

	select {
	case <-plainDone:
	case <-time.After(5 * time.Second):
		t.Fatal("执行器队列满时普通任务被挡住了——这条正是本卡要消掉的现象")
	}

	// 闸门还关着：执行器池只有 1 个 worker + 1 个队列位，剩下的必须还在堆里
	if scheduler.HeapLen() == 0 {
		t.Fatal("执行器队列满时任务不该消失，应留在堆里等空位")
	}
	for i := 0; i < execTotal; i++ {
		id := "exec-full-" + strconv.Itoa(i)
		item := scheduler.heap.Get(id)
		if item == nil {
			continue // 已经投递出去的那些本来就不在堆里
		}
		if status := item.(*Job).Status; status != StatusPending {
			t.Errorf("留在堆里的任务 %s 状态要仍是 pending，实际 %s", id, status)
		}
	}

	unblock()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && counters.done() < execTotal {
		time.Sleep(10 * time.Millisecond)
	}
	if got := counters.done(); got != execTotal {
		t.Errorf("空位腾出来之后留在堆里的任务要全部执行，实际 %d", got)
	}
}

// TestExecClass_NoBusyWait 是 §5.1 第四条：队列满的那段时间调度循环必须在等，
// 而不是拿一个已经过去的触发时间转圈。判据用"等待执行器空位"这条日志的出现次数：
// 兜底超时 500 毫秒，2 秒内正常只会个位数；忙等会是成千上万次。
func TestExecClass_NoBusyWait(t *testing.T) {
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	counters := &poolCounters{}
	gate := make(chan struct{})
	unblock := openGate(gate)
	defer unblock()

	scheduler := NewScheduler(nil, nil, nil, WithLogger(logger))
	scheduler.SetConcurrency(2)
	scheduler.SetExecConcurrency(1)
	scheduler.SetExecQueueCapacity(1)
	scheduler.RegisterHandlerClass("exec_work", blockedHandler(counters, gate, true), JobClassExec)

	now := time.Now()
	for i := 0; i < 8; i++ {
		if err := scheduler.Schedule(&Job{ID: "exec-spin-" + strconv.Itoa(i), Name: "exec_work", TriggerAt: now}); err != nil {
			t.Fatal(err)
		}
	}

	scheduler.Start()
	defer scheduler.Stop()

	time.Sleep(2 * time.Second)
	unblock()

	// 先停调度器再读日志缓冲：正在跑的调度循环还在往 strings.Builder 里写，
	// 一边写一边读会被 -race 抓下来。Stop 可重复调用，上面那条 defer 不会打架。
	scheduler.Stop()

	blocked := strings.Count(logs.String(), "due job is waiting for an executor slot")
	if blocked == 0 {
		t.Fatal("没等到等待执行器空位这条记录，用例没测到东西")
	}
	if blocked > 20 {
		t.Errorf("队列满时调度循环疑似忙等：2 秒内重查了 %d 次", blocked)
	}
}

// TestStop_WaitsBothPools 是 §5.1 第五条：两个池各有在途任务时 Stop 要等两边都返回。
// wg 少算任何一个池，Stop 就会在执行器任务还在跑时返回。
func TestStop_WaitsBothPools(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)
	scheduler.SetConcurrency(1)
	scheduler.SetExecConcurrency(1)

	started := make(chan string, 2)
	returned := make(chan string, 2)
	handler := func(ctx context.Context, job *Job) error {
		started <- job.ID
		<-ctx.Done()
		// 收尾留一点可见的时间差：Stop 若没等这个池，就会先于 returned 返回
		time.Sleep(200 * time.Millisecond)
		returned <- job.ID
		return ctx.Err()
	}
	scheduler.RegisterHandler("slow_plain", handler)
	scheduler.RegisterHandlerClass("slow_exec", handler, JobClassExec)

	if err := scheduler.Schedule(&Job{ID: "pool-plain", Name: "slow_plain", TriggerAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Schedule(&Job{ID: "pool-exec", Name: "slow_exec", TriggerAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	scheduler.Start()

	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("两个池都要有任务在跑")
		}
	}

	stopped := make(chan struct{})
	go func() {
		scheduler.Stop()
		close(stopped)
	}()

	for i := 0; i < 2; i++ {
		select {
		case <-returned:
		case <-time.After(5 * time.Second):
			t.Fatal("在途任务没有收到取消并返回")
		}
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop 没有等执行器池退出")
	}
}

// TestExecClass_CancelAndPauseShareControl 是 DoD 第三条：取消与暂停与队列无关，
// 两个池共用同一张控制表，档位任务不能因为换了池就躲掉控制。
func TestExecClass_CancelAndPauseShareControl(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)
	scheduler.SetConcurrency(1)
	scheduler.SetExecConcurrency(1)

	started := make(chan string, 1)
	cancelled := make(chan error, 1)
	scheduler.RegisterHandlerClass("exec_work", func(ctx context.Context, job *Job) error {
		started <- job.ID
		<-ctx.Done()
		cancelled <- ctx.Err()
		return ctx.Err()
	}, JobClassExec)
	scheduler.RegisterHandlerClass("exec_later", func(ctx context.Context, job *Job) error {
		return nil
	}, JobClassExec)

	if err := scheduler.Schedule(&Job{ID: "exec-cancel", Name: "exec_work", TriggerAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	scheduler.Start()
	defer scheduler.Stop()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("执行器任务没跑起来")
	}

	if err := scheduler.Cancel("exec-cancel"); err != nil {
		t.Fatalf("Cancel 失败: %v", err)
	}
	select {
	case err := <-cancelled:
		if err != context.Canceled {
			t.Errorf("取消要传到执行器任务的上下文里，实际 %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("取消没有传到执行器任务")
	}

	// 暂停：还没到点的档位任务停在 paused，恢复之前不会被投递
	if err := scheduler.Schedule(&Job{ID: "exec-pause", Name: "exec_later", TriggerAt: time.Now().Add(2 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.Pause("exec-pause"); err != nil {
		t.Fatalf("Pause 失败: %v", err)
	}
	if scheduler.heap.Get("exec-pause") != nil {
		t.Error("暂停之后的档位任务不该留在堆里")
	}
}

// TestExecClass_DisabledCreatesNoPool 是 DoD 第五条：没打开执行器时不建队列、不起协程。
//
// 卡片允许两种写法（NumGoroutine 差值或 SetExecConcurrency(0) 的分支测试），这里选后者：
// 同包其它测试会留下在途协程，进程级计数在本机 `go test ./core -race -count=5` 下会 ±1 抖动
// （实测出现过"应当多出 3 个，实际多出 2 个"），差值断言立不住。
// 改为读调度器自己的结论：关闭时连通道都不建， worker 协程因此不可能存在；
// 打开时通道容量与 RuntimeStats 报出的规模都要对上。
func TestExecClass_DisabledCreatesNoPool(t *testing.T) {
	disabled := NewScheduler(nil, nil, nil)
	disabled.SetConcurrency(2)
	disabled.SetExecConcurrency(0)
	disabled.SetExecQueueCapacity(0)
	disabled.Start()

	if disabled.execPoolEnabled() {
		t.Error("execConcurrency=0 时调度器不该认为执行器池已启用")
	}
	if disabled.execCh != nil {
		t.Errorf("执行器池关闭时不该建队列，实际容量 %d", cap(disabled.execCh))
	}
	if stats := disabled.RuntimeStats(); stats.ExecWorkers != 0 || stats.ExecQueueCap != 0 {
		t.Errorf("关闭状态下统计应为 0，实际 workers=%d cap=%d", stats.ExecWorkers, stats.ExecQueueCap)
	}
	disabled.Stop()

	enabled := NewScheduler(nil, nil, nil)
	enabled.SetConcurrency(2)
	enabled.SetExecConcurrency(3)
	enabled.SetExecQueueCapacity(7)
	enabled.Start()

	if !enabled.execPoolEnabled() {
		t.Fatal("execConcurrency=3 时执行器池应已启用")
	}
	if got := cap(enabled.execCh); got != 7 {
		t.Errorf("执行器队列容量应取自配置，实际 %d", got)
	}
	stats := enabled.RuntimeStats()
	if stats.ExecWorkers != 3 || stats.ExecQueueCap != 7 {
		t.Errorf("执行器池规模应报出配置值，实际 workers=%d cap=%d", stats.ExecWorkers, stats.ExecQueueCap)
	}
	// 通道里排满 3 条：worker 各拿一条在途，剩下 1 条留在队列里，
	// 这个组合只能由"确实起了 3 个执行器 worker"解释。
	gate := make(chan struct{})
	enabled.RegisterHandlerClass("held", func(ctx context.Context, job *Job) error {
		<-gate
		return nil
	}, JobClassExec)
	for i := 0; i < 4; i++ {
		if err := enabled.Schedule(&Job{
			ID:        "exec-pool-" + strconv.Itoa(i),
			Name:      "held",
			TriggerAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if n := enabled.execInFlight.Load(); n == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("执行器池只起了 %d 个在途任务，应为 3 个", enabled.execInFlight.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(gate)
	enabled.Stop()
}
