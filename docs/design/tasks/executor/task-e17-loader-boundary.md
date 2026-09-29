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
