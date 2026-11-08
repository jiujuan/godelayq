# TASK-RE05　恢复策略配置化

- 所属阶段：可靠执行
- 依赖任务：RE02（悬空 attempt 收口）
- 涉及文件：`cmd/server/main.go`（`installRestoreGuard`）、`core/config.go`、`core/scheduler_runtime.go`（Restore 调 AbandonAttempts 的落点已在 RE02 就位，本卡只补策略差异行为）
- 预计规模：小

## 1. 任务目标

崩溃恢复策略可配置（`scheduler.recovery_policy: auto|replay|pause`，默认 auto = 现状）；任何策略下，崩溃前 running 的尝试在账本上都有 `abandoned` 结局。

## 2. 背景与当前问题

设计文档 §3.5：现有守卫只有一种——`cmd/server` 的 `pauseRunningExecOnRestore` 硬编码"running 且 exec 类 → paused"，`executors.restore_policy=replay` 时不装守卫。策略按类别写死、不可整体配置；账本上崩溃瞬间的尝试永远悬空（RE02 之前连账本都没有）。

## 3. 要实现的功能

1. 配置 `scheduler.recovery_policy`（auto|replay|pause，默认 auto，Validate 拒绝未知值）：
   - `auto`：现状——default 池 replay（复位 pending）、exec 池 pause（守卫改判 paused）；
   - `replay`：所有类别一律复位 pending（等价 executors.restore_policy=replay 的全局版）；
   - `pause`：崩溃前 running 的一律停 paused（default 池也停——保守部署可用）。
2. `installRestoreGuard` 改读该配置：`replay` 时不装守卫；`pause` 时装"拦一切 running"的守卫；`auto` 时装既有 `pauseRunningExecOnRestore`。`RestoreGuard` 钩子机制本身不动（机制与配置分离，注释写明）。
3. 悬空收口：`Restore` 在改判**之前**对崩溃前 running 的快照调 `AbandonAttempts`（replay/pause/auto 都要调——"这次尝试没跑完"是事实，与改判结果无关）；`executors.restore_policy=replay` 分支同样不豁免。
4. 热更分级：ClassRestart（设计文档 §9）；`config_reload.go` 的类别表登记。

## 4. 实现步骤

1. 配置键 + Validate + example 同步。
2. `installRestoreGuard` 三分支。
3. Restore 收口挂接（若 RE02 已把 AbandonAttempts 放在 Restore 通用路径，本卡只验证三种策略下都触发）。

## 5. 测试要求

- 三种策略 × 两类别（default/exec）的恢复矩阵：快照 running 时重启后分别落 pending / paused / （auto 时按类别）。
- 悬空收口：三种策略下账本都出现 abandoned 行（依赖 RE02 测试基建）。
- `executors.restore_policy=replay` 与新键的组合语义有断言（新键优先于旧键的全局性，注释写清两者关系：旧的只管 exec 类，新的管全局）。
- 热重载类别测试登记。

## 6. 完成标准（DoD）

1. 恢复矩阵测试全绿；`auto` 与现状行为逐字节等价（既有恢复测试不松）。
2. abandoned 收口在三种策略下都发生。

## 7. 验收方式

配置 `recovery_policy: pause` → 起一个执行器任务执行中 kill -9 → 重启 → 任务停 paused、账本行 abandoned；改 `replay` 重做 → 任务被重跑、账本 = abandoned + 新 scheduled 行。

## 8. 不在本任务范围

`discard` 策略（恢复即判失败——没有需求驱动，不预做）、优雅关停路径（`handleInterrupted` 本就不经过守卫，语义不变）、多实例选主。

## 9. 风险与回滚

- **风险：新旧两个恢复配置键语义混淆**（`executors.restore_policy` vs `scheduler.recovery_policy`）。缓解：Validate 阶段同时出现且矛盾时记 warn（不拒启——旧键是既定契约）；两者关系写进 deployment.md（RE08）与本卡注释。
- 回滚：revert 后回硬编码现状；配置键残留无害。

## 10. 实现记录

（待实现后回填。）
