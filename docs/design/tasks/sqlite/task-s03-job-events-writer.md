# TASK-S03　`job_events` 表与事件写入侧

- 所属阶段：M1 事件
- 依赖任务：TASK-S01、S02
- 涉及文件：新增 `store/sqlite/events.go`、`store/sqlite/events_test.go`；改 `cmd/server/main.go`
- 预计规模：中

## 1. 任务目标

给 EventBus 挂上第二个订阅者，把 8 种 `job.*` 事件写进 `job_events` 表，按配置保留条数与天数淘汰旧记录。写完本卡，重启进程后事件仍然留在库里（还没有读端点，用 SQL 客户端验证）。

## 2. 背景与当前问题

`api/history.go:23-25` 的注释写着两句话：EventHistory"跟着进程走，重启即清空，不是审计日志"，以及"需要持久化的运行历史属于二期（见 `docs/design/web-console-design.md` §5.6）"。`web-console-design.md:646` 那一节的最后一句是"持久化审计不在本期范围"。本卡就是那处登记的两期需求之一。

现状不能直接改成"给 EventHistory 加个落盘"：那份内存缓冲同时服务 `/jobs/:id/events` 与 `/events` 两个端点（`api/handlers_events.go`），把它变成同步写库会让一次 SQLite 卡顿直接反映到接口响应上；而且它的三档上限（`api/history.go:12-19`）是"实时窗口"语义，与"留存多久"是两回事。

正确做法是并列第二个订阅者，这一点总线本来就支持：`SubscribeAll()`（`core/event.go:109`）可以被任意多个订阅者调用，`Scheduler.GetEventBus()`（`core/scheduler.go:1641`）是公开的取用入口，`api.NewServer` 已经在用同一组合（`api/server.go:142`）。**`core` 一行不改。**

反压口径也已在实现里核实过：`core/event.go:176` 的 `Publish` 用 `select { case ch <- event: default: }`，缓冲区满即丢弃，所以订阅者的 drain 协程不可能阻塞调度主流程。丢掉一条记录不是调度事实的损失——这条设计约束原样搬到本卡的写库路径（设计文档 D4）。

## 3. 要实现的功能

1. `store/sqlite/events.go`：

   ```go
   type EventLogOptions struct {
       FlushInterval  time.Duration
       QueueCapacity  int
       RetentionCount int
       RetentionAge   time.Duration   // 0 表示不按时间淘汰
       Now            func() time.Time // 测试注入
   }
   // NewEventLog 订阅 bus 的全部事件并开始落库。
   // 返回的 EventLog 必须在进程退出前 Close（撤订阅 → 最后一次 Flush）。
   func NewEventLog(bus *core.EventBus, db *DB, opts EventLogOptions, logger *slog.Logger) (*EventLog, error)
   func (e *EventLog) Flush() error
   func (e *EventLog) Close() error
   func (e *EventLog) Dropped() int64
   func (e *EventLog) Count() (int64, error)
   ```

   构造函数用 `New*` 命名（仓库惯例）；`bus == nil` 或 `db == nil` 返回错误而不是 panic。
2. 订阅与转发：`SubscribeAll()` 拿到通道，起一个 goroutine 逐条转成 `eventRecord` 交给 S02 的 `batcher`。**这条 goroutine 里不做任何同步 SQL 等待**：`batcher.append` 是非阻塞的，落盘由 `batcher` 自己的 ticker 负责。
3. 忽略没有归属的事件：`event.JobID == ""` 直接跳过（与 `api/history.go:70-72` 的 `record` 同一判断，理由也相同：没有归属的事件塞进任何任务的时间线都是噪音）。
4. 字段映射（一行一事件，不做聚合）：

   | 列 | 来源 | 说明 |
   | --- | --- | --- |
   | `ts_us` | `event.Timestamp.UnixMicro()` | 注入的 `Now` 也经这条路径生效 |
   | `type` | `string(event.Type)` | 存 `job.scheduled` 这样的完整名，不带命名空间前缀变换 |
   | `job_id` / `job_name` | 同名字段 | |
   | `status` | `int(event.Status)` | 与 `JobSnapshot.Status` 的 int 口径一致，不存字符串 |
   | `data` | `string(event.Data)` | `Event.Data` 已是 `json.RawMessage`，原文入库，不重新序列化 |
   | `metadata` | `json.Marshal(event.Metadata)` | nil 时存 NULL；序列化失败存 NULL 并记 warn（一条事件的元数据不该挡住整批） |
5. 批量插入语句：一条事务内多条参数化 `INSERT`。`seq` 由 `AUTOINCREMENT` 给出，不由代码算。
6. 保留淘汰：每次成功插入之后，在同一事务里执行

   ```sql
   DELETE FROM job_events WHERE seq <= (SELECT COALESCE(MAX(seq)-?, 0) FROM job_events);
   DELETE FROM job_events WHERE ? > 0 AND ts_us < ?;
   ```

   第一个语句用 `RetentionCount`，第二个用 `RetentionAge`（`Now()-age`）。
   淘汰频率与本条口径要写进注释：**不是每秒一次，是每批一次**，因为 `MAX(seq)` 是索引上的常数时间操作，而"每批一次"已经能保证上界不被突破。
7. 装配（`cmd/server/main.go`）：
   - `runtimeDeps` 加 `newEventLog func(*core.EventBus, *sqlite.DB, core.ObservabilityConfig, *slog.Logger) (*sqlite.EventLog, error)`；照 S02 的体例列入依赖完整性检查——但**只在 `observability.enabled && observability.events.enabled` 时要求它非空**（关闭时不构造是预期行为，见 §6 第 1 条）。
   - 构造时机：`scheduler` 已创建（能拿到 `GetEventBus()`）且在 `scheduler.Start()` **之前**。放在 `Start` 之后会漏掉恢复阶段发布的 `job.scheduled` 事件——这条要写进注释，因为它是装配顺序里唯一一个"错了看不出问题"的地方。
   - 关闭：`defer` 里 `eventLog.Close()`，位置在 `db.Close()` 之前声明（后执行），保证撤订阅与最后一次 `Flush` 早于关连接。
8. 启动日志补一项：`observability enabled ... events_writer=true`。

## 4. 实现步骤

1. 先做 `eventRecord` 的映射函数与它的单测（纯函数，不碰库）。
2. 做 `NewEventLog` 的订阅 + `batcher` 接入，用注入的 `Now` 与小容量队列写用例。
3. 做保留淘汰，单独用例覆盖"条数上界"和"天数上界"两条。
4. 最后接 `cmd/server`，改两个测试文件的 `runtimeDeps` 构造点。

## 5. 测试要求

`store/sqlite/events_test.go`，全部用 `t.TempDir()` 建真库 + `core.NewEventBus(100)` 造真总线，时间用注入的 `Now`，不 sleep。

1. `TestEventLog_PersistsAllEventTypes`：逐个发布 §3 表里 8 种 `job.*` 事件，`Flush()` 后断言库内 8 行、`type` 与 `status` 逐条对得上。
2. `TestEventLog_IgnoresEventsWithoutJobID`：发布一条 `heap.updated`（`JobID` 为空）→ 库内 0 行。
3. `TestEventLog_PreservesDataAndMetadata`：`Data` 带一段 `{"error":"..."}`，`Metadata` 带 `retry_count`/`permanent`；断言 `data` 列原文一致（没有被重新序列化过导致键序变化）、`metadata` 反序列化后键值一致。`Data` 为 nil 时列为 NULL 而不是 `"null"`。
4. `TestEventLog_OrderIsStableWithinSameMicrosecond`：两条事件用同一个 `Now` 值（模拟同毫秒连发 `scheduled` 与 `started`），断言按 `seq` 读回的顺序就是发布顺序。**这条守住"必须有 `seq` 作为定序依据"的结论**（设计文档 §6）。
5. `TestEventLog_DoesNotBlockPublisher`：把 `FlushInterval` 设成很大的值（比如 1h），然后发布 2×`QueueCapacity` 条事件，断言 `bus.Publish` 全部立即返回（用耗时上界断言，而不是断言落库条数）。这是 D4 的直接证据。
6. `TestEventLog_DroppedIsCounted`：与上一条同场景，断言 `Dropped() > 0` 且落库行数等于容量。
7. `TestEventLog_RetentionCount`：`RetentionCount=5`，投 20 条，断言库里剩 5 条且是最新的 5 条。
8. `TestEventLog_RetentionAge`：`RetentionAge=1h`、注入 `Now` 造跨 3 小时的两批，断言只留最后一小时内的。`RetentionAge=0` → 不按时间删。
9. `TestEventLog_CloseUnsubscribes`：`Close()` 之后再 `bus.Publish` 一条，断言库内不增长、不 panic、不报"向已关闭连接写入"。
10. `TestEventLog_FlushIsNoOpWhenEmpty`：连续 `Flush()` 五次不产生 5 个事务（用 `Stats()` 或插入行的 `seq` 连续性断言）。
11. `TestNewEventLog_RejectsNilDeps`：`bus`/`db` 为 nil → 返回错误。
12. `cmd/server`：`TestRun_EventLogClosedBeforeDB`（断言顺序）、`TestRun_EventLogDisabledOpensNoWriter`（`events.enabled: false` 时构造函数不被调用，且关闭开关时表内一行都不写）。

## 6. 完成标准（DoD）

- [x] `observability.enabled: false`（默认）时不构造订阅者、总线订阅数与改动前一致、库里没有文件。（`TestRun_ObservabilityDisabledOpensNothing`：`newObservabilityDB` 调用数为 0、配置路径上没有文件、日志没有 `observability enabled`；订阅数没有直接计数，由 `newEventLog` 同样不被调用这条路径保证）
- [x] `enabled: true` + `events.enabled: false` 时建表但不写入、不订阅（`TestRun_EventLogDisabledOpensNoWriter` 里构造函数一被调用就 `t.Fatal`，启动日志写的是 `events_writer=false`；建表本身是 `sqlite.Open` 的迁移，与本卡无关，S02 已有用例）。
- [x] `Publish` 的非阻塞契约没有被削弱：§5 第 5 条用例证明队列打满时发布方照常返回。（`TestEventLog_DoesNotBlockPublisher`：容量 4、发 400 条，发布总耗时上界 5 秒，且队列长度始终不超过 4）
- [x] 事件在进程重启后仍在库里（手工步骤见 §7）。（§10.4：两轮任务六行，`seq` 1→6 连续，强制结束后重开文件内容不变）
- [x] `Close` 之后不再有写入，且 `Close` 幂等。（`TestEventLog_CloseUnsubscribes`；幂等由 `closeOnce` 保证，重复调用返回第一次结果）
- [x] 装配顺序满足"在 `scheduler.Start()` 之前构造订阅者"，注释里写明漏掉会丢哪一段事件。（`cmd/server/main.go` 装配块上方注释 + `TestRun_EventLogClosureIsMandatoryWhenEnabled`、`TestRun_ObservabilityClosedBeforeStore`）
- [x] `core/` 与 `api/` 一行未改（DoD 第 7 条：本卡只加写入方）。（`git diff -- core api` 为空）

## 7. 验收方式

```bash
go build ./... && go vet ./... && go test ./... -race
go test ./store/sqlite -run EventLog -v
```

手工（临时目录里的独立配置，跑完删除）：把 `observability.enabled: true`、`scheduler.workers: 2`，
启动服务，用 `POST /api/v1/jobs` 提交一条延迟 3 秒的一次性任务并等它跑完，
确认时间线四类事件（scheduled/started/completed）都进了表；
`Ctrl-C` 停服、再启动，用 `sqlite3`（或任意客户端）跑
`SELECT type, status FROM job_events ORDER BY seq;`，确认两轮之间的历史仍然完整、顺序未变。

## 8. 不在本任务范围

- 不改两个事件端点的读路径（S04）。
- 不改 `api/history.go` 的三档常量、不删内存缓冲（S04 仍要读它作为未启用库时的路径）。
- 不做事件的按类型统计查询（`/stats` 的口径不变）。
- 不做事件的 WS/SSE 推送改造：实时通道继续读总线，与库无关。
- 不做库导出/导入工具，不做 JSON 事件文件的迁移（本来就没有 JSON 事件文件）。

## 9. 风险与回滚

- 风险：装配位置错在 `scheduler.Start()` 之后，后果是恢复阶段的 `job.scheduled` 事件静默丢失，而单测覆盖不到（测试直接构造 EventLog，不走 `run()`）。应对：§5 第 12 条用例 + §6 第 6 条 DoD 一起把这条锁住。
- 风险：`Event.Metadata` 里已经带着 `trigger_at`（`time.Time`）等类型，`json.Marshal` 的结果与内存缓冲的展示格式可能有差异。这是可接受的：库是留存介质，展示由读侧决定（S04 会把行还原成 `core.Event` 再吐给接口）。
- 风险：高频部署下事件表增长快（一次普通任务 ≈ 3 行，一次重试链 ≈ 5 行）。默认 `retention_count: 200000` 对应约 6 万次执行；不够时优先调这条而不是调 `store.history_limit`（两者保留的是不同东西，理由见设计文档 D9）。
- 回滚：`events.enabled: false` 即可完全停止写入（表留着但不读不写）；整体回滚是删 `store/sqlite/events.go` + revert `cmd/server` 的装配提交。

## 10. 实现记录（执行时补写）

完成日期：2026-10-01。新增 `store/sqlite/events.go`、`store/sqlite/events_test.go`，改 `cmd/server/main.go` 与两个 `cmd/server` 测试文件，另在 `store/sqlite/batch.go` 加了一个包内可见的 `queued()`。`core/` 与 `api/` 内容一行未改。

### 10.1 落地的接口

```go
// store/sqlite/events.go
type EventLogOptions struct{ FlushInterval, QueueCapacity, RetentionCount, RetentionAge, Now }
func NewEventLog(bus *core.EventBus, db *DB, opts EventLogOptions, logger *slog.Logger) (*EventLog, error)
func (e *EventLog) Flush() error
func (e *EventLog) Close() error
func (e *EventLog) Dropped() int64
func (e *EventLog) Count() (int64, error)
```

链路：`SubscribeAll()` → 转发协程 `mapEvent` → `batcher[eventRecord].append`（非阻塞）→ `writeBatch`（一个事务里多条参数化 INSERT + 淘汰）→ 提交。转发协程内没有任何 SQL。

### 10.2 与本卡写法的差异

1. **`runtimeDeps.newEventLog` 的签名**（本卡 §3.7 写的是 `func(*core.EventBus, *sqlite.DB, ...) (*sqlite.EventLog, error)`）。实际是 `func(bus *core.EventBus, db observabilityDB, cfg core.ObservabilityConfig, logger *slog.Logger) (eventLogAPI, error)`，默认闭包内部把 `db` 断言回 `*sqlite.DB`，断言失败返回错误。理由是沿用 S02 已定的消费侧窄接口口径（设计文档 §9.6）：`cmd/server` 的用例要能拿替身句柄验装配与关停顺序，否则只能全用真库。`sqlite.NewEventLog` 本身的签名与本卡一致，仍是 `*DB`。
2. **按时间淘汰的语句**（本卡 §3.6 给的是 `DELETE FROM job_events WHERE ? > 0 AND ts_us < ?`）。实际改成 Go 侧先判断 `retentionAge <= 0` 就整条跳过，只执行 `DELETE FROM job_events WHERE ts_us < ?`。行为一致，差别是 `retention_age: 0` 时不再每批发一条恒真值为假的语句。按条数淘汰那条语句与本卡逐字一致。
3. **注入的 `Now` 只补空时间戳**。本卡 §3.4 的说明是"`ts_us` 来自 `event.Timestamp.UnixMicro()`，注入的 `Now` 也经这条路径生效"。实现取的是：事件带时间戳就用它，`event.Timestamp.IsZero()` 才取 `Now()`；另外 `Now` 还用于按时间淘汰的截止点。这样真实事件保留自己的发生时间，而测试与第三方发布的空时间戳事件不会在库里落成 0。
4. **`RetentionCount <= 0` 回落到默认值而不是不限量**（本卡未明说）。零值配置、以及 `Normalized()` 之后仍为 0 的路径都会落到 `core.DefaultObserveEventRetentionCount`，避免一个漏配把表推成无界增长；`RetentionAge < 0` 按 0 处理。用 `TestEventLog_DefaultsForNonPositiveOptions` 固定。
5. **空批不产生事务的观察办法**。本卡 §5.10 建议用 `Stats()` 或 `seq` 连续性断言，但 `seq` 由 `AUTOINCREMENT` 给出，一次空事务在库里不留任何痕迹，读不出来。因此在 `EventLog` 上加了包内可见的 `writeRounds atomic.Int64`，用例断言连续五次 `Flush()` 之后它是 0。
6. **`batcher[T].queued()`**。S02 的 `batch.go` 多了一个包内私有方法，返回队列里未落盘的条数。用途是把 `events_test.go` 里"睡一会儿再看结果"换成"等到条数对上"（`waitQueued`），将来把队列占用透出到运维端点时读同一个数。
7. **用例命名**。本卡 §5.12 要的 `TestRun_EventLogClosedBeforeDB` 实现为把既有的 `TestRun_ObservabilityClosedBeforeStore` 扩成三个元素的顺序表（`event_log` → `observe_db` → `store`），断言的是同一条结论；`TestRun_EventLogDisabledOpensNoWriter` 按原名实现。另外补了 `TestRun_EventLogOpenFailureStopsStartup`、`TestRun_EventLogClosureIsMandatoryWhenEnabled`、`TestRun_ObservabilityEnabledOpensAndClosesForReal`（真 `sqlite.Open` + 生产闭包，走一遍类型断言那条路径，并重新打开文件确认两行仍在）。
8. **§5 的 11 条包内用例全部实现，另有 4 条补充**（`TestEventLog_MapEvent`、`TestEventLog_RetentionAgeZeroKeepsEverything`、`TestEventLog_DefaultsForNonPositiveOptions`、`TestEventLog_WriteFailureIsRetriedOnce`），共 15 条。
9. **`eventLogAPI` 不含 `Flush`**。装配侧只需要 `Close()` 与 `Dropped()`：落盘由写入器自己的周期负责，关停时它自己会把剩余批次写完，`run` 里没有单独 Flush 的时机。留着一个没人调的方法只会让人以为关停链路里有一次显式落盘。

### 10.3 验证证据

```
go build ./...                                     通过
go vet ./...                                       通过
go test ./... -race                                全绿（api / cmd/server / core / executor / store/sqlite）
go test ./store/sqlite -run "EventLog|NewEventLog" -v -race   15/15 PASS
go test ./cmd/server -run "Observability|EventLog" -v -race    8/8 PASS
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build ./...  通过
GOOS=windows GOARCH=386 CGO_ENABLED=0 go build ./...  通过
go build -tags dashboard ./... && go vet -tags dashboard ./...  通过
grep -rln "modernc.org/sqlite" --include=*.go .       只有 store/sqlite/db.go、store/sqlite/doc.go
git diff -- core api                                 空（内容未改）
```

`gofmt` 用去 CR 的临时副本跑，`cmd/server/main.go`、`store/sqlite/events_test.go` 有格式修正后写回；其余文件无变化。

每个提交另外用 `git archive <sha> | tar -x` 在新目录里重建中间态再验一次（`go build ./...`、`go vet ./...`、`go test ./store/sqlite ./cmd/server`）。第一轮就这样暴露出 §10.5 那条用例问题：仓库里带 `-race` 跑得过，副本里不带 `-race`、多包并发跑不过。

### 10.4 手工验收（本卡 §7）

在 `%TEMP%\s03-smoke` 里放独立二进制与独立配置（`configs/config.example.yaml` 的副本），全部路径走 `GODELAYQ_*` 环境变量覆盖，不碰仓库的 `configs/config.yaml` 与 `data/`：

```
GODELAYQ_SERVER_PORT=8137  GODELAYQ_SCHEDULER_WORKERS=2
GODELAYQ_OBSERVABILITY_ENABLED=true  GODELAYQ_OBSERVABILITY_PATH=<临时目录>/observe.sqlite
GODELAYQ_OBSERVABILITY_FLUSH_INTERVAL=50ms  GODELAYQ_SERVER_AUTH_TOKEN=<本机随机串>
```

- 第一轮启动日志：`msg="observability enabled" path=... schema_version=1 journal_mode=wal events_writer=true`。
- `POST /api/v1/jobs`（`payment_check`，`delay: 3s`）等它跑完，`sqlite3` 读回：

  ```
  1|job.scheduled|01a0f382-…c03|0
  2|job.started  |01a0f382-…c03|1
  3|job.completed|01a0f382-…c03|2
  ```

- 停进程（本机只能强制结束，见 10.5）、再启动，提交第二条任务（`email_send`，`delay: 3s`）：`seq` 从 4 续到 6，第一轮那三行内容、顺序都没变；`PRAGMA integrity_check` 返回 `ok`。
- `metadata` 列在真库里是 `{"is_repeat":false,"trigger_at":"2026-10-01T02:10:02.816594+08:00"}`、`{"attempt":0}`、`{"duration_ms":2002}` 这样的原文；`data` 列在这两条任务上都是空（NULL），因为都成功了、没有错误负载。

### 10.5 验证中发现并修掉的缺陷

**`TestEventLog_DroppedIsCounted` 的前提是错的**（第一版按本卡 §5.6 写成"发 2×容量条，断言 `Dropped() == 发布数 - 落库数`"）。总线给每个订阅者的通道同样是非阻塞投递（`core/event.go` 的 `select/default`），订阅通道缓冲 100，一次性发 400 条时**先在总线那一层就丢掉一截**，那些事件根本到不了写入器，也就不会计进 `Dropped()`。等式只在"总线一条没丢"时成立，而这取决于转发协程当次被调度的快慢：本机跑过、在全新检出的副本里 `go test ./...`（无 `-race`、多包并发）跑出 `expected 396 dropped, got 279`。

处置：发布条数改为正好等于一个订阅缓冲（100 条），这样总线不会丢，写入器的账就是确定的 4 条落库 + 96 条丢弃；用例注释写明"这条断言守的是写入器的账，不是总线的账"。反向验证过：`-count=20` 连跑 20/20 通过，副本里无 `-race` 连跑三轮通过。


### 10.6 未覆盖与已知边界

- **优雅停服这一步在本机做不到**：Windows 下无法从外部给控制台进程发 SIGTERM（`taskkill` 不带 `/F` 对该进程无效），所以手工验收的停止是强制结束。这意味着"重启后历史仍在"是用最硬的停法验的（那三行当时已经在 WAL 里），而"干净关闭会检查点 WAL""关闭顺序是先写入库→观测库→任务存储"两条仍只由 Go 用例覆盖（`TestCloseCheckpointsWAL`、`TestRun_ObservabilityClosedBeforeStore`），没有真进程响应可引。
- **`data` 列的失败原文没有真跑过**：手工那两条任务都成功。`{"error":…}` 那段的原文入库由 `TestEventLog_PreservesDataAndMetadata` 覆盖。
- **恢复阶段那批 `job.scheduled` 是否进库没有手工证据**：装配顺序的注释与 `TestRun_EventLogClosureIsMandatoryWhenEnabled`、`TestRun_ObservabilityEnabledOpensAndClosesForReal` 守住了"写入器在 `Start` 之前挂上"，但用例走的是替身调度器，没有真的从 `jobs.json` 恢复一条任务再读库。留待 S04 有读端点后一并补。
- **设计文档的代码草图与本卡不一致，S07 要对齐**：`sqlite-observability-design.md` §9 第 293 行写的是 `NewEventLog(bus, db, opts)`，没有 `logger` 形参，也没有 S04 要加的 `Events`/`Recent` 两个读方法。实现按本卡 §3.1 走（带 logger，因为元数据序列化失败与批次被放弃都要有地方记）。

