# TASK-E11　取消与超时：终止整棵进程树（Windows）

- 所属阶段：M2 进程执行
- 依赖任务：TASK-E09、E10
- 涉及文件：新增 `executor/proc_windows.go`、`executor/proc_windows_test.go`
- 预计规模：中

## 1. 任务目标

让 Windows 上的取消和超时也能结束派生的所有进程，并明确记录这条路径的可靠性限制。

## 2. 背景与当前问题

开发机就是 Windows（本仓库的测试常在 Windows 上跑），执行器必须在这上面可用。Windows 上没有 `SIGKILL`，也没有进程组语义，`cmd.Process.Kill()` 只结束直接子进程；`cmd.exe` 包装出去的子进程会变成孤儿。

E10 在 Unix 用的是进程组。Windows 上对应的正确机制是 Job Object（把进程放进作业对象，关闭句柄时作业内所有进程一起结束），但引入它需要 `golang.org/x/sys/windows` 或手写 `NewLazyDLL` 调用。本卡先采用不新增依赖的做法，并把限制写清楚。

## 3. 要实现的功能

1. `proc_windows.go`（`//go:build windows`）提供与 E10 同名的一组函数：
   - `sysProcAttr() *syscall.SysProcAttr`：设 `CreationFlags: CREATE_NEW_PROCESS_GROUP`，避免控制台 `Ctrl+C` 同时打到服务进程。
   - `killTree(cmd *exec.Cmd, grace time.Duration) error`：先 `cmd.Process.Kill()`（保证直接子进程一定死），再用 `taskkill /PID <pid> /T /F` 清理派生树。
     两步顺序要写明理由：`Kill` 是即时且必然成功的；`taskkill` 处理树但可能因权限或进程已退出而失败，失败时降级为 warn 日志。
   - `signalOf(*os.ProcessState) string`：Windows 上恒返回空串（没有信号概念），注释写明。
2. `taskkill` 的调用要求：
   - 用 `exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F")`，**参数分开传**，绝不拼字符串（与 E08 的"不经 shell"同一原则）。
   - 输出不写进产物文件（否则执行器自己的输出会污染任务输出）。用 `CombinedOutput()` 的结果只进 debug 日志。
   - 超时保护：给 `taskkill` 自身套 3 秒上下文，卡住时不阻塞 Handler 返回。
   - `taskkill` 不存在（极精简的 Windows 容器镜像）时降级为"只杀直接子进程"，记一条 warn，并且测试要跳过。
3. 已知限制的记录方式：在 `proc_windows.go` 顶部注释与本卡第 9 节同时写明：`taskkill /T` 是先枚举父子关系再逐个结束，不是原子操作，在枚举之后新派生的进程可能未被终止；彻底解法是 Job Object + `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`，登记为 E11a（后续卡片）。
4. 跨平台测试辅助：把 E10 里"造一个会派生子进程的脚本"的测试夹具抽成平台函数（Unix 用 `sh -c 'sleep 300 &'`，Windows 用 `cmd /c start /b cmd /c "timeout /t 300"` 之类的等价写法），两个平台文件各自实现。

## 4. 实现步骤

1. 加 `proc_windows.go` 的三个函数与顶部限制说明。
2. 确认 `proc.go`（E09/E10 已改）里的调用点在 Windows 下编译通过：`GOOS=windows go vet ./...`。
3. 加 Windows 专属测试，用 `//go:build windows` 保证只在 Windows 上运行。
4. 把"派生子进程"的夹具改成平台函数，并让 E10 的 Unix 用例复用它。

## 5. 测试要求

以下用例都带 `//go:build windows`，在非 Windows 上不编译，因此**必须在 Windows 上实际跑过一次**并在 DoD 注明。

1. `TestKillTree_Windows_GrandchildDies`：`cmd.exe` 里 `start /b` 起一个 `ping -n 300 127.0.0.1`，取消后断言该子进程已结束（用 `tasklist /FI "PID eq <pid>" /NH` 判断，输出里不含该 PID 即通过）。
2. `TestKillTree_Windows_TaskkillMissing`：把 `taskkill` 路径注入成一个不存在的值（用可替换的包级变量 `taskkillPath`）→ 断言仍返回成功（直接子进程已杀）并产生一条 warn 日志。
3. `TestKillTree_Windows_TimeoutPath`：`timeout: 1s` + `ping -n 300` → Handler 在有界时间内返回、`ExitError.TimedOut==true`。
4. `TestTaskkillOutput_NotInArtifact`：确认 `taskkill` 的输出不出现在产物文件里（产物内容只包含任务自己的 stdout/stderr）。
5. `TestWaitDelay_Windows`：与 E10 第 5.4 条等价的场景（子进程持有管道句柄且父先退），断言 `Wait` 在 `WaitDelay` 内返回。
6. `TestSignalOf_WindowsIsEmpty`：`signalOf` 返回空串且 `Exec.Signal` 在响应 JSON 里被省略。

## 6. 完成标准（DoD）

- [ ] 六条测试全部在 Windows 上跑过并记录结果（跑不了的用例要说明原因，不能默默跳过）。
- [ ] `GOOS=windows go build ./... && GOOS=windows go vet ./...` 通过；`GOOS=linux`、`GOOS=darwin` 同样通过。
- [ ] 未新增依赖（`go.mod` 无变化）。若实现过程中判断必须引入 `x/sys` 才能可靠终止，**停下来先确认**，因为这会改变本卡的取舍。
- [ ] 已知限制写在代码注释里，不只在文档里。
- [ ] 与 E10 共用的 `proc.go` 调用点没有平台判断散落其中（平台差异只留在 `proc_*_.go`）。

## 7. 验收方式

```bash
# Windows（Git Bash 或 PowerShell）
go test ./executor -run 'KillTree_Windows|Taskkill|WaitDelay_Windows|SignalOf_Windows' -v
GOOS=linux go build ./... && GOOS=darwin go build ./...
go test ./... -race
```

手工：配一个 `runtime: cmd`、脚本内容 `start /b ping -n 300 127.0.0.1` 的档位，提交后取消任务，`tasklist | findstr ping` 应无结果。

## 8. 不在本任务范围

- 不实现 Job Object 终止（登记为 E11a，见第 9 节）。
- 不做 Windows 服务账户/权限相关配置（属部署文档，E19）。
- 不改 Unix 路径的实现。

## 9. 风险与回滚

- 风险：`taskkill /T` 的竞态（枚举之后新派生的进程未被终止）是方法层面的固有缺陷，不是实现问题。因此第 3.3 条要求把它登记成后续卡片，而不是在本卡声称已彻底解决。
- 风险：以管理员权限运行的服务可以杀任意进程；以服务账户运行执行器时，`taskkill /F` 可能因目标进程属于更高权限而失败——降级路径必须可用（第 5.2 条）。
- 回滚：整文件新增，删除即回到 E09 的"只杀直接子进程"行为；`proc.go` 无需改动。

## 10. 实现记录（2026-09-30）

落地文件：新增 `executor/proc_windows.go`（`sysProcAttr` / `killTree` / `runTaskkill` / `signalOf`
与顶部四条限制说明）、新增 `executor/proc_windows_test.go`（六条 `//go:build windows` 用例 + 平台夹具）、
改造 `executor/proc_unix_test.go`（夹具改名并复用同一组平台函数）、`executor/proc.go` 与
`executor/proc_test.go` 各更新一处指向 E11 的旧注释。`go.mod` 无变化。

### 与卡片的偏离与补充

1. **§3.1 的两步顺序必须反过来**（本卡最重要的一处偏离）。卡片要求"先 `cmd.Process.Kill()` 保证直接子进程一定死，
   再用 `taskkill /T` 清理派生树"。本机实测这条顺序达不到目的：`taskkill` 的 `/T` 靠父子关系枚举，
   直接子进程一死，`taskkill /PID <pid> /T /F` 就只回一句"没有找到进程"（退出码 128），
   它派生的那一层原样活着。两次对照实验（同一条 `cmd /c cmd /c ping` 树）：
   先 `taskkill` 时日志列出三个 PID 全部终止；先 `Kill` 时 `taskkill` 报 128、孙进程继续跑。
   实现因此取"先 `taskkill /T /F`，再对直接子进程 `Kill`"，后者变成"taskkill 缺失或失败"时的保底手段。
   理由写在 `killTree` 的注释里，不只在本文。
2. **`grace` 参数在 Windows 上用不到**：没有可先发的温和信号，只能强杀。参数保留是为了与
   `proc_unix.go` 的同名函数同签名，`proc.go` 的调用点因此不需要平台判断（DoD 最后一条）。
3. **降级日志走 `slog.Default()`**：`killTree` 的签名由卡片固定，拿不到 Runner 的 logger。
   `cmd/server` 启动时已经 `slog.SetDefault(logger)`，所以这条 warn 与降级 debug 都会进服务日志、
   不会静默。测试里用 `slog.SetDefault` 接管成缓冲来做断言（用例结束恢复）。
4. **§5.2 的用例不起 Runner，直接调 `killTree`**：降级点就在 `killTree` 内部，
   用一条自己 `Start` 的进程树测它最短也最稳；断言按卡片口径——返回 nil（执行结论不被改写）、
   产生一条 warn（日志里带上取不到的那个路径）、直接子进程确实被结束。
   "测试要跳过"这条口径没有采用：注入了不存在的路径就是可控场景，没有需要跳过的环境条件。
   另外"找不到 taskkill"与"权限不足导致失败"在实现里是同一条降级路径（`runTaskkill` 返回错误即降级），
   没有为两者分出两套代码。
5. **§3.4 的夹具抽象落成五个同名平台函数**：`grandchildCommand`（派生孙进程的档位）、
   `pipeHeldBody`（直接子进程先退、孙进程占住输出的命令串）、`watchGrandchild`（拿到孙进程 PID）、
   `processAlive`（存在性判定）、`helperPIDs`（机器上同名辅助进程的集合）。
   Unix 侧的实现仍靠脚本把 PID 写进 workspace 的文件；Windows 侧不要求脚本配合，
   改成按进程树找：执行器是在测试进程里起子进程的，所以"本测试进程的后代里的 `ping.exe`"就是本次派生的那批。
   因此 `watchGrandchild`/`grandchildCommand` 的 workspace 参数在 Windows 未使用，签名保持与 Unix 一致。
   E10 的六条 Unix 用例改用同一组函数（原 `sleeperPIDs` 改名 `helperPIDs`），Linux 实跑仍然全绿。
6. **进程可见性判定不用 `tasklist`**：卡片 §5.1 提的 `tasklist /FI "PID eq <pid>"` 要解析文本、
   还依赖代码页与列格式，改成标准库 `syscall` 的 `OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION)`
   + `GetExitCodeProcess`（退出码不是 `STILL_ACTIVE`=259 即已结束）；进程枚举用
   `CreateToolhelp32Snapshot` + `Process32First/Next` 的 `ParentProcessID`。
   §7 的手工验收仍按卡片给的 `tasklist | findstr ping`。
   这条判定只用于测试，没有进生产代码：别人用户的进程可能因权限打不开句柄，会误判成"已结束"。
7. **未新增依赖**（DoD 第三条）：上面用到的 `CREATE_NEW_PROCESS_GROUP`、`ProcessEntry32`、
   `CreateToolhelp32Snapshot`、`OpenProcess`、`GetExitCodeProcess` 都在标准库 `syscall` 里，
   不需要 `golang.org/x/sys/windows`，因此没有触发"停下来先确认"那条。
8. **等待 Handler 返回的辅助函数要能重复调用**（写测试时踩到的）：执行结果只有一个值，
   正文取走之后清理函数再取就永远等不到——第一版用例因此整条挂住 60 秒被判失败。
   改成 `startCancellableRun` 返回一个"取结果"动作，取过之后直接返回已存的结果。
9. **冒烟的档位形态与 §7 不同**：`kind: script` + `runtime: cmd` 渲染出的 argv 是
   `[cmd.exe 脚本路径]`，而 `cmd.exe` 没有 `/c` 不会执行脚本，会起一个交互式 shell 挂在那里等输入
   （实测打印版本横幅后不退出，任务只能等到超时）。冒烟因此改用
   `kind: binary` + `program: cmd` + `fixed_args: [/c, slow.cmd]`，脚本仍在 workspace 内、
   由子进程工作目录解析。这是 E02/E08 那层的缺陷：`script` 档位对 `cmd`/`pwsh` 这类
   "必须带开关才执行文件"的解释器没有参数形状。已登记，见"留给后续卡片"第 2 条。
10. **取消路径上 `exit_code` 记的是 1**：`TerminateProcess` 给的固定退出码，不是任务自己的取值；
    `Signal` 为空（§3.1 第 3 条）。这条与卡片口径一致，写在这里免得后续把它当新问题。
11. **产物字节按原样存**：Windows 控制台输出是本地代码页（本机是 GBK），
    `meta.json` 里的尾部预览把非 UTF-8 字节编成了 `\ufffd`（替换字符）。
    产物文件本身没有损失；taskkill 的输出只进 debug 日志，同样是原样字节。登记见"未验证"最后一条。

### 验证结果

Windows 本机（`10.0.26200`，go1.26.4 windows/amd64）——DoD 第一条要求"六条在 Windows 上实际跑过"：

| 用例 | 结果 | 时长 | 判定点 |
| --- | --- | --- | --- |
| `TestKillTree_Windows_GrandchildDies` | PASS | 0.75s | 取消前 `ping.exe` 存活，取消后该 PID 结束且本测试进程后代里没有残留辅助进程 |
| `TestKillTree_Windows_TaskkillMissing` | PASS | 0.09s | `killTree` 返回 nil、日志出现 warn（含注入的不存在路径）、直接子进程结束；孙进程留住了（日志如实记录，属 §3.3 的限制不是实现失败） |
| `TestKillTree_Windows_TimeoutPath` | PASS | 1.53s | `TimedOut==true`、`errors.Is(err, DeadlineExceeded)`、可重试，返回时间远小于宽限期+管道上限，后代无残留 |
| `TestTaskkillOutput_NotInArtifact` | PASS | 5.35s | `.out` 只有 `job-output-marker` 一行；标记串只出现在 debug 日志，两个产物文件里都没有 |
| `TestWaitDelay_Windows` | PASS | 5.24s | 等满 `processWaitDelay` 后返回，结论是 `output stayed open past the wait limit`，已写入的 `started` 仍在产物里 |
| `TestSignalOf_WindowsIsEmpty` | PASS | 0.55s | `exit 5` 与取消两条路径的 `Signal` 都是空串，`job.Exec` 的 JSON 里没有 `"signal"` 键 |

- 三条会在机器上留下长命进程的用例（超时、产物、降级）都有清理：降级那条用系统 `taskkill` 显式收尾，
  不留孤儿。
- 全仓 `go test ./... -race -count=1`（Windows）全绿：api 95.6s / cmd/server 5.6s / core 8.9s / executor 21.6s。
- `GOOS=windows`、`GOOS=linux`、`GOOS=darwin` 的 `go vet ./...` 全部通过
  ——两条平台测试文件各自只在本平台编译，`proc.go` 的三行接线在三个平台上都成立。
- 改造夹具后的 Unix 用例在 WSL2 Ubuntu（go1.24.13 linux/amd64）实跑回归：
  `TestKillTree_GrandchildDies` 2.02s、`TestKillTree_TimeoutPath` 3.01s、
  `TestKillTree_NoOrphanAfterCancelStress` 2.06s、`TestWaitDelay_PipeHeldByGrandchild` 4.01s、
  `TestSignalOf` 0.01s、`TestGracefulShutdown_NotStalled` 2.22s 全 PASS；
  executor 整包 ok 18.9s，全仓 `go test ./...`（不带 `-race`）与 `go vet ./...` 全绿。
- 冒烟（Windows 真实服务端 + REST，一份独立配置在临时目录，鉴权用静态 token，
  `executors.required_role: operator`）：档位 `exec.win_tree` =
  `kind: binary` + `program: cmd` + `fixed_args: [/c, slow.cmd]`，脚本内容
  `start /b ping -n 300 127.0.0.1` 再加同一条 `ping`（让直接子进程自己也在计时）。
  - 提交后任务 `running`，`tasklist //FI "IMAGENAME eq ping.exe"` 看到 **两个** `PING.EXE`
    （一个后台派生、一个直接子进程）——正是本卡要消掉的情形。
  - `POST /api/v1/jobs/<id>/cancel` → 204；紧接着的 debug 日志把整棵树都列出来了：
    `executor taskkill finished pid=10912 output="成功: 已终止 PID 20836 (属于 PID 10912 进程) 的进程…PID 17672 (属于 PID 10912)…PID 10912 (属于 PID 13888)…"`，
    INFO 日志 `executor run finished ... exit_code=1 duration_ms=17716` 与 `job execution cancelled`（走中断分支，不消耗重试）。
  - 取消后 `tasklist //FI "IMAGENAME eq ping.exe"` 无匹配，卡片 §7 的
    `tasklist | findstr ping` 也是"没有结果"。
  - 产物 `a1.out` 只有任务自己的输出（`tree-smoke-start` 加 ping 回显），`a1.meta.json` 的
    `out_bytes=1868`、`artifact=available`，没有 taskkill 的痕迹。
  - 取消之后 `GET /jobs/<id>` 与 `/result` 都是 404（E10 已登记的可观测性问题，本卡复现，不改）。

### 未验证

- macOS：本卡不涉及新代码，Unix 路径未改动。
- 以服务账户运行、目标进程属于更高权限的 `taskkill /F` 失败场景：本机是同用户单机，
  只按"非零退出即降级"处理，没有构造出权限失败的用例。属部署侧检查（E19）。
- 真实"没有 taskkill"的精简镜像：只用注入不存在路径的方式模拟，没在无 `taskkill` 的容器镜像里跑过。
- `taskkill /T` 的竞态窗口（枚举之后新派生的进程漏掉）：无法写出稳定用例，留给 E11a 的机制消掉。
- 非中文代码页下的 `taskkill` 文本：本机只在 GBK 代码页验证过原样字节进日志；
  要不要转码、按什么代码页转，归 E19 部署文档。

### 留给后续卡片的接口形状

1. **E11a（Job Object）**：本卡的两个已知缺口都指向它——降级路径留孤儿（§5.2 那条用例如实记录了现象），
   以及 `/T` 枚举的竞态窗口。做法是把子进程放进 Job Object 并设
   `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`，关句柄即整棵树结束。
   代价是要引入 `golang.org/x/sys/windows` 或手写 `NewLazyDLL`，需要在开工时确认口径。
2. **`script` 档位的参数形状**（本卡第 9 条）：`cmd`、`pwsh` 这类解释器要带 `/c`、`-File` 之类的开关才会执行文件，
   而 `Render` 对 `script` 固定生成 `[runtime, script_path]`。要么在档位里声明开关，要么在加载时拒绝这类 runtime。
   归配置层的后续卡片（E12 之后的空档）或在 E19 文档里先写"cmd 档位请用 binary"。
3. **代码页与预览**（本卡第 11 条）：产物是原始字节、预览按 UTF-8 编码会出 `\ufffd`；
   若要在响应里给出可读文本，需要一条"按档位声明代码页转码"的口径，属 E19（Windows 部署）或读侧后续。

