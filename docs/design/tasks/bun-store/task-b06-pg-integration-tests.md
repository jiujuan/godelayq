# B06 PG 集成测试(方言矩阵 + 双平台脚本)

## 1. 任务目标

同一套 store 断言在 SQLite 与 PostgreSQL 两个后端上都跑;本机 PG 实测全绿;提供双平台
一键脚本。无 DSN 时一切照旧(干净跳过)。

## 2. 背景与当前问题

B02–B04 的测试 opener 参数化只挂了 SQLite;PG 的真实回归没有载体。

## 3. 要实现的功能

- `internal/store/pgtest` 测试辅助:`Database(t, purpose)` 连 `GODELAYQ_TEST_PG_DSN`
  (管理库)→ `CREATE DATABASE godelayq_test_<purpose>_<随机>` → 返回库级 DSN →
  `t.Cleanup(DROP DATABASE ... WITH (FORCE))`;env 缺失返回 skip 信号。
- 各 store 包的测试以 `t.Run("postgres", ...)` 挂载同一批断言函数。
- `cmd/server/main_integration_test.go` PG 变体:三库指向同一测试库,走完整装配→启动→关停。
- `scripts/test-pg.ps1` / `test-pg.sh`:读 `GODELAYQ_TEST_PG_DSN` → 跑
  `go test ./internal/store/... ./cmd/server` → 汇总;未配时打印指引退出。
- schema 断言助手按方言分流(sqlite_master vs information_schema/pg_indexes)。

## 4. 实现步骤

1. 写 pgtest 包;2. 各包挂 postgres 子测试;3. 集成测试 PG 变体;4. 双平台脚本;
5. 本机 PG 实跑全矩阵。

## 5. 测试要求

- 全矩阵:jobs/mgr/observe 全部断言 × {sqlite, postgres}。
- 无 DSN 环境:`go test ./...` 与主干行为一致(跳过,不失败)。

## 6. 完成标准(DoD)

本机 PG:`GODELAYQ_TEST_PG_DSN=... go test ./internal/store/... ./cmd/server` 全绿。

## 7. 验收方式

```
# Windows(Git Bash 亦可)
export GODELAYQ_TEST_PG_DSN='postgres://<user>:<pass>@localhost:5432/postgres?sslmode=disable'
bash scripts/test-pg.sh
```

## 8. 不在本任务范围

CI 接入(仓库尚无 CI)、数据搬迁、性能对比。

## 9. 风险与回滚

本机 PG 未启动/凭据不符——脚本前置探测给清晰报错;并行包互踩——每测试独立建库。
回退本卡提交。

## 10. 实现记录

(执行时填写)
