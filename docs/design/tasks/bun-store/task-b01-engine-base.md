# B01 engine 基座:sqlite 包改组为 engine 包

## 1. 任务目标

引入 bun v1.3.0,把 `internal/store/sqlite` 改组为 `internal/store/engine`:句柄从裸
`*sql.DB` 包装换成 `*bun.DB` 包装,迁移框架原样移植并预留方言扩展点。本卡结束时三库仍跑
SQLite,行为零变化。

## 2. 背景与当前问题

`store/sqlite/base.go` 直接暴露 `*sql.DB`(`SQL()`),业务包全部手写 SQL。为了后续
PostgreSQL,需要一个方言可插拔的底座;驱动红线("唯一引入驱动的包")要延续到新包。

## 3. 要实现的功能

- `engine.Dialect` 常量:`sqlite`(默认)/`postgres`(B05 前仅 sqlite 分支可用)。
- `engine.Options{Dialect, Path, DSN, BusyTimeout, Synchronous, MaxOpenConns}`。
- `engine.DB` 内嵌 `*bun.DB`,保留 `Path/JournalMode/Synchronous/Close`(once 语义)与
  现有全部机制行为:目录 0750/文件 0640 预建、`_pragma=` DSN、WAL 降级探测告警、
  MaxOpenConns<=0 折 1。
- `Migration{Version, Up []string, UpPostgres []string}` 与 `Migrate(table, ms)`:版本表名、
  每版本一个事务、先跑后记版本号的语义逐字保留;`UpPostgres` 本卡只定义不生效。
- `store/sqlite` 包删除,`doc.go` 红线说明随迁到 `store/engine`。

## 4. 实现步骤

1. `git mv internal/store/sqlite internal/store/engine`,重写 `base.go`→`engine.go`
   (bun.NewDB 包装、Dialect 分支骨架、Migration 扩展)。
2. `jobs.Handle`/`mgr`/`observe` 里 `*sqlite.DB` 引用改为 `*engine.DB`。
3. 更新 `package_guard_test.go` 与 `base_test.go`(改名 engine_test.go,断言迁移框架行为不变)。
4. `go.mod` 恢复 `github.com/uptrace/bun`(代码引用后 tidy 自动保留)。

## 5. 测试要求

- 单元:engine_test 覆盖打开/PRAGMA/迁移幂等/迁移回滚(沿用现有断言,补 Migration.UpPostgres
  为 nil 时与 Up 同源的表驱动用例)。
- 全量:`go build ./... && go vet ./... && go test ./...`。

## 6. 完成标准(DoD)

三库经 bun 底座打开,全仓无 `store/sqlite` 引用,守卫测试钉住"只有 engine import 驱动"。

## 7. 验收方式

```
go build ./... && go vet ./... && go test ./internal/store/... ./internal/core/...
```

## 8. 不在本任务范围

业务包 SQL 改写(B02–B04)、PG 打开路径(B05)。

## 9. 风险与回滚

bun.DB 包装层 API 不熟导致语义漂移——迁移框架与打开路径的现有测试原样保留,漂移即红。
回滚:回退本卡提交。

## 10. 实现记录

(执行时填写)
