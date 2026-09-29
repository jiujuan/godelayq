# TASK-E12　错误分类与重试规则接线

- 所属阶段：M2 进程执行
- 依赖任务：TASK-E09、E10、E11
- 涉及文件：`core/scheduler.go`、`core/retry.go`（只读参考）、`core/scheduler_handlers_test.go`、新增测试；`executor/exit.go` 补充
- 预计规模：小（改动集中，但要精确）

## 1. 任务目标

让调度器区分"重试有意义"和"重试没意义"两类失败：参数写错、命令不存在、权限不足这类问题直接判失败；超时、连接失败这类问题按现有退避策略重试。

## 2. 背景与当前问题

`core/scheduler.go` 的 `handleFailure` 只看 `job.RetryCount < job.MaxRetries`，不区分错误性质。对示例处理函数这样没问题，对执行器就会造成明显浪费：一个 `runtime: nod`（拼错的解释器名）配 `max_retries: 5` 的任务会重试 5 次、每次都失败在同一点，事件时间线里留下 5 条同样无用的记录，还会占着执行池名额。

现在 `handleFailure` 的签名是 `handleFailure(job *Job)`，错误本身没有传进来；`executor.ExitError` 已经在 E09 提供了 `Permanent() bool`。

## 3. 要实现的功能

1. `core` 里定义接口（不依赖 `executor`，方向保持 `executor → core`）：

   ```go
   // PermanentError 表示这次失败重试也不会变好，直接落终态。
   type PermanentError interface {
       error
       Permanent() bool
   }
   ```

2. `executeJob` 的失败分支把 `err` 传给 `handleFailure(job *Job, err error)`（未导出方法，改签名只影响包内）。
3. `handleFailure` 的判定顺序（注释要写清每一步为什么在这个位置）：
   1. `parkForcedPause` 检查（保持不变，位置第一）。
   2. `errors.As(err, &pe)` 且 `pe.Permanent()` → 走"不再重试"分支，事件里带 `permanent:true`。
   3. 其余照旧按 `RetryCount < MaxRetries` 决定。
4. "不再重试"分支复用现有的 `StatusFailed` 落盘逻辑，额外在失败事件的 `Metadata` 加 `"permanent": true`（新增键，`api/history.go` 与前端只做透传，不解析）。
5. `executor.ExitError` 的 `Permanent` 取值规则集中成一个函数，避免各分支手写：

   | 情况 | Permanent |
   | --- | --- |
   | 提交内容非法（参数越界、payload 结构错） | 是 |
   | 档位不可用（探测失败、程序找不到） | 是 |
   | 权限不足（脚本不可执行、目录不可写） | 是 |
   | 被取消 / 优雅关闭打断 | 不适用（走 `handleInterrupted`，不进这里） |
   | 超时 | 否 |
   | 退出码在 `profile.retry_on_exit` 里 | 否 |
   | 并发许可等待超时 | 否 |
   | 其它非 0 退出码 | 是（默认不重试，由 `retry_on_exit` 显式放开） |

6. `core/retry.go` 的 `ExponentialBackoffRetry` 不改：它只负责"什么时候重试"，不负责"要不要重试"。在本卡注释里说明这条分工，避免下一个人在这里加判断。

## 4. 实现步骤

1. 在 `core` 加 `PermanentError` 接口与 `handleFailure` 签名改动。
2. 在 `executor` 把 `Permanent` 的判定收敛成一个函数（`classifyExit` 之类），E09 各分支改为调用它。
3. 加测试（第 5 节），先写 `core` 侧（不依赖真实进程），再补 `executor` 侧的分类表驱动用例。
4. 检查 `docs/api.md` 里 `job.failed` 事件的 `metadata` 描述（E19 统一补文档，本卡先在代码注释里登记待补）。

## 5. 测试要求

1. `core/scheduler_handlers_test.go` 或新文件：
   - `TestHandleFailure_PermanentSkipsRetry`：处理函数返回一个实现 `Permanent()` 的自定义错误 + `MaxRetries: 3` → 任务直接 `StatusFailed`、`RetryCount` 不变、堆里不产生重试副本、有且只有一条失败事件、失败事件 `metadata.permanent==true`、**没有** `job.retrying` 事件。
   - `TestHandleFailure_NonPermanentStillRetries`：同样配置但 `Permanent()==false` → 产生 `job.retrying`，且退避时间仍由 `retryPolicy` 决定（复用既有断言方式）。
   - `TestHandleFailure_WrappedPermanent`：`fmt.Errorf("wrap: %w", permErr)` 包一层仍被识别（守住"用 errors.As 而不是类型断言"）。
   - `TestHandleFailure_ForcedPauseTakesPrecedence`：处于强制暂停中止流程中且错误是 permanent → 仍然停在 `paused`（确认第 3.3 条的顺序没写反）。
   - `TestHandleFailure_NilErrOrPlainErr`：普通 `errors.New` 不触发永久分支（回归保护：不改现有语义）。
2. `executor` 侧：
   - `TestClassifyExit`：第 5 节的表原样写成用例，八行全覆盖。
   - `TestRetryOnExit_Configurable`：`retry_on_exit: [75]` 的档位退出码 75 → `Permanent==false`；同档位退出码 1 → `Permanent==true`。
3. 端到端：`TestRunner_PermanentFailureNoRetry`（在 `executor` 的集成测试里，用真的调度器 + 临时存储）：提交 `max_retries: 2` 的非法参数任务，断言只执行 1 次。

## 6. 完成标准（DoD）

- [ ] `core` 不 import `executor`（依赖方向不变，`go list -deps ./core` 里不出现 executor 包）。
- [ ] 未实现 `Permanent()` 的错误行为与改动前完全一致（有回归用例）。
- [ ] 强制暂停的优先级仍在永久判定之前（有用例）。
- [ ] 永久失败不再产生 `job.retrying` 事件，也不再排期重试副本（两条断言都要）。
- [ ] `ExitError.Permanent` 的取值集中在一个函数里，E09 各分支没有散落的手写布尔。
- [ ] `docs/api.md` 里 `metadata.permanent` 的缺失被登记（E19 的卡片里要能查到这条待办）。

## 7. 验收方式

```bash
go build ./... && go vet ./... && go test ./... -race
go test ./core -run 'HandleFailure' -v
go test ./executor -run 'ClassifyExit|RetryOnExit|PermanentFailure' -v
```

手工：配一个 `runtime: godelayq-no-such-runtime` 的档位（E03 的探测会让它不可用），提交 `max_retries: 3` 的任务，`GET /jobs/:id/events` 里只应看到 1 次执行与 1 条失败，没有重试事件。

## 8. 不在本任务范围

- 不改退避算法与抖动（`core/retry.go`、`scheduler.max_retry_delay` 保持现状）。
- 不改 `JobStatus`：永久失败仍是 `StatusFailed`，不新增状态。
- 不做"失败后自动禁用某档位"之类的熔断。
- 不做重试次数上限的强制收紧（配置里 `max_retries` 由用户决定）。

## 9. 风险与回滚

- 风险：默认"非 0 退出码即不重试"是一个策略选择，可能不符合某些人"所有失败都重试"的直觉。缓解：`retry_on_exit` 显式放开，并在 E19 的文档里写清这条默认值；测试表里把它当成规范记录。
- 风险：`handleFailure` 改签名会让包内其它调用点漏改——编译期即报错，风险可控，但要注意 `parkForcedPause` 分支的返回值语义不能改。
- 回滚：`core` 侧改动是一个未导出方法签名 + 一个接口定义，revert 本卡提交即可；`executor` 侧的分类函数留在包里不影响编译。
