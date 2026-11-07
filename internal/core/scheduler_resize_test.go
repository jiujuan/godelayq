package core

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 这一组是 TASK-R03 的运行期 worker 扩缩用例。单独成文件而不是塞进
// scheduler_concurrency_test.go：那边判的是"一个池的容量与背压"（一次 Start 之内的事），
// 这里判的是"协程数在运行期变了之后到底谁还在跑"（跨过 ResizeWorkers 才出现的事）。
// 闸门与计数一律复用 core/scheduler_exec_class_test.go 里那一份 poolCounters/blockedHandler/
// waitActive/openGate（同包可直接用），不在两个文件里各写一套。
//
// 三条口径来自卡片 §5：判据等条件而不是睡时长——本文件只有两段例外（assertHoldsAt 与
// assertStopNotReturnedYet），它们等的是**反**条件，窗口长度只决定漏检概率，
// 所以每处都另配了一条不看时间的终局判据（并发峰值）；每个用例都 defer unblock()，
// 与 defer scheduler.Stop() 同时存在时 unblock 注册在它之后——defer 是后进先出，
// 收尾时闸门先开、Stop 后等，失败路径上不会有 worker 卡在闸门里把用例挂死；
// 调度器一律 NewScheduler(nil, nil, nil)，无存储也能跑。

// waitActiveExactly 等到普通池在跑数量**正好**是 want 并且连续两轮不变。
// 与 waitActive 的分别：那条判"至少占满名额"，缩容后要判"退场已经完成"，大于 want 必须继续等。
func waitActiveExactly(t *testing.T, c *poolCounters, want int, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	stable := 0
	for time.Now().Before(deadline) {
		plain, _ := c.active()
		if plain == want {
			if stable++; stable >= 2 {
				return
			}
		} else {
			stable = 0
		}
		time.Sleep(10 * time.Millisecond)
	}
	plain, _ := c.active()
	t.Fatalf("plain active never settled at exactly %d, got %d", want, plain)
}

// resizeJob 造一条即时任务，Name 落在本文件注册的处理函数键上。
// 构造形状照 core/scheduler_exec_class_test.go 的用法：只给 ID、Name、TriggerAt。
func resizeJob(prefix string, i int) *Job {
	return &Job{ID: prefix + "-" + strconv.Itoa(i), Name: "resize_work", TriggerAt: time.Now()}
}

// waitUntil 是"数到几个"这类判据的公共轮询：卡片只给了在跑数量的两条 wait，
// 而本卡一半判据落在 done() 与 retireRequests 这些单调量上，逐条抄 10ms 轮询要抄五遍。
func waitUntil(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("判据在 %v 内没有成立：%s", timeout, what)
}

// assertHoldsAt 是一段"必须保持不变"的观察窗口，等的是条件的**反面**：
// 在跑数量被关着的闸门钉死在 want，此时任何变化都只可能来自多起来的协程。
// 与"sleep 之后再断言"那种写法的分别：这里没有一个需要等的正条件（等一件不会发生的事等于白等），
// 窗口长度只决定漏检概率；每个用到它的用例另外还配了一条不看时间的终局判据（并发峰值）。
func assertHoldsAt(t *testing.T, c *poolCounters, want int, window time.Duration, what string) {
	t.Helper()

	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		plain, _ := c.active()
		if plain != want {
			t.Fatalf("%s：在跑数量应当保持在 %d，实际 %d", what, want, plain)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// assertStopNotReturnedYet 同上一段，判的是"Stop 还没返回"：闸门没开之前每一条在途任务都卡在
// 处理函数里，而 worker 的 wg.Done 在处理函数返回之后才执行，所以此刻返回就是漏算了协程。
func assertStopNotReturnedYet(t *testing.T, done <-chan struct{}, window time.Duration, what string) {
	t.Helper()

	select {
	case <-done:
		t.Fatalf("%s", what)
	case <-time.After(window):
	}
}

// TestResizeWorkers_UpRaisesConcurrency 是 §5.1：1 个 worker 时并发被钉在 1，
// 扩到 4 之后四条**同时**在跑——判据是在跑数量到了 4，不是读数。
func TestResizeWorkers_UpRaisesConcurrency(t *testing.T) {
	counters := &poolCounters{}
	gate := make(chan struct{})
	unblock := openGate(gate)

	scheduler := NewScheduler(nil, nil, nil)
	scheduler.SetConcurrency(1)
	scheduler.SetQueueCapacity(8)
	scheduler.RegisterHandler("resize_work", blockedHandler(counters, gate, false))
	scheduler.Start()
	defer scheduler.Stop()
	defer unblock()

	for i := 0; i < 4; i++ {
		require.NoError(t, scheduler.Schedule(resizeJob("up", i)))
	}
	// 只有 1 个 worker：第一条卡在闸门上，其余三条留在队列里
	waitActive(t, counters, 1, 0, 5*time.Second)
	plainBefore, _ := counters.active()
	require.Equal(t, 1, plainBefore, "扩容之前并发被 worker 数钉在 1")

	require.NoError(t, scheduler.ResizeWorkers(4))
	// 新起的 3 个 worker 各自领走队列里的一条：这条等的就是"四条真的同时在跑"
	waitActive(t, counters, 4, 0, 5*time.Second)
	assert.Equal(t, 4, scheduler.RuntimeStats().Workers, "读数与实跑同时到位")

	unblock()
	waitUntil(t, "扩容之后四条任务都要跑完", 5*time.Second, func() bool { return counters.done() >= 4 })
	assert.Equal(t, 4, counters.done(), "扩容不许把已经跑起来的任务丢掉")
}

// TestResizeWorkers_DownKeepsInFlightJobs 是 §5.2：4 个 worker 各卡住一条任务后缩到 1，
// 放开闸门四条全部跑完。断言 done()==4 而不是 >=1，这正是本卡与"缩容即在途中止"的分界。
func TestResizeWorkers_DownKeepsInFlightJobs(t *testing.T) {
	counters := &poolCounters{}
	gate := make(chan struct{})
	unblock := openGate(gate)

	scheduler := NewScheduler(nil, nil, nil)
	scheduler.SetConcurrency(4)
	// 队列容量故意不设置：留到后面一起判 Workers 与 QueueCapacity 是两个概念（§3.4）
	scheduler.RegisterHandler("resize_work", blockedHandler(counters, gate, false))
	scheduler.Start()
	defer scheduler.Stop()
	defer unblock()

	for i := 0; i < 4; i++ {
		require.NoError(t, scheduler.Schedule(resizeJob("down", i)))
	}
	waitActive(t, counters, 4, 0, 5*time.Second)

	require.NoError(t, scheduler.ResizeWorkers(1))
	// 读数即时变目标值、实跑仍占着 4 个名额：这条断言的是两者的区分
	stats := scheduler.RuntimeStats()
	assert.Equal(t, 1, stats.Workers)
	assert.Equal(t, 4, stats.QueueCapacity, "通道容量是启动期按当时的 worker 数建的，扩缩不改它")
	assert.Equal(t, 4, cap(scheduler.workCh), "读数与通道真实容量一致")
	plainInFlight, _ := counters.active()
	assert.Equal(t, 4, plainInFlight, "温和缩容不强杀在途任务")
	assert.EqualValues(t, 3, scheduler.retireRequests.Load(), "差额记成退场名额，而不是当场停掉协程")

	unblock()
	waitUntil(t, "缩容之前跑起来的四条都要收尾", 5*time.Second, func() bool { return counters.done() >= 4 })
	assert.Equal(t, 4, counters.done(), "缩容不许取消任何一条在跑的任务")

	// 三个名额由各跑完一条任务的协程消费；余额回到 0 才说明"退场"真的发生过
	waitUntil(t, "三个退场名额被消费完", 5*time.Second, func() bool {
		return scheduler.retireRequests.Load() == 0
	})
}

// TestResizeWorkers_DownConvergesToTarget 是 §5.3：缩到 1、等退场收敛、再投六条，
// 这一段普通池的并发峰值正好是 1。
//
// 判据落在退场收敛之后（waitActiveExactly 是它的门禁），不判"缩容瞬间峰值==1"：
// 退场窗口里在途任务还各占一个名额，那种写法必然 flake（卡片 §9 风险表最后一行）。
//
// 卡面 §5.3 的示例在四条任务都返回之后调 waitActiveExactly(counters, 1)——那时在跑数量停在 0
// 而不是 1，等待必然不到判据。这里让第二阶段的任务也卡在闸门上：活下来的那个 worker 领走一条后
// 停在 1，其余五条留在队列里没人领，"正好 1 且连续两轮不变"才是退场收敛的可观察证据。
func TestResizeWorkers_DownConvergesToTarget(t *testing.T) {
	first := &poolCounters{}
	gate1 := make(chan struct{})
	unblock1 := openGate(gate1)

	scheduler := NewScheduler(nil, nil, nil)
	scheduler.SetConcurrency(4)
	scheduler.SetQueueCapacity(8)
	scheduler.RegisterHandler("resize_work", blockedHandler(first, gate1, false))
	scheduler.Start()
	defer scheduler.Stop()
	defer unblock1()

	for i := 0; i < 4; i++ {
		require.NoError(t, scheduler.Schedule(resizeJob("conv", i)))
	}
	waitActive(t, first, 4, 0, 5*time.Second)
	require.NoError(t, scheduler.ResizeWorkers(1))

	// 换一份计数与另一道闸门：RegisterHandler 覆盖同名键是既有行为，所以不换调度器
	// 也能把第二阶段的计数清干净，第一阶段的在途任务仍记在 first 上。
	second := &poolCounters{}
	gate2 := make(chan struct{})
	unblock2 := openGate(gate2)
	defer unblock2()
	scheduler.RegisterHandler("resize_work", blockedHandler(second, gate2, false))

	for i := 4; i < 10; i++ {
		require.NoError(t, scheduler.Schedule(resizeJob("conv", i)))
	}

	unblock1()
	waitUntil(t, "第一阶段的四条全部跑完", 5*time.Second, func() bool { return first.done() >= 4 })
	assert.Equal(t, 4, first.done(), "缩容不该取消已经在跑的任务")

	// 退场收敛的门禁：第二阶段只有 1 条在跑，另外 5 条留在队列里等着
	waitActiveExactly(t, second, 1, 5*time.Second)

	unblock2()
	waitUntil(t, "第二阶段的六条全部跑完", 5*time.Second, func() bool { return second.done() >= 6 })
	plainPeak, _ := second.peaks()
	assert.Equal(t, 1, plainPeak, "退场收敛之后的并发峰值必须正好是目标值 1")
}

// TestResizeWorkers_RetireCheckPrecedesNextPickup 钉住退场判定的位置：它在 worker 去领
// 下一条任务**之前**，而不是执行完手上那条之后。
//
// 摆法：2 个 worker 全卡在闸门里 → 缩到 1（余额 1 没人在循环开头消费）→ 再排一条任务
// （通道容量 8，它留在那里没人领）→ 扩回 2（新协程一起来就在循环开头撞上这个余额）。
// 真实现里新协程直接退场：闸门还关着的时候余额就归零，在跑数量与并发峰值都停在 2。
// 判定挪到"执行之后"的那种写法里，新协程会先把队列里那条领走再退——峰值变 3、
// 余额在闸门开着的那段时间里始终是 1。
//
// 这条是 §10.3 的 M8 从"等价变异"变成真判据的原因；终局判据是不看时间的并发峰值，
// 中间那段 300ms 的观察窗口只负责让"余额先归零"这件事有个先后。
func TestResizeWorkers_RetireCheckPrecedesNextPickup(t *testing.T) {
	counters := &poolCounters{}
	gate := make(chan struct{})
	unblock := openGate(gate)

	scheduler := NewScheduler(nil, nil, nil)
	scheduler.SetConcurrency(2)
	scheduler.SetQueueCapacity(8)
	scheduler.RegisterHandler("resize_work", blockedHandler(counters, gate, false))
	scheduler.Start()
	defer scheduler.Stop()
	// 闸门那条 defer 注册在 Stop 之后：defer 是后进先出，失败路径上先开闸门再关停，
	// 不会留一个卡在处理函数里的 worker 把 Stop 挂到包级超时。
	defer unblock()

	for i := 0; i < 2; i++ {
		require.NoError(t, scheduler.Schedule(resizeJob("pickup", i)))
	}
	waitActive(t, counters, 2, 0, 5*time.Second)

	require.NoError(t, scheduler.ResizeWorkers(1))
	assert.EqualValues(t, 1, scheduler.retireRequests.Load(),
		"两个 worker 都卡在处理函数里，这个名额此刻不该被消费掉")

	require.NoError(t, scheduler.Schedule(resizeJob("pickup", 2)))
	require.NoError(t, scheduler.ResizeWorkers(2))

	// 新起的协程在循环开头就把余额吃掉了：这一步不依赖任何时长，只等这个单调事件。
	waitUntil(t, "扩进来的那个协程在领任务之前先退场，余额归零", 5*time.Second,
		func() bool { return scheduler.retireRequests.Load() == 0 })
	assertHoldsAt(t, counters, 2, 300*time.Millisecond,
		"扩进来的协程在退场前先把队列里那条领走了：退场判定不该放在执行之后")

	unblock()
	waitUntil(t, "三条任务全部跑完", 5*time.Second, func() bool { return counters.done() >= 3 })
	assert.Equal(t, 3, counters.done())
	plainPeak, _ := counters.peaks()
	assert.Equal(t, 2, plainPeak, "退场判定在前时，并发峰值不会超过缩容期间活着的名额 2")
}

// TestResizeWorkers_BlockedDispatchStillLands 是 §5.4：容量 1、worker 2，
// 两条任务都卡在闸门上时第三条占满通道、第四条让调度循环阻塞在 s.workCh <- job（见 dispatch）；
// 此时扩到 4，被阻塞的那条最终必须跑完。这条守住卡面 §2 的结论：
// 不重建通道，扩缩不会让正在阻塞的发送方落空。
func TestResizeWorkers_BlockedDispatchStillLands(t *testing.T) {
	counters := &poolCounters{}
	gate := make(chan struct{})
	unblock := openGate(gate)

	scheduler := NewScheduler(nil, nil, nil)
	scheduler.SetConcurrency(2)
	scheduler.SetQueueCapacity(1)
	scheduler.RegisterHandler("resize_work", blockedHandler(counters, gate, false))
	scheduler.Start()
	defer scheduler.Stop()
	defer unblock()

	queueBefore := scheduler.workCh
	for i := 0; i < 4; i++ {
		require.NoError(t, scheduler.Schedule(resizeJob("blocked", i)))
	}
	waitActive(t, counters, 2, 0, 5*time.Second)
	// 四条任务的去向要凑得齐：2 条在闸门上、1 条占满容量 1 的通道，剩下的第 4 条只能在
	// 调度循环那次阻塞的发送里——堆空了说明它已被弹出、正卡在 s.workCh <- job 上。
	waitUntil(t, "第四条任务压在阻塞的发送上", 5*time.Second, func() bool {
		plain, _ := counters.active()
		return plain == 2 && len(scheduler.workCh) == 1 && scheduler.HeapLen() == 0
	})

	require.NoError(t, scheduler.ResizeWorkers(4))
	assert.True(t, queueBefore == scheduler.workCh, "扩缩不得重建通道：那次阻塞的发送挂在旧通道上")

	unblock()
	// 判据是"四条都跑完"，不是 QueueLength 读数：那是瞬时值（RuntimeStats 的注释已说明）
	waitUntil(t, "阻塞在发送上的那条也落了地", 10*time.Second, func() bool { return counters.done() >= 4 })
	assert.Equal(t, 4, counters.done())
}

// TestResizeWorkers_RejectsNonPositiveTarget 是 §5.5 第一条：n<=0 报错且什么都不改。
// 重点判"没有偷偷回退到 DefaultConcurrency"——那条方向与 SetConcurrency 相反，
// 回退会把一条写错的配置一路跑下去（卡片 §3.3 的表）。
func TestResizeWorkers_RejectsNonPositiveTarget(t *testing.T) {
	idle := NewScheduler(nil, nil, nil)
	idle.SetConcurrency(4)
	require.ErrorContains(t, idle.ResizeWorkers(0), "workers must be positive, got 0")
	assert.Equal(t, 4, idle.concurrency, "未启动时也不能把 0 补成默认值")
	assert.Equal(t, 4, idle.RuntimeStats().Workers)

	counters := &poolCounters{}
	gate := make(chan struct{})
	unblock := openGate(gate)

	scheduler := NewScheduler(nil, nil, nil)
	scheduler.SetConcurrency(2)
	scheduler.SetQueueCapacity(8)
	scheduler.RegisterHandler("resize_work", blockedHandler(counters, gate, false))
	scheduler.Start()
	defer scheduler.Stop()
	defer unblock()

	for i := 0; i < 3; i++ {
		require.NoError(t, scheduler.Schedule(resizeJob("reject", i)))
	}
	waitActive(t, counters, 2, 0, 5*time.Second)

	for _, bad := range []int{0, -3} {
		require.ErrorContains(t, scheduler.ResizeWorkers(bad), "workers must be positive, got")
		assert.Equal(t, 2, scheduler.RuntimeStats().Workers, "报错之后读数不变")
		assert.Equal(t, 2, scheduler.concurrency, "报错之后启动期那份快照也不变")
		assert.EqualValues(t, 0, scheduler.retireRequests.Load(), "报错之后不留退场名额")
	}

	// 行为半边：两次非法入参之后，实跑并发仍是 2、三条任务一条不少地跑完
	plain, _ := counters.active()
	assert.Equal(t, 2, plain)
	unblock()
	waitUntil(t, "非法入参之后任务照常跑完", 5*time.Second, func() bool { return counters.done() >= 3 })
	assert.Equal(t, 3, counters.done())
}

// TestResizeWorkers_BeforeStartTakesEffectAtStart 是 §5.5 第二条：未 Start 时
// ResizeWorkers 等价于 SetConcurrency——只写下取值、不起协程，Start 按新值建池。
func TestResizeWorkers_BeforeStartTakesEffectAtStart(t *testing.T) {
	counters := &poolCounters{}
	gate := make(chan struct{})
	unblock := openGate(gate)

	scheduler := NewScheduler(nil, nil, nil)
	scheduler.SetConcurrency(1)
	scheduler.SetQueueCapacity(8)
	scheduler.RegisterHandler("resize_work", blockedHandler(counters, gate, false))

	require.NoError(t, scheduler.ResizeWorkers(6))
	assert.Equal(t, 6, scheduler.RuntimeStats().Workers, "未启动时读数就已按新目标走")
	assert.Equal(t, 6, scheduler.concurrency, "下一次 Start 建通道与协程取的就是这个值")
	assert.EqualValues(t, 0, scheduler.retireRequests.Load(), "未启动时不起协程也不记名额")

	scheduler.Start()
	defer scheduler.Stop()
	defer unblock()

	for i := 0; i < 6; i++ {
		require.NoError(t, scheduler.Schedule(resizeJob("boot", i)))
	}
	// 闸门法：六条同时在跑才解释得了"Start 起了 6 个 worker"
	waitActive(t, counters, 6, 0, 5*time.Second)
	unblock()
	waitUntil(t, "六条全部跑完", 5*time.Second, func() bool { return counters.done() >= 6 })
	assert.Equal(t, 6, counters.done())
	plainPeak, _ := counters.peaks()
	assert.Equal(t, 6, plainPeak, "峰值恰好 6：既没少起也没多起")
}

// TestResizeWorkers_SameValueIsNoOp 是 §5.5 第四条：n == 现值时返回 nil 且不新建协程。
//
// 卡面建议用 runtime.NumGoroutine() 差值 ±2，本仓不采用：core/scheduler_exec_class_test.go 的
// TestExecClass_DisabledCreatesNoPool 已经把理由写在那儿了——同包其它用例会在途留下协程，
// 进程级计数在本机 `go test ./core -race -count=5` 下会 ±1 抖动（实测翻红过"应当多出 3 个，实际多出 2 个"），
// 那种差值断言立不住。这里换成调度器自己的判据：3 个 worker 全卡在闸门上、队列里还压着三条，
// ResizeWorkers(3) 只要多起了协程，被压住的任务立刻有人领走，在跑数量与并发峰值都会越过 3。
func TestResizeWorkers_SameValueIsNoOp(t *testing.T) {
	counters := &poolCounters{}
	gate := make(chan struct{})
	unblock := openGate(gate)

	scheduler := NewScheduler(nil, nil, nil)
	scheduler.SetConcurrency(3)
	scheduler.SetQueueCapacity(8)
	scheduler.RegisterHandler("resize_work", blockedHandler(counters, gate, false))
	scheduler.Start()
	defer scheduler.Stop()
	defer unblock()

	for i := 0; i < 6; i++ {
		require.NoError(t, scheduler.Schedule(resizeJob("same", i)))
	}
	waitActive(t, counters, 3, 0, 5*time.Second)

	require.NoError(t, scheduler.ResizeWorkers(3))
	assert.Equal(t, 3, scheduler.RuntimeStats().Workers)
	assert.EqualValues(t, 0, scheduler.retireRequests.Load(), "幂等调用不留退场名额")
	assertHoldsAt(t, counters, 3, 300*time.Millisecond, "队列里还压着三条，多起一个协程就会立刻有人领走")

	unblock()
	waitUntil(t, "六条全部跑完", 5*time.Second, func() bool { return counters.done() >= 6 })
	plainPeak, _ := counters.peaks()
	assert.Equal(t, 3, plainPeak, "并发峰值不得超过现值：一次幂等调用不该带来额外协程")
}

// TestResizeWorkers_RestartClearsRetireRequests 是 §5.5 第三条：Start→Stop→Start 不留残留名额。
//
// 两段各有分工。第一段走卡面给的真实路径（缩容→Stop→Start）。这里余额会在关停中耗光，
// 是因为四个 worker 当时全卡在闸门里、各自跑完手上那条才回到循环开头——这不是通用性质：
// 闲着停在 select 上的 worker 走的是 stopCh 那一个分支，根本不经过消费点，
// 所以"带着余额关停"在一般情形下是可能发生的，真正兜住跨代残留的是第二段的 Start 清零。
// 第二段把余额直接写成 3，钉的就是 Start 里那行清零——真实路径到不了"带着余额重启"的状态，
// 只有把余额摆出来才判得动那一行；删掉清零，第二段重新 Start 起的 4 个 worker 会被退掉 3 个，
// waitActive(4) 就是它的判据。
func TestResizeWorkers_RestartClearsRetireRequests(t *testing.T) {
	counters := &poolCounters{}
	gate1 := make(chan struct{})
	unblock1 := openGate(gate1)

	scheduler := NewScheduler(nil, nil, nil)
	scheduler.SetConcurrency(4)
	scheduler.SetQueueCapacity(8)
	scheduler.RegisterHandler("resize_work", blockedHandler(counters, gate1, false))
	scheduler.Start()
	// 这一段的 Stop 注册在 unblock1 之前（defer 后进先出，闸门仍先开），
	// 中间那次 Stop 是显式调的，这里的 defer 只兜失败路径：第一段就 Fatalf 时也不留一个在跑的调度器
	defer scheduler.Stop()
	defer unblock1()

	for i := 0; i < 4; i++ {
		require.NoError(t, scheduler.Schedule(resizeJob("restart", i)))
	}
	waitActive(t, counters, 4, 0, 5*time.Second)
	require.NoError(t, scheduler.ResizeWorkers(1))
	assert.EqualValues(t, 3, scheduler.retireRequests.Load())

	unblock1()
	waitUntil(t, "关停之前在途任务全部收尾", 5*time.Second, func() bool { return counters.done() >= 4 })
	scheduler.Stop()
	assert.EqualValues(t, 0, scheduler.retireRequests.Load(),
		"本用例四个 worker 都在跑任务，跑完回到循环开头时把余额耗光（通用情形下不成立，见函数头）")

	gate2 := make(chan struct{})
	unblock2 := openGate(gate2)
	scheduler.RegisterHandler("resize_work", blockedHandler(counters, gate2, false))

	scheduler.SetConcurrency(4)
	scheduler.retireRequests.Store(3)
	scheduler.Start()
	defer scheduler.Stop()
	defer unblock2()

	assert.EqualValues(t, 0, scheduler.retireRequests.Load(), "Start 与复位 stopCh 一起把退场余额清零")
	assert.Equal(t, 4, scheduler.RuntimeStats().Workers)

	for i := 4; i < 8; i++ {
		require.NoError(t, scheduler.Schedule(resizeJob("restart", i)))
	}
	// 残留名额若没被清掉，这里最多只有 1 条能同时在跑
	waitActive(t, counters, 4, 0, 5*time.Second)
	unblock2()
	waitUntil(t, "重启之后的四条也都跑完", 5*time.Second, func() bool { return counters.done() >= 8 })
}

// TestResizeWorkers_StopWaitsForResizedWorkers 是卡片 §9 风险表第一行的正面证据：
// 扩容起来的 worker 必须算进 s.wg，少算一个 Stop 就会提前返回。
//
// 判据用"闸门还关着时 Stop 不得返回"：四条任务都卡在 blockedHandler 里，而 worker 的 wg.Done
// 在处理函数返回之后才执行，所以 Stop 一旦返回就说明有协程没进账。
// 这条比 §5.6 的压力用例更硬——它不看运气，只看记账。
func TestResizeWorkers_StopWaitsForResizedWorkers(t *testing.T) {
	counters := &poolCounters{}
	gate := make(chan struct{})
	unblock := openGate(gate)
	defer unblock()

	scheduler := NewScheduler(nil, nil, nil)
	scheduler.SetConcurrency(1)
	scheduler.SetQueueCapacity(8)
	scheduler.RegisterHandler("resize_work", blockedHandler(counters, gate, false))
	scheduler.Start()

	require.NoError(t, scheduler.ResizeWorkers(4))
	for i := 0; i < 4; i++ {
		require.NoError(t, scheduler.Schedule(resizeJob("stopwait", i)))
	}
	waitActive(t, counters, 4, 0, 5*time.Second)
	assert.Equal(t, 4, scheduler.RunningCount(), "四条都在 Handler 里，正是要被 wg 等到的那些")

	stopped := make(chan struct{})
	go func() {
		scheduler.Stop()
		close(stopped)
	}()
	assertStopNotReturnedYet(t, stopped, 300*time.Millisecond,
		"四条任务都卡在闸门上，Stop 却返回了：扩容起的协程没算进 wg")

	unblock()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("闸门已开、四条任务都已收尾，Stop 仍没返回：wg 记多了或者协程没退场")
	}
	assert.Equal(t, 4, counters.done())
	assert.False(t, scheduler.RuntimeStats().Started, "Stop 返回之后调度器不该还报在跑")
}

// TestResizeWorkers_ConcurrentResizeScheduleAndStats 是 §5.6 的并发压力：
// 一条协程在 1..8 之间来回扩缩 50 轮、一条持续投任务、一条读 RuntimeStats。
// 闸门一开始就是开的（处理函数即领即返），三条协程才真正咬在一起。
//
// 这条判三件事：扩缩期间任务照旧流动（done 到得了半数）、读数永远自洽
// （Workers 落在 1..8，QueueCapacity 一动不动——通道不因扩缩而换）、
// 最后 Stop 必然返回（有协程没退场就卡在这条上）。-race 干净本身也是判据之一。
// "新起的协程都算进 wg"那半边由上一条用例负责：这里判的是收敛，不是记账。
func TestResizeWorkers_ConcurrentResizeScheduleAndStats(t *testing.T) {
	const (
		total        = 150
		lowWorkers   = 1
		highWorkers  = 8
		resizeRounds = 50
	)

	counters := &poolCounters{}
	gate := make(chan struct{})
	unblock := openGate(gate)
	unblock()

	scheduler := NewScheduler(nil, nil, nil, quietLogger())
	scheduler.SetConcurrency(lowWorkers)
	scheduler.SetQueueCapacity(4)
	scheduler.RegisterHandler("resize_work", blockedHandler(counters, gate, false))
	scheduler.Start()
	defer scheduler.Stop()
	defer unblock()

	stopReaders := make(chan struct{})
	var movers sync.WaitGroup
	movers.Add(3)

	go func() {
		defer movers.Done()
		for round := 0; round < resizeRounds; round++ {
			n := lowWorkers + (round % highWorkers)
			if err := scheduler.ResizeWorkers(n); err != nil {
				t.Errorf("ResizeWorkers(%d) 失败: %v", n, err)
				return
			}
			// 每轮之间让出 1ms：50 轮扩缩才摊得开在任务流动的那段时间里，
			// 而不是在开跑的最初几微秒里全部撞完（那等于只压到了锁）
			time.Sleep(time.Millisecond)
		}
	}()

	go func() {
		defer movers.Done()
		for i := 0; i < total; i++ {
			if err := scheduler.Schedule(resizeJob("stress", i)); err != nil {
				t.Errorf("第 %d 条任务投递失败: %v", i, err)
				return
			}
		}
	}()

	go func() {
		defer movers.Done()
		for {
			select {
			case <-stopReaders:
				return
			default:
			}
			stats := scheduler.RuntimeStats()
			if stats.Workers < lowWorkers || stats.Workers > highWorkers {
				t.Errorf("Workers 读数跑出扩缩区间: %d", stats.Workers)
				return
			}
			// 队列容量是启动期建出来的通道容量，扩缩 worker 不碰它（卡片 §2）
			if stats.QueueCapacity != 4 {
				t.Errorf("QueueCapacity 读数被扩缩带跑了: %d, want 4", stats.QueueCapacity)
				return
			}
			// 1ms 的节流只是别让读数协程把两条池的锁占满，不参与任何判据
			time.Sleep(time.Millisecond)
		}
	}()

	waitUntil(t, "扩缩与投递同时进行时任务照常流动", 20*time.Second, func() bool {
		return counters.done() >= total/2
	})
	close(stopReaders)
	movers.Wait()

	stopped := make(chan struct{})
	go func() {
		scheduler.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop 没有返回：扩缩起来的协程里有谁没退场")
	}

	plain, exec := counters.active()
	assert.Equal(t, 0, plain, "Stop 返回时普通池不该还有在跑的任务")
	assert.Equal(t, 0, exec, "本用例不碰执行器池")
	assert.GreaterOrEqual(t, counters.done(), total/2, "这一轮压力真的跑掉了任务")
}

// TestResizeWorkers_ExecutorPoolNeverRetires 判的是退场判定的边界：只有默认池退场。
// 执行器池的 worker 数绑在 Start 建出的 execCh 上（卡片 §8 与设计文档 §10 的 N1），
// ResizeWorkers 不该动它一根毫毛。
//
// 顺序是有讲究的：先把默认池的两条任务卡在闸门上（没有默认池协程回到循环开头），
// 再让执行器池痛快地跑上八条。只有这期间执行器协程反复经过循环开头，
// "退场判定漏到 exec 那一侧"的错误实现才有机会把名额吃掉；真实现里名额一直挂着，
// 最后由默认池自己消费。反过来说，这条用例判不出"两个池抢同一个名额"要等到什么时候——
// 它判的是名额全程没被执行器动过，以及执行器池全程还是两条并发。
func TestResizeWorkers_ExecutorPoolNeverRetires(t *testing.T) {
	plain := &poolCounters{}
	plainGate := make(chan struct{})
	unblockPlain := openGate(plainGate)

	flowGate := make(chan struct{})
	close(flowGate) // 开着的闸门：执行器任务领了即返，好让协程一趟趟回到循环开头
	flow := &poolCounters{}

	scheduler := NewScheduler(nil, nil, nil)
	scheduler.SetConcurrency(2)
	scheduler.SetQueueCapacity(8)
	scheduler.SetExecConcurrency(2)
	scheduler.SetExecQueueCapacity(8)
	scheduler.RegisterHandler("resize_work", blockedHandler(plain, plainGate, false))
	scheduler.RegisterHandlerClass("resize_exec", blockedHandler(flow, flowGate, true), JobClassExec)
	scheduler.Start()
	defer scheduler.Stop()
	defer unblockPlain()

	for i := 0; i < 2; i++ {
		require.NoError(t, scheduler.Schedule(&Job{
			ID:        "plain-" + strconv.Itoa(i),
			Name:      "resize_work",
			TriggerAt: time.Now(),
		}))
	}
	waitActive(t, plain, 2, 0, 5*time.Second)

	require.NoError(t, scheduler.ResizeWorkers(1))
	assert.EqualValues(t, 1, scheduler.retireRequests.Load(), "默认池的差额记成一个退场名额")

	for i := 0; i < 8; i++ {
		require.NoError(t, scheduler.Schedule(&Job{
			ID:        "exec-flow-" + strconv.Itoa(i),
			Name:      "resize_exec",
			TriggerAt: time.Now(),
		}))
	}
	waitUntil(t, "执行器池的八条任务全部跑完", 5*time.Second, func() bool { return flow.done() >= 8 })
	assert.EqualValues(t, 1, scheduler.retireRequests.Load(), "执行器协程经过循环开头也不许碰这个名额")
	flowPlain, _ := flow.active()
	assert.Equal(t, 0, flowPlain, "执行器任务不该出现在默认池的计数栏里")

	// 名额还挂着，执行器池却能同时跑两条：两个协程一个都没少
	gated := &poolCounters{}
	execGate := make(chan struct{})
	unblockExec := openGate(execGate)
	defer unblockExec()
	scheduler.RegisterHandlerClass("resize_exec", blockedHandler(gated, execGate, true), JobClassExec)
	for i := 0; i < 2; i++ {
		require.NoError(t, scheduler.Schedule(&Job{
			ID:        "exec-hold-" + strconv.Itoa(i),
			Name:      "resize_exec",
			TriggerAt: time.Now(),
		}))
	}
	waitActive(t, gated, 0, 2, 5*time.Second)
	unblockExec()
	waitUntil(t, "两条被卡住的执行器任务收尾", 5*time.Second, func() bool { return gated.done() >= 2 })
	_, execPeak := gated.peaks()
	assert.Equal(t, 2, execPeak, "执行器池的并发一格都没少")

	// 名额最终由默认池消费：放开默认池的闸门，两条在跑的任务各退一个...留一个
	unblockPlain()
	waitUntil(t, "名额被默认池消费完", 5*time.Second, func() bool {
		return scheduler.retireRequests.Load() == 0 && plain.done() >= 2
	})
	assert.Equal(t, 1, scheduler.RuntimeStats().Workers)
}
