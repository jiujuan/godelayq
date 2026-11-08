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

- 2026-11-08 完成。新增 `models.go`（eventRecord/auditRecord/artifactRow bun 模型，字段
  导出化——bun 反射不能写未导出字段）；events/audit/artifacts 全走构建器。
- 批量写：tx+Prepare+循环 → `RunInTx` 内一次多值 INSERT；prune 两条 DELETE 走构建器
  （子查询原样保留）；audit 动态 WHERE 改为 `Apply(auditScope)` 链式过滤。
- 行还原函数（scanEvent/scanAudit/scanArtifact/rowScanner）被 `eventOf/auditEntryOf/
  artifactOf`（bun 行 → core 结构）替代；audit 的 role 列维持写原样字符串（与迁移前
  逐字一致）。
- **测试策略调整**：observe 45 个既有测试保持原样作为 SQLite 套件（仅字段名导出化
  跟进），不做逐个 opener 包装——B06 以独立的 PG 矩阵测试文件复用同一批场景断言，
  覆盖关键路径（往返/排序/保留/过滤分页/对账）。
- self-review 抓获并修复一处重写引入的逻辑倒置（ReconcileMissing 的 dirExists 判断）。
