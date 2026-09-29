# TASK-E09　进程执行主体

- 所属阶段：M2 进程执行
- 依赖任务：TASK-E04、E05、E06、E08
- 涉及文件：新增 `executor/proc.go`、`executor/env.go`、`executor/exit.go`、`executor/proc_test.go`；替换 `executor/handler_stub.go`
- 预计规模：大

## 1. 任务目标

实现 `script` 与 `binary` 两种档位的实际执行：起进程、传干净的环境变量、把输出流式写进产物文件、等它退出、把结论写进 `job.Exec` 并返回错误与否。取消和超时由 E10/E11 提供平台相关实现，本卡定义接口并给出"只能杀直接子进程"的默认实现。

## 2. 背景与当前问题

`core/scheduler.go` 的 `executeJob` 会调用 `job.Handler(execCtx, job)`，`execCtx` 已经带上取消与超时（`Job.Timeout` 大于 0 时用 `context.WithTimeout`）。这个上下文目前对示例处理函数够用（它们只 `select` 通道），但对真实进程不够：不主动取消 `exec.Cmd` 的话，进程会一直跑到自然结束，超时只能被观测、不能中断执行——`core/job.go` 的 `Timeout` 注释里已经写明这条限制。

另外 `cmd/server/main.go` 的示例处理函数里出现过 `slog.Info(..., "payload", string(job.Payload))`，说明日志容易把参数原样打出来。执行器任务的 payload 里可能带口令类参数，日志规范必须在本卡定下来。

## 3. 要实现的功能

1. `executor/proc.go`：

   ```go
   type Runner struct {
       profile   *Profile
       artifacts *ArtifactStore
       cfg       core.ExecutorsConfig
       logger    *slog.Logger
   }
   func NewRunner(p *Profile, a *ArtifactStore, cfg core.ExecutorsConfig, l *slog.Logger) *Runner
   func (r *Runner) Handler() core.Handler      // 交给 E04 的注册链路，替换 StubHandler
   ```

   `Handler` 的流程：
   1. `ValidateSubmission` + `Render`（E08）。失败 → 返回 `&ExitError{Reason: "invalid submission", Permanent: true}`，不起进程。
   2. `EffectiveTimeout` 求超时；由于 `core` 已经按 `Job.Timeout` 建了上下文，本卡要保证提交时写入的 `Job.Timeout` 就是有效值（E16 在 API 层做，本卡内再检查一次：若 `ctx` 无 deadline，用 `cfg.DefaultTimeout` 自己套一层，绝不出现"无限等待"）。
   3. 取档位并发许可（`MaxParallel` 的信号量，见第 4 点）。
   4. `ArtifactStore.Open(job.ID, job.Attempts)` 得到写入句柄；`defer Close`。
   5. 起进程，等退出，填 `job.Exec`，返回错误。
2. `executor/env.go`：`func BuildEnv(cfg core.ExecutorsConfig, p *Profile, sub *Submission) []string`
   - 以 `cfg.EnvAllow` 为白名单，从 `os.Environ()` 里挑出允许的键；`GODELAYQ_` 前缀键即使在白名单里也强制排除（配置写错也不该把服务凭据传下去）。
   - 追加 `p.Env`（档位固定值），再追加 `sub.Env`（payload 注入，已在 E08 过白名单）。后者覆盖前者，覆盖关系要在注释里写明。
   - Windows 上必须保留 `SystemRoot`、`COMSPEC`、`PATH`、`PATHEXT`，否则子进程起不来：把这几个键加进一个平台无关的"最低必要集"常量，并在 `Validate`（E01）里说明把它们写进 `env_allow` 是正常配置而不是漏洞。
   - 返回的切片顺序稳定（同名键只留最后一个，实现时先合并再按 `k=v` 输出，键名字典序），便于测试断言。
3. `executor/exit.go`：

   ```go
   type ExitError struct {
       ExitCode  int
       Signal    string
       TimedOut  bool
       Cancelled bool
       Permanent bool
       Reason    string   // 起进程之前就失败时用（如 invalid submission）
   }
   func (e *ExitError) Error() string
   func (e *ExitError) Is(target error) bool   // TimedOut→DeadlineExceeded；Cancelled→context.Canceled
   func (e *ExitError) Permanent() bool
   ```

   `Is` 的实现是本卡能正确接入既有分类逻辑的关键：`core/scheduler.go` 的 `executeJob` 用 `errors.Is(err, context.Canceled)` 判断"被打断"、用 `errors.Is(err, context.DeadlineExceeded) || errors.Is(execCtx.Err(), context.DeadlineExceeded)` 判断超时，只要 `Is` 正确，事件与重试分类不需要改就成立。
4. 输出接法：`cmd.Stdout = artifactWriter.Stdout()`、`cmd.Stderr = artifactWriter.Stderr()`（`io.Writer` 形式会让 `os/exec` 用管道 + 拷贝协程）。要求：
   - `Handler` 返回前必须等拷贝协程结束，否则产物文件可能不完整。做法是自建 `cmd.StdoutPipe()` 之外的显式 `io.Copy` 协程 + `WaitGroup`（用管道 + `copy` 到自己管理的 writer），**不要**直接把 `io.Writer` 交给 `Cmd` 后就 `Wait`。测试要覆盖"子进程输出很多（>64KB 管道缓冲）且立刻退出"的场景，断言文件内容完整。
   - `Close()` 后拿到的 `ArtifactInfo` 用来填 `ExecMeta.OutBytes/ErrBytes/Truncated`。
5. 结论填法：
   - `Kind`、`Profile`、`DurationMs` 必填。
   - `ExitCode`/`Signal` 来自 `cmd.ProcessState`（Windows 上 `Signal` 留空，注释说明）。
   - `Preview` 取 stderr 尾部，stderr 为空时取 stdout 尾部，长度 `cfg.Output.InlinePreview`。理由：失败时最有用的信息几乎都在 stderr。
   - `Artifact` 置 `available`（文件已写成功时）。
   - `Permanent` 规则：退出码在 `profile.RetryOnExit` 里 → `false`；否则非 0 → `true`（默认不重试，具体接线见 E12）。
6. 日志规范（写进 `Handler` 并在 `docs/deployment.md` 的 E19 里体现）：
   - 只记 `job_id`、`handler_key`、`profile`、`exit_code`、`duration_ms`、`truncated`。
   - **不记 `job.Payload`、不记 argv、不记 env**。这是与示例处理函数（会打 payload）不同的地方，注释要写明为什么。
7. 删除 `executor/handler_stub.go`，把 `Register`（E04）改成用 `Runner.Handler()`。E04 的 DoD 里已经登记了这条去向。
8. `context.Canceled` 与 `DeadlineExceeded` 的收尾：进程退出后要把 `ExitError.Cancelled/TimedOut` 设对，并且**先等产物写完再返回**，否则调度器落盘的摘要与实际文件状态可能不一致。

## 4. 档位并发许可

`Runner` 内持有一个容量为 `profile.MaxParallel` 的信号量（`semaphore.Weighted`）。取不到许可时：
- 等待上限 = 有效超时（不无限等）。
- 等待期间响应 `ctx` 取消。
- 等到超时仍无许可 → 返回 `&ExitError{Reason:"concurrency limit", Permanent:false, TimedOut:true}`，让调用方按超时处理。

注意：这只限制"同一档位同时跑几个"，不解决"执行器任务占满普通 worker"的隔离问题（那是 E13）。在本卡注释里明确写出这一点，避免误以为已经隔离。

## 5. 测试要求

用真实的极小程序，不依赖 node/php：Unix 用 `/bin/sh` 或 `bash`（`exec.LookPath` 取不到就 `t.Skip` 并在测试名里注明），Windows 用 `cmd.exe`。构造档位时把 `runtime` 设成 `os.Executable()` 之外的固定值，用测试辅助函数生成，避免每个用例重复。

1. `TestRunner_ExitCode`：脚本 `exit 3` → `Exec.ExitCode==3`、`Err != nil`、`Permanent==true`（未在 `retry_on_exit` 里声明）。
2. `TestRunner_SuccessPreview`：`echo hello` → 无错误、`OutBytes>0`、`Preview=="hello"`、`Artifact=="available"`、`DurationMs>0`。
3. `TestRunner_LargeOutputComplete`：子进程输出 2MB（`head -c 2000000 /dev/urandom | base64` 或等价的跨平台写法：循环打印固定行）→ 产物文件字节数与预期一致（考虑 `MaxBytes` 截断时 `Truncated==true`），证明拷贝协程被等住。
4. `TestPreview_PrefersStderr`：同时写 stdout 与 stderr 且都失败 → `Preview` 来自 stderr；只写 stdout 且失败 → 来自 stdout。
5. `TestRunner_BadSubmissionNoProcess`：payload 越界 → `Permanent==true`、`Reason=="invalid submission"`，且断言没有产生任何产物文件（证明没起进程）。
6. `TestRunner_ProgramMissing`：程序不存在 → 返回错误，`Exec` 仍然填了 `Kind`/`Profile`/`DurationMs`，`Permanent==true`，`ExitCode` 不写 0 之外的假值（注释里说明"没跑起来的进程没有退出码"）。
7. `TestBuildEnv`：
   - `GODELAYQ_SERVER_AUTH_TOKEN` 在 `os.Environ()` 里且不在白名单 → 不出现（用 `t.Setenv` 注入）。
   - 白名单里的键透传；档位 `env` 覆盖进程同名值；payload `env` 覆盖档位值。
   - 输出顺序按键名字典序且无重复键（`os/exec` 对重复键的行为依赖平台，必须在构造时就保证唯一）。
8. `TestExitError_Is`：`errors.Is(err, context.DeadlineExceeded)` 与 `errors.Is(err, context.Canceled)` 在四种组合下的结果；`Permanent()` 走接口断言 `var _ interface{ Permanent() bool } = &ExitError{}`。
9. `TestMaxParallel`：同一档位 `max_parallel: 1`，并发起两个长任务 → 一个在跑、一个在等；等待方在超时后拿到 `Reason=="concurrency limit"`。
10. `TestRunner_NoSecretInLogs`：用 `slog` 的 `CaptureHandler` 跑一次带 `secret` 参数的执行，断言日志文本里不含该参数值、不含完整 argv。
11. `TestHandler_Registers`：确认 E04 的注册链路已从桩切换（跑 `cmd/server` 的既有集成测试，`exec.*` 触发后错误信息不再是 `not implemented yet`）。

## 6. 完成标准（DoD）

- [ ] 端到端可跑：配一个 `echo` 档位，用 REST 提交、触发、在 `/result` 读到输出、在事件里读到退出码（E07 的端点此时才第一次被真实数据验证）。
- [ ] 产物完整性有专项测试（第 5.3 条），不是因为"小输出碰巧没丢"。
- [ ] 环境变量白名单生效，服务凭据不会传给子进程（第 5.7 条）。
- [ ] `ExitError.Is` 让调度器的超时/取消分类无需修改即正确（第 5.8 条 + 跑 `core` 全部既有测试）。
- [ ] `handler_stub.go` 已删除，`Register` 走真实 Runner。
- [ ] 日志不含 payload、argv、env，有测试证明。
- [ ] 未启用执行器时本卡对现有行为零影响（默认 `enabled: false`，`Runner` 不被构造）。

## 7. 验收方式

```bash
go build ./... && go vet ./... && go test ./... -race
go test ./executor -run 'Runner|BuildEnv|ExitError|MaxParallel|Preview' -v
```

手工（Unix）：

```bash
# 档位：name: hello, kind: script, runtime: bash, script: hello.sh（内容 echo hi; echo err >&2; exit 0）
GODELAYQ_EXECUTORS_ENABLED=true ./godelayq-server -config=configs/config.yaml &
curl -s -X POST localhost:8080/api/v1/jobs -H 'Content-Type: application/json' \
  -d '{"name":"exec.hello","delay":"2s","payload":{"args":{}}}'
sleep 4
curl -s "localhost:8080/api/v1/jobs/<id>" | grep -o '"exec":{[^}]*}'
curl -s "localhost:8080/api/v1/jobs/<id>/result" | grep -o '"content":"[^"]*"'
```

预期：`exec` 摘要里 `exit_code` 缺省（0 被 `omitempty` 省略）、`kind=="script"`、`preview` 是 `err`（stderr 优先）；`/result` 能读到 `hi`。

## 8. 不在本任务范围

- 不实现进程树终止（E10/E11）；本卡的默认取消实现只 `Kill` 直接子进程，并在代码注释里写明"完整终止见 E10/E11"。
- 不做 HTTP 档位（E15）。
- 不改 `core/scheduler.go` 的重试判定（E12）。
- 不做执行池隔离（E13）。
- 不做 CPU/内存/文件数配额（设计文档 §8 明确不做）。

## 9. 风险与回滚

- 风险：`Cmd` 的 `io.Writer` 与自建拷贝协程二选一，选错会出现"文件缺尾部"的偶发问题（第 5.3 条就是为此而写）。实现时先写这条测试再写代码。
- 风险：`Preview` 取 stderr 优先，会让"只有 stdout 的失败"看起来什么都没有。已经在第 3.5 条要求回落到 stdout，测试覆盖两种。
- 风险：信号量在 `Handler` 内等待会占住 worker 名额。已限定"等待上限等于有效超时"，但仍然是占用；这条要在 E13 的卡片里作为前置事实引用。
- 风险：Windows 与 Unix 的 `ProcessState.Sys` 类型不同（`syscall.WaitStatus` vs 不存在），信号解析必须放在平台文件里（E10/E11），本卡的 `proc.go` 只调用 `signalOf(cmd.ProcessState)` 这个平台函数，默认实现返回空字符串。
- 回滚：Runner 是新增，`Register` 改回 `StubHandler` 即可退回 E04 的状态，风险集中在 `main.go` 的一行改动。

---

## 10. 实现记录（2026-09-30）

改动文件：新增 `executor/proc.go`、`executor/env.go`、`executor/exit.go`、
`executor/proc_unix.go`、`executor/proc_windows.go` 与 `executor/proc_test.go`、
`executor/env_test.go`、`executor/exit_test.go`；删除 `executor/handler_stub.go`；
改 `executor/register.go`（注册真实执行器）、`executor/args.go`（生效超时的参数拆分、
`Submission.TimeoutValue`）、`executor/result.go`（预览改为 stderr 优先并抽出选择规则）、
`core/config.go` 与 `configs/config.example.yaml`/`configs/config.yaml`（`env_allow` 注释）、
`cmd/server/main.go` 与 `cmd/server/*_test.go`（装配与新增的装配检查）。
core 的调度器一行未改：超时与取消的分类靠 `ExitError.Is` 接入（§6.3 的口径）。

### 与卡片的偏离与补充

1. **输出接法用的是 `cmd.Stdout`/`cmd.Stderr` 直接挂写入器，没有自建拷贝协程**（§3.4 要求自建）。
   核实标准库后确认这条要求已由 `os/exec` 满足：`Cmd.Start` 为每个非 `*os.File` 的 writer
   建管道并起拷贝协程，`Cmd.Wait` 里的 `awaitGoroutines`（`src/os/exec/exec.go`）会等这些协程结束，
   `WaitDelay` 非 0 时才会强行关管道并返回 `ErrWaitDelay`。
   反过来，手写 `StdoutPipe` + `WaitGroup` 要与 `Wait` 关闭父端句柄竞争，是更差的那条路。
   §5.3 的完整性用例照写并通过：子进程输出 2,040,000 字节（远超管道缓冲），
   产物文件字节数与摘要 `out_bytes` 一致，一个字节都不差。
2. **`ExitError` 的永久失败标记只有一个方法，字段改名 `Retryable`**（§3.3 同时列了
   字段 `Permanent bool` 与方法 `Permanent() bool`，Go 里同名不能共存）。
   字段取反向语义：结构体零值就是"不重试"。若字段叫 `Permanent`，漏写它的构造点会默认允许重试，
   一个分类不明的失败会被反复投给同一个脚本。E12 要读的接口 `Permanent() bool` 不变，
   `executor/exit.go` 里有一条 `var _ interface{ Permanent() bool } = (*ExitError)(nil)` 的编译期断言。
3. **`ExitError` 多两个字段**：`Profile`（档位名，让每条错误自带归属）与 `Detail`（底层错误，
   `Unwrap` 暴露，所以 E08 的哨兵错误 `ErrWrongKind` 仍能从执行层的错误里读出来）。
   `Reason` 只放类别文本，测试与事件都按它分类。
4. **新增 `classifyFailure`**（把分类集中到一处）：按"上下文结束 → `ErrWaitDelay` →
   退出码 → 起进程失败"的顺序归类，`Runner` 各分支不再手写布尔。
   E12 只需读 `Permanent()`；§5.1/§5.6 的期望值由这一个函数决定，`executor/exit_test.go` 逐分支覆盖。
5. **`cmd.WaitDelay` 提前在本卡设置**（卡片把它列在 E10 §3.2）。原因在 Windows 上实测暴露：
   子进程退出后输出管道仍可能被派生进程持有，`Wait` 会一直等拷贝结束，Handler 不返回、
   worker 名额也不释放。常量 `outputWaitDelay = 5s`；E10 §3.5 那条
   "`killGrace`/`WaitDelay`/`shutdown_timeout` 的关系二选一"仍归 E10 定稿，届时改这个常量即可。
6. **`ErrWaitDelay` 单独一类，且判定为永久失败**：进程可能已经正常退出（退出码已填），
   问题是输出没收全；重跑会把脚本的副作用再做一遍，比缺一截输出更糟。
7. **默认取消只 `Kill` 直接子进程**（§8 的范围）。平台函数落在两个文件里：
   `proc_windows.go`（`//go:build windows`）与 `proc_unix.go`（`//go:build !windows`）
   各提供 `signalOf(*os.ProcessState) string` 的默认实现（返回空串）。
   E10 改 `proc_unix.go` 里的 `signalOf` 并加 `sysProcAttr`/`killTree`，
   E11 扩 `proc_windows.go` 加 `sysProcAttr`/`killTree`——两个文件本卡就建好，后续只往里加函数，
   不会出现同名函数被两个平台文件同时定义的情况。
8. **档位并发许可用带缓冲通道，不是 `semaphore.Weighted`**（§3.2/§4 写的是后者）。
   `golang.org/x/sync` 目前是间接依赖，为一个"占一个名额/退一个名额"的语义把它转成直接依赖不值得；
   等待上限、取消响应、等满按超时处理这三条行为都照卡片实现（§4 的三条要点都有用例）。
9. **`argv[0]` 换成探测得到的绝对路径**（E08 §10 第 8 条留给本卡）。
   `NewRunner` 内部调一次 `Probe` 取路径：与登记表启动那次重复几次文件系统查询，
   换来的是"拿到一个 Profile 就能构造可执行的 Runner"，不依赖调用方先探过。
   探测不可用时保留档位里的写法，让操作系统报"找不到文件"——错误更接近真实原因。
10. **预览从产物文件反向读，内存里不留输出副本**：`ArtifactStore.Tail`（`executor/artifact.go:430`）
    读 `inline_preview` 字节。`Result` 在执行路径上只承担摘要装配（`NewResult`/`Meta`），
    `Stdout`/`Stderr` 两个字段留给后续（http 档位）使用。
11. **预览的选择规则从 stdout 优先改成 stderr 优先**（§3.5），规则抽成 `ChoosePreview`，
    `Result.SetPreview` 与执行侧共用一条。E06 的 `result_test.go` 里"stdout 非空时不看 stderr"
    那条用例按新规则改写，并加了一条直接测 `ChoosePreview` 的用例。
    冒烟里两条流都有内容的失败任务，预览确实取自 stderr。
12. **`Submission` 多一个 `TimeoutValue` 字段**：`ValidateSubmission` 解析文本时就把它留下，
    执行侧不再解析一次文本。`checkTimeoutWithinProfile` 相应返回解析结果（内部函数，调用方只有一处）。
13. **`Profile.EffectiveTimeout` 的实现拆成 `timeoutWithin`**：Runner 手上只有
    `core.ExecutorsConfig`（装配时整份配置已归一化），旧签名要 `core.Config`。
    拆参数不复制规则，两条路径的合成结果一致；E08 的既有测试仍走 `EffectiveTimeout`。
14. **`Register` 签名扩成 `(registrar, reg, cfg, artifacts, logger)`**：真实执行器需要
    executors 一节求生效超时、需要产物存储写输出。`cmd/server/main.go` 的 `registerHandlers` 同步。
15. **新增一条装配检查**（卡片未列）：`cfg.Executors.Enabled` 为真但没传产物存储时，
    `registerHandlers` 直接返回错误。判断依据用运行配置而不是登记表开关——
    决定"这次启动要不要存储"的是前者；登记表在测试替身里可以与运行配置不一致。
16. **http 档位仍然注册 `Runner`，但执行分支明确回指 TASK-E15**：
    `Reason: "http profiles are executed by the http executor, which lands in TASK-E15"`，永久失败。
    这样注册链路只有一条构造路径，也不会留下一个占位文件等人来删（§3.7 要求删除 `handler_stub.go`，已删）。
17. **`BuildEnv` 永不返回 nil**（卡片未列，但这是白名单能否成立的前提）：
    `os/exec` 把 `nil` 解释成"继承当前进程的全部环境变量"，返回 nil 等于把服务凭据整包交给子进程。
    另外键名比较在 Windows 折成小写（`os.Environ()` 里是 `SystemRoot` 这类混合写法，
    逐字比较会让整条白名单失效），Unix 保持逐字（那里 `PATH` 与 `Path` 是两个变量）。
18. **最低必要集常量 `minimumEnvKeys`**（§3.2 要求）：`PATH`/`SystemRoot`/`COMSPEC`/`PATHEXT`
    无条件透传。按 §3.2 的后半句，在 `core/config.go` 的 `env_allow` 校验处补了注释，
    两份配置示例的对应说明也从"建议补 SystemRoot…"改成"由执行器无条件透传，再写一遍是正常配置"。
19. **覆盖顺序与 §3.2 有一处差别，差别来自 E08**：卡片写"payload 覆盖档位值"，
    实际 `checkSubmissionEnv`（`executor/args.go:332`）已经禁止 payload 覆盖档位固定的键。
    代码仍按 进程 → 档位 → payload 的顺序写入（与卡片一致），只是第三层只会新增键，不会改写第二层。
20. **`meta.json` 的内容结构在本卡定下来**：写的是 `core.ExecMeta` 本身
    （`WriteMeta(result.Meta)`，`executor/artifact.go:274`）。E06/E07 都没定这个结构，
    现在产物目录里那份文件与快照里的摘要是同一套字段，排障时可以直接对读。
21. **"没起进程"的路径不写 `DurationMs`**（§3.5 说三个字段必填）：`invalid submission`、
    `cannot build the command line`、`concurrency limit` 都是校验或排队失败，没有等待过程；
    写一个耗时数字反而会被读成"进程跑过这么久"。这三类都不建产物目录，
    `Artifact` 留空，接口侧仍是 E07 的 404（冒烟已验证）。
    真正起过进程但失败（程序缺失）的路径 `duration_ms` 有值、`Artifact=available`。

### 验证结果

单元测试（`go test ./executor -count=1 -v`，全部通过，无 skip）：

- §5 的十一条：`TestRunner_ExitCode`、`TestRunner_SuccessPreview`、
  `TestRunner_LargeOutputComplete`（2,040,000 字节完整落盘）、`TestPreview_PrefersStderr`
  （两条都有则取 stderr；只有 stdout 则回落）、`TestRunner_BadSubmissionNeverStartsProcess`
  （断言产物根目录为空）、`TestRunner_ProgramMissing`（`ExitCode` 保持 0）、
  `TestBuildEnv_*`（凭据前缀即使在白名单里也排除、三层合并、白名单外丢弃、
  Windows 启动变量透传、键名有序且唯一、空档位返回非 nil）、`TestExitError_Is`
  （四种组合）、`TestMaxParallel`、`TestRunner_NoSecretInLogs`、
  `TestRegister_HandlerRunsTheProfile`（注册链路已是真实执行器）。
- 补充分支：`TestRunner_RetryOnExitKeepsRetry`、`TestRunner_OutputCappedAndTruncated`、
  `TestRunner_Timeout`、`TestRunner_Cancelled`、`TestRunner_HttpProfileIsNotExecutedByProcessRunner`、
  `TestRunner_WithoutArtifactStore`、`TestClassifyFailure_*`（五个分支各一条）、
  `TestClassifyExit`、`TestChoosePreview`、`TestRegisterHandlers_MissingArtifactStoreFails`（cmd/server）。
- 全仓：`go build ./...`、`go vet ./...`、`go test ./... -race -count=1` 全绿
  （api 42.8s / core 8.0s / executor 3.3s / cmd/server 5.3s）。
- 跨平台：`GOOS=linux GOARCH=amd64`、`GOOS=darwin GOARCH=arm64`、`GOOS=windows` 的
  build + vet 均通过（平台文件按 `!windows` / `windows` 各自成立），`-tags dashboard` 构建通过。
- 冒烟（真实进程 + REST，Windows 上一份独立配置，鉴权用静态 token）：
  - `exec.hello`（`kind: binary`、`program: cmd`、`fixed_args: [/c, echo hi]`、
    必填参数 `day`、`env: REPORT_HOME`、`env_allow: [TRACE_ID]`）提交
    `{"args":{"day":"today"}}` → 任务 success，摘要
    `out_bytes=16`、`preview="hi --day=today\r\n"`、`artifact=available`、`duration_ms=24`；
    `GET /jobs/:id/result` 读到同一行内容，`size_bytes=16`。参数值作为**一个 argv 元素**追加，
    命令行里没有 shell 拼接的痕迹。
  - `exec.failing`（`echo boom 1>&2& exit /b 3`）→ 任务 failed，摘要 `exit_code=3`、
    `err_bytes=7`、`permanent=true`、`preview="boom \r\n"`（取自 stderr）；
    `job.failed` 事件的 `data` 同时带 `error` 文本 `profile "failing": exit status 3`
    与 `result` 摘要——E07 的读侧第一次由真实退出码验证。
  - 越界提交（`{"args":{"nope":"x"}}`）→ 任务 failed，摘要只有 `kind`/`profile`（无字节数、无产物标记），
    产物根目录里**没有**对应任务目录，`/result` 返回 `found=false`。证明校验失败时确实没起进程。
  - `exec.envprobe`（`fixed_args: [/c, set]`）在服务进程环境里确有
    `GODELAYQ_SERVER_AUTH_TOKEN` 与 `GODELAYQ_SERVER_AUTH_JWT_SECRET` 的情况下执行，
    payload 注入 `PROBE_VAR`：子进程 dump 出的环境只有 10 行，含 `PATH`、`SYSTEMROOT`、
    `COMSPEC`、`PATHEXT`、档位固定的 `REPORT_HOME`、payload 的 `PROBE_VAR=from-payload`；
    全文不含 `GODELAYQ_`，也不含那两个凭据值。服务端 token 同时是 HTTP 鉴权凭据，
    它出现在请求头里、出现在服务进程环境里，但没有出现在子进程里。
  - 产物目录：每个任务 `<job_id>/a1.out`、`a1.err`、`a1.meta.json`；
    `meta.json` 内容与快照里的 `exec` 摘要一致（第 20 条）。
  - 执行器日志只有 `job_id`/`handler_key`/`profile`/`kind`/`exit_code`/`duration_ms`/`truncated`
    七个字段，没有 payload、没有 argv、没有 env（§3.6）。

### 未验证

- Unix 上的真实进程行为：本机是 Windows，`sh -c` 分支与 SIGKILL/SIGTERM 的信号名解析
  只写了默认实现，`signalOf` 在 `!windows` 上仍返回空串。需要 Linux/CI 实跑（既有登记项）。
- 整棵进程树的终止：本卡只杀直接子进程。超时用例在 Windows 上是靠 `WaitDelay` 才让
  Handler 返回的（`cmd /c ping` 的孙进程仍活着），这条正是 E10/E11 要消掉的现象。
- 取消路径经 REST 的实际效果（`handleInterrupted` 不消耗重试）只有单测覆盖；
  冒烟没提交长任务再 `POST /jobs/:id/cancel`。
- 优雅关闭期间在途执行的表现（E10 §3.5 的时长关系）。
- `max_parallel` 等待期间占住 worker 名额的实际后果：本卡只验证了等待上限与失败归类。
- 提交期 400 与角色门禁：api 仍未调用 `ValidateSubmission`（E16），越界 payload 现在是
  "任务失败"而不是"提交被拒"。

### 留给后续卡片的接口形状

- E10：改 `proc_unix.go` 的 `signalOf`（从 `syscall.WaitStatus` 取信号名）并加
  `sysProcAttr`/`signalGroup`/`killTree`；`proc.go` 里需要接的三行是
  `cmd.SysProcAttr`、`cmd.Cancel`、`cmd.WaitDelay`（本卡已给后者的常量 `outputWaitDelay`）。
- E11：扩 `proc_windows.go`，`signalOf` 保持空串即可（Windows 无信号概念）。
- E12：`errors.As(err, &failure)` 后读 `failure.Permanent()`；退出码规则已收在
  `classifyExit`，超时/取消/`ErrWaitDelay` 三条分支的标记由 `classifyFailure` 统一给。
- E13：本卡的许可等待上限等于生效超时，等待期间仍占 worker（§4 已写明）。
- E14：产物按 `Open(job.ID, job.Attempts)` 分 attempt 建文件；崩溃恢复置 paused 时
  `Artifact` 已是 `available`（E06 §10 的约定）。
- E15：http 档位要替换的是 `Runner.Handler` 里那条 `KindHTTP` 分支的返回文本；
  注册链路不必再改。摘要里的 `HTTPStatus` 字段本卡没碰。
- E16：api 层调 `ValidateSubmission` 做 400 时，错误文本可直接复用——
  `invalid submission` 这条 `Reason` 与 `Detail` 里的 E08 文本已经带"哪个键、为什么、允许什么"。
