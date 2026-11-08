# TASK-RE04　重试分类透传

- 所属阶段：可靠执行
- 依赖任务：RE02（分类结论要落 attempt 行）
- 涉及文件：`executor/exit.go`；改 `core/scheduler_exec.go`（FinishAttempt 组装与事件 metadata）、`core/attempt.go`（若需补充字段语义）
- 预计规模：小

## 1. 任务目标

把执行器侧已经存在的十类失败分类变成持久事实：attempt 行的 `class/retryable/exit_code/signal/http_status/error_text` 与 `job.failed` 事件的 `"class"` metadata。

## 2. 背景与当前问题

设计文档 §3.4：`executor/exit.go` 的 `failureClass` + `classifyExit` 已单点判定"要不要重试"（TASK-E12 交付），但结论只在内存里存在一次——事件 metadata 带了 `"permanent"`/`"timeout"`，持久账本上什么都没有；排查"这条任务历史上失败过几次、每次为什么"只能靠翻日志。

## 3. 要实现的功能

1. `executor/exit.go`：`failureClass` 增 `String()`（值域 = 设计文档 §6.4 词表：invalid_submission / profile_unavailable / not_allowed / timeout / permit_wait / interrupted / exit_code / http_status / address_refused / transport）；`ExitError` 增 `Class string`，`newFailure`/`newStatusFailure` 构造点集中赋值（不新增第二份判定逻辑——只是把既有类别文本化）。
2. `core` 侧组装（`executeJob` 收尾处）：
   - 执行器任务：`errors.As` 取 `ExitError`，class/retryable/exit_code/signal/http_status 原样进 `FinishAttempt`；`error_text` 用 `Error()` 文本（E08 脱敏口径已在其内）。
   - 普通 handler：class = `handler_error`；`errors.Is(err, context.DeadlineExceeded)` → `timeout`；取消路径本就不进失败分支（RE01 已收口为 cancelled/ interrupted）。
   - `job.failed` 事件 metadata 增 `"class"`（additive；`"permanent"`/`"timeout"` 既有键不动，前端透传不解析——与 TASK-E12 同一手法）。
3. attempt 的 `retryable` 判定只读 `core.PermanentError`（与 `handleFailure` 同一函数，不出现第二份口径）；`retryable_set=false` 表示未判定（如 handler_error 无标记时），接口与库里都是 NULL 语义。

## 4. 实现步骤

1. `failureClass.String()` + `ExitError.Class` + executor 包内测试。
2. core 组装 + attempt 字段落库（依赖 RE02 的 FinishAttempt）。
3. 事件 metadata 增键。

## 5. 测试要求

- executor：十类各自的 `String()` 与 `Class` 断言（表驱动，与 `classifyExit` 用例合流）。
- core：执行器任务失败 → attempt 行 class/retryable/exit_code 与 `ExitError` 一致；普通 handler 失败 → `handler_error`；超时 → `timeout`。
- 事件：`job.failed` metadata 同时含既有键与新键（JSON 形状 additive 断言，既有形状逐字节不动的用例继续通过）。

## 6. 完成标准（DoD）

1. 十类词表在 `executor` 与设计文档 §6.4 一一对应，无第二份判定。
2. 失败任务账本行可见完整结论；事件 metadata 兼容。

## 7. 验收方式

提交一个 `runtime: nod`（拼错解释器）的档位任务 → `GET /jobs/:id/attempts` 该行 `class=profile_unavailable`、`retryable=false`；`GET /events` 该任务的失败事件带 `"class":"profile_unavailable"`。

## 8. 不在本任务范围

普通 handler 的强制分类通道（维持二分，设计文档 §4.2）、退避策略本身的改动（`RetryPolicy` 只管"何时"的分工不变）。

## 9. 风险与回滚

- **风险：事件 JSON 形状被前端隐式依赖**。缓解：只增键不增删；`api/history.go` 透传路径的既有断言继续跑。
- 回滚：revert 后 attempt 行少两列语义（class/retryable 为 NULL），账本结构不受影响。

## 10. 实现记录（2026-11-08）

分支 `feat/reliable-execution`。按卡交付，无设计偏离：

- **executor/exit.go**：`failureClass.String()` 十类词表（全仓唯一映射）；`ExitError` 增 `Class` 与 `HTTPStatus` 两个必填字段（构造点 `newFailure`/`newStatusFailure` 集中赋值——原实现状态码只参与重试判定不落错误对象，账本的 http_status 列因此补上来源）；`AttemptOutcome` 六方法实现 + 编译期断言（ok 语义按类别分列：exit_code/http_status 互不冒充）。
- **core/attempt.go**：`AttemptOutcome` 接口（依赖方向 executor → core 不变，与 PermanentError 同手法）+ `attemptClassOf`（超时优先 → 接口其次 → handler_error 兜底）。
- **core/scheduler_exec.go**：`finishAttempt` 加 outcome 闭包参数（仅 failed 行填明细，cancelled 行 class 留 NULL——cancel 原因在事件里）；`fillFailureAttempt` 按 outcome 填 class/retryable/exit_code/signal/http_status/error_text；`handleFailure` 签名加 `failureClass`（executeJob 算一次，事件 metadata 与账本行同源）；`job.failed` metadata 增 `"class"`（additive）。
- **普通 handler**：维持 PermanentError 二分不强制分类——class 记 handler_error/timeout，attempt 只有错误文本（与事件 error 字段同一暴露面）。
- **测试**：executor 十类词表表驱动 + 构造点分类/AttemptOutcome ok 语义；core 用 AttemptOutcome 替身（core 不能 import executor）验证明细落账三形态（outcome/handler_error/exit_code）+ attemptClassOf 顺序 + 事件 metadata class；既有 handleFailure 直调测试点补第三参。
- **验证**：`go build/vet` 干净；`go test ./... -count=1` 九包全绿。
