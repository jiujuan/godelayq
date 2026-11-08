# Q03 SQL 快照门禁

## 1. 任务目标

jobs/mgr/observe 三包各加 `sql_snapshot_test.go`：经 bun QueryHook 捕获生产 API 触发的
全部 SQL 文本，与 per-dialect golden 文件比对；`-update` 重写。

## 2. 背景与当前问题

行为断言（B06）证明"查出来的数据对"，不证明"生成的 SQL 没变"。bun 升级或有人改
构建器链时，SQL 文本可能悄悄漂移（占位符、引号、EXCLUDED 写法、ORDER BY 表达式），
某一方言在某个版本才炸。

## 3. 要实现的功能

- 记录 hook：`BeforeQuery` 收 `event.Query`（带方言占位符的完整语句）。
- 三包快照用例：操作序列固定，`t.Run("sqlite"/"postgres")` 两份 golden
  （`testdata/sql_snapshot_<dialect>.golden`）；PG 侧 pgtest 门控。
- 覆盖面：jobs（upsert/insert/delete/loadall/trim 扫描与删除/meta upsert/导入），
  mgr（三实体 CRUD + lower(name) 冲突目标 + meta），observe（批量插入、两条 prune、
  事件读路径、audit 动态过滤与分页、artifact upsert/标注/对账/exists/distinct）。
- `-update` 旗标重写 golden（`go test -run TestSQLSnapshot -update`）。

## 4. 实现步骤

1. 写 hook 与快照骨架（每包一份，结构同构）；
2. 跑 `-update` 生成 golden；
3. 连续两次生成零 diff 自证；4. 本机 PG 下 PG 侧 golden 同样自证。

## 5. 测试要求

golden 只有语句文本、无参数值（时间戳等是占位符参数）；map 迭代路径用单条目入参；
批量器显式 Flush 同步。PG 侧未配 DSN 时跳过（golden 不更新，避免本地静默改文件）。

## 6. 完成标准(DoD)

三包快照测试绿（sqlite 恒跑，PG 门控）；golden 落库；两次生成零 diff。

## 7. 验收方式

```
go test ./internal/store/... -run TestSQLSnapshot -v
GODELAYQ_TEST_PG_DSN='...' go test ./internal/store/... -run TestSQLSnapshot -v
```

## 8. 不在本任务范围

engine 底座自身的 PRAGMA/迁移 DDL（属迁移纪律 golden，P1 的 G7）、cmd/server 装配级
SQL（三包快照已覆盖其构成的语句）。

## 9. 风险与回滚

并发协程插入非确定语句——observe 侧只经显式 Flush 触发落盘，转发协程的操作串行化在
Flush 内；golden 抖动——发现即修操作序列而不是放宽比对。回退删测试与 golden。

## 10. 实现记录

(执行时填写)
