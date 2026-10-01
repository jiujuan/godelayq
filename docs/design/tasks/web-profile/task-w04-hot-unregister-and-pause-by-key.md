# TASK-W04　调度器的热摘除与按类型批量暂停

- 所属阶段：M1 可变注册表
- 依赖任务：无（与 W01-W03 并行）
- 涉及文件：`core/scheduler.go`、`core/scheduler_hot_registry_test.go`（新增）、`core/scheduler_test.go`
  （行号说明：本卡正文里的 `core/scheduler.go:NNN` 都是**动笔时**的位置；
  落地后新增了两个方法，其下的行号整体下移，现行位置见 §10.1）
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

- [x] 两个方法可用，`UnregisterHandler` 成对删两张表。
- [x] `PauseByHandlerKey` 复用 `pausePendingJob`，没有新写一份落盘逻辑（grep 确认）。
- [x] 运行中任务被跳过这一条既有注释也有用例。
- [x] `core/scheduler.go:266-267` 的注释已按事实改写，改写后的文字明确"入堆盖章不受影响"。
- [x] `SetEventPreviewLimit`/`SetRestoreGuard` 一行未改。
- [x] `go build ./...`、`go vet ./...`、`go test ./... -race` 全绿。

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

## 10. 实现记录（2026-10-01）

### 10.1 落地的接口与现行位置

`core/scheduler.go`：

- `UnregisterHandler(jobType string) bool` — `:292-300`，紧跟 `RegisterHandlerClass`（`:271-276`）。
  写锁内成对删 `handlers`/`handlerClasses`，返回该键此前是否注册着。
  注释三段：孤儿类别的后果（`HandlerClass` 仍回 `(JobClassExec, true)`，
  恢复守卫据此把已经没有处理函数的任务钉成 paused）、不停在跑的任务（`ForcePause` 的活）、
  I2"先落盘再生效"归调用方。
- `PauseByHandlerKey(handlerKey string) (int, error)` — `:924-956`，放在 `RetagGroup`（`:839-863`）之后。
  `store == nil` 或 `strings.TrimSpace(handlerKey) == ""` → `(0, nil)`；
  `LoadAll` 失败 → 包装错误返回；逐条 `snap.HandlerKey()` 匹配 →
  跳过 paused/成功/失败/取消 → 其余调 `s.Pause(snap.ID)`；
  `ErrJobNotPending`（正在执行）与 `ErrJobNotFound`（已被弹出或已被别的调用送走）静默跳过、不计条数；
  其它错误记 `logger.Error` 后继续。
- `RegisterHandlerClass` 的注释（`:263-270`）按事实改写：删掉"运行期没有人重新注册同一键"，
  换成"注册既可能发生在启动阶段（装配 `executors.commands`），也可能发生在运行期
  （档位的在线管理，`executors.web_enabled` 打开后由 api 层触发）；
  运行期的注册与摘除走 `RegisterHandlerClass`/`UnregisterHandler` 这一对，共用 `s.mu`，不需要新增锁"，
  并保留"类别盖章只发生在入堆时，堆里的任务不受影响"（D8）。
- `SetEventPreviewLimit`（`:242-254`）与 `SetRestoreGuard` 的 running-即-忽略一行未改。

测试：`core/scheduler_hot_registry_test.go`，12 个顶层用例，`core/scheduler_test.go` 零改动
（`newMockStore`/`collectEvents`/`snapshotOf` 现有helper够用）。

### 10.2 与本卡写法的差异

1. **文件名**：卡头写 `scheduler_hotregistry_test.go`，落地为 `scheduler_hot_registry_test.go`
   （与 `scheduler_exec_class_test.go`、`scheduler_restore_guard_test.go` 的下划线体例一致）。
2. **复用点在 `Pause` 而不是 `pausePendingJob`**：卡 §3.2 说"复用 `pausePendingJob`"。
   落地走 `s.Pause()`，它是 `pausePendingJob` 的调用方之一（`grep -n pausePendingJob core/scheduler.go`
   → 定义 `:497`、调用 `:492`（`Pause`）与 `:587`（`ForcePause` 的入堆分支）），
   因此落盘/唤醒/广播一条都没重写，DoD 那条成立。
   多带上的是 `Pause` 的两个哨兵判定，正是"运行中跳过"这条规则需要的判据——
   自己判 `isRunning` 会漏掉"刚被调度循环弹出、还没进 `cancelMap`"那一小段窗口。
   代价是"已 paused"必须由状态过滤先拦掉（`Pause` 对已暂停任务幂等返回成功，不拦就会重复计条数），
   这条已被下面的变异检查钉住。
3. **空键判定收紧**：`RetagGroup` 只拒空串（`:840-842`），这里用 `strings.TrimSpace` 连纯空白一起拒。
   空白键永远匹配不到任何注册键，传进来是调用方的 bug，不该让它在生产上跑一遍 `LoadAll`。
4. **卡 §5.2 的"重启读文件后仍是 paused"用真实存储**：mock store 落不了盘，
   该用例改用 `core.NewJSONFileStore(t.TempDir()/jobs.json)` 走
   `PauseByHandlerKey → Flush → Close → 重新打开 → Restore`，断言重开之后那条仍在盘上是 `paused`、
   且 `Restore` 没有把它放回堆（另一条同类型的普通任务则照常重排）。
5. 卡 §5.1 的"摘除后任务照常在 `executeJob` 判失败"按现状断言（收到 `job.failed`、快照 `status=failed`），
   没有新增任何行为；日志里那条 `no handler registered for job` 是既有分支。

### 10.3 验证证据

```
$ go test ./core -run 'TestScheduler_UnregisterHandler|TestScheduler_PauseByHandlerKey' -race -v
    → 12/12 PASS，ok godelayq/core 1.5s
$ go test ./core -race -count=3                                   → ok 38.8s（无 flake）
$ go build ./... && go vet ./...                                  → 通过
$ go test ./... -race -count=1  → ok godelayq/api 207.7s
                                  ok godelayq/cmd/server 5.7s
                                  ok godelayq/core 13.3s
                                  ok godelayq/executor 25.6s
                                  ok godelayq/store/sqlite 3.7s
```

`api/`、`executor/`、`cmd/server/` 本轮零改动即通过：`UnregisterHandler`/`PauseByHandlerKey`
目前没有生产调用方（W05/W06 才接线），因此全绿不代表"接线没问题"，只代表没有回归。

### 10.4 反向验证（三处变异）

每次只临时改一行，跑相关用例，确认它们真的会红，然后恢复原状（改前后 `go build` 均通过）：

| 变异 | 预期变红的用例 | 实测 |
| --- | --- | --- |
| 只删 `handlers`，不删 `handlerClasses` | `_ClearsBothHandlerTables` | `FAIL`（`HandlerClass` 的 `ok` 断言） |
| 状态过滤去掉 `StatusPaused`（已暂停的也再走一遍） | `_TouchesOnlyItsOwnPendingJobs`、`_SecondCallIsIdempotent` | 两条都 `FAIL`（条数 1→2；第二次调用返回非 0） |
| 逐条处置改成 `ForcePause` | `_SkipsRunningJobWithoutCancellingIt` | `FAIL`（条数 1→2、快照状态 running→paused=5） |

第三行正是 §9 风险表点名的那条"误伤 running"，哨兵有效。

### 10.5 手工验收

卡 §5.4 约定本卡无端点、现场走查留到 W06 §7，这里只做了**回归冒烟**（临时目录，跑完删除，未碰仓库 `data/`）：
一份临时 config 打开 `executors.enabled`、`required_role: operator`、静态 token，
`workspace`/store/产物目录全部指向 `%TEMP%\w04smoke`，起真实进程：

- 启动日志 `executor handlers registered total=1 registered=1 unavailable=0 degraded=0`；
  `GET /executors` 里 `exec.hello_demo` 的 `runtime_ok=true`。
- 提交 `exec.hello_demo`（`payload.args.day=today`）→ `success`，
  `exec.preview="hello from godelayq executor --day=today\n"`、`attempts=1`、`duration_ms=116`。
- 另一条 `delay: 30m` 的同类任务 → `POST /jobs/:id/pause` 回 `paused`，
  盘上 `jobs.json` 该条 `status=5`（这就是 `PauseByHandlerKey` 逐条复用的那条路径）。
- 硬杀进程后重启：`GET /stats` 为 `paused:1, heap_size:0`，那条暂停任务**没有**被复活重排——
  与 §10.3 那条落盘用例在真实进程里对上。
- `resume` → `pending`（剩余 29m2s），`cancel` → 随后 `GET` 404，`stats` 清零。

**未观测**：`UnregisterHandler` 与 `PauseByHandlerKey` 本身在真实进程里没有任何调用入口
（W06 的 DELETE 端点才触发），本节的冒烟只证明"加了这两个方法之后既有链路没坏"。

### 10.6 缺陷

本卡没发现生产缺陷；D-0101 仍挂着。

### 10.7 未覆盖项

- 纯内存部署（`store == nil`）里堆中的 pending 任务不会被这个动作暂停，与 `RetagGroup` 同口径，
  已在方法注释里写明"没有存储就没有可枚举的对象"。要覆盖它得给堆加一个全量遍历接口，本卡不做。
- "摘除之后仍能提交该类型任务"只断言到执行侧判失败，没有断言 HTTP 提交口的表现——
  那是 W06 的端点用例（`gateExecutorSubmission` 看的是登记表，不是调度器注册表，两边一致性归 W05/W06 验）。
- `PauseByHandlerKey` 与 `scheduleLoop` 弹出任务的竞态窗口没有专门造用例（会返回少计一条，
  方向安全：漏暂停 ≠ 误中止），依赖 D6"跑完就落终态"兜底。
