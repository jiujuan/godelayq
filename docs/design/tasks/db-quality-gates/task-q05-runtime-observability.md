# Q05 G4 可观测读数

## 1. 任务目标

`/admin/runtime` 增加 `db` 段：`sql.DBStats`、方言名、schema 版本、出箱积压行数；
EventBus 增加 `dropped_events_total` 丢弃计数一并暴露。运行时第一次能看到
"DB 占用"与"事件有没有被静默丢"。

## 2. 背景与当前问题

P0 之后 SQL 写得对有门禁，但运行时对 DB 占用零可见：连接池水位、慢查询是否存在、
出箱中继积压多少，都只能猜。链路分析还发现 EventBus.Publish 满即丢是静默的——
直发模式下慢订阅者丢事件无任何痕迹（`internal/core/event.go` Publish 注释自述
"避免拖慢调度主流程"，取舍本身保留，缺的是计数）。

## 3. 要实现的功能

- engine 侧暴露 `sql.DBStats`（MaxOpen/Open/InUse/Idle/WaitCount/WaitDuration）；
- store 侧上报方言名与 schema 版本（migrations 表 max version；JSON 部署上报
  `json`/无版本）；
- 出箱积压行数由 relay 提供待投递计数；直发模式该键缺失（不报 0 冒充能力）；
- `EventBus.Publish` 满丢处加 atomic 计数器 + 只读访问器；
- cmd/server 的 `observabilityDB` 消费方接口扩展，api 装配注入，`/admin/runtime`
  响应加 `db` 段。读数为瞬时值，字段注释沿用 RuntimeStats 的口径写法。

## 4. 实现步骤

1. engine 增加只读访问器；2. EventBus 计数器；3. relay 待投递计数；
4. observabilityDB 接口扩展 + api 组装；5. dto/文档同步（deployment.md 的
/admin/runtime 段）。

## 5. 测试要求

- 丢弃计数：制造"订阅存在但不消费、发布超过缓冲"的场景，断言计数增长；
- 直发模式：db 段不含 outbox 键；库模式：PG 侧（DSN 门控）真查 migrations 版本；
- 接口扩展同步更新既有测试桩。

## 6. 完成标准(DoD)

`/admin/runtime` 返回 db 段与 dropped_events_total；丢弃计数人为制造必增长；
两方言（含 JSON 部署）响应形状各有测试钉住。

## 7. 验收方式

```
go build ./... && go vet ./... && go test -count=1 ./...
# 运行 server 后（admin 角色）：
#   curl /admin/runtime → db 段字段齐全
```

## 8. 不在本任务范围

指标导出（Prometheus 等）、历史曲线、事件丢弃的补投、慢查询日志（Q06）。

## 9. 风险与回滚

消费方接口加方法会波及测试桩——与 schedulerAPI 扩展的既有做法同构，桩随卡更新；
DBStats 读取本身无锁竞争风险（标准库快照）。回退：摘掉 db 段与计数器，无状态残留。

## 10. 实现记录

- 2026-11-09 完成。读数词汇 `core.DBRuntimeStats` + `core.DBPoolStats` 落在 core/store.go
  （与 RuntimeStats 同层的公共形状）：jobs 填、api 嵌、cmd/server 直接断言注入，
  **不需要适配器**。池读数折算自 sql.DBStats，不把 database/sql 类型引进 core。
- 出箱积压计数随 `Store.DBRuntime()` 直查表（`pendingOutboxCount`，与 relay 的
  prune 截断判定共用同一条查询），**没有落在 relay 上**——与卡面 §3 的偏差：
  api 注入点是 store 类型断言，relay 不经过那条线，且周期缓存会给出陈旧值。
  计数语义只有一份（积压判定与 /admin/runtime 读数同源）。
- engine 新增 `Stats()`（sql.DBStats 透传）与 `MigrationVersion(table)`（与 Migrate
  共用版本表与读法，无占位符两方言通吃）。业务包不摸 *sql.DB。
- EventBus 满丢处加 atomic 计数器 `Dropped()`；Publish 的 per-type 与 all 两条路径
  都计入，语义是"投递次数"（一个订阅者丢一次计 1）。
- api：`dbRuntimeSource` 接口定义在消费方（eventReader 同惯例）、`WithDBRuntime`
  注入；RuntimeResponse 加 `db`（指针 + omitempty，与 Reload 同一手法）与
  `dropped_events_total`（值类型，始终在位）。读数失败 → db 键缺席 + error 日志
  （半截读数比缺席更易误读），端点照常 200。
- cmd/server：defaultNewServer 里 store 类型断言注入（与 attempts/idempotency
  同一取向），JSON 部署自然缺席。
- 测试：core（丢弃计数精确值，缓冲 2 丢 3；all 订阅者逐个计）、engine（版本读数
  含"表不存在报错不是 0"与空表 0 两分支；Stats 透传）、jobs（四读数 + 积压随
  AppendOutbox 增长，SQLite 真跑 + PG DSN 门控真跑）、api（db 键缺席/键集合封闭/
  失败缺席/丢弃计数经端点与总线一致且增长）。
- 已知边界：端点丢弃读数是全总线共享计数器，测试服务器自带订阅者（事件历史）
  超载时同样丢弃——端点侧只断"与总线一致且增长"，精确数字归 core 单测。
- 文档：docs/api.md 的 GET /admin/runtime 段补 db 字段表与 dropped_events_total
  口径（含"积压 vs 丢弃"的联合判读）。
- 自证：`go build ./... && go vet ./... && go test ./...` 全绿；本机 PG
  （GODELAYQ_TEST_PG_DSN 注入）jobs/engine/cmd/server 真跑全绿。
