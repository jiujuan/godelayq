# godelayq 存储层 bun 化与 PostgreSQL 就绪设计

> 状态：定稿（2026-11-08）。任务卡见 `tasks/bun-store/`。
> 关联：`mgrdb-design.md`（三库格局的来源）、`sqlite-observability-design.md`（观测库批量写与迁移框架的来源）、`../../architecture.md`（分层与消费方接口）。

## 1. 背景与目标

当前三个库（jobs / mgr / observe）全部是手写 SQL + `modernc.org/sqlite`。后续要接入
PostgreSQL，如果把 SQL 逐句翻译成 PG 方言，等于把 `internal/store` 里的每个调用点再写一遍。

方案：引入 [uptrace/bun](https://github.com/uptrace/bun)（SQL-first ORM，构建于 `database/sql`
之上，方言可插拔），把全部数据库操作改写为 bun 查询构建器 + bun 事务。查询层从此与方言解耦：
**SQLite 继续作为默认后端、行为零变化；PostgreSQL 成为真实可用的第二后端**。

非目标：不重设计表结构、不把 JSON 载荷拆成列、不动任何消费方接口、不提供 sqlite→PG 的
数据搬迁工具（记为后续项）。

## 2. 决策摘要

| # | 决策 | 理由 |
| --- | --- | --- |
| D1 | 用 bun 查询构建器，不用 bun 的 schema 迁移/模型自动建表 | 表名/列名是冻结契约；现有版本化迁移框架（每版本一个事务）已验证，保留它，只让语句按方言取值 |
| D2 | SQLite 保留 `modernc.org/sqlite`；PG 用 `pgx/v5/stdlib` | 两者都纯 Go、Windows 友好；pgx 是 PG 生态事实标准（备选 bun 自带 pgdriver） |
| D3 | `internal/store/sqlite` 改组为 `internal/store/engine` | "唯一引入驱动的包"红线延续：engine 同时收编 sqlite/pg 两驱动 + bun，其余 store 包只见 bun 与 engine |
| D4 | 沿用"JSON 载荷"模式（`snapshot_json`/`record_json`），bun 模型挂 tag 即可 | 行为零变化；schema 冻结；core 的 struct 不加任何东西（tag 是字符串，不引入 import） |
| D5 | 大小写不敏感名统一走 `WHERE lower(name) = lower(?)` / `ORDER BY lower(name)` | SQLite 端 `lower()` 与 `COLLATE NOCASE` 同为 ASCII 折叠，语义等价；PG 端建 `lower(name)` 唯一索引，一套查询代码两方言通用 |
| D6 | `Migration.Up` 为通用语句，新增 `UpPostgres` 仅在方言分歧时给出 | 绝大多数迁移两方言一致；分歧点只有 AUTOINCREMENT 与 COLLATE NOCASE 两类 |
| D7 | PG 集成测试走本机 PG，环境变量 `GODELAYQ_TEST_PG_DSN` 门控 | 不引入 Docker 依赖；密码不进仓库；未配 DSN 时测试干净跳过 |
| D8 | bun v1.3.0；`sqlitedialect`/`pgdialect` 与核心同模块同版本 | 消除方言包版本漂移问题 |

## 3. 现状盘点（已核实，2026-11-08）

全部裸 SQL 集中在 `internal/store/` 四个子包，约 58 个调用点、81 行 SQL；`cmd`、`api`、
`core`、`executor`、`web` 零 SQL：

| 文件 | 涉及函数 | 表 |
| --- | --- | --- |
| `store/sqlite/base.go` | `OpenFile`/`dsn`/`checkConnectionSettings`/`Migrate`/`SQL`/`JournalMode`/`Synchronous` | 机制层 |
| `store/jobs/db.go` | `upsert`/`Delete`/`LoadAll`/`trimTerminal` | jobs |
| `store/jobs/import.go` | `ImportOnce`/`alreadyImported`/`tableEmpty`/`markImported` | jobs、jobs_meta |
| `store/mgr/users.go` | `List`/`Get`/`Save`/`Delete`/`Count` | mgr_users |
| `store/mgr/groups.go` | 同上四方法 | mgr_groups |
| `store/mgr/profiles.go` | 同上四方法 | mgr_executor_profiles |
| `store/mgr/import.go` | `metaGet`/`metaSetNow` | mgr_meta |
| `store/observe/db.go` | `Stats` | 三张观测表 |
| `store/observe/events.go` | `writeBatch`/`prune`/`readEvents`/`Count` | job_events |
| `store/observe/audit.go` | `writeBatch`/`prune`/`Query`/`auditWhere`/`Count` | write_audit |
| `store/observe/artifacts.go` | `Record`/`markPurged`/`DeleteByJob`/`List`/`Exists`/`ReconcileMissing` | artifact_index |
| 各包 `schema.go` | version-1 DDL | 冻结契约 |

不受影响：core 的 JSON 文件存储（默认后端 + 一次性导入源）、`api/authstore.go`（内存态）、
executor 工件文件。受牵连：`cmd/server/main.go` 的 journal_mode 诊断日志、`core/config.go`
三个配置节、`configs/config.example.yaml`、各 `package_guard_test.go`。

## 4. 范围

**做**：上述 11 个文件的 SQL 全部改 bun；`store/sqlite`→`store/engine` 重构；三份 schema 的
方言化；配置面扩 dialect/dsn；env 绑定补齐；PG 打开路径；方言矩阵测试与本机 PG 集成；
文档同步。

**不做**：JSON 文件存储改造、表结构演进、数据搬迁工具、连接池配置面（PG 固定默认 4 连接）、
`GODELAYQ_*` 之外的配置来源。

## 5. 目标架构

### 5.1 engine 包

```
engine.Options{ Dialect, Path, DSN, BusyTimeout, Synchronous, MaxOpenConns }
engine.Open(opts, logger) (*engine.DB, error)
engine.DB{ *bun.DB; dialect; path; journalMode; synchronous; ... }
```

- `DB` 内嵌 `*bun.DB`：业务包直接 `h.NewInsert()/NewSelect()/NewDelete()/RunInTx(...)`。
- SQLite 分支完整保留现行为：目录 0750 / 文件 0640 预建、`_pragma=` DSN、WAL 降级探测告警。
- PG 分支：`sql.Open("pgx", dsn)` + Ping；`MaxOpenConns<=0` 时默认 4（SQLite 仍强制 1）；
  `Path()` 返回脱敏 DSN（去密码）；`JournalMode()/Synchronous()` 返回 "n/a"。
- `Migrate(table, migrations)` 原样移植：版本表名不变、每版本一个事务、跑成功才记版本；
  `Migration{Version, Up []string, UpPostgres []string}`，`UpPostgres` 为 nil 时与 `Up` 同。
- engine 对仓内业务包零依赖（不 import core），映射在各业务包 Open 里完成。

### 5.2 查询映射

| 现状 SQL | bun 形态 |
| --- | --- |
| upsert `ON CONFLICT(...) DO UPDATE SET col=excluded.col` | `NewInsert().Model(&row).On("CONFLICT (...) DO UPDATE").Set("col = EXCLUDED.col")` |
| tx + Prepare + 循环 Exec（批量写） | `RunInTx` 内 `NewInsert().Model(&rows)` 一次多值插入；空批次不执行 |
| tx + 循环 DELETE（trimTerminal） | `RunInTx` 内循环 `tx.NewDelete()`（语义不变，行数少不值得 IN 化） |
| 动态 WHERE（audit 查询） | `NewSelect()` 链式 `Where(...)`（bun 自动 AND 连接）+ `Limit/Offset` |
| `SELECT COUNT(*)` | `NewSelect().Model(...).Count(ctx)` |
| PRAGMA 读回 / 迁移 DDL / Stats | 保留原生 SQL（语句可移植，经 bun.Tx/`*sql.DB` 执行） |

模型：各包私有 `xxxRow` struct，显式 `bun:"table:...,column:..."` tag（不依赖命名推断），
自增列挂 `autoincrement`（INSERT 自动省略，兼容 SQLite AUTOINCREMENT 与 PG IDENTITY）。

### 5.3 方言分歧清单（全部差异仅此四处）

1. **自增主键**：SQLite `INTEGER PRIMARY KEY AUTOINCREMENT`；PG `BIGINT GENERATED ALWAYS AS IDENTITY`（job_events.seq、write_audit.seq）。经 `UpPostgres` 表达。
2. **大小写不敏感唯一**：SQLite `COLLATE NOCASE` PK；PG 普通 PK + `CREATE UNIQUE INDEX ... ON lower(name)`（mgr 三张名表）。查询统一 `lower(name)`。
3. **upsert 冲突目标**：mgr 三表 SQLite `ON CONFLICT (name)`；PG `ON CONFLICT (lower(name))`（表达式目标）。若 PG 验证不过，回退事务内查后插改。
4. **连接级设置**：SQLite 的 busy_timeout/WAL/synchronous 进 DSN；PG 无对应物，交给 DSN 本身与服务端，engine 记 "n/a"。

布尔仍存 INTEGER 0/1、时间戳仍存 BIGINT 纪元、JSON 仍存 TEXT——这些列零分歧。

## 6. 配置面

三节各新增两个键（节名不动，兼容旧配置）：

```yaml
store:
  sqlite:
    dialect: sqlite          # sqlite | postgres，默认 sqlite
    dsn: ""                  # dialect=postgres 时必填；sqlite 下填了直接报错
    path: ./data/goqsys_jobs.sqlite
    busy_timeout: 2s
    synchronous: normal
mgrstore:
  dialect: sqlite
  dsn: ""
observability:
  dialect: sqlite
  dsn: ""
```

- `core.Normalized()` 折默认 dialect=sqlite；`Validate()`：postgres 必须给 dsn，sqlite 给 dsn 报错。
- env 绑定补齐 `GODELAYQ_STORE_SQLITE_*`、`GODELAYQ_MGRSTORE_*`（此前未绑）、
  `GODELAYQ_OBSERVABILITY_DIALECT/DSN`——容器化 PG 部署靠 env 给 DSN。
- 重载分类：新键全部 ClassRestart（改方言/DSN 要重启，不做热切换）。
- `TestExampleConfigMatchesLocal` 口径不变：example 与本机 config 同步改。

## 7. 测试方案

**单元（不依赖外部环境，每次全跑）**：沿用"t.TempDir() 开真库"模式，SQLite + bun 全量回归
——这是"行为零变化"的证明面。关键重构：各 store 测试的 opener 参数化为
`func(t) *Store`，测试体方言无关；SQLite 恒跑，同一断言函数被 PG 侧复用。

**集成（env 门控）**：`GODELAYQ_TEST_PG_DSN` 指向管理库连接（如
`postgres://postgres:***@localhost:5432/postgres?sslmode=disable`）。测试辅助
`internal/store/pgtest`：连管理库 → `CREATE DATABASE godelayq_test_<用途>_<随机>` →
交出 DSN → `t.Cleanup(DROP DATABASE ... WITH (FORCE))`。各包测试以
`t.Run("postgres", ...)` 挂载；`cmd/server/main_integration_test.go` 加 PG 变体走完整
装配→启动→关停链（三库指向同一测试库：业务表名互不冲突）。

**脚本**：`scripts/test-pg.ps1` / `test-pg.sh`（与 dev 脚本同套双平台文化）：读
`GODELAYQ_TEST_PG_DSN` → `go test ./internal/store/... ./cmd/server` → 汇总。密码只经
env 传递，脚本与仓库不落明文。

**手工验收**（B06 卡）：`config.yaml` 三节切 `dialect: postgres` + dsn 起 dev 脚本，跑
提交→重启→恢复链路后改回 sqlite。

## 8. 兼容性与回滚

- 已存在的 SQLite 库文件无缝继续：DDL、迁移表名、版本号全部不变；`Migration.Up`（通用语句）
  就是原 SQLite 语句。
- PG 是全新部署路径，不迁移旧数据（后续项：一次性搬迁工具，形态类似 `ImportOnce`）。
- 回滚：整分支回退即可；每卡提交自身全绿，可按卡回滚到任意中间态。
- 风险与对策：PG `ON CONFLICT (lower(name))` 表达式冲突目标在 B05 首项验证（失败回退查后插改）；
  批量写改多值 INSERT 后空批次边界由测试钉住；关停顺序与单连接假设不在改动面，靠现有
  集成测试钉住。

## 9. 实施计划

| 卡 | 内容 | 完成态 |
| --- | --- | --- |
| B00 | 本设计文档 + 任务卡 | 方案可执行 |
| B01 | engine 基座：sqlite→engine、bun.DB 底座、Migrate 移植、守卫更新 | 三库仍跑 SQLite，全测试绿 |
| B02 | jobs 包 bun 化 | SQLite 全链绿 |
| B03 | mgr 包 bun 化（lower(name) 统一） | SQLite 全链绿 |
| B04 | observe 包 bun 化（批量写改造） | SQLite 全链绿 |
| B05 | PG 方言：pgx 驱动、PG DDL、配置面、诊断适配 | 本机 PG 可起三库（冒烟） |
| B06 | PG 集成测试矩阵 + 双平台脚本 + 手工验收 | 方言矩阵全绿 |
| B07 | 文档同步 | architecture/README 与实现一致 |
