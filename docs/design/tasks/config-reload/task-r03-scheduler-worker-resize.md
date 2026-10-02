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

### 10.2 与本卡写法的差异

### 10.3 验证证据

### 10.4 手工验收（本卡不起进程；接线后由 R07 场景 3 覆盖）

### 10.5 缺陷

### 10.6 未覆盖项
