# Q07 G6 语句超时

## 1. 任务目标

PG 侧 `statement_timeout` 经 DSN 注入，**只对业务连接生效**（迁移/对账连接排除）；
配置 `store.statement_timeout`，缺省 0=不设，启动日志 Warn 提示一次。

## 2. 背景与当前问题

SQLite 侧 busy_timeout/WAL/synchronous 已可配（engine/base.go），PG 侧没有任何
语句级上限：一条失控查询可以无限占住连接。W1–W3 合并后单事务变大，超时上限先行
是给它们兜底。

## 3. 要实现的功能

- 配置 `store.statement_timeout`（duration，0=不设，默认）；>0 时 PG 连接串注入
  `statement_timeout`（注入键名按所配驱动在卡内落实：运行时参数或连接后 SET）；
- **迁移连接排除（已拍板）**：迁移/对账连接单独拼 DSN，不带该参数——启动期长迁移
  不受影响；
- 缺省（0）时启动 Warn 一次，提示"PG 侧未设语句超时"（SQLite 侧有 busy_timeout，
  不在提示范围）；
- deployment.md 配置文档同步。

## 4. 实现步骤

1. 配置节 + Normalized/校验；2. engine PG 连接构造处注入 + 迁移连接排除；
3. 缺省 Warn；4. 文档。

## 5. 测试要求

- PG 侧（DSN 门控）：配置 100ms，注入 `pg_sleep(2)` 类查询断言被杀并报超时；
- 迁移连接：同一配置下建表/迁移路径不被杀（用例内完成一次迁移）；
- 未配置：启动日志恰一条 Warn；
- 与既有 DSN（GODELAYQ_TEST_PG_DSN/用户配置串）合并语义有测试钉住——不覆盖
  用户自带的同名参数要明确（覆盖或报错，二选一，卡内定并写注释）。

## 6. 完成标准(DoD)

配置生效（长查询被杀）、迁移连接豁免、缺省 Warn、DSN 合并语义钉住。

## 7. 验收方式

```
go build ./... && go vet ./... && go test -count=1 ./...
GODELAYQ_TEST_PG_DSN='...' go test -count=1 ./internal/store/... ./cmd/server/
```

## 8. 不在本任务范围

每语句 context 超时改造（应用层）、锁等待超时（lock_timeout）、SQLite 侧变更。

## 9. 风险与回滚

DSN 合并与现网连接串冲突——合并语义由测试钉住（见 §5）；全局超时误杀合法长查询
——默认 0 不设，开启是显式运维动作。回退：配置置 0。

## 10. 实现记录

- 待实施。
