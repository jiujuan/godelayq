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

- 2026-11-09 完成。钩子落在 `internal/store/engine/slowquery.go`：`slowQueryHook`
  （threshold/maxBytes/dialect/logger），AfterQuery 阶段 `time.Since(event.StartTime)`
  判定——`StartTime` 由 bun 在 beforeQuery 打点，且**无钩子时 bun 连 QueryEvent 都
  不建**（hook.go 短路），关闭路径是真零开销。Open 统一装配（Options.SlowQueryThreshold
  >0 才挂），业务包只传阈值。
- 日志形状：`msg="slow query"` + `sql`（前 1024 字节，`DefaultSlowQueryLogBytes`，
  rune 边界收口 + `…(truncated)` 标记）+ `elapsed` + `dialect` + `op`
  （bun `event.Operation()`，不用自己解析首关键字）+ 失败时的 `error`。
  "慢且失败"与"慢且成功"都记录。
- 配置：`store.slow_query_threshold`（core.StoreConfig，仅 sqlite/PG 后端生效）；
  校验两条——负数拒启（0 才是"关"，负值只可能是笔误）+ `>0 && <1ms` 拒启
  （`MinSlowQueryThreshold = 1ms`，本地常规查询亚毫秒级，压更低等于日志洪水）；
  reload 档位 `ClassRestart`（钩子 Open 时挂，热改不能重挂）。
- 映射链：jobs.Open 加 `slowQueryThreshold` 参数（historyLimit/historyTTL 同款
  显式传参）→ engine.Options；cmd/server 传 `cfg.Store.SlowQueryThreshold`。
- 测试发现并修正的一处：Windows 粗时钟下快速查询的 `elapsed` 可能为 0，
  "1ns 阈值 + 真跑 SQL"的触发用例不可靠——钩子判定/日志形状/截断改为**构造
  QueryEvent 的确定性单测**，Open 装配链路用真实慢查询（递归 CTE 纯计算 30 万
  次迭代，无 I/O 随机性，比 10ms 阈值慢两个数量级）验证。
- hook 并存：慢查询钩子 BeforeQuery 不改事件字段，快照捕获只发生在 BeforeQuery，
  golden 不受影响（engine 并存用例断言两个库捕获文本逐字节一致；快照测试保持
  0 阈值生成）。golden 两次生成零 diff（全量测试佐证）。
- 文档：deployment.md 存储节参数行 + 调参建议一条（含"W 系列基线证据来源、
  配合 /admin/runtime 的 db.pool.wait_* 读数"的用法）；config.example.yaml 同步。
  reload 矩阵表不补键名——该表本就不列全量重启档键（outbox 三键也不在），
  代码的 ClassRestart 表是权威。
- 自证：`go build ./... && go vet ./... && go test ./...` 全绿（默认关零输出）；
  PG 矩阵见提交前复跑记录。
