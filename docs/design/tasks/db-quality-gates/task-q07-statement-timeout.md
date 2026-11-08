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

- 2026-11-09 完成。注入机制：pgx/v5 把 URL 查询串里它不认识的参数当作**服务端
  运行时参数**逐会话应用——`withStatementTimeout`（engine/postgres.go）把毫秒值
  注入 DSN（URL 与 keyword/value 两形态都支持），`sql.Open` 前生效，池里每条
  连接都带上限。
- **迁移豁免与卡面的偏差**：卡面写"迁移连接单独拼 DSN"，实现改为**迁移事务内
  `SET LOCAL statement_timeout = 0`**（engine.Migrate，仅 PG 分支）。同一保证
  （启动期长 DDL 不受业务上限约束）但不开第二条连接、不碰 MaxOpenConns=1 的
  装配假设，且 SET LOCAL 事务结束即失效——豁免不会随连接归还泄漏给业务查询。
  事务外的记账查询（建表/读版本/落版本）是亚毫秒快语句，留在上限内。真库用例：
  100ms 上限下 `pg_sleep(2)` 业务语句被取消、同一语句在迁移事务里照常完成。
- 合并语义（卡内定）：DSN 已带 `statement_timeout` 时启动**报错**（两处上限取
  哪边都是猜）；亚毫秒取值在注入层直接拒绝（等价于"每条语句都失败"）。
- 缺省 Warn：openPostgres 在 StatementTimeout<=0 时记一条 Warn（每库一条，
  "没有上限"要说在明处）；openSQLite 对配置了该键的调用方 Warn"postgres-only、
  已忽略"（机制层不校验，提示可见）。
- 配置：`store.statement_timeout`（StoreConfig 级，仅 type=sqlite + dialect=postgres
  生效）。校验三条：负数拒启；sqlite 方言上写它拒启（与"sqlite 却写 dsn"同口径
  ——方言专属键写错方言是配置错误）；type=json 不查（与 outbox_* 同口径，文档
  说明）。reload 档位 ClassRestart（上限在 Open 拼 DSN 时定死）。
- 结构说明：真 PG 用例放 `internal/store/pgtest/engine_statement_timeout_test.go`
  （外部测试包 pgtest_test）——engine 的包守卫禁止**目录内任何文件**（含测试）
  引仓内包，而 pgtest 依赖 engine；"真库验证"也是 pgtest 的职责本位。
- 测试教训：kill 用例不能断言报错文本——服务端消息随 locale 走（本机 PG 中文
  "由于语句执行超时"，CI 英文），断 **sqlstate 57014**（pgx 拼进错误串的稳定部分）。
- jobs.Open 加 `statementTimeout` 参数（与 slowQueryThreshold 同款显式传参）；
  cmd/server 传 `cfg.Store.StatementTimeout`。
- 文档：deployment.md 参数行 + 调参 bullet（两豁免一边界：只管单条语句时长，
  不含事务总时长与 lock_timeout）；config.example.yaml 同步。
- 自证：全量 9 包 EXIT=0；本机 PG 矩阵 6 包 EXIT=0（kill/豁免/Warn 真跑，
  断言 57014）；DSN 注入纯函数 4 用例 + 配置校验 4 分支 + reload 档位表行。
