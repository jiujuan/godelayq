# 可靠执行任务卡（RE01 … RE08）

设计说明在 `../../reliable-execution-design.md`（下称"设计文档"），本目录把它拆成可逐个执行的卡片。
目标：把单任务调度做成可解释、可恢复、可审计的可靠执行——Job/Attempt 账本、幂等提交、
状态机、重试分类、恢复策略、事务性 Outbox、Cron 时区与 misfire、故障注入测试。

## 执行顺序

| 阶段 | 任务 | 内容 | 该卡结束时可演示的能力 |
| --- | --- | --- | --- |
| RE01 | 状态机 | 转移表 + `persistAndEmit` 收口 + Cancel 落终态 | 所有状态变化有唯一通道；取消后 GET 得 cancelled 记录 |
| RE02 | Attempt 账本 | `job_attempts` 表 + `AttemptLedger` + 调度器挂点 | 一次失败重试 = 账本两行；`GET /jobs/:id/attempts` 可查 |
| RE03 | 幂等提交 | `job_idempotency` 表 + `Idempotency-Key` + 指纹查重 | 同请求重发 200 不重复建任务，异内容 409 |
| RE04 | 重试分类 | `failureClass.String()` + 分类落 attempt 与事件 | 失败类别/retryable 在账本与 `job.failed` metadata 可见 |
| RE05 | 恢复策略 | `scheduler.recovery_policy` 配置化 + 悬空 attempt 落 abandoned | 崩溃恢复策略可配；账本无永远悬空的行 |
| RE06 | Cron 时区与 misfire | `timezone` 字段 + `misfire_policy` + `job.misfired` | 按任务时区触发；错过的窗口有策略有留痕 |
| RE07 | Outbox | `job_outbox` 表 + `UpdateWithEvents` + 中继器 + 读侧去重 | 状态变更与事件同事务；杀进程重启事件补投且不重复入库 |
| RE08 | 故障注入与文档 | 成套故障用例 + 三份文档同步 | §12 验收清单全绿 |

依赖：

```
RE01 ──► RE02 ──► {RE03, RE04, RE05, RE06}   （四卡互不依赖，可并行或单独挑做）
  └────► RE07                                （只依赖 RE01，可与 RE03-RE06 并行）
全部 ──► RE08                                （收口，需要前面全部落地）
```

- RE01 是地基：后续所有卡的状态写都走它收口的 `persistAndEmit`。
- RE02 给 RE04/RE05/RE06 提供 attempt 挂点（`trigger_source`、abandoned 收口）。
- RE07 与 RE02 无相互依赖，但同改 `store/jobs` 的迁移与 golden，**建议错峰提交**避免迁移版本号冲突（版本号分配已在设计文档 §6.1 定死：v2=attempts、v3=idempotency、v4=outbox、observe v2=加列）。

## 每张卡片的结构

十段结构，与 `../executor/`、`../mgrdb/` 一致：任务目标 / 背景与当前问题 / 要实现的功能 /
实现步骤 / 测试要求 / 完成标准（DoD）/ 验收方式 / 不在本任务范围 / 风险与回滚 / 实现记录。

## 全卡共同的验证口径

- `go build ./...`、`go vet ./...`、`go test ./...` 必须通过；本机有 PG DSN 时跑
  `scripts/test-pg.sh`（与 CI `postgres-matrix` 同口径）。
- 改 `core/config.go` 的配置键，同步 `configs/config.example.yaml` 与本机 `configs/config.yaml`，
  `go test -run TestExampleConfigMatchesLocal ./internal/core` 把关；热更分级登记进重载链的
  类别表（本系列新键全部 ClassRestart，见设计文档 §9）。
- 动 `store/jobs`、`store/observe` 的卡：跑
  `go test ./internal/store/<pkg>/ -run TestSQLSnapshot -update` 重生成两方言 golden，
  `git diff testdata/` 逐行评审，连跑两次确认稳定（[db-quality-gates-design.md](../../db-quality-gates-design.md) §6）。
- 红线自检：新代码不出现 `.SQL()/Query/QueryRow`、`DialectPostgres/DialectSQLite`、
  `fmt.Sprintf` 进构建器（storeguard 三守卫）；如必须方言分支，收口成单函数并在
  `package_guard_test.go` 登记豁免（先例：`mgr/models.go` 的 `nameConflictClause`）。
- 接口在消费方：`AttemptLedger`/`EventOutbox`/`SnapshotReader` 定义在 core 或 api（按消费方归属），
  实现方只做隐式实现 + 编译期断言（`var _ core.X = (*T)(nil)`）。
- 迁移冻结：既有版本（jobs v1、observe v1）语句一个字符不改；新表/新列只以新增版本号表达。
- import 一律 `github.com/jiujuan/godelayq/internal/...`。
- 文档同步统一在 RE08 收口（`docs/architecture.md`、`docs/api.md`、`docs/deployment.md`），
  但行为变化（如 Cancel 不再 404）在该卡交付时就写进本卡的"实现记录"。
