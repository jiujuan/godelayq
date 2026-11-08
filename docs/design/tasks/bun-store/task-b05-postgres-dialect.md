# B05 PostgreSQL 方言落地

## 1. 任务目标

engine 打开 PG 分支;三份 schema 给出 PG DDL;配置面扩 dialect/dsn 并补 env 绑定;
cmd/server 诊断日志按方言适配。结束时本机 PG 可起三库(冒烟)。

## 2. 背景与当前问题

B01–B04 后查询层已方言无关,但打开路径、DDL、配置都只有 SQLite 一条路。

## 3. 要实现的功能

- engine PG 分支:`sql.Open("pgx", dsn)` + Ping;MaxOpenConns<=0 默认 4;`Path()` 返回
  脱敏 DSN;`JournalMode()/Synchronous()` 返回 "n/a";`Migration.UpPostgres` 生效。
- 三份 PG DDL(仅分歧表):`job_events`/`write_audit` 的 seq 用
  `BIGINT GENERATED ALWAYS AS IDENTITY`;mgr 三张名表用普通 PK + `UNIQUE INDEX ON
  lower(name)`;其余表与 SQLite 同语句。
- mgr 三表 Save 的 PG 冲突目标 `ON CONFLICT (lower(name))`(本卡首项验证;不过则回退
  事务内查后插改)。
- 配置面:三节 `dialect`/`dsn`;`Normalized()` 折默认;`Validate()` 拒绝非法组合;
  env 绑定补 `GODELAYQ_STORE_SQLITE_*`/`GODELAYQ_MGRSTORE_*`/`GODELAYQ_OBSERVABILITY_DIALECT/DSN`;
  重载分类 ClassRestart;example.yaml 与本机 config 同步。

## 4. 实现步骤

1. engine:pgdialect 分支 + UpPostgres 应用 + DSN 脱敏助手。
2. 三包 schema.go 补 UpPostgres;mgr Save 冲突目标按 dialect 分支。
3. core/config.go + config_reload.go + example.yaml;main.go 日志适配。
4. 本机 PG 冒烟:临时程序或 go test 手工钩子连 `postgres://postgres:***@localhost:5432/...`
   建三库(凭据仅 env)。

## 5. 测试要求

- 单元:配置校验(合法/非法组合)、env 绑定、重载分类新键。
- 冒烟(门控):`GODELAYQ_TEST_PG_DSN` 存在时,engine PG 打开 + Migrate v1 全过。

## 6. 完成标准(DoD)

本机 PG 上三库建表成功,SQLite 路径全测试仍绿(行为零变化)。

## 7. 验收方式

```
go build ./... && go vet ./... && go test ./...
$env:GODELAYQ_TEST_PG_DSN='postgres://...'; go test -run TestPostgresSmoke ./internal/store/engine/
```

## 8. 不在本任务范围

方言矩阵全量测试(B06)、数据搬迁工具。

## 9. 风险与回滚

`ON CONFLICT (lower(name))` 表达式目标不被 PG 接受——回退查后插改;pgx 版本兼容——
随卡 pin。回退本卡提交。

## 10. 实现记录

(执行时填写)
