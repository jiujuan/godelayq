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

- 待实施。
