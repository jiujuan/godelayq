# 数据库质量门禁任务卡(Q01 … Q11)

设计说明在 `../../db-quality-gates-design.md`（P0）与
`../../db-quality-gates-p1-write-fusion-design.md`（P1+写合并），本目录把它们拆成
可逐个执行的卡片。P0 三卡串行：Q01 让矩阵自动跑，Q02/Q03 前需要 CI 的口径先行确定
（同一份命令）。

## 执行顺序

| 阶段 | 任务 | 内容 | 该卡结束时可演示的能力 |
| --- | --- | --- | --- |
| Q00 | 设计文档 | `docs/design/db-quality-gates-design.md` | 方案定稿 |
| Q01 | CI | `.github/workflows/ci.yml` | push/PR 自动跑快档 + PG 矩阵 |
| Q02 | 守卫 | `internal/store/storeguard` + 三包接线 | SQL() 逃生口/方言分支/动态拼接进测试期红线 |
| Q03 | 快照 | 三包 `sql_snapshot_test.go` + golden | bun 生成 SQL 文本被 golden 钉住，`-update` 重写 |
| Q04 | 可移植性修复 | CI 首跑暴露的 PG 方言问题 | 全矩阵绿 |
| Q05 | G4 可观测 | DBStats/方言/schema 版本/出箱积压/丢弃计数 → /admin/runtime | 运行时可见 DB 占用与事件丢弃 |
| Q06 | G5 慢查询 | bun QueryHook 阈值日志（默认关） | 超阈值查询 Warn 可查，SQL 截断 |
| Q07 | G6 语句超时 | PG statement_timeout 经 DSN，迁移连接排除 | 长查询有上限，缺省 Warn |
| Q08 | W1 账本并入 running | `transitionWithAttempt` + UpdateWithEvents 变体 | 热路径 6 写 → 5 写 |
| Q09 | W2 completed 并入终态 | 成功路径事件与快照同事务（出箱模式） | 成功路径再省 1 事务 |
| Q10 | W3 prune 降频 | seq 取模触发 | cron 路径空跑 DELETE 消除 |
| Q11 | G7 迁移纪律 | storeguard 三条新规则 | 版本/可移植性/v1 冻结进测试期红线 |

P1 依赖：Q05 先于 Q08–Q10（写次数读数是合并卡的证据）；Q06–Q07 与 W 系列无强依赖。
P2（G8 跨方言 schema 契约、G9 性能基准、G10 备份/kill -9 runbook）不在本批。

## 每张卡片的结构

与 `../bun-store/README.md` 的十段结构一致。

## 全卡共同的验证口径

- `go build ./...`、`go vet ./...`、`go test ./...` 必须通过；配
  `GODELAYQ_TEST_PG_DSN` 时 Q02/Q03 的 PG 侧一并真跑。
- 提交信息 Conventional Commits，中文；每卡一个提交，提交自身全绿。
- 守卫类改动要做过"人为违规必红"的自证；快照类改动要做过"两次生成零 diff"的自证；
  W 系列改动要给出 SQL 快照 diff 的逐行评审记录（预期语句序列变化之外零 diff）。
