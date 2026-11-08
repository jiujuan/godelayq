# 数据库质量门禁任务卡(Q01 … Q03)

设计说明在 `../../db-quality-gates-design.md`，本目录把它拆成可逐个执行的卡片。
P0 三卡串行：Q01 让矩阵自动跑，Q02/Q03 前需要 CI 的口径先行确定（同一份命令）。

## 执行顺序

| 阶段 | 任务 | 内容 | 该卡结束时可演示的能力 |
| --- | --- | --- | --- |
| Q00 | 设计文档 | `docs/design/db-quality-gates-design.md` | 方案定稿 |
| Q01 | CI | `.github/workflows/ci.yml` | push/PR 自动跑快档 + PG 矩阵 |
| Q02 | 守卫 | `internal/store/storeguard` + 三包接线 | SQL() 逃生口/方言分支/动态拼接进测试期红线 |
| Q03 | 快照 | 三包 `sql_snapshot_test.go` + golden | bun 生成 SQL 文本被 golden 钉住，`-update` 重写 |

P1/P2（可观测、慢查询、语句超时、迁移纪律、schema 契约、基准、runbook）不在本批。

## 每张卡片的结构

与 `../bun-store/README.md` 的十段结构一致。

## 全卡共同的验证口径

- `go build ./...`、`go vet ./...`、`go test ./...` 必须通过；配
  `GODELAYQ_TEST_PG_DSN` 时 Q02/Q03 的 PG 侧一并真跑。
- 提交信息 Conventional Commits，中文；每卡一个提交，提交自身全绿。
- 守卫类改动要做过"人为违规必红"的自证；快照类改动要做过"两次生成零 diff"的自证。
