# TASK-RE06　Cron 时区与 misfire 策略

- 所属阶段：可靠执行
- 依赖任务：RE02（misfire 的 attempt 记 `trigger_source=misfire`）
- 涉及文件：`core/cron.go`、`core/job.go`（Timezone/MisfirePolicy 字段）、`core/scheduler_job.go`（Schedule/Resume 的 cron 分支）、`core/scheduler_runtime.go`（Restore 的 misfire 判定）、`core/scheduler_exec.go`（handleSuccess 重排）、`core/event.go`（`EventJobMisfired`）、`api/dto.go`、`core/config.go`
- 预计规模：中

## 1. 任务目标

cron 触发按任务自己的时区计算；停机错过的窗口有策略（补跑一次 / 跳过）、有事件留痕（`job.misfired`）、账本可辨（trigger_source=misfire）。

## 2. 背景与当前问题

设计文档 §3.7：`CronParser.Next(expr, now)` 的时区跟随服务器本地；Job 无时区字段——同一份 cron 表达式在两个时区的部署上行为不同且无从声明。misfire 侧：Restore 把过期任务立即补跑一次，其余窗口静默蒸发，无事件无策略。

## 3. 要实现的功能

1. 时区：
   - `CreateJobRequest` 增 `timezone`（可选，IANA 名）；`Job`/`JobSnapshot` 增 `Timezone string`（omitempty）。提交时 `time.LoadLocation` 校验，非法 400；空 = 服务器本地（现状）。
   - `CronParser.Next(expr string, now time.Time, loc *time.Location)`：内部 `now.In(loc)` 交给 robfig（其 schedule 按入参时刻的时区展开）；nil loc = time.Local。全部 cron 调用点（`Schedule`、`handleSuccess`、misfire 计数）统一走带 loc 版本，旧签名删除或保留为薄包装——包内只有一份口径。
   - 一次性任务的 `TriggerAt` 绝对时刻不受时区转换影响（调用方给的就是绝对时刻）——只对 cron 表达式的展开有意义，注释写明。
2. misfire：
   - 判定点：`Restore` 与 `Schedule`（cron 分支）：`IsRepeat && CronExpr != ""` 且快照/入参 `TriggerAt < now - scheduler.misfire_grace_period`。
   - 策略：任务级 `MisfirePolicy`（fire_once|skip，omitempty = 继承全局 `scheduler.misfire_policy`，默认 fire_once）：
     - `fire_once`：立即入堆补跑一次（现状语义显式化）；attempt 的 trigger_source 记 `misfire`；
     - `skip`：不补跑，TriggerAt 直接改 `Next(now)`。
   - 两种策略都发布 `job.misfired` 事件：metadata `missed_count`（解析器从 TriggerAt 迭代 `Next` 到 now 统计，**封顶 1000** 防病态表达式——超过记日志说明计数截断）、`first_missed_at`、`policy`。
   - `grace_period` 默认 60s（`scheduler.misfire_grace_period`）：秒级重启不刷事件。
   - misfire 事件走 RE01 的 `emit` 通道（outbox 就绪后进箱，RE07）。
3. 配置：`scheduler.misfire_policy`、`scheduler.misfire_grace_period`（§9）。

## 4. 实现步骤

1. 时区字段 + parser 改签名 + 全调用点迁移（提交校验、Schedule、handleSuccess、Restore）。
2. misfire 判定与策略 + `job.misfired` 事件 + missed_count 计数助手（放 `core/cron.go`，纯函数好测）。
3. api 字段 + 配置 + 测试。

## 5. 测试要求

- 时区：同一表达式 `0 0 9 * * *` 在 Asia/Shanghai 与 America/New_York 各算一个 next（断言各自 09:00 本地）；DST 切换日（纽约 2026 春/秋令）逐小时推进无跳变异常；`timezone: Bogus/Zone` 提交 400。
- misfire：构造 TriggerAt 为 3 窗口前的 cron 快照 → Restore：fire_once 补跑 1 次 + `job.misfired`（missed_count=3）；skip 不补跑 + next 正确；grace 内过期（如 10s）不触发事件。
- 迭代封顶：把 TriggerAt 设为 5000 窗口前，断言 missed_count 封顶且有日志。
- attempt 的 trigger_source=misfire（依赖 RE02 基建）。

## 6. 完成标准（DoD）

1. 全部 cron 展开点带时区；快照往返（save→restore）后时区不丢。
2. misfire 两种策略 + 事件 + 封顶全部有测试；默认行为（无新配置）与现状一致——fire_once + grace=60s 内等同于"立即补跑"。

## 7. 验收方式

提交 `timezone: America/New_York` 的 cron 任务 → 控制台 trigger_at 显示为纽约 09:00（本地换算正确）；把系统时间前拨（或用 grace 内手工构造）跨 2 窗口重启 → 日志与 `GET /events` 可见 `job.misfired`（missed_count=2），任务只补跑一次。

## 8. 不在本任务范围

一次性任务的 misfire 策略（设计文档 §4.2）、cron 表达式语法扩展（robfig 既有能力为准）、控制台时区选择器。

## 9. 风险与回滚

- **风险：DST 边界的触发时刻出乎直觉**（2:30 在春季不存在）。缓解：robfig 的既有语义为准（跳到下一合法时刻），测试钉住不自己发明规则。
- **风险：misfire 计数迭代在病态表达式上退化**。缓解：封顶 1000 + 日志。
- **风险：grace_period 太小导致每次重启刷 misfire 事件**。默认 60s 起步，deployment.md（RE08）给调参建议。
- 回滚：revert 后回本地时区 + 立即补跑现状；快照里多出的 `timezone` 字段被旧代码忽略。

## 10. 实现记录（2026-11-08）

分支 `feat/reliable-execution`。按卡交付，两处口径补充进代码注释与设计文档：

- **core/cron.go**：`Next(expr, now, loc)`——nil 表示按入参时刻自带时区展开（与旧行为逐字节等价，robfig 按入参时区计算）；`CountMissedWindows(expr, from, to)`（迭代封顶 1000，截断返回 true）；`WithMisfirePolicy(policy, grace)` Option。
- **core/job.go**：Job/JobSnapshot 增 `Timezone`/`MisfirePolicy`（omitempty，快照往返有测试）；`EffectiveLocation`（非法名防御性回本地）与 `EffectiveMisfirePolicy`（任务级优先，未知值回 fire_once）。
- **core/scheduler_runtime.go**：`applyMisfirePolicy`——判定（IsRepeat+Cron+TriggerAt 非零+早于 now-宽限）→ fire_once 保持过期时刻（立即补跑，dispatchSource 盖 misfire 章）/ skip 改写下一窗口（回 scheduled 章），两种都发 `job.misfired`（missed_count/first_missed_at/policy，截断加标）；Restore 判定点在改判与入堆之前。
- **core/scheduler_job.go（Schedule 判定点）**：cron 分支三路径——TriggerAt 为零按表达式（含时区）取下一窗口；其余交 applyMisfirePolicy。**与旧行为的惟一差异**：宽限内的过期时刻从"重算 Next(now) 跳过该窗口"改为"立即触发该窗口"——宽限语义的自洽结论，已有测试（TestScheduler_Resume_CronJobKeepsRecentTrigger）按新契约改写；表达式合法性先验一次（三条路径都要求可解析）。
- **api**：`timezone`/`misfire_policy` 请求字段 + 提交期校验（非法 IANA 名/未知策略 400）；calculateTriggerTime 按声明时区算首轮。
- **配置**：`scheduler.misfire_policy`（fire_once|skip，默认 fire_once）+ `scheduler.misfire_grace_period`（默认 60s，负值拒）；ClassRestart；两份 YAML 同步；cmd/server 注入 WithMisfirePolicy。
- **测试**：misfire_test.go 九用例——计数（正常/空区间/封顶/非法表达式）、NY 时区跨春秋令墙钟不变、Restore fire_once/skip/宽限/封顶四态（含 dispatchSource 与事件 metadata 断言）、Schedule 双判定点、快照往返、EffectiveLocation 防御回退。
- **验证**：`go build/vet` 干净；`go test ./... -count=1` 九包全绿（TestExampleConfigMatchesLocal 含新键）。
