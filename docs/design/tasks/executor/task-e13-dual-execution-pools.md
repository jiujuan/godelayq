# TASK-E13　调度器拆分为两个执行池

- 所属阶段：M3 隔离
- 依赖任务：TASK-E09
- 涉及文件：`core/scheduler.go`、`core/scheduler_concurrency_test.go`、`api/handlers.go`、`api/handlers_admin.go` 及其测试、`cmd/server/main.go`
- 预计规模：大（改动在调度主循环上，必须谨慎）

## 1. 任务目标

让执行器任务用一套独立的 worker 与队列，不再和普通任务抢同一批执行名额。

## 2. 背景与当前问题

当前 `core/scheduler.go` 的 `Start` 只建一条 `workCh`（容量等于 `concurrency`），起 `concurrency` 个 `worker` 协程；`dispatch` 在队列满时**阻塞等待空位**，这是有意的背压设计（README 的核心特性里写明"到期风暴时调度循环阻塞入队形成背压"）。

执行器任务的特点是执行时间长（分钟级）、可能大量到期（一个 cron 档位一次触发几十条）。E09 的档位许可只限制"同一档位并发数"，等待许可时仍然占着 worker 名额。因此如果不分流，100 个执行器任务就会使普通任务全部无法执行——**而且在共享的 `dispatch` 上，被影响的不只是 worker，还有调度主循环本身**：调度循环一阻塞，连"到点弹出"都不做了，普通任务的准时性直接失效。这就是本卡必须存在的原因，也是为什么不能在 Handler 里加信号量解决。

## 3. 要实现的功能

1. 任务类别：

   ```go
   type JobClass int
   const (
       JobClassDefault JobClass = iota
       JobClassExec
   )
   ```

2. 注册时声明类别：
   - 新增 `func (s *Scheduler) RegisterHandlerClass(jobType string, handler Handler, class JobClass)`。
   - 既有 `RegisterHandler` 保持不变，等价于 `RegisterHandlerClass(..., JobClassDefault)`（这是兼容底线：所有现有调用点与测试不用改）。
   - 内部用 `handlerClasses map[string]JobClass` 记录，与 `handlers` 同一把锁保护。
   - `executor` 的注册链路（E04 的 `Register`）改用 `RegisterHandlerClass`。
3. 独立的执行队列与 worker：
   - 字段：`execCh chan *Job`、`execConcurrency int`、`execQueueCapacity int`、`execInFlight atomic.Int64`、`execSlotFreed chan struct{}`（容量 1，非阻塞发送）。
   - `SetExecConcurrency(int)` / `SetExecQueueCapacity(int)`：与 `SetConcurrency`/`SetQueueCapacity` 同样的规则（0 回退默认、`Start` 之后调用忽略并记 warn）。
   - `Start`：`execConcurrency > 0` 时创建 `execCh` 并起 `execConcurrency` 个 `worker(execCh)`；`wg.Add(1 + workers + execWorkers)`。
   - `worker` 改成 `worker(ch chan *Job)`，退出时如果是 exec 队列则 `notifySlot()`。
4. 投递策略（本卡最关键的一段）：
   - `scheduleLoop` 在 `heap.Peek()` 拿到队首任务后、调用 `PopIfDue` **之前**，判断该任务类别的队列是否还有空位：
     - `JobClassDefault`：不做预判，保持现有"满则阻塞"的背压行为不变。
     - `JobClassExec` 且队列已满：**不弹出**，在 `select` 上等待 `execSlotFreed` / `stopCh` / `resumeCh` / 一个短超时（建议 500ms，注释说明这是防止漏信号的兜底），然后重新走一轮循环。任务因此留在堆与存储里，状态仍是 pending，与 README 的"未执行任务保留在堆与存储中"一致。
   - `dispatch` 保持"按类别选通道"，并且对 exec 类别的发送改为**非阻塞**（预判已经保证有空位；如果预判后仍满，返回 false 让调用方按"未投递"处理并把任务留在堆里）。
   - 明确禁止的写法：让 exec 队列满时阻塞调度循环（等于没隔离）；把任务弹出后又塞回堆（`TriggerAt` 已过期，会造成忙等）。
5. 取消表、暂停、强制暂停、挂起开关：**共享，不按类别拆**。`cancelMap`、`paused`、`suspend` 的语义与队列无关，拆开会引入"挂起只挂了普通任务"这类无法解释的行为。
6. `RuntimeStats` 增加：

   ```go
   ExecWorkers     int  `json:"exec_workers"`
   ExecQueueCap    int  `json:"exec_queue_capacity"`
   ExecQueueLength int  `json:"exec_queue_length"`
   ExecRunning     int  `json:"exec_running"`
   ```

   `Running` 与 `QueueLength` 的既有含义改为"仅普通池"，`api/handlers.go` 的 `GetStats` 输出与 `docs/api.md` 里 `running` 的描述要相应说明（文档在 E19 收尾，本卡先把措辞确定下来并在代码注释里登记）。
   注意：含义变更会影响既有断言。要求 `GetStats` 里 `stats.Running` 改为 `Running + ExecRunning`（对使用者而言"正在执行"仍是总数），并在 `RuntimeStats` 的字段注释上写明 `Running` 只含普通池。
7. `cmd/server/main.go`：`schedulerAPI` 接口加 `SetExecConcurrency`/`SetExecQueueCapacity`，从 `cfg.Executors.Concurrency`/`QueueCapacity` 取值；`enabled=false` 时传 0（不建 exec 池）。`api/handlers_admin.go` 的运行时端点自动透出新字段（`RuntimeStats` 是整体序列化，无需改处理器）。

## 4. 实现步骤

1. 先加类别与 `RegisterHandlerClass`，`dispatch` 仍走单通道，跑测试确认无回归。
2. 加 exec 通道、worker 参数化、`Start`/`Stop` 的 wg 计数。
3. 实现预判 + `execSlotFreed` 通知，这一步单独提交，方便定位调度循环的问题。
4. 加 `RuntimeStats` 字段与 `GetStats` 的合成逻辑。
5. 接 `main.go` 配置。
6. 最后写第 5.4 条的压测用例。

## 5. 测试要求

1. `core/scheduler_concurrency_test.go` 补：
   - `TestExecClass_UsesOwnPool`：`SetConcurrency(2)` + `SetExecConcurrency(1)`，注册 3 个普通任务与 3 个 exec 任务同时到期 → 普通池并发 2、exec 池并发 1，两组互不阻塞（用各自的最大并发计数断言）。
   - `TestExecClass_DefaultKeepsBlockingBackpressure`：exec 池关闭（`SetExecConcurrency(0)`）时，所有任务走老路径，既有断言不变。
   - `TestExecClass_FullQueueKeepsJobInHeap`：exec 队列容量 1、worker 全部占住，堆里再来 5 个 exec 任务 → 这些任务仍在堆里（`HeapLen` 不减少）、状态仍是 `StatusPending`、调度循环对**普通任务**仍然准时（关键断言：同时到期一个普通任务，它必须在阈值内被执行）。
   - `TestExecClass_NoBusyWait`：上一条场景下运行 2 秒，统计调度循环的迭代次数（加一个测试用的计数器或断言 CPU 时间），证明没有忙等。
   - `TestStop_WaitsBothPools`：两个池各有在途任务时 `Stop` 会等两边返回，且不超 `wg` 计数导致的提前退出（既有的 `TestScheduler_Stop` 风格）。
2. `core` 既有测试全部不改断言通过（特别是 `TestScheduler_Stop`、`scheduler_recovery_test.go`、`suspend_test.go`）。
3. `api`：
   - `TestGetStats_RunningIncludesExecPool`：exec 任务在跑时 `stats.running` 是两池之和。
   - `TestAdminRuntime_ExposesExecPool`：`GET /admin/runtime` 响应含四个新字段且数值正确。
4. 手工：Windows/Linux 各跑一次 `go test ./core -run TestExecClass -race -count=5`，确认没有偶发失败（并发测试要重复跑）。

## 6. 完成标准（DoD）

- [ ] 执行器队列满时不会阻塞普通任务的调度：有用例直接证明（第 5.1 第三条）。
- [ ] `RegisterHandler` 行为不变，所有既有调用点与测试零修改。
- [ ] 取消/暂停/挂起对两个池一致生效（补一条 exec 任务被 `Cancel` 与 `Pause` 的用例）。
- [ ] `wg` 计数与 `Stop` 等待覆盖 exec worker，`go test -race` 无 goroutine 泄漏告警。
- [ ] `enabled=false` 时不创建 exec 通道与协程（用 `runtime.NumGoroutine()` 前后差值断言，或 `SetExecConcurrency(0)` 的分支测试）。
- [ ] `RuntimeStats.Running` 的含义变化在字段注释、`docs/api.md` 待办、`GetStats` 合成逻辑三处都有记录。

## 7. 验收方式

```bash
go build ./... && go vet ./... && go test ./... -race
go test ./core -run 'ExecClass' -race -count=5 -v
go test ./api -run 'GetStats_Running|AdminRuntime' -v
```

手工：把 `scheduler.workers` 设成 4、`executors.concurrency` 设成 1，配一个 `sleep 10` 档位一次提交 20 条，同时提交 20 条 `payment_check`（1 秒完成）；观察普通任务是否仍按 4 并发稳定推进、`GET /api/v1/stats` 与 `GET /api/v1/admin/runtime` 的两池数字是否合理。

## 8. 不在本任务范围

- 不做运行时调整池大小（`SetConcurrency` 在 `Start` 后无效的既有限制保持；见 `web-console-design.md` §1 非目标）。
- 不做按档位拆分更多池（只有一类 exec）。
- 不改 `executors.concurrency` 的默认值（4）。
- 不做前端展示（E18）。

## 9. 风险与回滚

- 风险：改动调度主循环。要求实现分三步提交（第 4 节），任何一步测试变红就停在那一步，不要跨步修。
- 风险：`execSlotFreed` 是"非阻塞发送 + 容量 1"的通知，存在通知被合并的情况（两次释放只触发一次唤醒）。因为有 500ms 兜底超时，最坏情况只是延迟 500ms 投递，不会死锁。这条要写在注释里，否则后来者会以为是 bug 并去掉兜底。
- 风险：`stats.running` 语义变化会误导已经在看这个数字的运维（原来就是"已进入 Handler 的数量"，现在仍是总数，只是新增了拆分维度）。第 6 节要求文档待办登记，避免口头承诺。
- 回滚：本卡改动集中在 `core/scheduler.go` 与一处接口，`git revert` 单提交即可；回滚后 exec 任务回到共享池，功能仍可用，只是没有隔离。
