# B03 mgr 包 bun 化(含 lower(name) 统一)

## 1. 任务目标

`internal/store/mgr` 的 users/groups/profiles/import 全部 SQL 改 bun;大小写不敏感查询
统一为 `WHERE lower(name) = lower(?)` 与 `ORDER BY lower(name)`,SQLite 端行为不变,
为 PG 的 `lower(name)` 唯一索引铺路。

## 2. 背景与当前问题

现有查询靠 `COLLATE NOCASE` PK 命中,PG 没有该 collation;查询层必须先与 collation 解耦。

## 3. 要实现的功能

- `userRow/groupRow/profileRow/metaRow` bun 模型。
- Get/Delete/ORDER BY 全走 `lower(name)`;Save 的 upsert 冲突目标 SQLite 仍为
  `ON CONFLICT (name)`(NOCASE PK 拦大小写变体),PG 目标在 B05 经 `dialect` 分支给
  `ON CONFLICT (lower(name))`。
- `RowsAffected` 判缺失的语义保留(ErrUserNotFound/ErrGroupNotFound/ErrProfileNotFound)。
- import.go 的 metaGet/metaSetNow 与三个一次性导入标记不变。

## 4. 实现步骤

1. 定义四个 row 模型;改写 users/groups/profiles 与 import。
2. 测试 opener 参数化(同 B02 口径)。
3. schema.go `migrations` 改 `[]engine.Migration`。

## 5. 测试要求

- 现有 users_test/import_test 断言原样通过。
- 新增:大小写变体用例显式钉住(Save "Foo" 后 Get "foo" 命中、Delete "FOO" 删掉、
  ORDER BY 大小写混合序)——把 lower(name) 改写的行为收进测试。

## 6. 完成标准(DoD)

`store/mgr` 零手写 SQL(迁移 DDL 除外),大小写语义测试绿。

## 7. 验收方式

```
go build ./... && go vet ./... && go test ./internal/store/mgr/... ./internal/api/...
```

## 8. 不在本任务范围

PG DDL 与表达式冲突目标(B05)。

## 9. 风险与回滚

lower() 在 SQLite 是 ASCII 折叠,与 NOCASE 同语义——大小写变体用例保证等价;PG 表达式
冲突目标风险记在 B05。回退本卡提交。

## 10. 实现记录

(执行时填写)
