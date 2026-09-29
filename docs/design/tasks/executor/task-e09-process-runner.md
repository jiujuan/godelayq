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
