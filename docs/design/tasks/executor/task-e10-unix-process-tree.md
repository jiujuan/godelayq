# TASK-E10　取消与超时：终止整棵进程树（Unix）

- 所属阶段：M2 进程执行
- 依赖任务：TASK-E09
- 涉及文件：新增 `executor/proc_unix.go`、`executor/proc_unix_test.go`；改 `executor/proc.go`（接上 `Cancel` 钩子）
- 预计规模：中

## 1. 任务目标

在 Linux/macOS 上，取消任务或执行超时时，把派生的整棵进程树都结束掉，不留孤儿进程。

## 2. 背景与当前问题

`os/exec` 的默认行为是：上下文结束时只 `Kill` 直接子进程。脚本 `bash -c "sleep 600 &"`（或档位脚本里 fork 出去的后台进程）在父 `bash` 被杀之后仍然存活，继续占用 CPU、内存，甚至继续写产物文件。

同时 `core/scheduler.go` 的 `executeJob` 在取消时走 `handleInterrupted`，把任务状态落回 pending 等下次恢复，也就是说**调度器认为这次执行已经结束了**。如果孙进程还在跑，实际就出现"状态显示没在跑、机器上还有进程"。

## 3. 要实现的功能

1. `proc_unix.go`（`//go:build !windows`）提供：

   ```go
   func sysProcAttr() *syscall.SysProcAttr           // {Setpgid: true}
   func signalGroup(pid int, sig syscall.Signal) error // syscall.Kill(-pid, sig)
   func killTree(cmd *exec.Cmd, grace time.Duration) error
   func signalOf(state *os.ProcessState) string       // 从 syscall.WaitStatus 取 Signalled()/Signal()
   ```

2. `proc.go` 里给 `cmd` 设：
   - `cmd.SysProcAttr = sysProcAttr()`
   - `cmd.Cancel = func() error { return killTree(cmd, killGrace) }`
   - `cmd.WaitDelay = killGrace + 2s`：防止孙进程继承了管道写端导致 `Wait` 永久阻塞（`os/exec` 文档里明确这条：`Cancel` 之后仍未关闭的 I/O 由 `WaitDelay` 兜底）。**没有 `WaitDelay` 会出现"进程已被杀但 Handler 不返回"，比不杀更难排查。**
   - `killGrace` 取常量 3 秒，写注释说明取值理由（够 `bash` 自己收尾，又不至于拖慢关闭）。
3. `killTree` 的顺序：先 `SIGTERM` 到进程组，等 `killGrace`，仍存活则 `SIGKILL` 到进程组。判断"仍存活"的方式：`cmd.ProcessState == nil`（`Wait` 未返回）配合 `signalGroup(0)` 探测不可靠，改为"发 SIGTERM 后等 `WaitDelay` 之内 `cmd.Wait` 是否返回"，即把判断交给 `os/exec` 自己，避免重复实现一套状态机。实现方式要在注释里写清选择了哪一种。
4. 结果标记：取消或超时结束时，`ExitError.Cancelled`/`TimedOut` 必须为真，且 `Signal` 字段填上实际致死信号（若可取）。
5. 关停路径：`core/scheduler.go` 的 `Stop` 会取消在途任务上下文，于是 `killTree` 会在优雅关闭期间被调用。要求：`scheduler.shutdown_timeout`（默认 5s）与 `killGrace`(3s) + `WaitDelay`(5s) 的关系要在文档里说明清楚——最坏情况 Handler 需要 5 秒返回，而关闭只等 5 秒。为此把 `WaitDelay` 改为 `min(2*killGrace, 剩余关闭时间)`，或者直接把 `killGrace` 降到 2s 并把 `WaitDelay` 设为 4s，并把 `shutdown_timeout` 的推荐值在 E19 的部署文档里提到 10s。**二选一并记录结论。**

## 4. 实现步骤

1. 先写平台文件与函数签名，默认 `signalOf` 返回空串。
2. 改 `proc.go` 接 `SysProcAttr`/`Cancel`/`WaitDelay`。
3. 写测试（需要 `bash` 或 `/bin/sh`；取不到就 skip，但必须在 Linux 环境实际跑过一次并在 DoD 里注明）。
4. 处理 `WaitDelay` 与优雅关闭时长的关系，改完把结论写进注释和 `docs/deployment.md`（E19 收尾）。

## 5. 测试要求

1. `TestKillTree_GrandchildDies`：脚本 `sh -c 'sleep 300 & echo $! > pidfile; wait'`，档位跑起来后取消上下文 → 断言 pidfile 里的孙进程已不存在（`syscall.Kill(pid, 0)` 返回 `ESRCH`）。这是本卡的核心用例，不能省。
2. `TestKillTree_TimeoutPath`：`timeout: 1s` + 脚本 `sleep 30` → Handler 在约 1s+宽限内返回、`ExitError.TimedOut==true`、`errors.Is(err, context.DeadlineExceeded)==true`、孙进程消失。
3. `TestKillTree_NoOrphanAfterCancelStress`：并发起 20 个带后台子进程的短任务并全部取消，结束后统计测试进程组内残留的 `sleep` 进程数为 0（用 `ps` 或 `/proc` 遍历；平台不支持就 skip 并说明）。
4. `TestWaitDelay_PipeHeldByGrandchild`：孙进程持有 stdout 写端且父先退出 → `Wait` 仍在 `WaitDelay` 内返回（否则整个 Handler 挂死）。这条最容易忘，必须有。
5. `TestSignalOf`：`kill -9 $$` 式脚本（或 `sh -c 'kill -TERM $$'`）→ `Signal` 为 `SIGTERM`，`ExitCode` 不写成 0。
6. `TestGracefulShutdown_NotStalled`：一个 30 秒的脚本任务在跑时调用 `Scheduler.Stop`，断关停在 `shutdown_timeout` 内完成（用时长断言，容差放宽到 2 倍避免不稳定）。

## 6. 完成标准（DoD）

- [ ] 第 3.5 条的时长关系有明确结论、写进注释、并被第 5.6 条测试守住。
- [ ] `Setpgid` + 负 PID 信号生效，孙进程被清除，有专项用例。
- [ ] `WaitDelay` 已设置，管道被继承的场景不会挂死 Handler。
- [ ] 取消与超时两种路径的 `ExitError` 标记正确，`core` 的事件分类不需要改动就正确（跑 `core` 全量测试）。
- [ ] 只在 Unix 生效的代码带 `//go:build !windows`，Windows 构建不受影响（`GOOS=windows go build ./...` 通过）。

## 7. 验收方式

```bash
go build ./... && go vet ./... && go test ./executor -run 'KillTree|WaitDelay|SignalOf|GracefulShutdown' -v
GOOS=windows go build ./... && GOOS=darwin go build ./...
go test ./... -race
```

手工（Linux）：起一个 60 秒带后台子进程的档位，`POST /jobs/:id/cancel` 后用 `ps -ef | grep sleep` 确认没有残留。

## 8. 不在本任务范围

- 不做 Windows 终止（E11）。
- 不做 `SIGSTOP`/暂停单个任务进程（本设计里暂停任务=中止执行，不是冻结进程）。
- 不做进程数、CPU、内存配额（设计文档 §8）。

## 9. 风险与回滚

- 风险：`Setpgid` 之后如果将来引入"从 shell 包装执行"（有人为了支持管道而加 `sh -c`），进程组仍然是同一个，杀组依旧有效，但脚本内部的 `setsid` 会脱离进程组。这条要在注释里写明：档位脚本不得使用 `setsid`/`nohup` 脱离进程组。
- 风险：`kill(-pid)` 在 PID 复用窗口内可能误伤。宽限期 2-3 秒 + 只发给自己创建的进程组，风险可接受，但注释要如实写明而不是假装没有。
- 回滚：`proc.go` 里三行赋值去掉即回到 E09 状态；平台文件可整文件删除。
