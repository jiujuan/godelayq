# TASK-W04　调度器的热摘除与按类型批量暂停

- 所属阶段：M1 可变注册表
- 依赖任务：无（与 W01-W03 并行）
- 涉及文件：`core/scheduler.go`、`core/scheduler_hotregistry_test.go`（新增）、`core/scheduler_test.go`
- 预计规模：小–中

## 1. 任务目标

给 `core.Scheduler` 两个新能力：运行期摘除一个任务类型（`UnregisterHandler`），
以及把某个任务类型下所有未终态任务置为 `paused`（`PauseByHandlerKey`）。
本卡是"删除档位"（W06）的两个后端前置动作，与档位、执行器、页面都无关。

## 2. 背景与当前问题

注册表 `handlers`/`handlerClasses` 已经在 `s.mu` 保护下成对写
（`core/scheduler.go:269-272`），读侧全部 `RLock`（276-279、289-294、303-309），
所以**加锁不是本卡要解决的问题**。缺的是：

1. 没有摘除入口。删档位后若表里还留着 handler，那条任务类型仍能提交，
   而它的档位定义已经不在登记表里了——这是最危险的组合。
2. `core/scheduler.go:266-267` 写着"类别在注册时定下，入堆时按注册表盖章，之后改注册不会影响
   已经在堆里的任务——注册本来就发生在启动阶段，运行期没有人重新注册同一键"。
   这句话今天仍然对（唯一的写入口是 `executor/register.go:84` 与 `cmd/server/main.go:570-573`），
   但 W05/W06 会打破它，注释必须同步改写，否则后来人照它推理会得出错误结论。
3. 删档位时那批已提交的任务需要一个批量处置动作。现有形状可参照 `RetagGroup`（812-836）：
   遍历快照 → 匹配 → 逐条动 → 返回条数，非原子且幂等。

## 3. 要实现的功能

1. `func (s *Scheduler) UnregisterHandler(jobType string) bool`
   - 写锁内 `delete(s.handlers, jobType)` 与 `delete(s.handlerClasses, jobType)` **成对做**，
     与 271-272 的成对写对称。
   - 返回该键此前是否注册着（false 表示什么也没动）。
   - 不取消任何在跑的任务，不清 `cancelMap`（那是 `ForcePause` 的活，D6 明确划清界限）。
2. `func (s *Scheduler) PauseByHandlerKey(handlerKey string) (int, error)`
   - 照 `RetagGroup` 的形状：`store.LoadAll()` → 按 `snap.HandlerKey()` 匹配 → 逐条处置 → 返回条数。
   - 处置规则：
     - 在堆里的（pending）：从堆摘出，落 `paused`，语义与 `Pause`（451）一致——
       **复用 `pausePendingJob`**，不要另写一份落盘逻辑。
     - 正在执行的：`isRunning(jobID)` 为真时**跳过**，不动它（D6：不强杀），
       返回条数不计它。这条要写进注释与用例，因为它和"删档位=中止"的直觉相反。
     - 已是 `paused` / 终态：跳过（幂等，重复调用不改变结果）。
   - `store == nil` 或空键 → `(0, nil)`，与 `RetagGroup:813-815` 同一条。
   - 中途单条失败：记 Error 日志、继续后面的、不算进条数（同 `RetagGroup:826-833`）。
3. 改写 `core/scheduler.go:266-267` 那段注释，把"运行期没有人重新注册同一键"换成事实：
   运行期确实可能重注册与摘除（档位在线管理，`web_enabled` 打开后由 api 层触发），
   但类别盖章仍然只发生在入堆时——已在堆里的任务不受影响（D8）。
4. 不改 `SetEventPreviewLimit`/`SetRestoreGuard` 的 running-即-忽略（249-252）：
   它们改的是全局执行参数，与注册表无关；本卡**不新增** running 检查，
   并在 §8 里说明为什么这两个方法的行为与新入口不同。

## 4. 实现步骤

1. 加 `UnregisterHandler`，紧跟 `RegisterHandlerClass` 之后，成对删 + 注释说明"调用方负责先落盘再生效"（I2）。
2. 加 `PauseByHandlerKey`，放在 `RetagGroup` 旁边，注释里点名"形状抄 `RetagGroup`，
   差别只在匹配键与不动运行中任务"。
3. 改写 §3.3 那段注释。
4. 补测试（§5）。
5. `go build ./... && go vet ./... && go test ./core -race`，再全量。

## 5. 测试要求

1. `UnregisterHandler`：
   - 注册 → 摘除 → `LookupHandler` 与 `HandlerClass` 都回 `ok=false`（后者是恢复守卫的判据，
     `cmd/server/main.go:613-624` 依赖它）。
   - 摘除不存在的键 → `false` 且注册表其余内容不变。
   - 摘除后同名再注册 → 一切恢复可用（W06 的 PUT 走的就是这条路径）。
   - 摘除不影响已在堆里的任务：先 `Schedule` 一条，再摘 handler，任务仍在堆里；
     触发时按 `executeJob` 的既有分支判失败（不新增行为，只是断言现状）。
2. `PauseByHandlerKey`：
   - 三条 pending 同类型 → 返回 3，全部 `paused`，重启读文件后仍是 `paused`
     （照 `core/scheduler_test.go` 里既有的落盘断言方式）。
   - 混合状态（pending + paused + 终态 + 另一类型）→ 只动该类型的 pending，计数准确。
   - **运行中的那条被跳过**：用 `isRunning` 可控的替身/慢 handler 造出 running，
     调用后它既不是 paused 也没被取消，返回值不含它。
   - 幂等：连续调用两次，第二次返回 0。
   - `store == nil` → `(0, nil)`。
3. 并发：8 个 goroutine 循环 `Schedule`+`UnregisterHandler`，`-race` 下通过。
   不要求断言业务结果，只要求不产生 data race（这条守的是"新增写入口没破坏 s.mu 的覆盖范围"）。
4. 手工：本卡无端点，跳过；`PauseByHandlerKey` 的现场走查在 W06 §7 做。

## 6. 完成标准（DoD）

- [ ] 两个方法可用，`UnregisterHandler` 成对删两张表。
- [ ] `PauseByHandlerKey` 复用 `pausePendingJob`，没有新写一份落盘逻辑（grep 确认）。
- [ ] 运行中任务被跳过这一条既有注释也有用例。
- [ ] `core/scheduler.go:266-267` 的注释已按事实改写，改写后的文字明确"入堆盖章不受影响"。
- [ ] `SetEventPreviewLimit`/`SetRestoreGuard` 一行未改。
- [ ] `go build ./...`、`go vet ./...`、`go test ./... -race` 全绿。

## 7. 验收方式

```bash
go test ./core -run 'TestScheduler_UnregisterHandler|TestScheduler_PauseByHandlerKey' -race -v
go test ./core -race -count=3
go build ./... && go vet ./...
```

预期：全部 `ok`；`-count=3` 无 flake。

## 8. 不在本任务范围

- 不碰 `executor` 包、不读 `web_enabled`、不知道"档位"这个概念（`core` 不许 import `executor`）。
- 不做强制中止在跑任务（`ForcePause` 已有，档位删除不附带它，D6）。
- 不改 `SetEventPreviewLimit`/`SetRestoreGuard` 的 running 拒绝——**这两处与本卡的新入口是两类事**：
  前者改全局执行参数（改到一半会让已发出的事件与后续事件口径不一），后者只增删注册表的条目。
  §3.4 要求原样保留，别"顺手统一"。
- 不做堆里任务的 `job.class` 回溯修正（D8）。

## 9. 风险与回滚

| 风险 | 说明 | 处置 |
| --- | --- | --- |
| 删了 handler 但表里仍有档位（W06 顺序错） | 该类型任务能提交、没有档位定义可用，`gateExecutorSubmission` 直接放行成普通任务 | I2"落盘先于生效"由调用方保证；本卡的注释里点名这条约束（§4.1） |
| `PauseByHandlerKey` 误伤 running | 与 D6 相反，等于隐式 force-pause | §5.2 那条跳过用例是哨兵 |
| 非原子的中途失败 | 部分任务已 paused | 与 `RetagGroup` 同口径（幂等 + 调用方重试），注释写清 |
| 注释没改 | 后来人照 266-267 推理认为运行期不写表 | DoD 单列一条 |

回滚：本卡纯新增两个方法 + 一段注释改写。退回删方法、还原注释即可，无调用点会断
（W06 尚未接线时这两个方法没有生产调用方）。

## 10. 实现记录（执行时补写）

（待补：落地的接口 / 与本卡写法的差异 / 验证证据 / 手工验收 / 缺陷 / 未覆盖项）
