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
