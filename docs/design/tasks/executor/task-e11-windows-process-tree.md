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
