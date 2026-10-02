# TASK-R03　调度器的运行期 worker 扩缩

- 所属阶段：M1 落点
- 依赖任务：R01（分类表把 `scheduler.workers` 钉在热更档）；本卡的实现不依赖 R02
- 涉及文件：`core/scheduler.go`、`core/scheduler_resize_test.go`（新增）、
  `docs/design/config-reload-design.md`（若落地与卡面有偏离，在 R07 统一标注）
- 预计规模：中偏大（本系列唯一改动执行模型的一张）

## 1. 任务目标

新增 `Scheduler.ResizeWorkers(n)`：在不重启、不中断在途任务的前提下，把普通执行池的
worker 数量调到 `n`。扩容立刻起新协程；缩容只写下目标值，多出来的 worker 在**跑完手上那条任务、
回到循环开头时**自己退场（设计文档 R6 的温和缩容）。

本卡结束时 `ResizeWorkers` 还没有调用方（接线在 R06），但"改了并发数真的改变同时执行的任务数"
这件事必须能被单元测试证明。

## 2. 背景与当前问题

`scheduler.workers` 现在只有 `SetConcurrency`（`core/scheduler.go:153-167`）这一个入口，
而它承诺"Start 之后调用不生效并记录日志"。worker 协程与队列通道都在 `Start` 里一次建好
（`core/scheduler.go:1141-1146`、`:1168-1175`），`Stop` 靠 `wg` 等它们全部退出
（`core/scheduler.go:1206-1228`）。所以运行期调并发数缺三样：

1. 一个"此刻想要几个 worker"的目标值（现在只有构造期的 `s.concurrency`）。
2. 一条不起新协程就改不了、起了就退不掉的通路。
3. 一个不会被扩缩打断的在途任务语义。

**为什么队列容量不在本卡**：默认池的投递方（调度循环）阻塞在 `s.workCh <- job` 上
（`core/scheduler.go:1327-1332`），换通道会让正在阻塞的那次发送永远等不到结果——
发送的对象还挂在旧通道上。worker 数与通道容量是解耦的（容量只决定"排队多少个"，
并发由协程数决定），所以扩 worker 不碰通道就能生效。这条判断写进注释，
因为它就是设计文档 R5 的全部内容。

## 3. 要实现的功能

### 3.1 新字段

```go
// core/scheduler.go 的 Scheduler 结构体里，紧挨 concurrency/queueCapacity/workCh 那一段加：

// targetWorkers 是"此刻想要几个 worker"，供 RuntimeStats 读数与扩缩判定。
// 用原子而不是 s.mu：执行协程在循环开头读它（决定自己该不该退场），
// 而那个位置不能拿写锁——executeJob 之后回到循环开头时可能正持有其它协程在等的资源。
targetWorkers atomic.Int32

// retireRequests 是"还有几个 worker 该退场"的计数。缩容只加这个计数，
// 不主动通知任何协程：正在跑任务的 worker 跑完、回到循环开头时看到计数>0 就自己减一并 return。
// 这条形状（而不是给每个 worker 一个身份序号）是有意的：
// 序号方案在 Start→Stop→Start 之后要重新对齐编号（Start 会重建 stopCh 与 workCh，
// 老协程全退、新协程从 0 开始），而计数方案只需 Start 清零一次。
retireRequests atomic.Int32
```

### 3.2 worker 循环

```go
// worker 的循环开头加退场判定（queue 与 exec 两个参数保持原样，本卡只动默认池的用法）：
func (s *Scheduler) worker(queue chan *Job, exec bool) {
        defer s.wg.Done()
        for {
                // 缩容的退场点：只在默认池生效。执行器池的 worker 数与"池存不存在"绑在
                // Start 的通道构造上（execCh 为 nil 即没有池），运行期扩缩另议，
                // 见 docs/design/config-reload-design.md §10 的 N1。
                // exec=false 时若 retireRequests 有余额就减一并退出；用 CAS 循环保证
                // 两个同时退场的协程不会把计数减成负数。
                if !exec && s.shouldRetire() {
                        return
                }
                select {
                case <-s.stopCh:
                        return
                case job := <-queue:
                        ...原样...
                }
        }
}

// shouldRetire 消费一个退场名额；返回 true 表示当前这个 worker 该退出了。
// 余额为 0 时不做任何修改。计数只在缩容时增加、在退场与 Start 时清零，
// 因此不会出现"名额留到下一代 worker"的问题（Start 会清）。
func (s *Scheduler) shouldRetire() bool
```

`Start`（`core/scheduler.go:1128-1176`）改三处：

- `s.targetWorkers.Store(int32(workers))`；
- `s.retireRequests.Store(0)`（与既有的"复位停止信号"同一理由：跨重启不留残留名额）；
- 建 `workCh` 的容量口径不动（仍由 `queueCapacity` 回退到 `workers`）。

### 3.3 扩缩入口

```go
// ResizeWorkers 在运行期把普通池的 worker 数调整到 n（n>=1）。
//
// 与 SetConcurrency 的分工：SetConcurrency 只在 Start 之前有效、运行期 warn 并忽略（既有口径原样保留），
// ResizeWorkers 专给配置重载链用、运行期生效。两者都改 targetWorkers，
// 但只有 ResizeWorkers 会真的起协程。
//
// 三条语义：
//   - 扩容：立刻为差额起 goroutine，新协程与启动期起的完全同构（同一个 worker 方法、
//     同一个 s.wg），因此 Stop 会等它们，一个都不能少算。
//   - 缩容：只写下目标值并记退场名额，不强杀、不取消在途任务；多出来的 worker
//     跑完手上那条任务、回到循环开头时退场。若之后一直没有任务流动，
//     读数（RuntimeStats.Workers）已经变小而实跑协程可能还多几个——这是温和缩容的代价，
//     不是缺陷，注释里要写清。
//   - 队列容量不变：通道换不得（见 §2）。
//
// 调度器未启动时调用等价于 SetConcurrency(n)：只写下取值，等 Start 起协程。
func (s *Scheduler) ResizeWorkers(n int) error
```

失败与边界（都要有用例）：

| 入参 | 行为 |
| --- | --- |
| `n <= 0` | 返回错误 `workers must be positive, got N`，**不改任何状态**（不回退到 `DefaultConcurrency`：重载链传进 0 说明配置写错，静默补默认会把错配置跑下去；这条与 `SetConcurrency` 的"非正数回退默认"方向相反，理由写进注释） |
| 调度器没在跑 | 只写 `s.concurrency` 与 `targetWorkers`，不起协程，返回 nil |
| `n == 现值` | 不做任何事，返回 nil（重载链靠 `Diff` 保证只在真变了时调用，但幂等仍要有） |
| 扩容 | 持 `s.mu` 读 `running` 与 target，**起协程放在放锁之后**（`go` 语句本身不阻塞，但把 `wg.Add` 放在锁内、`go` 放在锁外，避免持锁期间任何意外） |
| 与 `Stop` 并发 | 见下方"并发安全"段 |

并发安全（这段推理必须进注释，因为它是本卡唯一不可见的设计）：
`ResizeWorkers` 全程在 `s.mu` 内判定 `s.running`，而 `Stop` 在**同一把锁内**先置
`running=false` 再 `close(stopCh)`（`core/scheduler.go:1207-1221`）。于是只有两种交错：

- `ResizeWorkers` 先拿到锁：它起了新协程并计入 `wg`；这些协程会立刻看到已关闭的 `stopCh` 并退出，
  `Stop` 的 `wg.Wait()` 仍然收敛。
- `Stop` 先释放锁：`ResizeWorkers` 看到 `running==false`，一个协程都不起。

不会出现"`wg.Add` 落在 `Wait` 已经返回之后"的那种违反，因为计数器归零必然发生在
`Stop` 释放锁之后，而那时 `running` 已经是 false。

### 3.4 读数口径

`RuntimeStats`（`core/scheduler.go:1775-1803`）：

```go
Workers: int(s.targetWorkers.Load()),   // 期望并发，随 ResizeWorkers 立刻变
// QueueCapacity 的算法保持原样：容量<=0 时回退到 s.concurrency（启动期那个值）——
// 通道是那时建出来的，读数必须与通道的真实容量一致。
```

给 `Workers` 与 `QueueCapacity` 两行加注释说明"一个是期望并发、一个是启动期定下的通道容量，
扩缩之后两者可以不相等"，否则这两个数字会被读成同一个概念。
`/api/v1/pools` 与 `/api/v1/admin/runtime` 都吃这份 stats，本卡不改 `api`。

## 4. 实现步骤

1. `core/scheduler.go`：加两个原子字段 → 改 `worker`（退场判定 + `shouldRetire`）→
   改 `Start`（Store target、清 retire）→ 加 `ResizeWorkers` → 改 `RuntimeStats` 的 `Workers`。
2. 新增 `core/scheduler_resize_test.go`：§5 的用例。计数辅助函数复用
   `core/scheduler_exec_class_test.go:16-101` 的 `poolCounters`、`blockedHandler`、`waitActive`、
   `openGate`（同包可直接用；不要在两个文件里各写一份闸门计数）。
3. 跑 §7 的命令；再跑 `go test ./core -race -count=5 -timeout 30m`。
4. 全量 `go build ./... && go vet ./... && go test ./... -race -count=1`。

## 5. 测试要求

新文件 `core/scheduler_resize_test.go` 只用**同包既有**的计数设施：`poolCounters`
（`core/scheduler_exec_class_test.go:16-43`，有 `active()`/`peaks()`/`done()` 三个读法）、
`blockedHandler`（`:45-76`，闸门开着才返回并 `finished++`）、`waitActive`（`:79-94`，
等的是条件不是时长）与 `openGate`（`:97-101`，幂等开闸门）。本卡另加两个本地 helper：

```go
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

// resizeJob 造一条即时任务，Name 落在已注册的处理函数键上
// （构造形状照 core/scheduler_exec_class_test.go:117-123：只给 ID、Name、TriggerAt）。
func resizeJob(prefix string, i int) *Job {
        return &Job{ID: prefix + "-" + strconv.Itoa(i), Name: "resize_work", TriggerAt: time.Now()}
}
```

三条口径：判据一律"等条件"而不是 sleep（§9 风险表第五行）；每个用例都 `defer unblock()`，
闸门不会漏开把用例挂死；调度器一律 `NewScheduler(nil, nil, nil)`（与既有用例一致，无存储也能跑）。

### 5.1 扩容真的提高并发

```go
// TestResizeWorkers_UpRaisesConcurrency：1 个 worker 时并发被钉在 1，扩到 4 后四条同时在跑。
func TestResizeWorkers_UpRaisesConcurrency(t *testing.T) {
        counters := &poolCounters{}
        gate := make(chan struct{})
        unblock := openGate(gate)
        defer unblock()

        scheduler := NewScheduler(nil, nil, nil)
        scheduler.SetConcurrency(1)
        scheduler.SetQueueCapacity(8)
        scheduler.RegisterHandler("resize_work", blockedHandler(counters, gate, false))
        scheduler.Start()
        defer scheduler.Stop()

        for i := 0; i < 4; i++ {
                require.NoError(t, scheduler.Schedule(resizeJob("up", i)))
        }
        // 只有 1 个 worker：第一条卡在闸门上，其余三条留在队列里
        waitActive(t, counters, 1, 0, 5*time.Second)
        if plain, _ := counters.active(); plain != 1 {
                t.Fatalf("plain active before resize = %d, want exactly 1", plain)
        }

        require.NoError(t, scheduler.ResizeWorkers(4))
        // 新起的 3 个 worker 各自领走队列里的一条
        waitActive(t, counters, 4, 0, 5*time.Second)

        unblock()
        deadline := time.Now().Add(5 * time.Second)
        for time.Now().Before(deadline) && counters.done() < 4 {
                time.Sleep(10 * time.Millisecond)
        }
        assert.Equal(t, 4, counters.done(), "扩容之后四条任务都要跑完")
}
```

### 5.2 缩容不打断在途任务

```go
// TestResizeWorkers_DownKeepsInFlightJobs：4 个 worker 各卡住一条任务，缩到 1，
// 放开闸门后四条全部跑完且都判成功——一条都不被取消。
func TestResizeWorkers_DownKeepsInFlightJobs(t *testing.T) {
        counters := &poolCounters{}
        gate := make(chan struct{})
        unblock := openGate(gate)
        defer unblock()

        scheduler := NewScheduler(nil, nil, nil)
        scheduler.SetConcurrency(4)
        scheduler.SetQueueCapacity(8)
        scheduler.RegisterHandler("resize_work", blockedHandler(counters, gate, false))
        scheduler.Start()
        defer scheduler.Stop()

        for i := 0; i < 4; i++ {
                require.NoError(t, scheduler.Schedule(resizeJob("down", i)))
        }
        waitActive(t, counters, 4, 0, 5*time.Second)

        require.NoError(t, scheduler.ResizeWorkers(1))
        // 读数即时变目标值，实跑仍占着 4 个名额——这条断言的是两者的区分（§3.4）
        assert.Equal(t, 1, scheduler.RuntimeStats().Workers)

        unblock()
        deadline := time.Now().Add(5 * time.Second)
        for time.Now().Before(deadline) && counters.done() < 4 {
                time.Sleep(10 * time.Millisecond)
        }
        assert.Equal(t, 4, counters.done(), "缩容不许取消任何一条在跑的任务")
}
```

断言 `done()==4` 而不是 `>=1`：这正是本卡与"缩容即在途中止"那种方案的分界。

### 5.3 退场完成后不再补新并发

```go
// TestResizeWorkers_DownConvergesToTarget：缩到 1、等退场收敛、再投六条不卡的任务，
// 断言这一段的并发峰值正好是 1。
func TestResizeWorkers_DownConvergesToTarget(t *testing.T) {
        // 前半同 §5.2：4 个 worker、4 条卡住的任务、ResizeWorkers(1)、unblock()、等 done()==4
        waitActiveExactly(t, counters, 1, 5*time.Second) // 三个退场名额已被消费完

        // 换一份计数与一个不卡的处理函数：RegisterHandler 覆盖同名键是既有行为
        // （core/scheduler.go:271-276），所以这里不换调度器也能把计数清干净
        counters = &poolCounters{}
        open := make(chan struct{})
        close(open)
        scheduler.RegisterHandler("resize_work", blockedHandler(counters, open, false))

        for i := 0; i < 6; i++ {
                require.NoError(t, scheduler.Schedule(resizeJob("conv", i)))
        }
        deadline := time.Now().Add(5 * time.Second)
        for time.Now().Before(deadline) && counters.done() < 6 {
                time.Sleep(10 * time.Millisecond)
        }
        plainPeak, _ := counters.peaks()
        assert.Equal(t, 1, plainPeak, "退场收敛之后的并发峰值必须正好是目标值 1")
}
```

判据落在**退场收敛之后**（`waitActiveExactly` 是它的门禁）。
不要去断言"缩容瞬间峰值==1"——退场窗口里仍可能各领一条，那种写法必然 flake。

### 5.4 队列容量不变、阻塞的投递仍能落地

```go
// TestResizeWorkers_BlockedDispatchStillLands：容量 1、worker 2，两条任务都卡在闸门上时
// 第三条占满通道、第四条让调度循环阻塞在 s.workCh <- job（core/scheduler.go:1327-1332）；
// 此时扩到 4，被阻塞的那条最终必须跑完。
// 这条守住 §2 的结论：不重建通道，扩缩不会让正在阻塞的发送方落空。
```

实现要点：`SetQueueCapacity(1)` + `SetConcurrency(2)`，投 4 条任务，
`ResizeWorkers(4)`，再 `unblock()`；判据是 `counters.done()==4`，
**不是** `QueueLength` 读数（那是瞬时值，`core/scheduler.go:1773-1774` 的注释已说明）。

### 5.5 边界与重启

- `ResizeWorkers(0)` / `ResizeWorkers(-3)` 返回错误，且 `RuntimeStats().Workers` 不变。
- 未 `Start` 时 `ResizeWorkers(6)` 返回 nil，随后 `Start` 起 6 个 worker（用 §5.1 的闸门法：峰值恰好 6）。
- `Start→Stop→Start` 后 `retireRequests` 已清零：先缩容制造 3 个待退场名额，`Stop`，
  再 `Start`（`SetConcurrency(4)`），投 4 条任务必须 4 条都能并发跑起来
  （残留名额没被下一代 worker 消费）。
- `n == 现值`：返回 nil 且不新建协程（用 `runtime.NumGoroutine()` 差值断言，
  允许 ±2 的抖动；这是 `core/scheduler_exec_class_test.go:430` 已经采用的写法）。

### 5.6 并发压力

一个协程在 1..8 之间来回 `ResizeWorkers` 50 轮，另一个协程持续投快速任务，
第三个协程读 `RuntimeStats()`。跑完 `Stop()` 必须返回（不退场干净就卡死在这里）。
`-race -count=5` 必须干净。这条用例是本卡的"wg 记账不能少算"证据：
少算一个 worker 就会让 `Stop` 提前返回，而 `-race` 与 `Stop` 返回共同把它暴露出来。

## 6. 完成标准（DoD）

- [ ] `ResizeWorkers` 五条语义（正数校验、未启动等价、扩容起协程、缩容温和、幂等）各有用例。
- [ ] 扩容后"同时在跑的任务数"真的上升（§5.1 断言到 4），缩容后在途任务一条都不被取消
      （§5.2 断言 4 条全跑完）——两条都是行为判据，不是读数判据。
- [ ] `RuntimeStats.Workers` 反映目标值、`QueueCapacity` 仍是通道真实容量，
      两者差异在注释里说清（否则读数会被误读）。
- [ ] 新起的 worker 计入 `s.wg`，`Stop` 会等它们；§5.6 的并发用例里 `Stop` 必然返回。
- [ ] `SetConcurrency`/`SetQueueCapacity` 的"Start 之后调用则 warn 并忽略"口径**未被改动**，
      既有用例（`core/scheduler_concurrency_test.go`、`core/scheduler_exec_class_test.go`）
      未经修改即通过。
- [ ] 默认池行为在不调 `ResizeWorkers` 时与改动前一致（本卡不接线，全仓不该有任何路径调它）。
- [ ] `go test ./core -race -count=5 -timeout 30m`、`go build ./...`、`go vet ./...`、
      `go test ./... -race -count=1` 全绿；新增文件已 `gofmt -w`。

## 7. 验收方式

```bash
go test ./core -run 'TestResizeWorkers' -v
go test ./core -race -count=5 -timeout 30m
go build ./... && go vet ./...
go test ./... -race -count=1
```

预期：第一条列出 §5 的全部用例名并 `--- PASS`；第二条无 flake。
若第一条出现某条用例长时间不到判据（尤其 §5.3 的收敛等待与 §5.4 的阻塞投递），先看是不是 worker 全被退场名额吃掉——
那是本卡最容易写错的地方，不要靠延长超时糊过去。

## 8. 不在本任务范围

- 执行器池（`execCh`）的运行期扩缩，以及"关闭状态下把池建起来"：设计文档 §10 的 N1。
- 队列容量、`scheduler.shutdown_timeout` 的任何运行期改动。
- 在途任务的取消或超时调整：本卡一条都不碰 `cancelMap`。
- `api` 层的新字段与新端点（R06）。
- 把 `ResizeWorkers` 接到配置重载链上（R06）；本卡不碰 `cmd/server`。

## 9. 风险与回滚

| 风险 | 说明 | 处置 |
| --- | --- | --- |
| `wg` 少算新 worker | `Stop` 提前返回，进程在优雅关闭时丢掉在跑的普通任务 | §5.6 的压力用例 + `Add` 与 `go` 必须成对出现在同一个函数体内 |
| 退场名额跨重启残留 | 第二次 `Start` 起的 worker 少于预期，现象是"并发莫名其妙小了" | §5.5 专门一条；`Start` 里 `retireRequests.Store(0)` 与既有 `stopCh` 复位并排放 |
| 缩容读数与实跑不一致造成误判 | `Workers` 已经是 1，`Running` 可能还是 4 | 这是温和缩容的定义，注释与 §3.4 说明；R07 的文档同步里也要写进使用者文档 |
| `ResizeWorkers(0)` 回退默认值 | 与 `SetConcurrency` 一致看起来更"整齐"，但会把一条错配置跑下去 | §3.3 定死返错，用例覆盖 |
| 用例靠 sleep 判定 | flake | 一律用 10ms 轮询等条件（`waitActive`/`waitActiveExactly`），峰值断言落在退场收敛之后（§5.3） |
| 与优雅关闭的时序 | R06 会先停 watcher 再 `server.Stop`，本卡不需要为此改任何东西 | 若 R06 发现关闭期仍有注册动作，那是 R06 的顺序问题 |

回滚：本卡只动 `core/scheduler.go` 一个生产文件（加字段、`Start` 三行、`worker` 一个判定、
一个新方法、`RuntimeStats` 一行），无调用方，`git revert` 单提交即可，不影响任何既有语义。

## 10. 实现记录（执行时补写）

### 10.1 落地的接口

生产代码只动 `core/scheduler.go`（按符号定位；卡面 §2/§3 里的行号是 R02 之前的，已经整体后移）：

| 符号 | 内容 |
| --- | --- |
| `Scheduler.targetWorkers atomic.Int32` | 新字段，与 `concurrency`/`queueCapacity`/`workCh` 同一段 |
| `Scheduler.retireRequests atomic.Int32` | 新字段，紧随其后 |
| `NewScheduler` | 建好体之后把 `targetWorkers` 初始化成 `s.concurrency`（原子字段进不了字面量） |
| `Scheduler.SetConcurrency` | 与 `concurrency` 一起写 `targetWorkers`；"Start 之后 warn 并忽略"的口径一字未改 |
| `Scheduler.ResizeWorkers(n int) error` | 新方法，位置在 `RunningCount` 与 `Start` 之间 |
| `Scheduler.Start` | `retireRequests.Store(0)` 与既有 `stopCh` 复位并排；`targetWorkers.Store(workers)`；`workCh` 容量口径未动 |
| `Scheduler.worker(queue, exec)` | 循环开头加 `if !exec && s.shouldRetire()` 退场判定，两个参数签名未动 |
| `Scheduler.shouldRetire() bool` | 新私有方法，CAS 消费一个退场名额 |
| `RuntimeStats.Workers` | 由 `s.concurrency` 改读 `s.targetWorkers`；`QueueCapacity` 算法原样，两个字段的注释写清口径差别 |

测试新增 `core/scheduler_resize_test.go`：**12 条用例**（`TestResizeWorkers_*`；前 11 条是实现轮，
第 12 条 `…_RetireCheckPrecedesNextPickup` 是复核轮补的，见 §10.2 第 11 条）+ 卡面指定的
`waitActiveExactly`、`resizeJob`，另加三个本地 helper：`waitUntil`（10ms 轮询等条件）、
`assertHoldsAt`（"必须保持不变"的观察窗口）、`assertStopNotReturnedYet`（同上的 Stop 版）。
第十二条 `TestResizeWorkers_RetireCheckPrecedesNextPickup` 是复核轮补的，用来判退场判定的**位置**
（见 §10.2 第 11 条与 §10.3 的 M8）。
计数设施全部复用 `core/scheduler_exec_class_test.go` 的 `poolCounters`/`blockedHandler`/
`waitActive`/`openGate`，本文件没写第二份闸门计数。

### 10.2 与本卡写法的差异

1. **§5.5 第四条不用 `runtime.NumGoroutine()`**。卡面引用 `core/scheduler_exec_class_test.go:430`
   作为"进程级计数差值 + ±2 抖动"的先例，这条引用是反的：那段注释（`TestExecClass_DisabledCreatesNoPool`
   头上）说的正是"卡片允许两种写法，这里选了另一种"，因为同包其它用例会在途留下协程，
   进程级计数在本机 `-race -count=5` 下会 ±1 抖动（实测翻红过"应当多出 3 个，实际多出 2 个"）。
   替代判据见 `TestResizeWorkers_SameValueIsNoOp`：3 个 worker 全卡在闸门上、队列里还压着三条，
   `ResizeWorkers(3)` 之后要求"在跑数量保持在 3"（多起一个协程就会立刻有人领走被压住的任务），
   并以终局的并发峰值 3 收尾（不看时间、不靠采样运气），附带断 `retireRequests` 仍为 0。
2. **§5.3 的 `waitActiveExactly` 换了摆法**。卡面示例在第一阶段四条都返回之后等"正好 1"，
   那时在跑数量是 0，等待必然不到判据（改坏之后能红，改好之前也红）。改成让第二阶段的任务
   也卡在另一道闸门上：活下来的 worker 领走一条后停在 1、其余五条留在队列里没人领，
   "正好 1 且连续两轮不变"才真的等于"退场已经收敛"。峰值判据仍然落在收敛之后。
3. **运行期分支不回写 `s.concurrency`**。卡面 §3.4 要求 `QueueCapacity` 的回退值等于通道真实容量，
   而那个回退值取的就是 `concurrency`；扩缩写它会报出通道并不具备的容量。于是 `concurrency`
   定死为"启动期快照"，运行期只有 `targetWorkers` 动。未启动那条分支仍按卡面回写 `concurrency`。
   代价（热更值不跨进程内重启）登记在 §10.5 D1。
4. **`NewScheduler` 与 `SetConcurrency` 也要写 `targetWorkers`**，卡面 §3.2 只列了 `Start` 那一处。
   不补的话 `Workers` 改读 `targetWorkers` 之后，未启动的调度器会报 0：
   `core/runtime_stats_test.go` 与 `api/handlers_admin_test.go` 两条既有用例都断在未启动状态，
   而本卡不许改既有用例。
5. **`ResizeWorkers` 扩容时在锁内把 `workCh` 取成局部变量**，锁外的 `go` 语句用这个引用。
   卡面只要求"`wg.Add` 在锁内、`go` 在锁外"；这一步是为了不在持锁期间之外再读共享字段。
   由此留下的交错（放锁后另一条协程做完了 `Stop→Start`）登记在 §10.5 D2。
6. **三条卡面没有的用例**：
   - `TestResizeWorkers_StopWaitsForResizedWorkers`：卡面 §9 风险表第一行要求"`Add` 与 `go`
     成对"有证据，而 §5.6 的压力用例判不了少算（少算只让 `Stop` 提前返回，不会自己报错）。
     这里用"闸门还关着时 `Stop` 不得返回"直接判记账。
   - `TestResizeWorkers_ExecutorPoolNeverRetires`：退场判定的 `!exec` 半边在 §5 的用例里
     怎么摆都不会红，补一条"名额全程没被执行器协程动过 + 执行器池并发一格没少"。
   - `TestResizeWorkers_RetireCheckPrecedesNextPickup`（复核轮补的第十二条）：钉住退场判定
     在"领下一条任务之前"而不是"执行完手上那条之后"，摆法与判据见 §10.3 的 M8。
7. **§5.2 多加了读数对照**（`Workers`=1、`QueueCapacity`=4、`cap(workCh)`=4、`retireRequests`=3），
   并把 `SetQueueCapacity` 故意留空，好让"回退值取启动期 `concurrency`"这条口径也被断到。
   卡面那两条行为断言（`Workers` 即时变、`done()==4`）原样保留。
8. **§5.4 多加了前提凑数与"通道没被换"两条**：先等到"2 条在跑 + 通道里 1 条 + 堆空"这个状态
   （凑不齐就说明第四条没压在阻塞的发送上，用例是空的），再断 `queueBefore == scheduler.workCh`。
9. **§5.6 压力用例里读数的 `Workers` 区间与 `QueueCapacity` 恒定**是卡面没列的额外自洽检查；
   扩缩协程每轮之间让出 1ms（否则 50 轮会在最初几毫秒里撞完，只压到锁不压到交错），
   读数协程同样 1ms 节流——两处都只决定重叠程度，不参与判据。
10. **注释按实现改写过一处卡面措辞**：§3.1 说 `targetWorkers` 用原子是"执行协程在循环开头读它"，
    实际执行协程读的是 `retireRequests`（`targetWorkers` 只有 `ResizeWorkers` 与 `RuntimeStats` 读），
    代码注释按实际写，保留"两个概念两个字段"的理由。

**复核轮（fresh-context 复核判"仍需返工"）：1 条 Important + 3 条 Minor，全部处理**

11. **Important：M8 曾被记成"等价变异"，那条记录是错的**。初版的理由是"退场判定放在循环开头还是
    放在 `executeJob` 之后，观察到的并发数与任务数一模一样"——漏掉了**余额悬着时有新协程进来**这一形状：
    判定在开头，新协程一条任务都不领就退；判定在执行之后，它会先领走队列里那条再退。
    据此补了第十二条用例 `TestResizeWorkers_RetireCheckPrecedesNextPickup`
    （2 个 worker 全卡在闸门里 → 缩到 1 让余额悬着 → 再排一条任务 → 扩回 2，
    判据是"闸门还关着时余额就归零"加终局不看时间的并发峰值 2）。
    补进去之后 M8 判红、其余 11 条不受影响，也就是它补的是真空而不是重复覆盖（§10.3 的 M8 行）。
    **这条用例的首版把 `defer unblock()` 注册在 `defer scheduler.Stop()` 之前**，
    失败路径上 defer 后进先出会先跑 `Stop`，而 `Stop` 要等还卡在被关闭闸门里的 worker——
    第一次跑 M8 时整个包撞到 5 分钟超时才失败。现在按本文件既有口径把闸门那条 defer
    注册在 Stop 之后（收尾时先开闸门再关停），M8 变成 5.02s 定点判红。
    这个次序错误只有真的跑一遍变异才暴露得出来，记在这里是为了留住"变异也在测用例本身"这件事。
12. **Minor：`TestResizeWorkers_RestartClearsRetireRequests` 的注释把用例构造写成了通用性质**。
    原写"每个 worker 退出前都至少经过一次循环开头，余额在此耗光"——实情是该用例里四个 worker
    当时全在处理函数里、跑完手上那条才回到循环开头；闲着停在 `select` 上的 worker 走的是 `stopCh`
    那一支，根本不经过消费点，所以"带着余额关停"在一般情形下是可能的，跨代残留真正兜住它的是
    `Start` 里的清零（也正是第二段用例存在的理由）。函数头的注释改成用例内的口径
    （那条断言的消息当时漏改，下一次复核才收口，见第 16 条）。
13. **Minor：`RuntimeStats.Workers` 的字段注释不够诚实**。原写"实跑协程要等在途任务跑完才收敛"，
    实情是闲着的多余协程在途任务跑完之后仍不收敛，要等**下一条任务**把它叫回循环开头。
    R06 会把这行读数原样透到 `/api/v1/admin/runtime`，运维按旧措辞会误判，
    已改成"要等任务重新流动才收敛"并写明闲着的协程停在 `select` 上既不检查名额也不会退出。
    （`ResizeWorkers` 自己的文档注释本来就是这个口径，无需改。）
14. **Minor：与 `Stop` 交错的那段注释承诺略强**。原写扩容起来的新协程"会立刻看到已关闭的 `stopCh`
    并退出"——`select` 的两个分支是随机的，它也可能先领到一条队列里正等着的任务再退。
    收敛性不受影响（那条任务跑完就退），注释按实际改成"随后就会退出"。

**复核轮的复核**（第二次 fresh-context 复核：四条全部判为已解决、M8 由复核者本人重跑确认"只被第十二条判红、
其余 11 条全绿"，另出 2 条 Minor，均已处理）：

15. **Minor：`ResizeWorkers` 里两处"本仓只有一个 `Start` 调用点"的说法过宽**。
    实情是 `examples/demo1/main.go:29` 与 `examples/demo2/main.go:41` 也各调一次 `Scheduler.Start`，
    只是它们各是独立 main、都不接 `ResizeWorkers`。D2/D1 的结论不受影响
    （任何单个可执行文件都凑不出"一次 Stop 未完又起一次 Start"的交错），
    但这条口径不能写成"全仓唯一调用点"。两处注释与 §10.5 的 D2 行都收回到"生产进程 `cmd/server`
    只 `Start` 一次"这个范围。
16. **Minor：第 12 条要改的断言消息漏改了一处**。`TestResizeWorkers_RestartClearsRetireRequests`
    里那句 `retireRequests` 归零的断言消息仍写成无条件的"worker 退出前都经过循环开头，余额在此耗光"，
    与同一个函数头上刚改过的说明自相矛盾。现在消息带上了用例范围
    （"本用例四个 worker 都在跑任务…通用情形下不成立，见函数头"）。

### 10.3 验证证据

本机 Windows / Git Bash，`-count=5` 一律配 `-timeout 30m`。

```bash
$ gofmt -w core/scheduler.go core/scheduler_resize_test.go
$ gofmt -l core/scheduler.go core/scheduler_resize_test.go      # 无输出
# 仓级 gofmt -l 被既有 CRLF 文件污染，本卡只判自己碰的两个文件；两个文件都是 LF 换行、无差异

$ go test ./core -run 'TestResizeWorkers' -v -timeout 5m        # 12 条用例，=== RUN 行从略
--- PASS: TestResizeWorkers_UpRaisesConcurrency (0.03s)
--- PASS: TestResizeWorkers_DownKeepsInFlightJobs (0.02s)
--- PASS: TestResizeWorkers_DownConvergesToTarget (0.04s)
--- PASS: TestResizeWorkers_RetireCheckPrecedesNextPickup (0.33s)
--- PASS: TestResizeWorkers_BlockedDispatchStillLands (0.02s)
--- PASS: TestResizeWorkers_RejectsNonPositiveTarget (0.02s)
--- PASS: TestResizeWorkers_BeforeStartTakesEffectAtStart (0.02s)
--- PASS: TestResizeWorkers_SameValueIsNoOp (0.33s)
--- PASS: TestResizeWorkers_RestartClearsRetireRequests (0.04s)
--- PASS: TestResizeWorkers_StopWaitsForResizedWorkers (0.31s)
--- PASS: TestResizeWorkers_ConcurrentResizeScheduleAndStats (0.08s)
--- PASS: TestResizeWorkers_ExecutorPoolNeverRetires (0.05s)
PASS
ok  	godelayq/core	1.454s

$ go test ./core -race -count=5 -timeout 30m
ok  	godelayq/core	61.689s       # 五轮无 flake、无 race 报告（终态字节：复核两轮都改完之后重跑）

$ go test ./core -race -count=5 -run TestResizeWorkers -timeout 30m
ok  	godelayq/core	7.864s        # 十二条扩缩用例单独压五轮

$ go build ./... && go vet ./...
# 无输出

$ go test ./... -race -count=1 -timeout 30m
ok  	godelayq/api		108.962s
ok  	godelayq/cmd/server	6.326s
ok  	godelayq/core		13.117s
ok  	godelayq/executor	22.410s
ok  	godelayq/store/sqlite	3.752s
                                # 五个包全 ok
```

既有用例未经修改即通过：`core/scheduler_concurrency_test.go`、`core/scheduler_exec_class_test.go`、
`core/runtime_stats_test.go`、`api/handlers_admin_test.go` 都在上面那两轮全量跑里（core 五轮 `-race`
与全仓一轮 `-race`），断言一字未改。

变异反向验证八条（M1~M7 用脚本 `%TEMP%/r03_mutations.py`，M8 用 `%TEMP%/r03mut/m8.py`；
变异前先取字节副本，每条改坏→跑对应用例→
下一条，最后整体还原，全部读写用 `open(p,'rb')`/`open(p,'wb')`，不做文本模式往返）：

| 编号 | 变异 | 判红的用例 | 实际报红信息 |
| --- | --- | --- | --- |
| M1 | `worker` 从不退场（退场判定短路成 `false &&`） | `TestResizeWorkers_DownConvergesToTarget`（5.03s 判红） | `plain active never settled at exactly 1, got 4` |
| M2 | 退场判定漏到执行器池（去掉 `!exec` 守卫） | `TestResizeWorkers_ExecutorPoolNeverRetires`（5.03s） | `执行器协程经过循环开头也不许碰这个名额`：expected `int(1)`、actual `int32(0)`；随后 `两个池没有同时占满自己的名额（普通在跑 0 期望 0，执行器在跑 1 期望 2）` |
| M3 | `Start` 不再清零 `retireRequests` | `TestResizeWorkers_RestartClearsRetireRequests`（5.02s） | `Start 与复位 stopCh 一起把退场余额清零`：expected `int(0)`、actual `int32(3)`；随后 `普通在跑 1 期望 4` |
| M4 | `ResizeWorkers(0)` 静默回退 `DefaultConcurrency` | `TestResizeWorkers_RejectsNonPositiveTarget`（0.00s） | `An error is expected but got nil.` |
| M5 | 扩容起的新 worker 不计入 `wg`（删 `s.wg.Add(start)`） | `TestResizeWorkers_StopWaitsForResizedWorkers`（包级 0.519s） | `panic: sync: negative WaitGroup counter`，栈落在 `core.(*Scheduler).worker` 的 `defer s.wg.Done()`，`created by core.(*Scheduler).ResizeWorkers` |
| M6 | `RuntimeStats.Workers` 仍读 `s.concurrency` | `TestResizeWorkers_DownKeepsInFlightJobs`（0.02s） | `Not equal: expected: 1, actual: 4`（缩容之后的 `Workers` 读数） |
| M7 | 未启动分支不回写 `concurrency` | `TestResizeWorkers_BeforeStartTakesEffectAtStart`（5.00s） | `下一次 Start 建通道与协程取的就是这个值`：expected `6`、actual `1`；随后 `普通在跑 1 期望 6` |
| M8 | 退场判定从"循环开头"挪到 `executeJob` 返回之后 | `TestResizeWorkers_RetireCheckPrecedesNextPickup`（5.02s，其余 11 条全绿） | `判据在 5s 内没有成立：扩进来的那个协程在领任务之前先退场，余额归零` |

复核轮的 M8 用另一条脚本（`%TEMP%/r03mut/m8.py`）单跑，
实现轮首次记录时两个文件是 76342 / 27665 字节；复核轮补用例与改注释之后的终态是
`core/scheduler.go` **76721 字节**（sha256 前缀 `235a09edce06a189`）、
`core/scheduler_resize_test.go` **30958 字节**（前缀 `dcd9deddb0c6d36a`）——
字节数与实现轮不同只因为复核轮改了注释并补了第十二条用例，最后一次复核又收了 `Start`
调用点的口径与一条断言消息。
M1、M5 与 M8 在复核轮被重跑过（复核者一次、本卡作者一次，M8 两次都是"只被第十二条判红、
其余 11 条全绿"），M2~M7 沿用实现轮的记录——复核轮的改动全是注释与新增用例，
没有触及那六条的判据路径。
**每一轮变异的还原都核对过字节**：脚本从变异前的字节副本写回，自己断言
`open(SRC,'rb').read() == 备份` 为真（实现轮那七条跑完时报 `restored byte-identical: True`，
`sha256sum -c` 对变异前记录的 `core/scheduler.go` 76342 字节与 `core/scheduler_resize_test.go`
27665 字节两行都是 `OK`；M8 那三次分别报 `22f9f34e67753be5` 一致）。
每次还原之后都重跑过 `go build ./... && go vet ./...`（无输出）与
`go test ./core -run 'TestResizeWorkers'`（12 条全 PASS）。

**M8 曾被本卡的初版记成"等价变异"，那条记录是错的，复核轮把它推翻并补了判别用例**：
初版的理由是"两个位置说的都是跑完手上这条再退场，观察得到的并发数一模一样"——漏掉了一种摆法：
**扩进来的那个协程自己撞上还有余额**时，两个位置的行为并不相同。判定在循环开头，新协程一条任务都不领就退；
判定在执行之后，新协程会先领走队列里那条再退。`…_RetireCheckPrecedesNextPickup` 造的就是这个形状
（2 个 worker 全卡在闸门里 → 缩到 1 让余额悬着 → 再排一条任务 → 扩回 2），
判据是"闸门还关着时余额就归零"加上终局不看时间的并发峰值 2。
这条用例补进来之后 M8 判红，其余 11 条不受影响——也就是说它补的是真空，不是重复覆盖。

### 10.4 手工验收（本卡不起进程；接线后由 R07 场景 3 覆盖）

本卡不接线，所以没有起过进程，`ResizeWorkers` 至今零调用方（全仓 `grep -rn ResizeWorkers --include=*.go`
只剩 `core/scheduler.go` 的定义与 `core/scheduler_resize_test.go` 的用例；
`cmd/server/config_reload_smoke_test.go` 里出现的那一句是那条用例说明"本卡没给入口"的注释，不是调用）。
取而代之做的是三件静态核对：

1. 确认**生产进程** `cmd/server` 里 `scheduler.Start()` 只有一个调用点、`Stop()` 在启动失败与优雅关闭两条路径上，
   也就是说 §10.5 D2 那条交错在今天的进程生命周期里凑不出来。（另有两个 `Start` 调用点在
   `examples/demo1`、`examples/demo2` 这两个独立 main 里，它们不接 `ResizeWorkers`，也不构成反例——
   复核轮提醒过不要把这条写成"全仓唯一调用点"，见 §10.2 第 15 条。）
2. 确认 `/api/v1/pools` 与 `/api/v1/admin/runtime` 读的就是 `Scheduler.RuntimeStats`（`api/handlers.go`、
   `api/handlers_admin.go` 各一处），所以本卡改的 `Workers` 口径会直接透到那两个端点上——
   接线之后不需要再动 api 就能看见新值。
3. 把 R06/R07 要补的场景 3 写清楚：跑起进程 → 改 `configs/config.yaml` 的 `scheduler.workers`
   → 不重启，先看 `/api/v1/admin/runtime` 的 `scheduler.workers` 是否立刻变成新值（期望并发口径），
   再看投递速率（缩容时队列长度 `queue_length` 上升、扩容时回落），
   并确认 `queue_capacity` 读数不变（通道没换）。

### 10.5 缺陷

| # | 现象 | 处置 |
| --- | --- | --- |
| D1 | 热更出来的并发数不跨进程内重启：运行期扩缩只改 `targetWorkers`，`Start` 会把它按 `concurrency`（启动期快照）重新覆盖。`Stop→Start` 之后并发退回启动值，`/admin/runtime` 那一刻也会跳回去 | 登记不修（今天没有进程内重启；归 R06 —— 接线时在 `Start` 之后重放一次，或在 `ReloadService` 里记住 applied 值并补 `ResizeWorkers`） |
| D2 | 若 `ResizeWorkers` 放锁之后、`go` 语句执行之前另一条协程做完了 `Stop→Start`，这批新协程领的是上一代的 `workCh`、等的是新一代的 `stopCh`，会把那次 `Stop` 的 `wg.Wait()` 拖住 | 登记不修（今天触发不了：**生产进程 `cmd/server` 只 `Start` 一次**，`examples/demo1:29`、`examples/demo2:41` 各是独立 main、也只 `Start` 一次且都不接 `ResizeWorkers`——注意这条口径别说成"全仓只有一个 Start 调用点"，那是错的，只是不影响结论；归 R06/后续 —— 修法是把停止信号与队列成对传进 `worker`，那要改卡面 §3.2 定死的签名） |
| D3 | `ResizeWorkers` 只有下限没有上限：热更一个 `scheduler.workers: 100000` 会立刻起十万条协程 | 登记不修（本卡按卡面只做"n<=0 报错"；归 R06 在重载链上补上界校验，配置层 `Normalized` 目前也只挡负数） |
| D4 | 温和缩容期间读数与实跑不一致（`Workers` 已是新值、实跑协程还多几个），要等任务流动才收敛 | 登记不修，按设计保留（卡面 §3.3 已把它定义为代价）；归 R07 的使用者文档，运维读数要连 `running`/`queue_length` 一起看 |

未发现"改动破坏既有语义"的缺陷：`SetConcurrency`/`SetQueueCapacity` 的 warn-and-ignore 口径、
执行器池的建池条件、`workCh` 容量算法与 `Stop` 的取消时序都没动，既有用例未改一字即通过。

### 10.6 未覆盖项

- **执行器池的运行期扩缩**与"关停状态下把池建起来"（§8、设计文档 §10 的 N1）：本卡只保证
  `!exec` 这一侧不退场，不提供任何扩缩执行器池的能力。
- **§10.5 D1/D2 的交错**没有用例：都要在一次 `Stop` 未完成期间再跑一次 `Start`，
  与生产装配方式冲突，硬造出来的用例只会绑住 R06 的实现细节。
- **`retireRequests` 被减成负数**只有 CAS 实现保证，没有专门的并发用例撞它
  （§5.6 的压力用例里缩容轮次会反复出现多个协程同时退场，属于被动经过，没断言"计数恒非负"）。
- **api 层的读数**：`/pools`、`/admin/runtime` 的 `workers` 字段随扩缩变化没有端点级用例（本卡不改 api）。
- **`ResizeWorkers` 的上界**（D3）既没实现也没用例，等 R06 决定校验口径后一并补。
