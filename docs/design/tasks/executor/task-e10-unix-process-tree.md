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

---

## 10. 实现记录（2026-09-30）

改动文件：`executor/proc.go`（接上 `SysProcAttr`/`Cancel`/`WaitDelay` 三行与两个时长常量）、
`executor/proc_unix.go`（本卡的主体：`sysProcAttr`/`signalGroup`/`killTree`/`signalOf` 的真实实现）、
`executor/proc_windows.go`（补两个同名函数保证跨平台接线可编译，行为与 E09 相同）、
新增 `executor/proc_unix_test.go`（§5 的六条用例，带 `//go:build !windows`）；
顺带修 `executor/proc_test.go` 里两条依赖机器速度的既有断言，以及
`api/shutdown_test.go` 的连接复用问题（Linux 全量跑时暴露，非执行器范围，见第 9 条）。

### 与卡片的偏离与补充

1. **§3.5 的时长关系选了"killGrace 2s + WaitDelay 4s"这一支**，另一条
   （`WaitDelay = min(2*killGrace, 剩余关闭时间)`）走不通，理由要记下来：拿不到"剩余关闭时间"。
   `core.Scheduler.Stop` 取消在途任务之后是无期限等 worker 收敛的，
   `scheduler.shutdown_timeout` 目前只作用于 cmd/server 的 HTTP 优雅关闭
   （`cmd/server/main.go` 里 `deps.timeout` 只传给 `server.Stop(ctx)`）。
   因此 Handler 返回的上界就是 `killGrace + processWaitDelay ≈ 6s`；
   推荐把 `shutdown_timeout` 配到 10s 以上这件事登记给 E19 的部署文档，本卡把口径写进常量注释。
2. **E09 那个 5 秒的兜底常量在本卡改名并缩短**：`outputWaitDelay` → `processWaitDelay = 4s`。
   语义也从"进程已退出但管道没关"扩展成"取消之后等管道"，因为它现在与 `killGrace` 配对计量。
3. **`killTree` 不探测"进程是否还活着"**（§3.3 要求写清选了哪一种）。
   卡片提的两种判断方式都有问题：`cmd.ProcessState` 在 `Wait` 返回之前恒为 nil，
   而 `signalGroup(pid, 0)` 会把"已退出但还没被收尸的僵尸"读成活着，于是每条正常取消路径都要多绕一次判断还是判不准。
   本实现的做法是：SIGTERM 拿到 ESRCH 就说明进程组已经不存在，直接结束；
   否则固定等一个宽限期，再无条件补一次 SIGKILL（组已退出的话同样只拿到 ESRCH，忽略）。
   代价是取消路径最晚多花一个宽限期，好处是不在这里重复实现 os/exec 已有的状态跟踪。
4. **`Cancel` 只在"发信号本身失败且原因不是没这个进程组"时返回错误**。
   这条是标记正确性的前提：返回 nil 时 os/exec 会把这次执行归因到上下文结束，
   `ExitError.TimedOut`/`Cancelled` 才拿得到；返回错误时 os/exec 报的是
   `exec: canceling Cmd: ...`，虽然分类仍按上下文走，但错误文本会误导排障。
   另外 `cmd.Process == nil` 时直接返回 nil（os/exec 保证 Start 失败不会调用 Cancel，
   留这一层是让本函数可以脱离那条约定被直接调用）。
5. **信号名表用平台自己的常量做键**（不是照数字硬编）：Linux 与 macOS 的
   `SIGUSR1`/`SIGCHLD`/`SIGSTOP` 等编号不同，按数字写死的名字在另一平台上一定是错的。
   没收录的信号返回 `SIG#<编号>` 而不是空串——摘要里确实有一个信号，只是本表没名字。
6. **`proc_windows.go` 在本卡补最小实现**（卡片把 Windows 归 E11）。原因是 §3.2 要求的三行接线写在
   跨平台的 `proc.go` 里，Windows 侧必须有同名函数才能编译：`sysProcAttr()` 返回 nil、
   `killTree` 只杀直接子进程，行为与 E09 完全一致；E11 再换成 `CREATE_NEW_PROCESS_GROUP`
   与 `Kill` + `taskkill /T /F`。
7. **§5.3 与 §5.6 的残留进程统计用"差集"**：读 `/proc/<pid>/stat` 的 comm 字段筛出 `sleep`，
   与用例开始之前采集的集合比较。机器上本来就有的 sleep、以及同包内"管道被孙进程持有"那条用例
   留下的计时中的 sleep，都不该被算成本卡的残留；不取差集的话这条用例会在别的用例之后失败。
8. **`TestRunner_*` 里两条断言是本卡才发现的既有问题**（E09 §10 的"未验证 Unix"因此可以收口）：
   `TestRunner_SuccessPreview` 与 `TestRunner_ProgramMissing` 都断言了 `DurationMs` 非零，
   而 `sh -c 'echo hello'` 在 Linux 上不到 1 毫秒就结束，`Milliseconds()` 取整为 0。
   断言依赖了机器速度。改法是：那两条只断言摘要字段存在与取值，另立
   `TestRunner_DurationIsRecorded` 用一秒的命令验"计时跨度真的覆盖进程运行期间"。
9. **`api/shutdown_test.go` 的连接复用问题**（非执行器范围，Linux 全量跑暴露）：
   用例先 `http.Get` 再 `Stop`，然后用默认 `http.DefaultTransport` 再请求一次，
   这次请求复用了上一次的保活连接，服务端是否在监听根本没被读到——Linux 上表现为
   "期望错误但拿到 nil"，Windows 上因为连接被关而重试才碰巧通过。
   改成带 `DisableKeepAlives` 的独立 Transport 之后，断言与它自己的注释
   （"关停后端口不再接受新连接"）说的是同一件事。
10. **取消后的可观测性**：`POST /jobs/:id/cancel` 之后任务从 `GET /jobs/:id` 读不到（api 的取消语义），
    所以冒烟里那一记 404 是预期行为；执行摘要随任务一起消失，产物文件仍按 E06 的
    启动孤儿清理 + 过期清理口径处理。本卡不改这条，登记给后续（E14/E16 的可观测性一并看）。

### 验证结果

Linux 实跑（WSL2 Ubuntu，`Linux 6.6.87.2-microsoft-standard-WSL2 x86_64`，
go1.24.13 linux/amd64；代码副本在 `~/e10`，避开 `/mnt/d` 的文件系统与执行位语义）：

- §5 的六条全部通过，逐条时长（本卡的判定点就在这些数字上）：
  `TestKillTree_GrandchildDies` 2.02s（= 宽限期）、`TestKillTree_TimeoutPath` 3.00s（1s 超时 + 宽限期）、
  `TestKillTree_NoOrphanAfterCancelStress` 2.08s（20 条并发全部取消后无新增 sleep）、
  `TestWaitDelay_PipeHeldByGrandchild` 4.01s（等满 `processWaitDelay` 后返回，产物内容仍是 `started`）、
  `TestSignalOf` 0.01s（SIGTERM/SIGKILL 两条取到名字，`exit 0` 为空）、
  `TestGracefulShutdown_NotStalled` 2.23s（30 秒脚本在跑时 `Stop`，关停在秒级完成）。
- 孙进程判定的证据链：脚本把 `sleep` 的 PID 写进 workspace 里的文件，取消前 `kill -0` 可读（alive），
  取消后同一 PID 报 ESRCH（gone），`/proc` 遍历里也没有新增 `sleep`。
- executor 整包在 Linux：`ok godelayq/executor 21.9s`；全仓 Linux `go build ./...` +
  `go vet ./...` + `go test ./... -count=1` 全绿（api 5.9s / cmd/server 4.0s / core 5.8s）。
  `-race` 在 Linux 未跑：这个 WSL 镜像里没有 gcc，race 运行时需要它（见"未验证"）。
- 冒烟（Linux 上的真实服务端 + REST，鉴权用静态 token）：档位 `exec.slow_tree`
  是 `kind: script` + `runtime: sh` + 脚本 `sleep 300 & echo $! > grandchild.pid; wait`。
  提交并触发后取消：`POST /jobs/<id>/cancel` 返回 204（`who=machine role=machine`），
  执行器收尾日志 `executor run finished ... kind=script exit_code=-1 duration_ms=5018 truncated=false`，
  紧接着 core 记 `job execution cancelled`（走中断分支，不消耗重试）。
  脚本记录的孙进程 PID 在取消前是 alive、取消后 gone；`pgrep -f 'slow.sh|sleep 300'` 计数 0。
  `exit_code=-1` 是被信号结束的标志（Linux 上 `ProcessState.ExitCode()` 对信号结束返回 -1），
  信号名由 `Signal` 字段承载，本例的取消路径里摘要与冒烟日志都符合预期。

Windows 与跨平台回归：

- `go build ./...`、`go vet ./...`、`go test ./... -race -count=1` 全绿
  （api 46.8s / core 8.2s / executor 4.6s / cmd/server 5.4s）。
- `GOOS=linux go vet ./...`、`GOOS=darwin go vet ./...` 通过
  ——两条平台测试文件各自只在本平台编译，接线三行在三个平台上都成立。

### 未验证

- macOS 真机：`signalOf` 与进程组写法只做了 `GOOS=darwin` 的 build + vet，本机没有 macOS 可跑。
- Linux 上的 `-race`：WSL 镜像缺 gcc，race 运行时不可用；竞态结论只在 Windows 侧取得。
  本卡新增的并发路径只有 `killTree` 里的定时器与 os/exec 的取消协程交互，测试里都在同包顺序执行。
- EPERM 与 PID 复用窗口（§9 第二条风险）：没有构造用例，只按"非 ESRCH 的错误返回给 os/exec"处理。
- 档位脚本用 `setsid`/`nohup` 脱离进程组的情形：按 §9 的口径只在注释里写明覆盖不到，不做终止尝试。
- 20 条并发用例是在 WSL2 单用户环境跑的；容器化部署里若子进程属于别的用户，
  `kill(-pgid)` 会拿到 EPERM，那种环境的验证归部署检查。

### 留给后续卡片的接口形状

- E11：替换 `proc_windows.go` 的 `sysProcAttr`（`CREATE_NEW_PROCESS_GROUP`）与
  `killTree`（先 `Kill` 再 `taskkill /T /F`），`signalOf` 保持空串。
  `proc.go` 的三行接线不用再动。
- E12：`ExitError.Signal` 在 Unix 上已经有值，`Permanent()` 判定不受影响；
  信号结束的进程 `ExitCode` 是 -1（Linux），重试判定别把 -1 当成脚本自己的退出码。
- E13：`killGrace + processWaitDelay ≈ 6s` 是取消之后 Handler 仍占着 worker 的额外上界，
  分池与队列容量要把它计入执行池的占用时间。
- E19：部署文档需要两条——`scheduler.shutdown_timeout` 建议配到 10s 以上；
  并写明它只覆盖 HTTP 优雅关闭，`Scheduler.Stop` 等在途 worker 是无期限的，
  有界性来自本卡的宽限期与管道兜底。
