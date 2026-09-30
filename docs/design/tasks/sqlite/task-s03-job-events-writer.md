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

- [ ] `observability.enabled: false`（默认）时不构造订阅者、总线订阅数与改动前一致、库里没有文件。
- [ ] `enabled: true` + `events.enabled: false` 时建表但不写入、不订阅（用一条用例把"不订阅"固定下来）。
- [ ] `Publish` 的非阻塞契约没有被削弱：§5 第 5 条用例证明队列打满时发布方照常返回。
- [ ] 事件在进程重启后仍在库里（手工步骤见 §7）。
- [ ] `Close` 之后不再有写入，且 `Close` 幂等。
- [ ] 装配顺序满足"在 `scheduler.Start()` 之前构造订阅者"，注释里写明漏掉会丢哪一段事件。
- [ ] `core/` 与 `api/` 一行未改（DoD 第 7 条：本卡只加写入方）。

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
