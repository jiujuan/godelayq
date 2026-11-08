# Q06 G5 慢查询日志

## 1. 任务目标

bun `QueryHook` 按阈值打慢查询 Warn 日志（SQL 文本截断 + 耗时 + 方言），
配置 `store.slow_query_threshold`，默认关。

## 2. 背景与当前问题

慢查询目前只能靠用户报告"接口慢"倒查。W1–W3（Q08–Q10）要动热路径事务结构，
动之前必须先有"哪些语句慢、慢多少"的眼睛——本卡是它们的基线证据来源。

## 3. 要实现的功能

- AfterQuery 阶段测耗时，超过阈值打 Warn：SQL 文本（**只记前 N 字节**，N 为常量，
  取向同 eventPreviewLimit）、耗时、方言、语句类别（由调用栈不可靠，按查询首关键字）；
- 配置 `store.slow_query_threshold`（duration）：0=关（默认）；非 0 设下限守卫
  （对齐 `minObserveFlushInterval` 的思路，防"每条查询都进日志"刷屏）；
- 关闭时零开销路径：阈值 ≤0 直接 return，不进日志组装；
- 与 Q03 快照捕获 hook 并存：两个 hook 挂同一句柄时互不污染（hook 链顺序有测试）。

## 4. 实现步骤

1. hook 实现 + 配置接线（Normalized/校验/example 同步）；2. 截断常量与日志字段；
3. engine 句柄装配处挂 hook；4. deployment.md 配置文档。

## 5. 测试要求

- 注入超阈值查询（如 SQLite 侧构造大扫描）断言 Warn 出现且文本被截断；
- 默认关：全量测试输出零慢查询日志；
- 阈值下限：配置非法值拒启（对齐既有配置校验口径）；
- 快照测试回归：hook 并存不改变 golden。

## 6. 完成标准(DoD)

开启时超阈值查询有 Warn 且 SQL 截断；默认关零输出；golden 零 diff。

## 7. 验收方式

```
go build ./... && go vet ./... && go test -count=1 ./...
# 手动：配置阈值启动 server，触发查询，观察日志
```

## 8. 不在本任务范围

慢查询聚合/统计面板、PG `pg_stat_statements` 集成、自动 kill。

## 9. 风险与回滚

安全口径已拍板：内联参数渲染会把 payload 值打进 SQL 文本——截断 + 默认关两道闸；
hook 每查询都走（即使关）的开销为一次整数比较。回退：配置置 0 或摘 hook。

## 10. 实现记录

- 待实施。
