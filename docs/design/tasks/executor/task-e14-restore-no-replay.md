# TASK-E14　崩溃恢复不重复执行已启动的执行器任务

- 所属阶段：M3 隔离
- 依赖任务：TASK-E13
- 涉及文件：`core/scheduler.go`、`cmd/server/main.go`、`core/scheduler_recovery_test.go`（或新文件）、`api/handlers_lifecycle.go`（恢复路径核对）
- 预计规模：小

## 1. 任务目标

服务崩溃或强杀之后重启时，把"崩溃那一刻正在执行"的执行器任务标记为 `paused`，而不是自动重新跑一遍。

## 2. 背景与当前问题

`core/scheduler.go` 的 `Restore` 会把所有非终态、非 `paused` 的快照重新入队，并把状态改成 `StatusPending`（那里的注释写得很清楚）。这个行为对示例处理函数无害——它们只是打日志。对执行器任务就是问题：一个正在执行 `INSERT`、下单、发信、删文件的进程被强杀，重启后又跑一遍，可能造成重复的对外副作用。

关键在于"上次到底跑没跑完"是无法得知的：进程被 `kill -9` 时来不及写任何结论，快照里留在 `StatusRunning`（`executeJob` 在执行前会 `store.Update` 落一次 running 态）。所以 `running` 快照的含义就是"结果未知"，对未知结果自动重跑，等于让系统替人做决定。

注意区分两种情况，本卡只管第二种：

| 崩溃时的快照状态 | 含义 | 处理 |
| --- | --- | --- |
| `pending` | 从未开始执行 | 照常重排（现有行为不变） |
| `running` | 已开始执行，结果未知 | 本卡：默认置为 `paused`，等人确认 |

正常优雅关闭不走这里：`core/scheduler.go` 的 `handleInterrupted` 会把被打断的任务落成 `pending`（注释里写明"交由下次 Start 的 Restore 重新入队"），也就是运维主动停服重启的场景仍然会自动重跑。这条差异要在代码注释和文档里都写清楚，否则很容易被理解成"执行器任务永不重跑"。

## 3. 要实现的功能

1. `core` 增加恢复钩子（避免 `core` 依赖 `executor` 与配置）：

   ```go
   // RestoreGuard 在 Restore 逐个处理快照时被调用，返回 want=true 表示
   // 用 newStatus 覆盖默认的 StatusPending 复位结果，且此时不入堆。
   type RestoreGuard func(snap JobSnapshot) (newStatus JobStatus, want bool)

   func (s *Scheduler) SetRestoreGuard(g RestoreGuard)
   ```

   - 钩子为 nil 时行为与现在完全一致（兼容底线，所有既有测试不改）。
   - 钩子返回"要改状态"时：更新内存中的快照状态，通过 `store.Update` 落一次盘，并**不 PushItem**。
   - 钩子内 panic 时按"不改"处理并记 error（恢复不能因为钩子失败而中断，否则任务直接消失）。
   - 落盘失败的后果是"下次启动再判一次"，可接受，但必须记日志。
2. `cmd/server/main.go` 装配守卫：`executors.enabled` 为 true 且 `restore_policy == "pause"` 时安装；`replay` 时不安装。守卫逻辑：

   ```go
   if snap.Status == int(core.StatusRunning) && classOf(snap.HandlerKey()) == core.JobClassExec {
       return core.StatusPaused, true
   }
   ```

   需要 E13 的类别查询：给 `Scheduler` 加 `HandlerClass(key string) (JobClass, bool)`。
3. 数量可观测：恢复时如果被置为 paused 的有 N 个任务，记一条 info 日志 `paused N executor jobs after crash`，并在 `GET /api/v1/stats` 现有 `paused` 计数里自然体现（`GetStats` 已按快照状态统计 `Paused`，无需改）。
4. 事件：为每个被置为 paused 的任务发布一条 `EventJobPaused`，`Metadata` 里带 `{"reason":"restore_after_crash","forced":true}`。理由：控制台的暂停提示与 `web/src/composables/useJobEvents.ts` 的时间线都读这条事件，没有事件的话运维只会看到一个莫名的 paused 状态。
5. 恢复动作：这类任务用现有 `POST /jobs/:id/resume` 即可回到待执行；`Resume` 的实现是否需要改动要在本卡验证——`Resume` 会把 paused 重新排期，`TriggerAt` 已过期时按既有的"过期立即补跑"逻辑走（`Schedule`/`Restore` 注释里提到这条口径）。手工确认一次，结论写进本卡第 6 节。
6. 单个任务恢复之外，还要确认批量恢复接口 `POST /jobs/batch-ops`（`api/handlers_lifecycle.go`）对这批 paused 任务可用，不需要新增端点。

## 4. 实现步骤

1. 在 `Restore` 里加钩子调用点，位置在 `job.Status = StatusPending` 之后、`heap.PushItem` 之前。
2. 加 `HandlerClass` 查询方法（E13 的 `handlerClasses` map 已在）。
3. `main.go` 装守卫。
4. 写测试；第 5.1 条要构造真实的崩溃现场（见下）。

## 5. 测试要求

崩溃现场的构造方式：直接用 `store.Update(JobSnapshot{Status: int(StatusRunning), ...})` 写一份"上次正在执行"的数据文件，再新建 `Scheduler` 并 `Start()`。这是 `core/scheduler_recovery_test.go` 里已有的做法，沿用即可，不需要真的 `kill -9` 子进程。

1. `core`：
   - `TestRestore_GuardSkipsRunningExec`：running 的执行器快照 + 守卫 → 结果状态 `StatusPaused`、不在堆里、`store` 里也是 paused、发布了一条 `job.paused` 事件且 `metadata.reason=="restore_after_crash"`。
   - `TestRestore_GuardIgnoresPending`：pending 的执行器快照 → 照常入堆（证明"未开始执行"不受影响）。
   - `TestRestore_NoGuardUnchanged`：不设守卫 → 与现有行为逐条一致（回归底线）。
   - `TestRestore_GuardPanicIsSafe`：钩子 panic → 任务照常入堆、记 error、`Restore` 不返回错误。
   - `TestRestore_GuardStoreUpdateFailure`：用会失败的 store 桩 → 记日志、不中断后续快照处理。
   - `TestRestore_GuardOnlyAffectsExecClass`：多个 running 快照（一个执行器、一个普通）→ 只有执行器被置为 paused，普通的照常复活。
2. `cmd/server`：
   - `TestRun_InstallsRestoreGuardWhenPausePolicy`：`enabled=true` + `restore_policy=pause` → `spyScheduler` 收到守卫安装；`replay` → 未安装。
   - 复用既有 `runtimeDeps` 替换闭包的写法，不要起真服务。
3. 手工验证（第 3.5 条要求的结论）：起服务、提交一个 60 秒的执行器任务、`kill -9` 服务端进程、重启，确认该任务显示 `paused`；点"恢复"，确认它重新排期并真的执行了一次。把这条流程补进 E19 的部署文档。

## 6. 完成标准（DoD）

- [ ] 未启用执行器或未装守卫时，`Restore` 的行为与改动前逐条一致（回归用例）。
- [ ] "崩溃时 running"与"崩溃时 pending"两种输入有不同结果，且都有用例。
- [ ] 优雅关闭路径仍然自动重跑，这条差异写在 `Restore` 与守卫安装处的注释里，并写进文档待办。
- [ ] 这些任务能通过现有 `resume` / `batch-ops` 恢复，手工验证过并记录结论。
- [ ] 钩子失败（panic 或 store 写失败）不会导致任务丢失。
- [ ] `go test ./core -race -count=3` 稳定通过（恢复路径涉及启动顺序，需要重复跑）。

## 7. 验收方式

```bash
go build ./... && go vet ./... && go test ./... -race
go test ./core -run 'Restore' -race -count=3 -v
go test ./cmd/server -run 'RestoreGuard' -v
```

## 8. 不在本任务范围

- 不做幂等键、去重表、执行租约（彻底解决重复副作用要靠这些，见设计文档 §11 第 5 条，本期不做）。
- 不改 `handleInterrupted` 的 pending 落盘语义（那是优雅关闭路径，行为要保持）。
- 不给 paused 任务加新的状态或子标记（`Metadata.forced` 已能区分来源）。
- 不做自动超时判废（置为 paused 后无限期等待人工处理是预期行为）。

## 9. 风险与回滚

- 风险：运维看到一批 paused 任务会以为是系统故障。缓解：事件带 `reason`、日志有汇总计数、E18 的前端提示要专门处理这一种来源。
- 风险：守卫依赖 `HandlerClass`，而类别来自注册表。若某档位因配置改动被删除，其历史任务的 `HandlerKey` 查不到类别 → 守卫返回"不改"，任务会被自动重跑；而 `executeJob` 又会因找不到处理函数直接判失败（`no handler registered`），副作用不会真的发生。这条链条要在注释里写清，避免误以为"档位删除=任务被重放"。
- 风险：`store.Update` 在 `Restore` 过程中被调用，而此时 `Restore` 正在遍历 `LoadAll` 的结果。要求遍历用快照的副本切片（`LoadAll` 已返回新切片），且不能在持锁状态下调用 store（避免与合并落盘协程互锁）。
- 回滚：删掉守卫安装即恢复旧行为，`Restore` 的钩子调用点可以留着（nil 钩子等价于无操作）。

## 10. 实现记录（2026-09-30）

落地文件：`core/scheduler.go`（`RestoreGuard` 类型、`RestoreReasonAfterCrash` 常量、`SetRestoreGuard`、
`HandlerClass`、`Restore` 里的改判点、`askRestoreGuard`、`holdRestored`）、
`core/job.go`（`JobSnapshot.HandlerKey`）、
`cmd/server/main.go`（`schedulerAPI` 两个新方法、`installRestoreGuard`、`pauseRunningExecOnRestore`）、
新增 `core/scheduler_restore_guard_test.go`（8 条）与 `cmd/server` 两条装配用例，
`core/scheduler_test.go` 的 `mockStore` 加了一个可注入的 Update 失败。

### 与卡片的偏离与补充

1. **`SetRestoreGuard` 在 `Start` 之后不生效并记日志**（§3.1 没提这条限制）。
   加它的理由是 `Restore` 是 `Start` 的第一步：晚一步装钩子，任务已经排期甚至跑起来了，
   静默生效只会让装配顺序写错这件事看不出来。与 `SetConcurrency` 等既有 setter 同一体例。
2. **§3.2 的 `classOf(...)` 用公开方法 `HandlerClass(key) (JobClass, bool)` 实现**，
   守卫要求第二个返回值为 true 才改判。这正好落进 §9 第二条风险：档位被删掉之后它的历史任务查不到类别，
   于是放行、重新排期、在 `executeJob` 里因找不到处理函数直接判失败——副作用不会再来一遍。
   这条链条写在 `pauseRunningExecOnRestore` 的注释里。
3. **新增 `JobSnapshot.HandlerKey()`**（§3 没列 `core/job.go`）：守卫拿到的是快照不是 `Job`，
   而"Type 优先、回退 Name"这条规则必须与入堆时那次查表完全相同，否则同一个任务会出现
   "排期时算档位、恢复时算普通任务"。两份实现只差在接收者类型，注释里互相钉了同步要求。
4. **钩子返回非 `paused` 状态时不发暂停事件**，改为记一条 warn。
   §3.4 的事件形状是按 paused 写的；将来的用法若要改判成别的状态，
   发 `job.paused` 就是在给出一条不成立的说明，那种用法需要自己的事件类型。
   因此该分支目前只有代码路径、没有用例（见"未验证"）。
5. **事件 `metadata` 除 `reason`/`forced` 之外带 `trigger_at` 与 `attempts`**：
   与 `Pause`、`parkForcedPause` 两处已有的暂停事件对齐，控制台时间线读 `attempts`。
   这条在本卡特别有用——重启后事件历史是空的（进程内存），
   "上次到底跑过几轮"只能从事件的 `attempts` 与存储的时间字段读出来。
6. **汇总日志用结构化字段而不是把数字写进句子**：§3.3 给的文本是
   `paused N executor jobs after crash`，实现为
   `msg="paused executor jobs after crash" count=N reason=restore_after_crash`，
   与本仓库其余日志的写法一致（采集端按字段索引，不解析句子）。
7. **§9 第三条（遍历期间不能持锁调 store）核对结果：满足**。
   `Restore` 全程不持 `s.mu`——只在读注册表盖章与取钩子时短暂加读锁，取完即放；
   `store.LoadAll` 返回的是新切片，改判时写的 `store.Update` 不会改动正在遍历的集合。
   因此守卫可以放心回头调 `HandlerClass`（它要拿读锁），不会自己和自己撞死。
8. **`restore_policy` 的比较带 `TrimSpace`**，与配置校验（`core/config.go`）同一口径：
   认不出的取值一律走安全侧装守卫，只有明确写 `replay` 才不装。
9. **测试放在新文件** `core/scheduler_restore_guard_test.go`（§5.1 允许"或新文件"）；
   `cmd/server` 三条装配用例共用一个 `runWithExecutorConfig` 骨架，只换配置取值。
   `mockStore` 加了 `updateErr` 字段——此前只能模拟 Save/Load 失败，改判落盘这条路径没有注入口。
10. **多做一条用例**：`TestRestore_GuardPausedJobCanBeResumed`（§3.5 只要求手工确认）。
    它同时钉住两件事——被停住的任务能用现有 `Resume` 走回 pending，
    以及重新入堆时类别照旧盖章、回到的是执行器池。
    §3.6 的批量恢复核对结论：端点与调度器都不需要改，`Scheduler.Resume` 是同一条路径，
    端点侧已有 `api/handlers_lifecycle_test.go` 的 batch-ops resume 用例覆盖。

### 验证结果

Windows 本机（`10.0.26200`，go1.26.4 windows/amd64）：

| 命令 | 结果 |
| --- | --- |
| `go build ./... && go vet ./...` | 通过 |
| `go test ./... -race -count=1` | api 90.1s、cmd/server 5.5s、core 12.3s、executor 21.6s 全 ok |
| `go test ./core -race -count=3` | ok（DoD 第六条要求重复跑，恢复路径与启动顺序有关） |
| `go test ./core -run 'TestRestore_\|TestScheduler_SetRestoreGuard' -race -count=2` | 8 条用例两轮全 PASS |
| `GOOS=linux/darwin/windows` 的 `go build` + `go vet`、`go build -tags dashboard ./...` | 全部通过 |
| 既有用例 | `scheduler_recovery_test.go`、`pause_test.go`、`suspend_test.go`、`api/handlers_lifecycle_test.go` 断言一字未改，全绿 |

冒烟（Windows 真实服务端 + REST，独立配置在临时目录，`scheduler.workers=4`、
`executors.concurrency=2`、`queue_capacity=2`；档位 `exec.sleep25` = `kind: binary` +
`program: cmd` + `fixed_args: [/c, sleep25.cmd]`；普通任务用 `report_generate`（10 秒）；
提任务用静态 token，读运维端点用 ops 账号的 JWT）：

**A. `restore_policy: pause`（默认）**

1. 提交 2 条档位任务 + 1 条 `report_generate`，轮询到 `/admin/runtime` 报
   `exec_running=2`、`running=1`（三条都在执行中）后 `taskkill /F` 硬杀服务端（等价于 `kill -9`：不给任何收尾机会）。
2. 杀完之后直接读 `jobs.json`：三行全是 `status=1`（running），`attempts=1`——
   这就是本卡要处理的输入，不是构造出来的假数据。
3. 重启后的启动日志两行：
   `restored jobs from store count=1`、`paused executor jobs after crash count=2 reason=restore_after_crash`。
4. `GET /jobs/<id>`：两条档位任务都是 `paused`，`exec` 摘要为 `null`
   （崩溃没留下结论，改判不替它编一个）；`report_generate` 已自动重新排期并跑到 `success`。
5. `GET /jobs/<id>/events` 只有一条 `job.paused`，
   `metadata={"attempts":1,"forced":true,"reason":"restore_after_crash","trigger_at":"..."}`。
6. 确认动作两条路都走通：`POST /jobs/<id>/resume` → 200、状态 pending；
   `POST /jobs/batch-ops {"action":"resume","ids":[...]}` → 207、`succeeded=1`、状态 pending。
   恢复后各自真的执行了一次（`executor run finished ... exit_code=0 duration_ms=25400 / 25377`），
   最终 `/stats` 为 `paused=0`、`completed=3`、`failed=0`。

**B. `restore_policy: replay`（同一条档位任务，同样的硬杀）**

重启后的日志只有 `restored jobs from store count=1`，没有 paused 汇总；
`GET /jobs/<id>` 直接是 `running`——策略开关确实只影响装不装守卫，`Restore` 的默认行为一字未动。

### 未验证

- Linux 下的 `kill -9` 现场：本轮用 Windows 的 `taskkill /F` 构造崩溃（同样不给进程收尾机会），
  没有另行在 WSL 里重做一遍。改判逻辑与平台无关，但"崩溃现场"这一环只有一侧的实跑记录。
- 钩子返回 `paused` 之外状态的分支（第 10 节第 4 条）：只有代码路径与 warn 日志，没有用例。
- 优雅关闭路径的对照：§2 那条差异写在注释与 E19 文档项里，冒烟 A 里 `report_generate`
  的行为是"崩溃后自动重跑"的证据，不是"优雅关闭后自动重跑"的证据（后者是既有行为，未回归测试）。
- macOS 分支（只做了 `GOOS=darwin` 的编译与 vet）。
- 崩溃瞬间留在系统里的子进程：强杀服务端之后 `cmd.exe`/`ping.exe` 会脱离父进程继续跑完
  （E11 的整树终止只挂在取消与优雅关闭两条路径上）。本卡不改这条，属已知限制。
- `docs/deployment.md` 与 `docs/api.md` 的相关措辞：按 §3 归 E19，本卡只把结论与操作步骤登记进
  E19 卡片的 §3.1 第 2 条与 §3.2 第 9 条。

### 留给后续卡片的接口形状

1. E16 的提交权限、E18 的前端都按 `metadata.reason == core.RestoreReasonAfterCrash`（字符串
   `restore_after_crash`）区分来源，常量已在 `core` 导出，不要再抄字面量。
2. E18 的暂停提示要把"崩溃后停住"与"人停的"分开显示：两者都是 `StatusPaused` + `forced:true`，
   只有 `reason` 能分辨（E19 §3.1 已登记该字段的说明）。
3. `RestoreGuard` 是通用的状态改判入口。若以后要加"崩溃后自动重跑但留一条审计事件"这类策略，
   新装一个返回 `StatusPending` 的钩子即可，`Restore` 不需要再改；
   但要注意 §3.1 的约定：钩子返回 hold=true 就代表"不进堆"，要重排就别拦。
4. HTTP 档位（E15）的崩溃恢复不需要另装守卫：判定看的是快照状态与类别，与 `kind` 无关。
