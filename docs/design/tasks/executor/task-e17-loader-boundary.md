# TASK-E17　目录加载器对执行器任务的边界

- 所属阶段：M5 收口
- 依赖任务：TASK-E04
- 涉及文件：`core/load.go`、`core/load_test.go`（或 `core/load_watcher_test.go`）、`examples/demo2/main.go`、`core/config.go`（注释）
- 预计规模：小

## 1. 任务目标

给目录任务加载器加一道判断：默认不接受 `exec.` 前缀的任务文件，除非调用方明确允许。目的是让"能往目录里写文件"不等于"能执行命令"。

## 2. 背景与当前问题

`core/load.go` 的 `DirectoryLoader` 会读取目录下匹配的 JSON 文件，按 `FileJobFormat.Name` 作为处理函数查找键，并从 `LoaderOptions.HandlerMap` 自动绑定（该结构体的 `Name` 字段注释就写着"必填，同时作为 Handler 查找键"）。这条路径上没有任何凭据。

需要先说清一件事，避免把本卡的理解写偏：**服务端二进制今天并没有启用目录加载器**。已核实：`NewDirectoryLoader` 只出现在 `core/load.go`、`examples/demo2/main.go` 与测试里，`cmd/server/main.go` 不引用它，`configs/config.example.yaml` 也没有加载器配置项（`core/config.go` 的注释说明未实现的选项故意不收录）。

所以本卡的边界是**库层面的防护**：任何人在自己的程序里把 `Scheduler` 和 `DirectoryLoader` 接起来、又开启了执行器，就会面对"目录写权限等于命令执行权限"这个问题。默认拒绝让这种组合必须显式打开。

## 3. 要实现的功能

1. `LoaderOptions` 增加字段：

   ```go
   // AllowExecJobs 允许加载 exec. 前缀的任务文件。
   // 默认 false：写目录的权限不等于执行权限。
   // 注意：服务端二进制当前不启用加载器（见 docs/design/executor-design.md §6.9），
   // 本字段约束的是自行接入 DirectoryLoader 的程序。
   AllowExecJobs bool
   ```

2. 判断位置：在解析出 `FileJobFormat` 之后、绑定 `HandlerMap` 与 `Schedule` 之前。命中拒绝时的处理与现有"解析失败"路径一致：写入 `ErrorDir`（若配置了）并按既有归档/删除策略处理源文件，记一条 **warn**，日志字段包含文件路径与原因 `executor jobs are not accepted from the loader`。
   - 不要静默丢弃文件：文件留在原处会被反复扫描（`KeepAfterLoad` 的"已处理"记录只在成功路径上写）。
   - 不要把错误文件写回原目录（会造成无限循环）。
3. `examples/demo2/main.go`：演示如何打开这个开关，并在注释里写清"这里显式允许是因为示例目录只有本机可写"。示例代码要保持能编译（`go vet ./...` 覆盖）。
4. `executors.loader_allow` 配置项（E01 已加）的实际使用点：`Registry` 暴露 `LoaderAllowed() bool`，供自行接入加载器的程序读取，避免"配置文件里写了但没人读"。如果最终发现没有任何调用方，就在 E01 的配置注释里写明"预留给库使用者，服务端不读取"——**不要把结论停在"看起来已生效"**。
5. 前缀常量化：`const ExecHandlerPrefix = "exec."` 放在 `executor` 包会引入 `core → executor` 的反向依赖，因此这个常量定义在 `core`（例如 `core/job.go` 里 `// ExecPrefix ...`），`executor` 与 `core/load.go` 共用。注册键生成（E02 的 `HandlerKey()`）也引用它。

## 4. 实现步骤

1. 加 `core.ExecPrefix` 常量，改 `executor.Profile.HandlerKey()` 引用它（E02 目前是字符串字面量拼接）。
2. 加 `LoaderOptions.AllowExecJobs` 与判断分支。
3. 加测试。
4. 更新 `examples/demo2` 与配置注释。
5. 决定第 3.4 条的结论（`LoaderAllowed()` 还是"预留注释"），并在本卡第 6 节记录结论。

## 5. 测试要求

`core` 包内的既有加载器测试用 `t.TempDir()` 造目录与文件，沿用该写法。

1. `TestLoader_RejectsExecJobByDefault`：写一个 `{"name":"exec.echo", ...}` 的文件 → 任务不进调度器（`HeapLen()==0`）、`HandlerMap` 里的处理函数未被调用、文件被移走（按当前 `PostLoadAction` 断言目标位置）、日志里有 warn。
2. `TestLoader_AllowExecJobs`：`AllowExecJobs: true` → 正常加载并绑定处理函数（用一个会置位的假 Handler 断言确实执行到了）。
3. `TestLoader_ExecPrefixIsExact`：`{"name":"execx"}`、`{"name":"payment_check"}` 不受影响；`{"name":"exec."}`（空前缀后的名字）按拒绝处理（并说明为什么空前缀也拒绝：注册键为 `exec.` 说明配置写错了）。
4. `TestLoader_RejectedFileNotLooping`：连续扫描两次，第二次不再报同一个文件的错（文件已被移走），且不产生重复的错误副本堆积（`ErrorDir` 里同名文件的处理沿用既有加时间戳的逻辑，确认不与它冲突）。
5. `TestLoader_NoErrorDirConfigured`：没配 `ErrorDir` 时拒绝路径不 panic，源文件按 `PostLoadAction` 处理。
6. 回归：`core/load_test.go` 与 `core/load_watcher_test.go` 既有用例全部不改断言通过。

## 6. 完成标准（DoD）

- [ ] 默认配置下，写文件到监控目录无法产生执行器任务，有测试证明"Handler 未被调用"（不是只看状态）。
- [ ] 被拒绝的文件有明确去向和一条 warn 日志，不会反复扫描，不会丢在原地无人知晓。
- [ ] `ExecPrefix` 常量在 `core` 定义，`executor` 与 `core/load.go` 都引用它，没有第二处字面量。
- [ ] `executors.loader_allow` 有明确结论：要么被 `Registry.LoaderAllowed()` 暴露并被示例读取，要么在配置注释里写明"服务端不读取"。不允许出现"配置存在但行为未定义"。
- [ ] `examples/demo2` 仍能直接运行（`go run ./examples/demo2` 行为与之前一致，只多一个可选开关）。
- [ ] `go test ./core -race` 全绿，包括既有的 100ms 静默窗口相关测试（`loaderDebounceInterval` 的用例对本卡改动敏感）。

## 7. 验收方式

```bash
go build ./... && go vet ./... && go test ./... -race
go test ./core -run 'Loader' -race -v
```

手工（照 demo2 的目录）：

```bash
go run ./examples/demo2 &
printf '{"name":"exec.hello","delay":"1s","payload":{"args":{}}}' > job_queue/exec_try.json
# 观察：文件被移入错误目录、日志出现一条 warn、没有执行器被调用
```

## 8. 不在本任务范围

- 不给服务端加"启用目录加载器"的配置项（那是独立功能，需要先补齐 `LoaderOptions` 的配置化，属新特性而非本设计范围）。
- 不做任务文件签名或来源校验（能写文件就能伪造任何任务，这条边界只针对执行器前缀）。
- 不改加载器的静默窗口、归档与错误隔离策略。
- 不引入按分组限制加载器提交的机制。

## 9. 风险与回滚

- 风险：默认拒绝会让"用文件投递跑脚本"的既有用法（如果有）突然失效。目前服务端不启用加载器，风险面很小；但 `examples/demo2` 一旦被复制成生产代码，就需要显式打开开关——文档（E19）要写明开关名字。
- 风险：`ExecPrefix` 常量放进 `core` 会让 `core` 出现一个只对执行器有意义的概念。这是为了避免反向依赖而做的取舍，注释里要写明理由，否则后人会试图把它移回 `executor`。
- 回滚：删掉 `AllowExecJobs` 判断即可恢复原行为；`ExecPrefix` 常量可以留着（纯命名改善）。

## 10. 实现记录（2026-09-30）

落地文件：`core/job.go`（新增 `ExecPrefix` 与 `IsExecHandlerKey`）、`core/load.go`
（`LoaderOptions.AllowExecJobs`、`LoadFile` 里的拒绝分支、新方法 `rejectExecJob`、
原因常量 `execJobRejectReason`）、`executor/profile.go`（`HandlerKeyPrefix` 改成引用
`core.ExecPrefix`）、`executor/registry.go` 与 `core/config.go`（`loader_allow` 的结论注释）、
`examples/demo2/main.go`（显式写出开关与一行运行期提示）；
新增测试 `core/load_exec_boundary_test.go`（6 条用例）与 `executor/profile_test.go` 里的一条
跨包一致性用例。

### 与卡片的偏离与补充

1. **判断点在 `json.Unmarshal` 之后、`formatToJob` 之前**（§3.2 只要求"绑定 `HandlerMap`
   与 `Schedule` 之前"）：既然决定不接受，就没必要先把文件换算成 Job 再丢弃。
2. **拒绝返回 `nil` 而不是 `error`**（§3.2 说"与现有解析失败路径一致"）。这里刻意不一致：
   解析失败那条路是调用方记 error 级日志 + 调 `handleErrorFile`，而"这类文件我们不从目录接受"
   是加载器的一个有意判断，不是故障。做成返回 error 会同时留下 error 级日志和错误副本，
   一行 warn 才是这件事应有的音量。于是拒绝路径在 `rejectExecJob` 里自己走完
   "warn + 复制进 `ErrorDir` + 按 `PostLoadAction` 处理源文件"，对外返回 nil。
3. **`KeepAfterLoad` 下被拒绝的文件也记进 `processedFiles`**：这条正是 §3.2 那个顾虑的落地——
   不记就会被每轮扫描反复拒绝。用例 `TestLoader_RejectedFileNotLooping` 连扫两轮，
   warn 只有一行、错误副本只有一份。
4. **`DeleteAfterLoad` 与 `handleErrorFile` 的分工写在代码里**：`handleErrorFile` 在配了
   `ErrorDir` 且策略是删除时会顺手删掉源文件，所以那条分支之后不再调 `postProcess`，
   否则会对一个已经不存在的文件删第二次。没配 `ErrorDir` 时源文件仍按策略处理（§5.5）。
5. **`ArchiveAfterLoad` + `ErrorDir` 的两份拷贝**：错误目录里是"原文件 + 一行原因"，
   归档目录里是原文件本身。这是 §3.2 两条要求叠加之后的自然结果，冒烟现场如此
   （`job_errors/exec_try.json.error` 与 `job_archive/<时间戳>_exec_try.json`）。
6. **常量名用 `core.ExecPrefix`**（§3.5 写的是 `ExecHandlerPrefix`），并保留
   `executor.HandlerKeyPrefix` 作为指向它的同义常量：既有导出名有调用方
   （`api/executors_submission_test.go` 与 `Profile.HandlerKey()`），删掉它属于无谓的破坏。
   DoD 第三条"没有第二处字面量"由此满足——`"exec."` 这个串只在 `core/job.go` 出现一次。
7. **判断走 `core.IsExecHandlerKey(name)` 而不是裸 `strings.HasPrefix`**：它对名字做前后空白
   再去前缀比较。带空白的写法在注册表里查不到东西，但按"是不是执行器键"判断时不该因此
   被当成普通任务放过去；`"exec."`（只有前缀、没有档位名）也按执行器键拒绝（§5.3 的要求）。
8. **`executors.loader_allow` 的结论（§3.4）**：`Registry.LoaderAllowed()` 保留，作为该配置项
   唯一的读取方；服务端不启用加载器，所以它在服务端没有行为差别。
   `core/config.go` 与 `executor/registry.go` 的注释都写明"接线在调用方"——
   自行接入加载器的程序把 `LoaderAllowed()` 的结果填进 `LoaderOptions.AllowExecJobs`，
   加载器本身不回读配置（`core` 读不到执行器登记表）。结论不再是"看起来已生效"。
9. **`examples/demo2` 显式写 `AllowExecJobs: false`**（§3.3 说"演示如何打开这个开关"）：
   示例若默认打开，§7 的手工验收就跑不出拒绝现场。做法是字段写出来、注释给出怎么打开、
   打开的前提（目录收成只有服务账号可写）以及配置项到字段的接法；另在启动提示里加一行
   "exec. 开头的任务文件默认不被接受"。
10. **测试文件位置**：新增 `core/load_exec_boundary_test.go` 而不是塞进 `core/load_test.go`
    （卡片列的文件）。本组用例要同时看日志缓冲、归档目录与错误目录，写法与
    `core/load_format_test.go`（testify + 日志捕获）一致；`core/load_test.go` 用的是朴素
    `testing` 风格。辅助函数起名 `writeNamedJobFile`，因为 `core/load_watcher_test.go`
    已经有同包内的 `writeJobFile`。
11. **跨包一致性用例放在 `executor`**（`TestHandlerKeyPrefixMatchesCoreLoader`）：
    要断言"档位实际注册出来的键，加载器那道判断认得"，只有 `executor` 能同时看到两边
    （`core` 不能反向 import 执行器包，这正是常量放 `core` 的原因）。

### 验证结果

Windows 本机（`10.0.26200`，go1.26.4 windows/amd64）：

| 命令 | 结果 |
| --- | --- |
| `go build ./... && go vet ./...` | 通过 |
| `go test ./... -race -count=1` | api 112.1s、cmd/server 5.4s、core 12.6s、executor 22.1s 全 ok |
| `go test ./core -run 'Loader' -race -v` | §5.1…§5.5 六条新用例全 PASS |
| 既有用例 | `core/load_test.go`、`core/load_watcher_test.go`、`core/load_format_test.go` 断言一字未改，全绿（§5.6 的回归要求） |
| `GOOS=linux GOARCH=amd64`、`GOOS=darwin GOARCH=arm64` 的 `go build` | 通过 |
| `go build -tags dashboard ./...` | 通过 |

新增用例清单：

| 用例 | 覆盖的卡片条目 |
| --- | --- |
| `TestLoader_RejectsExecJobByDefault` | §5.1（不进堆、Handler 未调用、源文件按策略离开、错误副本带原因、一行 warn、没有 error 级日志） |
| `TestLoader_AllowExecJobs` | §5.2（打开后入队、绑上的就是那个函数，并直接调用它确认置位） |
| `TestLoader_ExecPrefixIsExact` | §5.3（`execx`/`payment_check`/`my_exec_job`/`EXECUTE` 照常加载；`exec.` 与带前导空白的写法被拒） |
| `TestLoader_RejectedFileNotLooping` | §5.4（连扫两轮：warn 一行、错误副本一份） |
| `TestLoader_NoErrorDirConfigured` | §5.5（没配 ErrorDir 不 panic，源文件仍被删掉） |
| `TestExecPrefixHelper` | `ExecPrefix` 与 `IsExecHandlerKey` 的取值表 |
| `TestHandlerKeyPrefixMatchesCoreLoader`（`executor`） | DoD 第三条的跨包一致性 |

### 冒烟（真实进程，照 §7 的 demo2 目录）

`go build -o demo2.exe ./examples/demo2` 后在临时目录里跑起来（fsnotify 监控 `./job_queue`，
`ArchiveAfterLoad` + `ErrorDir=./job_errors`），往里放两个文件：

```
{"id":"exec_try","name":"exec.echo","delay":"1s","payload":{"args":{}}}
{"id":"plain_try","name":"payment_check","delay":"1s","payload":{"order_id":"ORD-1"}}
```

现场（日志时间是本机 +08:00）：

| 观察点 | 结果 |
| --- | --- |
| `./job_queue` | 空，两个文件都被处理走 |
| `./job_errors` | 只有 `exec_try.json.error`，末尾一行 `// ERROR: executor jobs are not accepted from the loader` |
| `./job_archive` | `20260930_164455_exec_try.json` 与 `20260930_164455_plain_try.json`（第 5 条说的两份拷贝） |
| 进程日志 | 一行 `WARN executor job file rejected by the loader path=job_queue\exec_try.json job_name=exec.echo reason="executor jobs are not accepted from the loader" hint="set LoaderOptions.AllowExecJobs ..."`，没有 error 级日志，也没有第二次 warn |
| 执行侧 | 日志里只有 `检查支付: {"order_id":"ORD-1"}`，`exec.echo` 从未被执行（demo2 也没有注册这个名字） |
| `jobs.json` | 落盘的任务名只有 `payment_check` |

### 未验证与遗留

- **`AllowExecJobs: true` 的现场只有单元测试**：示例程序按第 9 条保持 false，要跑出"打开之后
  确实加载"的现场得改示例或另写一个装配程序。单元测试里那条断言的是入队、绑定与调用三层。
- Linux 的 inotify 路径与那一侧的 `-race` 未实跑（WSL2 缺 gcc），与其他卡片同一条遗留。
  监控模式下的拒绝走的是与扫描模式同一个 `LoadFile`，两条路径共用一个判断。
- 目录加载器仍未接入服务端（§8 明确不做），因此本卡的边界对 HTTP 提交路径没有影响：
  提交路径的档位判定在 TASK-E16。
- `executors.loader_allow` 现在的完整含义写在第 8 条：配置项 → `Registry.LoaderAllowed()` →
  调用方填 `LoaderOptions.AllowExecJobs`。服务端里它是"可被读取但没有消费者"的状态，
  这一点已写进配置注释与 E19 的部署文档清单，别让读者以为打开它服务端就会接受目录里的
  执行器任务。
- `docs/deployment.md` 与 `docs/api.md` 的同步归 E19，已把要写的两条补进该卡 §3.2。
