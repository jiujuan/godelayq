# B02 jobs 包 bun 化

## 1. 任务目标

`internal/store/jobs` 的 db.go/import.go 全部 SQL 改写为 bun 查询构建器与 RunInTx,
`core.Store` 接口语义逐字不变。

## 2. 背景与当前问题

upsert/Delete/LoadAll/trimTerminal/ImportOnce 都是手写 SQL,方言锁死;迁移 PG 需要经
查询构建器解耦。

## 3. 要实现的功能

- `jobRow` bun 模型(显式 table/column tag,`snapshot_json` 载荷模式不变)。
- `upsert`:`NewInsert().On("CONFLICT (id) DO UPDATE").Set(...EXCLUDED...)`。
- `trimTerminal`:`RunInTx` 内循环按 ID 删(语义不变)。
- `ImportOnce`:`RunInTx` 内批量插入 + 标记 upsert,`alreadyImported/tableEmpty/markImported`
  走构建器;标记键值与事务边界不变。
- `LoadAll`:单列扫描进 `[]string` 后逐条 Unmarshal(与现状一致)。

## 4. 实现步骤

1. 定义 `jobRow`;改写 db.go 四个方法与 import.go 四个函数。
2. 测试 opener 参数化:`openStore(t)` 保留为 SQLite opener,测试体改为接收 opener 的
   独立函数(PG 挂载点留给 B06,本卡只接 SQLite)。
3. schema.go 的 `migrations` 类型改为 `[]engine.Migration`(UpPostgres 留空)。

## 5. 测试要求

- 现有 jobs_test 全部断言原样通过(opener 参数化不改断言)。
- 新增:upsert 幂等覆盖、ImportOnce 二次调用幂等(现有用例已覆盖则不重复)。

## 6. 完成标准(DoD)

`store/jobs` 零手写 SQL 字符串(迁移 DDL 除外),`go test ./internal/store/jobs/...` 绿。

## 7. 验收方式

```
go build ./... && go vet ./... && go test ./internal/store/jobs/... ./internal/core/...
```

## 8. 不在本任务范围

PG DDL(B05)、PG 测试挂载(B06)。

## 9. 风险与回滚

批量插入空切片边界、`EXCLUDED` 关键字大小写——测试钉住;回滚回退本卡提交。

## 10. 实现记录

- 2026-11-08 完成。db.go/import.go 全部走 bun 构建器；`RunInTx` 实际签名为
  `(ctx, *sql.TxOptions, fn)`（比设想多一个参数，传 nil）。
- 测试重构：`openStore` → `storeOpener`/`openSQLiteStore` + `runWithStoreBackends`
  （`storeOpt` 钩子同时覆盖配置节与留痕参数），断言体方言无关，B06 直接挂 PG。
- 批量导入改为事务内一次多值 INSERT（空切片跳过），其余语义逐字保留。
