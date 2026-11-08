# TASK-RE02　Attempt 执行账本

- 所属阶段：可靠执行
- 依赖任务：RE01
- 涉及文件：新增 `core/attempt.go`、`store/jobs` 迁移 v2 与 `attempts.go`；改 `core/job.go`（CloneForRetry/cron 重排保 Attempts）、`core/scheduler_exec.go`、`core/scheduler_runtime.go`、`core/scheduler.go`（Option 注入）、`api/handlers.go`（新端点）、`core/config.go`
- 预计规模：中

## 1. 任务目标

每次尝试一行账本：开始（何时、为什么触发）、结束（结论、类别、耗时）可查可审计；崩溃遗留的在途行由 Restore 收口。

## 2. 背景与当前问题

设计文档 §3.1：`Job.Attempts` 只是计数，`Exec *ExecMeta` 只存最近一次结论；"第 N 次尝试为什么失败"只能去事件流里翻，而事件流有丢弃、有保留上限，不是账本。另有既有缺陷：`CloneForRetry` 与 cron 重排都不拷贝 `Attempts`，重试后计数清零，事件里的 attempt 序号从 0 重来。

## 3. 要实现的功能

1. `core/attempt.go`：`Attempt`/`AttemptStatus`（running|success|failed|cancelled|abandoned）/`AttemptTrigger`（scheduled|retry|restore|misfire）与 `AttemptLedger` 接口（设计文档 §6.3，四个方法 + 读方法）。接口定义在 core（消费方是调度器与 api），实现方 `store/jobs` 隐式实现 + `var _ core.AttemptLedger` 编译期断言。
2. `store/jobs` 迁移 v2：`job_attempts` 表（DDL 见设计文档 §6.1，双方言；`trigger_source`/`error_text` 命名避开 PG 保留字）。`PruneAttempts` 按任务保最近 `store.attempts_keep_last` 条（默认 20），与写入同事务周期执行；任务快照被终态留痕淘汰时连带删它的 attempts（`trimTerminal` 的删除事务里追加）。
3. 调度器挂点（都经 RE01 的 `persistAndEmit` 所在协程，无新锁）：
   - `executeJob` 开始：`BeginAttempt(jobID, {seq: ++job.Attempts, status: running, trigger: ...})`——trigger 的取值：重试路径 `retry`、Restore 补跑 `restore`、misfire 补跑（RE06 起）`misfire`、其余 `scheduled`。判定依据写进注释（重试 = 刚经 `CloneForRetry`，restore = 本次执行来自 Restore 入堆的快照，用 job 上的来源标记传递，不猜）。
   - 收尾：`FinishAttempt`（success/failed/cancelled 三种 status，class/retryable 等 RE04 再填，本卡先记 status/时间/耗时）。
   - `Job.Attempts` 修复：`CloneForRetry` 与 `handleSuccess` 的 cron 重排保留计数；`job.started` 事件 metadata 的 attempt 值随之为全生命周期单调。
4. Restore 收口：处理崩溃前 running 的快照时（改判 replay 或 pause 都一样）调 `AbandonAttempts(jobID, snap.Attempts, now)`——悬空行落 `abandoned` + finished_at。
5. JSON 模式降级：`AttemptLedger` 为 nil 时挂点全部跳过（构造时记一次 info 日志说明降级，不逐次记）。
6. 新端点 `GET /api/v1/jobs/:id/attempts`（设计文档 §8.2）：ledger 能力缺失返回 200 + `"ledger": false`；RBAC 与既有读端点同档（reader 可读）。
7. 配置：`store.attempts_keep_last`（§9 口径）。

## 4. 实现步骤

1. core 接口与结构 + 单测（attempt 状态/trigger 的 String/Parse 对称）。
2. jobs 迁移 v2 + `attempts.go`（模型、五方法、PruneAttempts、trimTerminal 连带删除）+ SQL 快照 golden 重生成。
3. 调度器挂点与 Attempts 修复。
4. Restore 收口 + api 端点 + 配置。

## 5. 测试要求

- `store/jobs` 双后端（`runWithStoreBackends`）：begin/finish/abandon/读回/prune 全覆盖；PG 缺 DSN 自动跳过。
- `core/attempts_test.go`：成功任务 = 1 行 success；失败重试 = 2 行（scheduled+retry，seq 连续）；JSON 模式（nil ledger）不写不炸。
- 崩溃恢复：构造 running 快照 → Restore → 悬空行 abandoned + 重跑开出新 seq。
- `GET /jobs/:id/attempts`：库模式 200+items；JSON 模式 200+`ledger:false`。

## 6. 完成标准（DoD）

1. 迁移 v2 双方言落地，v1 语句未动；SQL golden 两份重生成且 diff 只含新语句。
2. attempt 行与任务状态同协程同事务能力就绪（本卡 begin/finish 与快照写仍分两次调用，合并留给 RE07 的 `UpdateWithEvents`——注释说明）。
3. `Attempts` 计数跨重试/cron 单调，`job.started` metadata 不再清零。
4. 全量测试绿（含 PG，若 DSN 可用）。

## 7. 验收方式

dev.ps1 起服 → 提交 `max_retries: 2` 的必失败执行器任务 → `GET /jobs/:id/attempts` 看到 3 行（scheduled+retry+retry，status 全 failed）→ 重启进程确认无悬空 running 行。

## 8. 不在本任务范围

失败分类字段的真实取值（RE04）、attempt 与快照同事务（RE07）、misfire trigger 的实际写入（RE06）、控制台账本页。

## 9. 风险与回滚

- **风险：attempt 写放大拖慢执行路径**。sqlite 单连接 + 直写下每 attempt 两次事务（begin/finish）。缓解：keep_last 剪枝控制表规模；若压测显示明显劣化，begin 并入 running 快照写的事务（RE07 顺带做），并在卡内记录实测数据。
- **风险：trigger 来源判定出错**（restore 补跑被记成 scheduled）。缓解：来源标记显式传递（Restore 入堆时打标），不用启发式；测试覆盖三条来源。
- 回滚：revert 后遗留表无害；`job_attempts` 行不被旧代码读写。

## 10. 实现记录（2026-11-08）

分支 `feat/reliable-execution`。按卡交付；两处口径调整已同步设计文档 §6.3：

- **core/attempt.go**：`AttemptStatus`/`AttemptTrigger`（int + String/Parse 对称）、`Attempt`（可空结论字段用 `*Set` 布尔表达"未判定"）、`AttemptLedger` 接口、`WithAttemptLedger` Option、`DefaultAttemptsKeepLast=20`。Job 增 `dispatchSource`（不入快照：CloneForRetry 盖 retry、Restore 盖 restore、零值即 scheduled）。
- **口径调整 ①**：`PruneAttempts` 从全表改为**按任务**（每次收尾后调用）——主键前缀索引使代价为常数，全表清理没有必要（停跑任务天然只剩最近几行）；设计文档 §6.3 已同步。
- **store/jobs**：迁移 v2（`job_attempts`，双方言；`retryable` 列沿用全仓 INTEGER 0/1 先例——`sql.NullBool` 直绑会在 PG 上报类型错，模型用 NullInt64 承载）；attempts.go 五方法（begin/finish 共用 upsert，幂等重放）；`trimTerminal` 与 `Delete` 级联删账本行（同事务）。
- **调度器挂点**：`beginAttempt`（running 落盘后）/`finishAttempt`（三个收尾 + parkForcedPause，均在各自 transition 之前以保时长基准）/`Restore` 的 `AbandonAttempts`（崩溃前 running 的快照，无论改判结果）。nil 账本全跳过，构造日志降级说明一次。
- **Attempts 断链修复**：`CloneForRetry` 与 cron 重排保留计数——`job.started` 的 attempt 值与快照计数跨重试/轮次单调，测试钉住。
- **api**：`attemptReader` 窄接口（消费方）+ `WithAttemptReader` Option + `GET /jobs/:id/attempts`（limit 默认 20 上界 100；JSON 部署 200 + `ledger:false` + 空数组 + detail，不是 503）。
- **配置**：`store.attempts_keep_last`（默认 20，<=0 回默认，负数不解释成"不留账本"）；热更档 ClassRestart；example/本机两份 YAML 同步，TestExampleConfigMatchesLocal 过。
- **测试**：store/jobs 双后端五个用例（round-trip/upsert 幂等/abandon/prune/trim 级联，本机 PG 实跑全过）；core 挂点四用例 + CloneForRetry 单测（内存账本替身）；api 端点三用例（降级/形状/limit 校验）。
- **SQL golden**：双方言重生成，diff 只含账本 upsert/abandon/读回/裁剪与删除级联，逐行评审通过；连跑两次稳定。
- **验证**：`go build/vet` 干净；`go test ./... -count=1` 全绿（含 PG DSN 的 jobs/sql 快照矩阵）。
