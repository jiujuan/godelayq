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

## 10. 实现记录（2026-09-30）

落地文件：`core/scheduler.go`（`JobClass` 与两个常量、`RegisterHandlerClass`/`classOfKey`、
执行器池的五个字段与两个 setter、`Start`/`Stop` 的协程与 `wg` 计数、`worker(queue, exec)`、
`scheduleLoop` 的投递预判、`dispatch`/`dispatchExec`、`dispatchable`、`RuntimeStats` 四个新字段）、
`core/job.go`（不导出的 `class` 字段）、`core/heap.go`（`PopIfDueWhere`、`HasDue`、共用的 `eachDue`）、
`executor/register.go`（注册链路改走 `RegisterHandlerClass`）、
`api/handlers.go`（`GetStats` 把 `running` 合成两池之和）、
`cmd/server/main.go`（`schedulerAPI` 三个新方法 + 按 `executors.enabled` 决定池规模）、
新增 `core/scheduler_exec_class_test.go`（7 条）与 `api/pools_stats_test.go`（3 条），
`core/heap_test.go`、`executor/register_test.go`、`cmd/server/main_integration_test.go` 各补一处。

### 与卡片的偏离与补充

1. **`SetExecConcurrency(0)` 的含义是"不建这个池"，不是 §3.3 写的"0 回退默认值"**。
   §3.3 那条与 §3.7"enabled=false 时传 0（不建 exec 池）"直接冲突：照 §3.3 实现就没有关掉池的入口，
   而且没装执行器的进程会凭空多出 4 个协程。默认值 4 由配置层负责
   （`Normalized` 补齐，`enabled=true` 时显式写 0 会被配置校验拒绝，见 `configs/config.example.yaml` 该键的注释），
   setter 不重复一遍。负数同样按关闭处理。`SetExecQueueCapacity` 保持卡片口径：0 或负数表示与 worker 数相等。
2. **类别写在 `Job` 上一个不导出的字段（`job.class`），入堆与恢复时按注册表盖章**，
   而不是像 §3.4 那样在堆的判断里查注册表。原因是加锁顺序：
   `dispatchable` 在堆的写锁里被调用，查注册表要拿 `s.mu`，
   而包内既有路径全是"先 `s.mu` 再碰堆"（`RuntimeStats`、`parkForcedPause`），
   反过来的顺序就是将来死锁的位置。盖章之后堆的判断只读字段与 `len/cap`，不加任何锁。
   `Restore` 里补盖章是因为类别不进快照：重启恢复出来的档位任务否则全部落回共享池。
3. **改了 `core/heap.go`（卡片没列这个文件）**：要满足"队列满时不弹出、后面的普通任务照样按时投递"，
   只有两种做法——弹出后塞回堆（`TriggerAt` 已过期，立刻再被弹出，正是 §3.4 禁止的忙等），
   或者让堆支持"跳过不可投递的堆顶"。取后者：`PopIfDueWhere(now, allow)` 保留原 `PopIfDue` 的快路径
   （堆顶到期且可投递时一次比较返回），只在"堆顶到期但被挡住"时才遍历；
   遍历靠四叉堆的性质剪枝（子节点触发时间不早于父节点，遇到未到期的节点即停）。
   配套的 `HasDue(now)` 用来区分"没有到期的事"与"有到期但都被挡住"两种等待方式。
4. **`dispatchExec` 投不进去时返回 `true` 并自己善后**（塞回堆 + 敲一次空位信号 + 一条 Error 日志），
   不是 §3.4 说的"返回 false 让调用方按未投递处理"。因为 `dispatch` 的 `false` 在本卡之后只表达一件事：
   收到停止信号、调度循环应当退出（`scheduleLoop` 里就是 `return`）。
   用它表示"没投进去"会让循环直接结束，档位任务再也不会被投递。
   这条分支按构造不该发生：预判已确认有空位，而且执行器队列只有调度循环一个生产者。
5. **空位信号在 worker 取出任务之后立刻发，不等执行结束**。等的条件是"队列少了一个"，
   不是"跑完了一个"；放后面会让调度循环多等一次执行时长。
6. **`execInFlight` 用 `atomic.Int32`**（卡片写 `Int64`），与既有的 `inFlight` 同一类型：
   两者要进同一个 `RuntimeStats`，类型不一致就得在两处各写一次转换。
7. **`RuntimeStats.Running`/`QueueLength` 只算普通池，`/stats` 的 `running` 是两池之和**，
   这是 §3.6 要求的口径，落在三处：`RuntimeStats` 结构体注释、`GetStats` 的合成代码注释、
   以及 `docs/api.md` 的待办登记（归 E19，本卡不改文档）。
   顺带一条实测事实：`/api/v1/admin/runtime` 是 ops 档端点，静态 token（machine≈operator）拿不到，
   冒烟要用 ops 账号登录后的 JWT 读。
8. **测试写在新的 `core/scheduler_exec_class_test.go`**，不是 §5.1 指定的 `scheduler_concurrency_test.go`：
   那边全部用例的前提是"只有一个池"，本卡的用例需要一套自己的计数辅助函数，
   混在一起两边都不能一眼读完。文件头注释里写了这条理由。
   §5.1 要求的五条用例内容一条不少（`TestExecClass_UsesOwnPool`、`_DefaultKeepsBlockingBackpressure`、
   `_FullQueueKeepsJobInHeap`、`_NoBusyWait`、`TestStop_WaitsBothPools`），
   另加两条：取消与暂停对档位任务一致生效（DoD 第三条）、关闭时不建池（DoD 第五条）。
   `RegisterHandler` 的兼容路径不需要单独用例——既有全部用例都在用它。
9. **DoD 第五条用调度器状态断言，没有用 `runtime.NumGoroutine()` 差值**（卡片允许两种之一）。
   实测原因：同包其它测试会留下在途协程，`go test ./core -race -count=5` 下差值会 ±1 抖动
   （出现过"应当多出 3 个，实际多出 2 个"）。改断言 `execCh` 是否为 nil、
   `cap(execCh)` 与 `RuntimeStats` 报出的规模，再用"3 条同时卡在 Handler 里"证明 worker 确实起了 3 个。
10. **调度循环新增一条 Debug 日志**（§3 未要求）："到期任务正在等执行器池空位"。
    §5.1 第四条要求证明没有忙等，靠日志出现次数才能数得准，同时这也是运维现场唯一痕迹。
11. **本卡之外顺手修掉一个既有数据竞争**：`Schedule` 原先是"先入堆、后落盘"，
    任务一进堆就可能被 worker 取走并就地改写 `Status/Attempts/UpdatedAt`，
    而落盘要读这三个字段——同一个指针、两个协程。
    改成"先落盘、后入堆"（`core/scheduler.go` 里那段注释写了原因）。
    本卡之前没暴露是因为窗口极窄：HEAD 上整包 `-race -count=5` 跑一轮、这条用例单独 `-count=20`
    都复现不出来；本卡改动让调度循环醒得更勤，同一台机器上 `-race -count=3` 跑三轮就有一轮报出来。
    落盘失败仍只记日志、照常入堆，容错口径没变。
12. **§5.4 手工配方里的 `payment_check` 实测是 2 秒不是 1 秒**（`cmd/server/main.go` 的
    `handlePaymentCheck` 睡 2 秒），20 条按 4 并发跑完约 10 秒，冒烟按这个时长设阈值。

### 验证结果

Windows 本机（`10.0.26200`，go1.26.4 windows/amd64）：

| 命令 | 结果 |
| --- | --- |
| `go build ./... && go vet ./...` | 通过 |
| `go test ./... -race -count=1` | api 101.3s、cmd/server 5.6s、core 13.1s、executor 23.9s 全 ok |
| `go test ./core -run ExecClass -race -count=5` | ok，无偶发失败 |
| `go test ./core -race -count=5` ×4 轮 | 4 轮全绿（整包重复 20 次）。作为对照：修复第 11 条之前 `./core -race -count=3` 跑 3 轮，1 轮报出该数据竞争、1 轮败在第 9 条的协程计数上 |
| `go test ./api -run 'GetStats\|AdminRuntime'` | ok |
| `GOOS=linux/darwin/windows` 的 `go build` + `go vet`、`go build -tags dashboard ./...` | 全部通过 |
| 既有用例 | `scheduler_concurrency_test.go`、`scheduler_recovery_test.go`、`suspend_test.go`、`pause_test.go` 断言一字未改，全绿 |

冒烟（Windows 真实服务端 + REST，独立配置在临时目录；`scheduler.workers=4`、
`executors.concurrency=1`、`executors.queue_capacity=1`；档位 `exec.sleep8` =
`kind: binary` + `program: cmd` + `fixed_args: [/c, sleep8.cmd]`，脚本内 `ping -n 9`；
鉴权用静态 token 提任务、ops 账号的 JWT 读运维端点）：

同时提交 6 条档位任务与 20 条 `payment_check`，两池读数随时间变化：

| 时刻 | 普通池 `running/queue` | 执行器池 `running/queue` | `heap_size` | `/stats` 的 `running` |
| --- | --- | --- | --- | --- |
| +2s | 4 / 4 | 1 / 1 | 11 | 5 |
| +4s | 4 / 4 | 1 / 1 | 7 | 5 |
| +6s | 4 / 4 | 1 / 1 | 4 | 5 |
| +8s | 4 / 0 | 1 / 1 | 3 | 5 |
| +10s | 0 / 0 | 1 / 1 | 3 | 1 |

- 普通任务的准时性：20 条 `payment_check` 在前 10 秒内全部 `success`（`/stats` 的 `completed` 到 21），
  期间执行器池 1 个名额一直被占着，`queue_length=4` 说明普通队列始终满员——这正是 §2 说的
  "共享池时调度循环会卡在投递上"的场景，现在两条路各走各的。
- 队列满时任务留在堆与存储里：+10s 时 `pending=4`，其中 3 条还在堆里（`heap_size=3`）、
  1 条在执行器队列里，状态都是 pending；等到空位后逐条投递，最终 26 条全部 `success`、`failed=0`。
- 执行器池确实是 1 个并发：`executor run finished` 六条的 `duration_ms` 分别 8144/8131/8125/8130/8124/8126，
  完成时刻 07:42:43 → 07:43:24，严格串行（每条形同 8.1 秒 + 一次队列交接）。
- `/stats` 的 `running` 在 +2s～+8s 恒为 5 = 普通池 4 + 执行器池 1，与第 7 条的合成分工一致；
  同一时刻 `/admin/runtime` 的拆分字段给的是 `running=4`、`exec_running=1`。
- 顺带跑出的一条反向证据：第一轮冒烟的 payload 误写成 `{"seq":1}`，
  六条档位任务全部以 `permanent=true`、`retry_count=0` 落 failed（E08 的提交校验 + E12 的不重试），
  说明档位任务确实进了执行器池并在跑，也说明分池没有改变失败路径。

### 未验证

- 优雅关闭时 `Stop` 等两个池的在途任务：只有 `TestStop_WaitsBothPools` 一条单元证据。
  真实进程侧没法在 Windows 上发 SIGTERM（Git Bash 的 `kill` 打不到 Windows 进程），
  冒烟的收尾用 `taskkill /F` 硬终止，因此"关停不打断档位任务"这一条只在测试里有据。
- Linux 侧的 `go test ./core -race -count=5`：WSL2 没有 gcc，`-race` 跑不起来；
  本轮也没有在 WSL 里重跑不带 `-race` 的 core 包（E10 建的临时环境未复用）。
- 通知被合并时的最坏延迟：没有测量"漏一次唤醒 → 500ms 后补投递"的实际时长，
  兜底超时只有 `TestExecClass_NoBusyWait` 间接证明它在起作用。
- macOS 分支（本机没有 Darwin 环境，只做了 `GOOS=darwin` 的编译与 vet）。
- `docs/api.md` 的 `running` 措辞与四个新字段：按 §3.6 留给 E19，本卡未改文档。

### 留给后续卡片的接口形状

1. E14 的崩溃恢复要区分池：`Restore` 已经按注册表给恢复出来的任务盖好 `class`，
   按档位判定 `restore_policy` 时直接读这个字段，不要再回头查注册表。
2. E16 的提交权限判定不应从 `handlerClasses` 反查（那是调度器的内部表，且在 `core` 里）；
   入口是 `executor.Registry`，api 侧本来就握着它。
3. E18 读 `/admin/runtime` 的四字段即可，不需要新端点。若要做"两池"视图，
   注意 `Running` 只含普通池，页面上要写清楚是哪个池。
4. 若将来要运行时调整池大小，先解决 `SetConcurrency`/`SetExecConcurrency` 在 `Start` 之后忽略这条限制
   （§8 明确列为非目标，本卡保持原样）。
5. `dispatchable` 是"按池决定是否投递"的唯一入口。以后再加类别（例如按档位再拆池）
   只需扩这个函数与 `dispatch` 的选路，堆侧的 `PopIfDueWhere` 不用动。
