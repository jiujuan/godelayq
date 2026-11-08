# Q02 红线守卫扩展

## 1. 任务目标

新增测试辅助包 `internal/store/storeguard`（AST 级），三条红线接进
jobs/mgr/observe 三包的 `package_guard_test.go`：`SQL()` 逃生口白名单、
方言分支白名单、动态 SQL 拼接禁令。

## 2. 背景与当前问题

bun 化之后业务包留有 `SQL()` 逃生口（observe Stats 合法使用）与构建器里的裸字符串。
守卫缺失时，"原生 SQL 回潮""查询层按方言分叉""Sprintf 拼进 Where"三类回潮
不会有任何征兆，直到某一方言在线上炸掉。

## 3. 要实现的功能

- `storeguard.GuardNoSQL(t, dir, allow)`：生产文件不得出现 `.SQL()` 选择器调用。
- `storeguard.GuardNoDialectBranch(t, dir, allow)`：不得引用 `DialectPostgres`/
  `DialectSQLite` 标识符。
- `storeguard.GuardNoDynamicSQL(t, dir)`：构建器方法
  （Where/WhereExpr/On/Set/Order/OrderExpr/ColumnExpr/GroupExpr/Having/Exec/ExecContext）
  的直接参数不得是 `fmt.Sprintf` 调用。
- 三包 `package_guard_test.go` 接线：白名单 observe `db.go`（Stats）、mgr `models.go`
  （nameConflictClause）；只扫生产文件（非 _test.go），与既有守卫同取向。

## 4. 实现步骤

1. 写 storeguard（go/ast 遍历，违规带文件名报错）；
2. 三包接线；
3. 自证：人为制造一次违规确认必红，还原后全绿。

## 5. 测试要求

守卫对现有代码零误报；自证用例在实现记录中描述违规构造方式与变红证据。

## 6. 完成标准(DoD)

三条红线进测试期；三包守卫测试绿；guard 对仓内代码零误报。

## 7. 验收方式

```
go test ./internal/store/... -run TestGuards -v
go test ./internal/store/...
```

## 8. 不在本任务范围

迁移纪律守卫（golden 钉 version 1 DDL，P1）、linter、engine 包自身（它是底座，
驱动与原生语句合法）。

## 9. 风险与回滚

AST 规则过严误伤合法用法——白名单文件级（不是行级），误报时优先检查用法是否真的
该存在；经变量中转的拼接拦不住（注释明示，门禁抓高频捷径）。回退还原接线即可。

## 10. 实现记录

- 2026-11-08 完成。storeguard 三规则：GuardNoRawSQL（SQL()/Query/QueryRow 家族——
  后者是 *sql.DB 经嵌入提升出的逃生口，与 SQL() 一样绕过构建器）、
  GuardNoDialectBranch、GuardNoDynamicSQL（构建器直接实参查 fmt.Sprintf）。
- 接线：jobs（三规则零豁免）、observe（原生出口豁免 db.go——Stats 的可移植原生语句）、
  mgr（方言分支豁免 models.go——nameConflictClause 全仓唯一分支点）。
- **补齐欠账**：mgr 此前没有任何 package_guard_test（doc.go 写了红线但没守卫），
  本卡一并补上驱动/消费方 import 守卫（含 pgx）。
- 自证：mgr 下临时构造三类违规（.SQL()、engine.DialectPostgres、Sprintf 进 Where），
  守卫逐条报文件:行号并 FAIL；删除后全绿。全仓零误报。
