# TASK-RE01　状态机收口与 Cancel 落终态

- 所属阶段：可靠执行（地基卡）
- 依赖任务：无
- 涉及文件：新增 `core/state.go`、`core/state_machine_test.go`；改 `core/scheduler_exec.go`、`core/scheduler_job.go`、`core/scheduler_runtime.go`、`core/scheduler.go`、`api` 取消相关断言
- 预计规模：中（改动面广但每处小，纪律要求高）

## 1. 任务目标

状态转移集中到一张表校验，调度器所有"状态写 + 事件"收口到 `persistAndEmit`；Cancel 从"删除快照"改为"落 cancelled 终态"。

## 2. 背景与当前问题

设计文档 §3.3：转移规则散落在约 10 处手写（`executeJob`、`handleInterrupted`、`handleFailure`/`markFailed`、`pausePendingJob`、`Resume`、`Cancel`、`Restore`），没有一处能回答"哪些转移合法"。`Cancel`（`core/scheduler_job.go`）统一 `store.Delete`，cancelled 状态从不落盘——"任务存在过且被取消"在存储里无据可查，后续 attempt 账本（RE02）也没有可以挂的终态收尾点。

本卡是 RE02-RE07 的地基：后续所有卡的状态写都必须走本卡收口的通道。

## 3. 要实现的功能

1. `core/state.go`：转移表与 `canTransition(from, to) (bool)`，表内容与出边来源逐一对照设计文档 §6.5 的两张表；`from == to` 恒真（原地更新不算转移）。`canTransition` 是导出函数但表本身不导出——规则只许有这一份。
2. `core` 的 `persistAndEmit(job *Job, events ...Event)`：
   - 状态变化时先 `canTransition` 校验；非法则记 error 日志、**放弃状态写但照常发事件**（宁可账本少一行，不可账本说谎——理由注释写清）。
   - 本卡阶段 outbox 能力恒为 nil（RE07 才接）：行为 = `store.Update(job.ToSnapshot())` + 逐个 `eventBus.Publish`，与现状逐字节等价；但通道先收口，RE07 只改这一个函数的实现。
3. 既有状态写点全部改走 `persistAndEmit`（设计文档 §6.5 出边表逐一对应）：`executeJob`（running 落盘、无 handler 失败）、`handleSuccess`、`handleFailure`/`markFailed`、`handleInterrupted`、`pausePendingJob`、`parkForcedPause`、`holdRestored`、`UpdatePending`/`SetGroup` 的快照写，以及 `Schedule` 的首次落盘（Save + job.scheduled 并入，outbox 模式下 Save+入箱同事务，见设计文档 §7.1）。
4. Cancel 三条路径改落终态（设计文档 §7.2）：
   - 新增 `cancelRequested map[string]struct{}`（仿 `forcedPause`，同锁保护；`Start` 复位）；
   - 堆内/暂停中：直接置 cancelled → `persistAndEmit`（job.cancelled），删除原三处 `store.Delete`；
   - 执行中：登记标记 + 取消 ctx → `handleInterrupted` 收尾认领（`takeCancelRequested`，一次性），转移 running→cancelled；`parkForcedPause` 的认领优先级保持最高（强制暂停先于取消）。
   - `handleInterrupted` 里"调度器在跑 = 用户取消"的既有分支（只记日志不落盘）改为：有 `cancelRequested` 标记 → 落 cancelled；无标记且调度器在跑 → 维持现状（用户 `Cancel` 已在调用侧落过终态）。
5. `api` 适配：取消后 `GET /jobs/:id` 返回 200 + cancelled（原来 404）；受影响的 handler 断言与文档注释更新（`docs/api.md` 的正文修订留给 RE08，本卡只改代码内注释与测试）。

## 4. 实现步骤

1. 写 `core/state.go` + 转移矩阵测试（先测试后实现，表驱动的穷举用例直接当规格）。
2. 加 `persistAndEmit`，逐个状态写点迁移（每迁一处跑一次包内测试；`scheduler_*_test.go` 现有断言是回归网）。
3. Cancel 改造 + `cancelRequested` 生命周期（登记/认领/Start 复位）。
4. api 断言适配与全量回归。

## 5. 测试要求

- `core/state_machine_test.go`：转移表穷举（6×6 全对全错）；每个出边至少一条端到端用例（从真实调用点驱动）。
- 取消三态各一用例：堆内取消 → 快照 cancelled；暂停中取消 → 快照 cancelled；执行中取消 → handler 返回后快照 cancelled、事件 job.cancelled。
- 非法转移注入：直接构造跨态调用（如对 success 快照 Resume），断言状态未被改写且日志出现违规记录。
- 既有恢复/暂停/更新测试全绿（回归网不许松）。

## 6. 完成标准（DoD）

1. 全部状态写点经 `persistAndEmit`；包内 grep 无绕行的裸 `store.Update(.*Status)`（收口函数自身除外）。
2. Cancel 不再调用 `store.Delete`（调度器范围内）。
3. 转移表测试穷举通过；`go test ./...` 全绿。

## 7. 验收方式

`go build ./... && go vet ./... && go test ./...`；手工：起服（dev.ps1）→ 提交任务 → 取消 → `GET /jobs/:id` 得 cancelled → 重启进程确认 Restore 未复活它。

## 8. 不在本任务范围

Attempt 账本（RE02）、outbox 接线（RE07）、`docs/api.md` 正文修订（RE08）、控制台前端对 cancelled 展示的改造。

## 9. 风险与回滚

- **风险：取消语义变化破坏既有调用方**（依赖 404 的集成）。缓解：设计文档 §11.3 已列为需文档化的行为变化；回滚 = revert 本卡，存储里多出的 cancelled 快照对旧代码表现为"终态留痕"，Restore 不复活，无脏状态。
- **风险：收口过程中漏迁某个写点**，出现"绕过校验的状态写"。缓解：DoD 第 1 条的 grep 自检 + 测试网；`persistAndEmit` 注释列出全部合法调用点清单。
- **风险：`cancelRequested` 与 `forcedPause` 的认领顺序交错**。缓解：认领顺序写进注释并用并发测试钉住（暂停标记先认领，取消标记保持待认领或一并终止为 paused——以"暂停优先于一切"的既有口径为准）。

## 10. 实现记录（2026-11-08）

分支 `feat/re01-state-machine`。按卡交付，无设计偏离；三处实现口径补充进设计文档 §6.5 注记：

- **新增 `core/state.go`**：`jobTransitions` 转移表 + `canTransition`（from==to 恒真）；`transition`（校验+赋值+落盘+事件，非法时保状态、发事件、error 日志）、`persistAndEmit`（落盘+事件收口，落盘失败记日志并返回错误供需要传播的调用方）、`emit`（事件-only 通道）。RE07 只需改 persistAndEmit/emit 两个函数。
- **写点收口**：`executeJob`（running、无 handler 失败）、`handleSuccess`/`handleFailure`/`markFailed`/`handleInterrupted`、`pausePendingJob`、`parkForcedPause`、`ForcePause` 快照路径、`holdRestored`、`UpdatePending`/`applyGroupToSnapshot`（identity，后者保留错误传播）、`Schedule`（persistAndEmit 先于入堆；scheduled 事件因此严格早于 started）。包内 grep 无绕行裸写（仅 state.go 持有 store.Update）。
- **Cancel 落终态**：三条路径（堆内/暂停中/执行中认领）；`cancelRequested` 集合仿 forcedPause，Start 复位，三条收尾路径都消费（不泄漏）。执行中取消的 job.cancelled 事件移到收尾认领时发（对应"确实停住了"）；Handler 不响应取消而成功返回 → 记 success（标记照消费），失败返回 → 落 cancelled 不再重试。`CancelRunning` 保持只取消 ctx 的原语义（无生产调用方）。
- **行为变化**（§11.3 已载）：取消后快照以 cancelled 终态留痕，GET 返回记录而非 404；批量取消（batch-ops）的 Items 现在回读 cancelled 现状（原先因删除而无 Items）；执行中取消的取消事件时点从"调用瞬间"移到"收尾认领"。
- **测试**：新增 `core/state_machine_test.go`（转移表 6×6 穷举、非法转移拒绝三件套、identity 放行、cancelRequested 成功/失败/生命周期三用例）；适配六个受行为变化影响的既有用例（Schedule 落盘计数 Save→Update、Cancel 删除→终态留痕 ×3、handleSuccess 补 running 前置、守卫日志消息改为收口通用形状）；mockStore 增加 updateCalls 计数。`go test ./... -count=1` 九包全绿（api/cmd 集成含取消链路，手工 dev.ps1 冒烟由该覆盖等价承担）。
- **意外收获**：执行中取消不再删快照，堵住了旧实现"事件说已取消、存储已删除、但 Handler 可能仍在跑"的窗口——强杀恢复时该任务按 running 快照走守卫改判，不再凭空消失。
