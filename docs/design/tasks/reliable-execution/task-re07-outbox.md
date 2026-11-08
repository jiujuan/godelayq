# TASK-RE07　事务性 Outbox 与中继器

- 所属阶段：可靠执行
- 依赖任务：RE01（`persistAndEmit` 收口点）
- 涉及文件：新增 `core/outbox.go`、`store/jobs/outbox.go`、`store/jobs/outbox_test.go`；改 `store/jobs` 迁移 v4、`store/observe` 迁移 v2 与 `events.go`/`models.go`/`schema.go`/`package_guard_test.go`、`core/event.go`、`core/scheduler.go`（收口函数接 outbox 能力）、`cmd/server/main.go`（装配与关停顺序）
- 预计规模：大（本系列最大的一张卡）

## 1. 任务目标

库模式下"状态变更与事件同事务"：状态落了事件必在，事件至多延迟一个中继周期送达，崩溃后补投且读侧去重。JSON 模式保持现状直发（能力边界见设计文档 §4.1）。

## 2. 背景与当前问题

设计文档 §3.6：`EventBus` 队满即丢，`EventLog` 批量落库与任务快照写入不同库不同事务——"状态写成功、事件没了"的窗口两层（总线丢弃 + 批量丢弃），且 Restore 不补发。对"观测日志"这是既定取舍（[sqlite-observability-design.md](../../sqlite-observability-design.md) D4），对"审计链"不够。

## 3. 要实现的功能

1. `core/outbox.go`：`EventOutbox` 接口（`UpdateWithEvents(snapshot, events)` / `AppendOutbox(events)`，设计文档 §6.3）。调度器持有可 nil 引用（Option 注入），`persistAndEmit` 与 `emit` 按能力分派：
   - outbox 可用：状态路径走 `UpdateWithEvents`（一次事务：快照 upsert + 出箱批量 INSERT），事件-only 路径走 `AppendOutbox`（独立事务）；**调用侧不再自行 Publish**（双发禁令写进接口注释）。
   - 不可用（JSON 模式）：现状路径（`Update` + `Publish`）。
2. `store/jobs` 迁移 v4：`job_outbox` 表（DDL 设计文档 §6.1）；`outbox.go` 实现接口 + `OutboxRelay`：
   - 周期 `store.outbox_flush_interval`（默认 200ms，下限 10ms）批量取 `pending ORDER BY seq LIMIT 256`；
   - 逐条回填 `Event.OutboxSeq` 后 `eventBus.Publish`（总线语义不变，订阅满丢弃照旧——读侧有 outbox_seq 可对账）；全批投完同一事务标记 dispatched；
   - 启动先重放一轮残留 pending（崩溃补投，at-least-once）；
   - 淘汰：dispatched 超 `store.outbox_retention_age`（默认 72h）删；pending 超 `store.outbox_max_pending`（默认 100000）按最旧截断 + error 日志；
   - `Relay.Stop` 幂等，等在途批处理完（关停顺序见下）。
3. 读侧去重：`core.Event` 增 `OutboxSeq int64`（omitempty）；`store/observe` 迁移 v2 给 `job_events` 加 `outbox_seq` 可空列 + 部分唯一索引（DDL 设计文档 §6.2）；`EventLog.mapEvent` 透传该字段，`writeBatch` 插入按冲突忽略。**冲突子句带 `WHERE outbox_seq IS NOT NULL` 谓词且两方言写法不同——方言分支收口成一个函数（先例 `mgr/models.go` 的 `nameConflictClause`），并在 `observe/package_guard_test.go` 登记豁免**。直发模式（JSON 部署 + 观测库开启）的事件 `OutboxSeq=0`，列保持 NULL，行为不变。
4. 装配与关停（`cmd/server/main.go`）：
   - 装配：任务库为 sqlite/PG 时建 relay 并注入调度器 Option；依赖完整性检查沿用既有取向（outbox 开而句柄缺失 = 拒启）；
   - **关停 defer 链最前加 `relay.Stop`**：必须先于事件/审计写入器关闭（发布落在已关闭的订阅通道上会 panic——既有 `Unsubscribe` 后 close 的顺序决定了这一点）；设计文档 §7.7 的位次图抄进注释。
5. 配置：`store.outbox_flush_interval` / `outbox_retention_age` / `outbox_max_pending`（§9，全部 ClassRestart）。

## 4. 实现步骤

1. core 接口 + `persistAndEmit`/`emit` 分派（nil 路径先全绿，行为零变化）。
2. 迁移 v4 + jobs 侧 `UpdateWithEvents`/`AppendOutbox`/relay + golden 重生成。
3. observe 加列 + 去重插入 + 守卫豁免登记 + observe golden 重生成。
4. 装配、关停顺序、配置。

## 5. 测试要求

- `store/jobs/outbox_test.go`（双后端）：`UpdateWithEvents` 中途失败全回滚（快照与出箱都不在）；`AppendOutbox` 顺序稳定；relay 批量投递 + dispatched 标记 + TTL 淘汰 + max_pending 截断计数。
- 崩溃补投：手工插入 pending 行 → new relay → 全部重放且**顺序 = seq**；重复重放不炸（at-least-once 的对端保证靠 observe 去重）。
- observe：同 `outbox_seq` 二次插入不产生第二行（sqlite + PG 都验）；NULL 序列不受唯一索引影响。
- core：outbox 模式下调度器不再直接 Publish（事件只经 relay 一条路）；nil 模式与现状逐字节等价（既有事件测试全绿即是回归网）。
- 关停顺序：集成测试里 relay.Stop 先于写入器关闭（假句柄记录调用流水，既有"句柄可替换"手法）。

## 6. 完成标准（DoD）

1. 杀进程场景：崩溃前已提交/已执行/已失败的任务，重启后事件在 `job_events` 完整补齐且无重复行。
2. WS/SSE 延迟上界 ≈ `store.outbox_flush_interval`（文档化，实测一次记录在实现记录）。
3. 迁移 v4 + observe v2 落地，四份 golden 重生成；方言分支只有收口函数一处（守卫测试过）。

## 7. 验收方式

dev.ps1 起服（store.type=sqlite + observability.enabled）→ 提交任务跑完 → `sqlite3` 里 `job_outbox` 全 dispatched；手工把一批 dispatched 改回 pending 重启 → `job_events` 行数不变（去重生效）而 WS 收到补投（可识别 outbox_seq）。

## 8. 不在本任务范围

事件外推第三方、订阅端（api 缓冲/EventLog）的批量语义改造、`EventBus` 本身的可靠化（它仍是 best-effort 广播，可靠性由出箱表承担）。

## 9. 风险与回滚

- **风险：中继器成为事件可见延迟的常数下限**。缓解：flush_interval 可调至 10ms；读路径（GET /events 库模式）本就来自 EventLog 批量，延迟量级未变差。
- **风险：单连接任务库上出箱事务与调度写入互相排队**。缓解：批小（256）、周期 200ms；与既有"单连接消除 SQLITE_BUSY"的取舍一致，压测数据记入实现记录。
- **风险：双发或漏发**（分派分支写错）。缓解：接口注释的禁令 + core 测试断言"outbox 模式无直发"。
- 回滚：配置不装 relay 即回现状；遗留 pending 行无害。

## 10. 实现记录（2026-11-08）

分支 `feat/reliable-execution`。按卡交付，三处实现口径说明：

- **core**：`EventOutbox` 接口（UpdateWithEvents/AppendOutbox，双发禁令写进接口注释）+ `WithEventOutbox`；`Event.OutboxSeq`（omitempty）；state.go 的 `persistAndEmit`/`emit` 按能力分派——outbox 模式下状态写与事件进任务库同事务、事件由中继器投递，nil 退回直发（与 RE01 之前逐字节等价）。`job.scheduled` 随状态写同事务（persistAndEmit 的 events 参数），started/completed/failed/retrying/misfired/resumed 走 AppendOutbox——core 出箱分派测试钉住这个形状。
- **store/jobs**：迁移 v4（job_outbox，双方言；PG 自增用 IDENTITY）；upsert 与终态淘汰抽成 `upsertSnapshot(bun.IDB)`/`trimTerminalTx(ctx, tx)` 供事务路径复用（淘汰规则只许一处，事务边界由调用方决定）；UpdateWithEvents 事务含快照 upsert + 出箱批量 INSERT + 终态淘汰（含账本级联）；**刻意不含 attempt 行**——begin/finish 由 worker 单独调用，崩溃遗留 running 行由 Restore 收口 abandoned（自愈）。OutboxRelay：单协程按 seq 顺序投递（OutboxSeq 回填）→ 同事务标记 dispatched；启动先重放一轮残留 pending；dispatched 按 TTL 淘汰、pending 超限按最旧截断（error 留痕）；一批取满立即续跑（崩溃重放收敛快）。
- **store/observe**：迁移 v2（job_events 加 outbox_seq + 部分唯一索引）；写入按 `ON CONFLICT (outbox_seq) WHERE outbox_seq IS NOT NULL DO NOTHING` 去重——**两方言冲突子句同形，未设方言分支点**（守卫零豁免保持）；NULL 序号（直发行）不进索引、永不冲突；读端点回传 outbox_seq（客户端可识别重放旧事件）。
- **cmd/server**：newScheduler 注入 WithEventOutbox（结构断言）；run() 在观测层写入器挂上之后、scheduler.Start 之前建 relay（启动重放的事件要被 EventLog 收进库）并 Start；**defer 声明晚于 obsEnv.close——关停时 relay.Stop 先于写入器关闭**（发布落在已关闭订阅通道上会 panic，位次是硬约束，注释写明）。JSON 存储无 relay，直发现状。
- **配置**：store.outbox_flush_interval（默认 200ms，低于 10ms 拒启）/ outbox_retention_age（72h）/ outbox_max_pending（100000），全部 ClassRestart；两份 YAML 同步。
- **测试**：jobs 双后端五用例（UpdateWithEvents 同事务产物/AppendOutbox/投递顺序与 OutboxSeq 回填/崩溃重放/Stop 幂等/TTL+积压截断）；observe 双后端去重（三行带序号去重 + 直发行各成一行）+ 读端点回传序号；core 出箱分派两用例（双发禁令 + identity 写形状）；四处 schema 版本断言 1→2；四份 golden 双方言重生成（diff 仅 outbox 语句与 outbox_seq 列），连跑两次稳定。
- **验证**：`go build/vet` 干净；`go test ./... -count=1` 九包全绿（本机 PG 全矩阵实跑）。
