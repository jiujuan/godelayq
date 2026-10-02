# TASK-R06　接线：重载链、回滚与 /admin/runtime 的重载状态

- 所属阶段：M2 接线
- 依赖任务：R01（`Diff`/`ReloadState`）、R02（六个 setter）、R03（`ResizeWorkers`）、
  R04（`Applier.ApplyConfig`）、R05（`ConfigWatcher`）
- 涉及文件：`cmd/server/reload.go`（新增）、`cmd/server/reload_test.go`（新增）、
  `cmd/server/main.go`、`cmd/server/main_integration_test.go`（替身补方法）、
  `api/reload_state.go`（新增）、`api/handlers_admin.go`、`api/server.go`（一个 Option）、
  `api/handlers_admin_test.go`
- 预计规模：大（本系列把前面五张卡接成一条链，也是唯一同时动 `cmd` 与 `api` 的一张）

## 1. 任务目标

把 R01–R05 交付的零件接成一条能跑的链：文件变化 → 整份读回 → `Diff` →
逐项应用（失败逐项回滚）→ 换入 `applied` → 日志与 `/admin/runtime` 给出结论。
本卡结束时，改 `configs/config.yaml` 里那些热更键，**不重启**就能在真实进程里看到效果。

## 2. 背景与当前问题

前面五张卡交付的都是"零件"，各自单包可验，但没有一条链把它们串起来。缺这条链时会漏四件事，
而这四件事正是本卡的全部难度：

1. **`applied` 这份权威没人持有**。设计文档 I1 说的是"重载成功才换"，那必须有一个地方存着
   "当前生效的那份配置"，而 `cmd/server` 现在只有 `deps.config`（`cmd/server/main.go:129`），
   它在 `run()` 里被 `Normalized()` 一次就分发给各构造闭包（`:176-311`），之后再没人回头看。
2. **顺序与回滚**。八个热更落点分属四个包，应用顺序错了会留下半套（例如档位表已换而
   调度器处理函数没换）；回滚必须是逆序。
3. **窄接口要扩**。`run()` 只通过 `schedulerAPI`、`eventLogAPI`、`auditLogAPI` 这些
   装配方自定的窄接口看下游（`cmd/server/main.go:20-127`），R02/R03 的新方法不在这些接口里，
   接不上就只能在 `reload.go` 里做类型断言——那是这套装配风格明确避免的
   （对照 `newExecutorProfileStore` 交回**实例**而不是记录的理由，`:143-145`）。
4. **重载状态要有读口，而且不能把凭据带出去**（设计文档 §5.4）。

## 3. 要实现的功能

### 3.1 `cmd/server/reload.go`：链本体

```go
// reloader 持有"当前生效的那份配置"，并把一次文件变化走完一条固定顺序的链。
//
// 为什么是一个类型而不是 run() 里的一串闭包：这条链的状态（applied、串行锁、
// 最近一次结论）跨多次重载存活，塞进 main.go 会让那个文件同时承担装配与运行期行为两件事。
type reloader struct {
        mu       sync.Mutex // 串行整条链：两次文件变化不能交叉着读同一份配置（设计文档 §4 的并发口径）
        applied  core.Config
        cfgPath  string             // 启动时真正读到的那个文件的绝对路径
        deps     reloadTargets      // 八个落点，见 §3.2
        logger   *slog.Logger
}

// Reload 走完整条链。它同时是 core.ReloadFunc 的实现，因此必须自串行（R05 的契约）。
//
// 七步，顺序固定：
//  1. candidate, err := core.LoadConfig(cfgPath)
//     失败（语法错、未知键、Validate 不过、文件被删）→ 结论 rejected，applied 不动，
//     记 error，返回。**绝不退回默认值**：那会让整台机器静默变成默认配置在跑。
//  2. candidate = candidate.Normalized()
//  3. change := core.Diff(applied, candidate)
//  4. change.HasRejections() → 结论 rejected（RejectedKeys 逐条列出），applied 不动，记 error。
//     这一步早于任何写入，所以拒绝档的代价是"什么都没发生"，不是"改了一半"。
//  5. !change.HasChanges() → 结论 unchanged，applied 不动（内容相同的存盘不该重登记档位）。
//  6. 逐项应用热更档（§3.2 的顺序），每项成功后把它的 undo 压栈。
//     任一项失败 → 逆序执行 undo，结论 failed（或 degraded，见 §3.3），applied 保持旧值，记 error。
//  7. 全部成功 → applied = candidate，结论 ok（AppliedKeys 来自热更档），
//     同时把 Restart 档的键名抄进 IgnoredKeys，记一条 info，并 SetDebounce(candidate.Reload.Debounce)。
//
// 第 7 步末尾那句是 reload.debounce 能热更的唯一落点：窗口改了，下一次事件起用新值。
func (r *reloader) Reload() (core.ReloadState, error)

// 结论的保存与并发可读由 R05 的 ConfigWatcher 负责（Reload 把结论作为返回值交回它），
// 所以 reloader 不存状态、也没有 State 方法：api 的读口注入的是 watcher。
// 唯一要在这里说的是 LastAppliedAt 由本方法填（只有它知道"这次真的应用了"）。
```

三条必须写进注释的判断：

- **环境变量压住的键**：`LoadConfig` 每次都重新绑 `GODELAYQ_*`（`core/config.go:734-794`），
  所以环境里有常驻覆盖时 `Diff` 可能给出"无变化"。本卡不加来源判定（待拍板 P4），
  但 `unchanged` 那条 info 文案要写明"取值以配置文件与环境变量的合并结果为准"
  （设计文档 §5.3）。
- **`reload.enabled` 改了不生效**：它属重启档，会出现在 `IgnoredKeys` 里。
  监听器不会因为文件里把它改成 false 而自己停掉——这条要在 `IgnoredKeys` 的日志文案里说清，
  否则运维会以为已经关掉了热重载。
- **`executors.commands` 在 `executors.enabled=false` 时**：`Applier.ApplyConfig` 会返回错误
  （R04 §3.3 第 1 步）。本卡把它特殊处理成"改动已记录但执行器未启用，本次不生效"：
  结论仍是 `ok`，`AppliedKeys` 含 `executors.commands`，同时记一条 warn 说明没生效的原因。
  理由与 `core/config.go` 里"关闭即惰性"的既有口径一致（`executors.enabled: false` 时本节取值全部不生效），
  而不是让一次无意的档位改动把整次重载判成失败。

### 3.2 八个落点与应用顺序

```go
// reloadTargets 是热更落点的集合。全部是接口或函数值，与 defaultRuntimeDeps 的风格一致：
// run() 与 reloader 都不认识具体实现，测试替身因此不需要真的起进程。
type reloadTargets struct {
        setLevel     func(level string) error          // R02: core.SetLogLevel(levelVar, ...)
        setWorkers   func(n int) error                 // R03: scheduler.ResizeWorkers
        setRetry     func(maxDelay time.Duration)      // R02: scheduler.SetRetryPolicy(&core.ExponentialBackoffRetry{...})
        setRetention func(limit int, ttl time.Duration)// R02: store.SetHistoryRetention
        setEventRetention  func(count int, age time.Duration) // R02: EventLog.SetRetention（未启用时为 nil）
        setAuditRetention  func(count int, age time.Duration) // R02: AuditLog.SetRetention（未启用时为 nil）
        setCommands  func(candidate core.Config) error        // R04: applier.ApplyConfig（未启用执行器时为 nil）
        setDebounce  func(d time.Duration)                        // R05: watcher.SetDebounce（watcher 未建时为 nil）
}
```

应用顺序（顺序即依赖，注释里逐条给理由）：

| # | 热更键 | 落点 | 为什么在这个位置 |
| --- | --- | --- | --- |
| 1 | `logging.level` | `setLevel` | 最先：后面每一步都要用它记日志，级别改了应当立刻生效于本次重载自己的日志 |
| 2 | `reload.debounce` | `setDebounce` | 早于任何可能失败的动作，让"窗口写小了"立刻在下一次事件起作用 |
| 3 | `scheduler.max_retry_delay` | `setRetry` | 只换策略实例，无副作用，放前面降低回滚复杂度 |
| 4 | `store.history_limit`/`history_ttl` | `setRetention` | 与 #3 一样是取值替换；下一次写入的 trim 才用到（R02 §3.5） |
| 5 | `observability.*.retention_*` | `setEventRetention`/`setAuditRetention` | 写入器未启用时对应函数为 nil：那两条键改动**跳过并记入 IgnoredKeys**，而不是失败（总开关关着时本节的取值本来不生效） |
| 6 | `scheduler.workers` | `setWorkers` | 唯一会起协程的一步，放在纯取值替换之后；失败时前面几步的 undo 都已就位 |
| 7 | `executors.commands` | `setCommands` | 最后：它是唯一会改调度器处理函数表的一步，R04 保证它失败时不动任何东西，放在末尾让"表换了但别的东西没换"的窗口最短 |

每个落点的 undo 由同一段代码在应用前记录旧值（`applied` 里的对应字段就是旧值，
不需要额外的读取口），压进栈；`setCommands` 的 undo 是 `setCommands(旧 applied)`——
R04 的 `ApplyConfig` 可重入且幂等（其 §5.2 倒数第二条用例），所以逆序重放是安全的。

`degraded` 的判定（设计文档 I2）：undo 过程中任何一步返回错误 → 结论 `degraded`，
`Error` 里同时带原始失败与回滚失败的文本，记 error。这是本卡唯一允许"内存里半新半旧"的状态，
而它必须被读出来。

### 3.3 窄接口扩项（`cmd/server/main.go`）

| 接口 | 新方法 | 谁实现 |
| --- | --- | --- |
| `schedulerAPI` | `ResizeWorkers(n int) error`、`SetRetryPolicy(core.RetryPolicy)` | `*core.Scheduler`（R03、R02），替身 `spyScheduler`（`cmd/server/main_integration_test.go:1887` 附近）补两个方法并记录调用流水 |
| `eventLogAPI` | `SetRetention(count int, age time.Duration)` | `*sqlite.EventLog`（R02），替身同步 |
| `auditLogAPI` | `SetRetention(count int, age time.Duration)` | `*sqlite.AuditLog`（R02），替身同步 |
| `core.Store` | （R02 已加 `SetHistoryRetention`） | `stubStore` 补一个记录调用的桩 |

`Store` 接口新增的 `SetHistoryRetention` 是 R02 落的，本卡只消费。

### 3.4 `run()` 里的装配

```go
// 位置：server.Start() 成功之后、signal.Notify 之前（cmd/server/main.go:622-629）。
// 三个条件缺一不可：
//   - cfg.Reload.Enabled 为假 → 一个 goroutine 都不建，行为与本系列之前一致；
//   - cfgPath 为空（启动时没读到任何文件，走的是默认值）→ 记一条 warn 并跳过（设计文档 §5.2）；
//   - watcher 构造失败 → 记 error 后继续提供服务，**不阻止启动**：
//     热重载是便利，不是承重结构；因它起不来而让进程拒启，等于把新能力变成新故障面。
if cfg.Reload.Enabled {
        watcher, err := deps.newConfigWatcher(cfgPath, cfg.Reload.Debounce, reloader.Reload, deps.logger)
        ...
        go watcher.Run(ctx)
        defer watcher.Close()  // 注意：见下面"关闭顺序"
}
```

**关闭顺序**：`watcher.Close()` 必须在 `server.Stop(ctx)` 与 `scheduler.Stop()` 之前完成
（设计文档 §7.8）。落地形态不是 `defer`（`run()` 里的 defer 会晚于它们执行），
而是在收到信号之后、`server.Stop` 之前显式调用；`reloader` 的链可能正在执行，
`Close` 会等在途调用返回（R05 §3 已定这条）。这条顺序的用例判据：
关闭日志里"watcher closed"这一条的时间戳早于"shutting down server"
（用现成的 `logger` 属性 + 替身 watcher 记录调用流水，不靠读时钟）。

`newConfigWatcher` 作为 `runtimeDeps` 的一个函数值字段（默认实现就是 `core.NewConfigWatcher`），
这样集成测试能塞一个假 watcher 进去驱动整条链，而不必真的等文件系统事件。

### 3.5 `api`：读口

```go
// api/reload_state.go（新增，独立文件而不是塞进 handlers_admin.go：
// 它是"可选依赖 + 一个转换函数"两件事，与 handlers_admin.go 里那五个端点不同类）

// ReloadStateReader 提供最近一次配置重载的结论。cmd/server 在装配时把它接进服务。
type ReloadStateReader interface{ State() core.ReloadState }

// WithReloadState 注入读口。未注入时 RuntimeResponse.Reload 整个字段缺省。
func WithReloadState(r ReloadStateReader) Option
```

- `RuntimeResponse` 加 `Reload *core.ReloadState \`json:"reload,omitempty"\``
  （`api/handlers_admin.go:17-28`），指针 + `omitempty` 才是"没启用就整个键都不给"，
  值类型会给出一个 `result:""` 的空对象——那会被读成"启用过但从没重载"。
- `GetRuntime` 里只在读口非 nil 时填这个字段。
- 路由与档位**不变**：`/admin/runtime` 已经是 ops 档（`api/server.go:325-327`），
  本卡不新增端点、不加 `auditActions` 行（README 的共同口径里那条"涉及端点要同步审计映射"
  在本系列只在"证明它不需要"上生效，见 §5.5）。
- **不外泄取值**：`AppliedKeys`/`IgnoredKeys`/`RejectedKeys` 只有键名；
  `Error` 文本来自 `LoadConfig`/`Validate`/`Diff` 的错误串，其中可能含文件路径但不能含取值。
  R01 的 `Diff` 已经把取值挡在 `ChangedKey.Val` 里、不进状态对象，本卡加一条用例守住
  （§5.4 第 3 条）。

## 4. 实现步骤

1. `cmd/server/main.go`：`runtimeDeps` 加 `newConfigWatcher` 字段与 `reloadTargets` 的构造；
   窄接口扩项（§3.3）；`Applier` 变量提到 `if` 外（R04 §4.6 的构造条件已改）；
   `reloader` 的创建放在依赖装配齐了之后、`scheduler.Start()` 之前（它要引用 store、scheduler、
   两个写入器、applier、levelVar、watcher）。
2. `cmd/server/reload.go`：`reloader` + `Reload` 七步 + `undo` 栈 + `State`。
3. `cmd/server/main.go` 的 `main()`：`core.NewLoggerWithLevelVar` 替换 `core.NewLogger`
   （`cmd/server/main.go:324`），levelVar 经参数传给 `defaultRuntimeDeps`。
4. `api/reload_state.go` + `api/handlers_admin.go` + `api/server.go` 的 Option；
   `run()` 里 `deps.newServer` 把它注入（沿用 `opts` 追加的既有形状，`cmd/server/main.go:276-304`）。
5. `cmd/server/main_integration_test.go`：替身补方法 + §5 的用例；
   `api/handlers_admin_test.go`：读口两条。
6. 跑 §7；再跑全量。

## 5. 测试要求

### 5.1 `reloader.Reload` 的单元用例（`cmd/server/reload_test.go`，替身全假、不起进程）

替身 `fakeTargets` 记录每个落点的调用次序，并可预设某一步失败。

1. `TestReload_AppliesEveryHotKey`：一次改动同时含 `logging.level`、`scheduler.workers`、
   `scheduler.max_retry_delay`、`store.history_limit`、`executors.commands` →
   断言调用次序严格等于 §3.2 的 `1,2,3,4,6,7`（`reload.debounce` 的 #2 也在这条里，
   它的期望是 `setDebounce` 被以新值调用一次），结论 `ok`、`AppliedKeys` 含这五个键。
2. `TestReload_RollbacksInReverseOnFailure`：把 `setWorkers` 预设成失败 →
   断言 `setLevel`/`setDebounce`/`setRetry`/`setRetention` 各被以**旧值**再调一次、
   次序与新值调用相反，`setCommands` 从未被调用，结论 `failed`、
   `Error` 含失败原因，`applied` 与调用前逐字段相等（用 `reflect.DeepEqual`）。
3. `TestReload_DegradedWhenRollbackFails`：`setWorkers` 失败且 `setRetry` 的 undo 也失败 →
   结论 `degraded`、`Error` 同时含两条文本、记一条 error 日志。
4. `TestReload_RejectsCredentialChange`：`server.auth.token` 变 →
   **所有落点函数都未被调用**（这是 I2 最硬的证据）、结论 `rejected`、`RejectedKeys` 含该键。
5. `TestReload_RejectsPermissionFieldInProfile`：`executors.commands[0].script` 变 → 同 #4。
6. `TestReload_BadYAMLKeepsApplied`：`LoadConfig` 失败（临时目录里写一份含未知键的 YAML）→
   结论 `rejected`、`Error` 含 viper/yaml 的原文、`applied` 不变、落点函数零调用。
   再写一份合法文件 → 下一次 `Reload` 成功，证明"修好文件就自动恢复"（设计文档 §8 那条）。
7. `TestReload_UnchangedDoesNotTouchExecutors`：内容与 `applied` 等价 →
   结论 `unchanged`、`setCommands` 未被调用（防止每次存盘都重登记档位）。
8. `TestReload_RestartKeysAreReportedNotApplied`：`scheduler.queue_capacity` 变 →
   结论 `ok`（它不属于热更，但这次改动本身合法）、`IgnoredKeys` 含该键、
   `AppliedKeys` 为空——同时 `HasChanges()` 为真所以没走 `unchanged` 分支。
9. `TestReload_ObservabilityKeysSkippedWhenDisabled`：观测层未启用（两个 retention 函数为 nil）而
   `observability.events.retention_count` 变 → 结论 `ok`、该键进 `IgnoredKeys`、不失败。
10. `TestReload_CommandsWithExecutorsDisabled`：执行器未启用而 `executors.commands` 变 →
    结论 `ok`、`AppliedKeys` 含该键、记一条 warn 说明"未启用，本次不生效"
    （§3.1 第三条特殊处理）。
11. `TestReload_ConcurrentReloadsSerialized`：两个 goroutine 同时 `Reload`，
    替身里记录"进入/离开"配对 → 断言两次的临界区不重叠，且第二次能看到第一次的结果
    （`applied` 已推进，所以第二次的 `Diff` 是 `unchanged`）。`-race` 必须干净。

### 5.2 装配与关闭顺序（`cmd/server/main_integration_test.go`）

12. `TestRun_WatcherNotStartedWhenReloadDisabled`：`reload.enabled=false` →
    `newConfigWatcher` 从未被调用，且既有断言全部不变（"默认关闭"这条口径在本卡的落点）。
13. `TestRun_WatcherNotStartedWithoutConfigFile`：`reload.enabled=true` 但启动时没读到文件
    （`-config` 指不到、`configs/config.yaml` 不存在的那种部署）→ 不起 watcher、
    记一条 warn、进程照常起来。
14. `TestRun_WatcherClosedBeforeServerStop`：替身 watcher 与替身 server 各记录调用流水，
    断言 `watcher.Close` 早于 `server.Stop` 早于 `scheduler.Stop`（§3.4 的关闭顺序）。
15. `TestRun_WatcherFailureDoesNotBlockStartup`：`newConfigWatcher` 返回错误 →
    `run()` 正常进入等待信号阶段，且有一条 error 日志。

### 5.3 端到端一轮（本卡的手工验收，也是 R07 场景表的雏形）

`%TEMP%` 下独立目录（配置 + `data/` + 产物目录），端口挑 IPv4/IPv6 都空闲的，
`reload: {enabled: true, debounce: 200ms}` 起进程，逐条改文件并核对：

| 改的键 | 判据 |
| --- | --- |
| `logging.level: info→debug` | `GET /api/v1/stats`（或任何一条会打 debug 的动作）后 stdout 出现 debug 行；`/admin/runtime` 的 `reload.result=="ok"` 且 `applied_keys` 含该键 |
| `scheduler.workers: 4→12` | `/api/v1/pools` 的 workers 读数变 12（`RuntimeStats.Workers`，R03 §3.4） |
| 写一个含未知键的 YAML | `reload.result=="rejected"`、`error` 有内容、**level 与 workers 仍是上一步的新值**（证明 `applied` 没退） |
| 修好该文件 | 下一个窗口内 `result` 变 `ok` 或 `unchanged`，无需重启 |
| `server.auth.token` 改一个新值 | `result=="rejected"`、`rejected_keys` 含该键、用旧 token 仍能用（最硬的一条：拒绝档确实什么都没动） |

冒烟完删除临时目录；每一步的输出与判据抄进 §10.4。

### 5.4 `api` 的两条

1. `TestGetRuntime_HasNoReloadFieldWhenNotInjected`：不带读口时响应 JSON 里**没有** `reload` 键
   （不是 `"reload":{}`）——判据要读原始 body 或 `map[string]any` 的键集合，
   不能用现成的 `decodeRuntime`（`api/handlers_admin_test.go:20` 附近）：它解码进结构体，
   缺键与空对象在解码后看不出差别。
   既有断言不受影响：那三条用例都是解码后按字段断言，没有整对象 `JSONEq`
   （与 W07 那次必须同步 `JSONEq` 期望值的情形不同，实现记录里如实写"零既有断言改动"）。
2. `TestGetRuntime_ExposesReloadState`：注入一个假读口 → 响应里七个字段齐、`result` 是封闭取值之一。
3. `TestGetRuntime_LeaksNoConfigValues`：假读口里放 `AppliedKeys:["server.auth.token"]` 与
   一句含文件路径的 `Error`，断言响应里既没有旧取值也没有新取值
   （把 canary 值塞进读口的返回值，再 `strings.Contains` 反证）。

### 5.5 不需要新审计行（一条反向用例）

`TestRuntimeReadDoesNotWriteAudit`：启用观测层台账，连续 `GET /api/v1/admin/runtime` 三次，
断言台账里查不到任何 `admin.runtime` 行——本卡没有新增写操作，读端点本来就不进台账
（`docs/design/sqlite-observability-design.md` §6.3 的既有口径）。

## 6. 完成标准（DoD）

- [ ] §3.1 的七步顺序与三条判断全部落地，§5.1 的 11 条用例绿。
- [ ] 应用顺序与 §3.2 那张表逐条一致，且 #1 那条用例断言的是**调用次序**而不是"都调过"。
- [ ] 两条"什么都没动"的证据都在：拒绝档（§5.1 #4/#5 落点函数零调用）与
      坏文件（§5.1 #6）；凭据那条还要在冒烟里用旧 token 反证一次（§5.3 最后一行）。
- [ ] `degraded` 可达且可读（§5.1 #3），不是只在注释里存在的一个字符串。
- [ ] `reload.enabled=false` 时零变化：不起 watcher（§5.2 #12）、`/admin/runtime` 无 `reload` 键
      （§5.4 #1）、全仓 `-race` 与之前同样绿。
- [ ] 关闭顺序：先停 watcher，再关 server，再停调度器（§5.2 #14 的调用流水判据）。
- [ ] `api` 侧只加了一个 Option 与一个字段，路由表与角色档位一字未改（`git diff api/server.go` 里
      `setupRoutes` 无变化），且没有新增 `auditActions` 行（§5.5 反证）。
- [ ] 窄接口四项扩项后，`cmd/server` 的 `reload.go` 里**没有任何类型断言**
      （`grep -n "\.(\*\|assert"  cmd/server/reload.go` 只允许命中测试替身相关行，理想是零命中）。
- [ ] 冒烟表（§5.3）五行全部实测，逐行抄进 §10.4。
- [ ] `go build ./...`、`go vet ./...`、`go test ./... -race -count=1` 全绿；
      `go test ./cmd/server -race -count=5 -timeout 30m` 无 flake；新增文件已 `gofmt -w`。

## 7. 验收方式

```bash
go test ./cmd/server -run 'TestReload' -v
go test ./cmd/server -run 'TestRun_Watcher' -v
go test ./api -run 'TestGetRuntime' -v
go test ./cmd/server -race -count=5 -timeout 30m
go build ./... && go vet ./...
go test ./... -race -count=1
```

预期：第一条列出 §5.1 的 11 条；第二条列出 §5.2 的 4 条；第三条含"没有 `reload` 键"那条。
若 `./api` 整包跑（不带 `-run`）耗时长，本卡只需 `-run` 过滤 + R07 再跑全量。

## 8. 不在本任务范围

- 不改任何 setter 的内部实现（R02/R03/R04 已交付）。
- 不改监听器的触发判据（R05）。
- 不新增任何写端点、不动鉴权、不动 CORS、不改角色档位。
- 不动前端：`web/` 零改动，`/admin/runtime` 的新字段本期只在 REST 与日志里可见
  （设计文档 §10 的 N5）。
- 不做"配置变更台账"入 SQLite（设计文档 §10 的 N6）。
- 不做 `applied` 的跨重启持久化——它只在内存里，重启后由 `LoadConfig` 重新定一次。

## 9. 风险与回滚

| 风险 | 说明 | 处置 |
| --- | --- | --- |
| 回滚链比应用链更长、更容易漏 | 每个落点都要有 undo，漏一个就出现"回滚了一半" | §3.2 明确"应用前记录旧值压栈、失败逆序重放"；§5.1 #2/#3 两条用例分别覆盖有 undo 与 undo 失败 |
| `applied` 与 `deps.config` 漂移 | 接线时若有人继续读 `deps.config`，读的是启动值而不是当前值 | 本卡把 `reloader.applied` 定成唯一权威；`run()` 里既有装配只在启动阶段读配置，不新增运行期读取点。实现记录 §10.2 里逐条列出确认过的读取点 |
| 在途重载与优雅关闭交错 | 关闭期间仍可能有一次 `ApplyConfig` 改处理函数表 | §3.4 的显式 `watcher.Close()` 早于 `server.Stop`；§5.2 #14 断言流水 |
| 页面档位写入与重载同时进来 | 两条链都会动登记表 | R04 已让两条链共用 `Applier.writeMu`；§5.1 #11 只测重载侧串行，跨链交错由 R04 §5.5 覆盖 |
| `/admin/runtime` 整对象断言 | 新增顶层键会让既有 `JSONEq` 红 | §5.4 #1 明说这条，并照 W07 先例同步期望值 |
| `LoadConfig` 的错误文本带出取值 | 状态里的 `Error` 直接来自配置层 | R01 的错误串只带键名与路径；§5.4 #3 的 canary 用例是防线，不许省 |
| 冒烟里"没生效"与"没触发"分不清 | 本系列的被测对象就是配置文件 | §5.3 的判据一律取 `reload.result` 与 `applied_keys`，不看任务跑得快慢（README 的共同口径） |

回滚：本卡的产物是一条链 + 一个可选读口。链没接上时全仓回到 R05 之前的状态，
`api` 的两个新增（Option 与字段）在被删除后即回到旧响应形状。
因此**可以整卡 `git revert`**；R01–R05 的零件留在原地但不被调用，无行为影响。

## 10. 实现记录（执行时补写）

### 10.1 落地的接口与窄接口扩项清单

### 10.2 与本卡写法的差异（含既有断言被改动的逐条说明）

### 10.3 验证证据

### 10.4 端到端冒烟实录（§5.3 五行，逐行给命令与观察到的字段）

### 10.5 缺陷

### 10.6 未覆盖项
