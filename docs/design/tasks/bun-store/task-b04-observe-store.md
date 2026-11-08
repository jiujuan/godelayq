# B04 observe 包 bun 化

## 1. 任务目标

`internal/store/observe` 的 events/audit/artifacts/db 全部 SQL 改 bun;批量写从
tx+Prepare+循环改为事务内一次多值插入;batcher 的合并/重试一次/丢弃计数语义不动。

## 2. 背景与当前问题

events/audit 批量写用 Prepare+循环 Exec;audit 查询手拼动态 WHERE;方言锁死且代码量大。

## 3. 要实现的功能

- `eventRow/auditRow/artifactRow` bun 模型(seq 挂 `autoincrement`,insert 自动省略)。
- `writeBatch`(events/audit):`RunInTx` 内 `NewInsert().Model(&rows)` 多值插入 + prune;
  空批次直接返回;重试一次后丢弃计数的行为不变。
- `prune`:两条 DELETE(条数封顶/按时间)语句经构建器,子查询原样保留。
- `Query`(audit):链式 Where 动态过滤 + `Limit/Offset` + 先计数后取页,分页语义不变。
- artifacts:`Record` upsert(`ON CONFLICT (job_id, attempt)`)、`markPurged`(RowsAffected)、
  `ReconcileMissing` 的 `DISTINCT job_id`、`Exists` 的 `LIMIT 1`。
- `Stats`:保留原生可移植 SQL(计数+版本号)。

## 4. 实现步骤

1. 定义三个 row 模型;改写 events.go/audit.go/artifacts.go/db.go。
2. `eventRecord/auditRecord` 的 `sql.NullString` 保留(bun 原生支持 Valuer/Scanner)。
3. 测试 opener 参数化(同 B02 口径);`countQuery` 助手保留给 SQLite 端。

## 5. 测试要求

- 现有 observe 五个测试文件断言原样通过。
- 新增:空批次 flush 不产生 SQL 错误(批量插入边界)。

## 6. 完成标准(DoD)

`store/observe` 零手写 CRUD SQL(prune 子查询、Stats、迁移 DDL 除外),批量写行为不变。

## 7. 验收方式

```
go build ./... && go vet ./... && go test ./internal/store/observe/... ./cmd/server/...
```

## 8. 不在本任务范围

PG DDL(IDENTITY 序列)与测试挂载(B05/B06)。

## 9. 风险与回滚

多值 INSERT 的参数数量上限(SQLite 变量数上限)——批量容量受 batcher 上限约束,实测钉住;
回退本卡提交。

## 10. 实现记录

(执行时填写)
