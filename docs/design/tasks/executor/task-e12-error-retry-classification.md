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

## 10. 实现记录（2026-09-30）

落地文件：`core/retry.go`（`PermanentError` 接口、`isPermanentFailure`、与 `RetryPolicy` 的分工注释）、
`core/scheduler.go`（`handleFailure(job, err)`、`markFailed`、失败事件的 `permanent` 键）、
`executor/exit.go`（`failureClass` 七个类别、`classifyExit` 改成按类别判定、`newFailure`、`summaryPermanent`）、
`executor/proc.go`（七处错误构造点全部改走 `newFailure`，摘要的 `permanent` 与返回的错误同源）、
新增 `core/scheduler_permanent_test.go` 与 `executor/retry_e2e_test.go`，`executor/exit_test.go` 补分类表。

### 与卡片的偏离与补充

1. **`permanent` 键只在真值时写入事件 `metadata`**（§3.4 的口径）。不写 `"permanent": false`，
   这样既有失败事件的 JSON 形状一字不变；读侧（`api/history.go` 与前端）本来就透传这个 map。
2. **"不再重试"与"重试耗尽"共用一个 `markFailed`**。卡片 §3.4 说"复用现有的 `StatusFailed` 落盘逻辑"，
   做法是把那三行抽成方法而不是在新分支里重抄一遍：两处落盘逻辑一旦分开，
   后面改其中一处就会出现"同一个失败在两种路径下留痕不同"。
3. **§3.5 的表落成 `classifyExit(class failureClass, exitCode int, retryOnExit []int) bool`**。
   函数名沿用 E09 的 `classifyExit`，但签名从 `(p *Profile, exitCode int)` 改成按类别判定——
   卡片要求"取值规则集中成一个函数"，而 E09 那个版本只回答退出码那一项，
   超时与许可那两项散在 `classifyFailure` 和 `permitFailure` 里各写各的布尔。
   类别定义见 `failureClass` 的七条注释，与表格逐条对应。
4. **表里"档位不可用"与"权限不足"在代码里各自归入一个类别，但没有为操作系统报出的权限错误另设第三类**：
   `failureNotAllowed` 管执行器自己能判定的环境项（没有产物存储、产物文件建不出来），
   `failureProfileUnavailable` 管档位与进程启动项（探测失败、程序找不到、进程因权限起不来）。
   后者的权限原因是操作系统在 `Start` 阶段报的，执行器不再细分原因——两行的结论都是永久失败，
   多分一类只会让表格行数与代码类别数对不上。
5. **摘要里的 `permanent` 改为与返回给调度器的错误同源**（`summaryPermanent`），这是本卡顺带修掉的一处既有不一致：
   E09/E11 的实现只在 `execute` 里给摘要赋 `Permanent`，
   参数越界、许可等不到、产物建不出来这三条早退路径把摘要留在零值 `false`。
   接上重试判定之后，这条不一致会变成接口上看得见的自相矛盾——
   "摘要写着会重试、实际一次都不再跑"。取消与优雅关闭打断的摘要仍写 `false`：
   那种路径不算任务失败（调度器走 `handleInterrupted`），不该在摘要里留下"重试没有意义"的判断。
   因为产物目录里的 `a1.meta.json` 在返回之前就写好，`permanent` 在进入收尾时先落一次、
   defer 里按同一个函数再算一次，两处没有两套规则。
6. **`permitFailure` 的两个子情形文本改了**：等许可期间上下文是超时结束时，`Reason` 从 `"cancelled"`
   改成 `"timed out"`，与它自己设的 `TimedOut=true` 一致（旧写法两个子情形都写 `"cancelled"`）。
   没有测试依赖旧文本；`concurrency limit` 那条文本不变。
7. **§5.1 第 4 条（强制暂停优先）改成直接调 `handleFailure` 的白盒用例**。走"执行中 ForcePause"的真实链路时，
   取消上下文一定先被 `executeJob` 认成中断分支交给 `handleInterrupted`（实测事件里的 note 是 `interrupted`），
   `handleFailure` 的这一分支只在"暂停标记已就位、错误确实传到失败收尾"时才走得到。
   用例因此直接摆好标记再调函数，断言四件事：停在 `paused`、不消耗重试、堆里没有副本、标记被消费掉。
8. **§5.3 的端到端用例加了一条对照**：`TestRunner_TimeoutFailureStillRetries`。
   只验"永久失败不重试"的话，把判定写成"所有失败都不重试"也能通过，
   所以同一条接线上必须有一条可重试失败的正向用例（超时经真实调度器仍然排期重试）。
9. **既有三处 `handleFailure(job)` 调用点补上了错误参数**（`core/scheduler_test.go` 两处、
   `core/scheduler_recovery_test.go` 一处），传的是普通 `errors.New`，断言一字未改——
   这同时是 §6 第二条"未实现 `Permanent()` 的错误行为与改动前完全一致"的证据之一。
10. **依赖方向的证据**：`executor` 侧的编译期断言从匿名接口改成 `_ core.PermanentError = (*ExitError)(nil)`，
    接口签名一改就编译不过；`go list -deps ./core` 里仍然没有 `executor`（DoD 第一条）。

### 验证结果

单元测试（Windows 本机，Go 1.26.4）：

- `core` 新增五条（`core/scheduler_permanent_test.go`）全 PASS：
  `TestHandleFailure_PermanentSkipsRetry`（一次执行、`RetryCount` 为 0、堆里没有副本、
  一条 `job.failed` 带 `permanent=true`、零条 `job.retrying`）、
  `TestHandleFailure_NonPermanentStillRetries`（排期重试、`next_retry_at` 仍由 `retryPolicy` 给、
  失败事件里没有 `permanent` 键）、
  `TestHandleFailure_WrappedPermanent`（`fmt.Errorf("%w")` 包一层仍认得）、
  `TestHandleFailure_ForcedPauseTakesPrecedence`（见第 7 条）、
  `TestHandleFailure_NilErrOrPlainErr`（nil、普通错误、包过一层的普通错误、显式标成非永久四种都照旧重试；
  外加"重试耗尽仍落 failed"）。
- `executor` 分类表：`TestClassifyExit` 八行全覆盖（卡片 §3.5 的表原样搬进用例，
  另加"retry_on_exit 写了 0 也不算重试"和"档位没声明就永久失败"两条边界断言）、
  `TestNewFailureMarksFlagsByClass`（标记由类别出）、
  `TestRetryOnExit_Configurable`（真起进程拿到退出码 75/1，同一档位两种结论）、
  `TestRunner_PermanentFailureNoRetry`、`TestRunner_TimeoutFailureStillRetries`、
  `TestRunner_PermanentInArtifactMeta`（摘要与 `a1.meta.json` 的 `permanent` 一致）。
- 既有用例一字未改也仍然全绿：`TestExitError_*`、`TestClassifyFailure_*`、`TestRunner_*`、`TestMaxParallel`。
- 全仓 `go test ./... -race -count=1`（Windows）全绿：api 92.2s / cmd/server 5.5s / core 9.0s / executor 22.2s。
- `go vet ./...` 与 `GOOS=linux`、`GOOS=darwin` 的 `go vet ./...` 全过；`go build ./...` 三平台通过。

冒烟（Windows 真实服务端 + REST，一份独立配置在临时目录，静态 token 鉴权，`required_role: operator`）：

| 档位与提交 | 结论 | 事件时间线 |
| --- | --- | --- |
| `exec.perm_arg`，payload `{"args":{"day":"tomorrow"}}`（越界），`max_retries: 3` | 摘要 `permanent=true`、`retry_count: 0` | `scheduled` → `started` → `failed{retry_count:0, max_retries:3, timeout:false, permanent:true}`，**没有 `job.retrying`**，只执行一次 |
| `exec.exit_nine`（退出码 9 未声明），`max_retries: 2` | 摘要 `permanent=true`、`exit_code=9`、`retry_count: 0` | 三条事件同上，`failed` 带 `permanent:true`，无重试 |
| `exec.slow_ping`（档位 `timeout: 1s`，进程 30 秒），`max_retries: 1` | 摘要里没有 `permanent`、`retry_count: 1` | `failed{timeout:true}` → `retrying{next_retry_at: +40s}` → `scheduled`（第二轮同样超时） |
| `exec.exit_three` + `retry_on_exit: [3]`，`max_retries: 1` | 摘要 `permanent` 缺省、`exit_code=3` | 七条事件：两轮执行、一次 `failed{timeout:false}`（无 `permanent` 键）+ 一次 `retrying`，第二轮耗尽名额后落 failed |

- 第一轮冒烟还意外跑出了表里"档位不可用/环境不允许"那一行的真实进程版本：
  `exec-workspace` 目录当时还没建，`exec.exit_three` 报的是
  `cannot start the process: fork/exec C:\WINDOWS\System32\cmd.exe: The directory name is invalid.`，
  摘要 `permanent=true`、`duration_ms=0`、只执行一次、没有重试事件。
- 卡片 §7 给的"配一个 `runtime: godelayq-no-such-runtime` 的档位"这条手工路径走不通，
  冒烟改用上面的路径：探测不可用的档位根本不会被注册（E04 的注册逻辑），
  提交期就被拒掉——实测 `POST /jobs` 返回 `400 unknown job type`
  （`details: job type 'exec.no_such_profile' not registered`），压根不会产生执行事件，
  也就看不到"重试没被抑制"的现象。

### 未验证

- 文档：`docs/api.md` 的 `job.failed` 事件 `metadata` 说明仍缺 `permanent` 一项。
  E19 卡片 §3.1 已把它列入收口清单，`core/scheduler.go` 的注释里也标了归 E19（本卡按 §4 第 4 条不改文档）。
- 已存在的 `jobs.json` 里旧执行摘要没有 `permanent` 键，读回是零值 `false`。
  这只影响展示（重试判定读的是错误对象，不读摘要），没有做数据迁移。
- 真实"权限不足"起进程失败（非当前用户可写的目录、以服务账户运行时的目标进程）：
  冒烟里那条是"目录不存在"造成的同分支失败。
- `retry_on_exit` 与超时同时命中的组合（超时优先于退出码）只有 `TestClassifyFailure_InterruptedWinsOverExitCode`
  这一条单元证据，没做端到端构造。

### 留给后续卡片的接口形状

1. `core.PermanentError` 已经是公开接口：HTTP 执行器（E15）只要让它的错误带上 `Permanent()`，
   重试判定不需要再改 `core`。连接类失败按卡片 §3.5 的口径应当返回可重试。
2. E16 把参数校验提前到提交期（400）之后，`failureInvalidSubmission` 这条在运行期会少见，
   但分类不变：运行期仍然要挡住直接写存储或目录加载器送进来的任务。
3. 若以后要按档位声明"所有失败都重试"这类策略，入口是 `classifyExit` 一处；
   不要在 `core` 里加策略开关——`core` 只认接口，规则归执行器自己（§3.1 的依赖方向）。
