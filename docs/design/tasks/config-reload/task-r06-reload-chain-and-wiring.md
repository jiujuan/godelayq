# TASK-R06　接线：重载链、回滚与 /admin/runtime 的重载状态

- 所属阶段：M2 接线
- 依赖任务：R01（`Diff`/`ReloadState`）、R02（六个 setter）、R03（`ResizeWorkers`）、
  R04（`Applier.ApplyConfig`）、R05（`ConfigWatcher`）
- 涉及文件：`cmd/server/reload.go`（新增）、`cmd/server/reload_test.go`（新增）、
  `cmd/server/main.go`、`cmd/server/main_integration_test.go`（替身补方法）、
  `cmd/server/config_reload_smoke_test.go`（R02 留下的冒烟具，本卡换成生产链，见 §10.2 第 8 条）、
  `cmd/server/main_test.go` 与 `cmd/server/profile_merge_test.go`（`defaultRuntimeDeps` 加参数后的跟进）、
  `api/reload_state.go`（新增）、`api/reload_state_test.go`（新增）、`api/audit_test.go`
  （`auditServer` 加可变参数，见 §10.5c 的 I-4）、`api/handlers_admin.go`、`api/server.go`（一个 Option）。
  卡面原来点的 `api/handlers_admin_test.go` **一字未动**：§5.4 那三条用例要求判"键不存在"必须绕开
  解码进结构体的 `decodeRuntime`，所以读面测试全部落在新增的 `api/reload_state_test.go` 里（复核轮抓到
  这条清单与树不符，见 §10.5c 的 M-8）。
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

// Reload 走完整条链，它就是交给 core.ConfigWatcher 的那个 ReloadFunc。
//
// 串行这件事分两层，别混（R05 已交付的事实）：
//  1. watcher 持自己的 reloadMu 把"调用 + 保存状态"整段串行，所以同一个 watcher 不会并发调进来两次；
//     （调用点是防抖计时器的协程，不是事件循环。）
//  2. r.mu 保护的是**另一件事**：applied 这份权威同时被装配阶段与关停路径读，且链本身要能直接调
//     ——§5.1 #11 就是用两个 goroutine 直接调 Reload 的，链必须自己站得住，不能指望上游替它串行。
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
//     同时把 Restart 档的键名抄进 IgnoredKeys，记一条 info。
//
// reload.debounce 不在第 7 步：它是 §3.2 顺序表里的 #2（早于任何可能失败的动作），落点是 setDebounce，
// 窗口改了下一次事件起用新值；它的 undo 就是把窗口写回 applied 里那份旧值。
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
}
// 关停不用 defer：收到信号之后按下面"关闭顺序"那一段的四步显式调用。
```

**关闭顺序（本卡的一处卡面前提被 R05 的实现证伪，已按实现口径重写）**：
`watcher.Close()` **不等在途的 `ReloadFunc` 调用返回**——这是 R05 的既定口径（卡 R05 §3 的 `Close` 注释
与 `ReloadFunc` 契约第 1 条末尾：那一次调用可能正握着写链的锁，等它只会把关停拖死；R05 实测的变异也
只证到"不再新起"，不证"绝不重叠"）。所以原写的"先 `Close` 就能保证链已收尾"不成立：`Close` 之后仍可能
有一次 `ApplyConfig` 在改处理函数表，而那正是设计文档 §7.8 要避免的交错。

补一个由 `reloader` 自己提供的收口入口，顺序变成四步：

```go
// Stop 等在途那一串走完，并让之后的调用直接返回（不再读配置、不再动任何落点）。
// 形状：先 r.mu.Lock() —— 它就是整条链的串行锁，拿到它等于在途链已经交还；
// 然后置一个 stopped 位点再解锁。此后就算有人直接调 Reload 也只是立刻返回一个空结论，
// 不会有人新起一条链（触发源已被 watcher.Close() 断掉）。
func (r *reloader) Stop()
```

调用次序：`watcher.Close()` → `reloader.Stop()` → `server.Stop(ctx)` → `scheduler.Stop()`；
落地形态不是 `defer`（`run()` 里的 defer 会晚于它们执行），而是在收到信号之后显式按这个次序调。
§5.2 #14 的判据随之改成**替身调用流水**的先后（watcher.Close 早于 reloader.Stop 早于 server.Stop
早于 scheduler.Stop），不读时钟；§5.1 另加一条 **#12**：把某个落点卡在闸门上、另起协程调 `reloader.Stop()`，
闸门放开之前 `Stop` 不得返回（这条判的是"等在途"那一半，与 R05 那条"只等循环"的判据正对着）。

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
12. `TestReload_StopWaitsInFlightChain`：把 `setWorkers` 卡在一道闸门上、另起 goroutine 调 `reloader.Stop()`，
    闸门放开之前 `Stop` 不得返回；放开之后 `Stop` 返回，且随后一次 `Reload` 立即返回空结论、
    落点函数零调用（§3.4 的"等在途 + 拒绝后续"两半）。**注意 defer 次序**：开闸门的 `defer` 必须注册在
    调 `Stop` 那条之后，否则失败路径上会等一个卡在已关闭闸门里的协程、整包撞到超时（R03/R05 各踩过一次）。
11. `TestReload_ConcurrentReloadsSerialized`：两个 goroutine 同时 `Reload`，
    替身里记录"进入/离开"配对 → 断言两次的临界区不重叠，且第二次能看到第一次的结果
    （`applied` 已推进，所以第二次的 `Diff` 是 `unchanged`）。`-race` 必须干净。

### 5.2 装配与关闭顺序（`cmd/server/main_integration_test.go`）

12. `TestRun_WatcherNotStartedWhenReloadDisabled`：`reload.enabled=false` →
    `newConfigWatcher` 从未被调用，且既有断言全部不变（"默认关闭"这条口径在本卡的落点）。
13. `TestRun_WatcherNotStartedWithoutConfigFile`：`reload.enabled=true` 但启动时没读到文件
    （`-config` 指不到、`configs/config.yaml` 不存在的那种部署）→ 不起 watcher、
    记一条 warn、进程照常起来。
14. `TestRun_CloseOrder`：替身 watcher、替身 reloader 与替身 server 各记录调用流水，
    断言 `watcher.Close` 早于 `reloader.Stop` 早于 `server.Stop` 早于 `scheduler.Stop`（§3.4 的关闭顺序）。
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
   一句含文件路径的 `Error`，断响应里有路径、有键名，而**服务自己配置里那三份凭据取值**
   （静态 token、JWT 密钥、账号密码哈希）一处都不出现。
   ~~把 canary 值塞进读口的返回值，再 `strings.Contains` 反证~~——这一句在执行时被证伪并删掉：
   `reloadStatusOf` 是原样透出，塞进读口的取值必然出现在响应里，照字面写只会得到一条
   判"透出成功"的假绿。取值不许进状态的闸门在产出方（`core.ChangedKey` 只带 `Path`），
   这条用例守的是"api 这层不主动把配置值搬进读数"。缺陷表 D-R0606 记的就是这个卡面前提。
4. 两条自由文本（`error` / `watcher_error`）要有**正向**判据（复核轮补）：注入的文本必须逐字出现在
   响应里。没有这一条，"把 `Error: state.Error` 那一行删掉"这种改动能全绿通过，
   而运维再也看不到失败原因——那正是本系列要破的静默（I3）。

### 5.5 不需要新审计行（一条反向用例）

`TestRuntimeReadDoesNotWriteAudit`：启用观测层台账，连续 `GET /api/v1/admin/runtime` 三次，
断言台账里查不到任何 `admin.runtime` 行——本卡没有新增写操作，读端点本来就不进台账
（`docs/design/sqlite-observability-design.md` §6.3 的既有口径）。

## 6. 完成标准（DoD）

- [x] §3.1 的七步顺序与三条判断全部落地，§5.1 的 12 条用例绿（实到 16 条，含三条附加守卫与一条上界边界）。
- [x] 应用顺序与 §3.2 那张表逐条一致，且 #1 那条用例断言的是**调用次序**而不是"都调过"
      （`assertExactCalls` 比的是整条流水的逐字相等）。
- [x] 两条"什么都没动"的证据都在：拒绝档（§5.1 #4/#5 落点函数零调用）与
      坏文件（§5.1 #6）；凭据那条还要在冒烟里用旧 token 反证一次（§5.3 最后一行：旧 200、新 401）。
- [x] `degraded` 可达且可读（§5.1 #3），不是只在注释里存在的一个字符串。
- [x] `reload.enabled=false` 时零变化：不起 watcher（§5.2 #12）、`/admin/runtime` 无 `reload` 键
      （§5.4 #1）、全仓 `-race` 与之前同样绿。
- [x] 关闭顺序：先停 watcher，再收口重载链，再关 server，再停调度器（§5.2 #14 的调用流水判据），
      且"链正卡在某个落点上时 `reloader.Stop()` 必须等它"有独立用例（§5.1 #12）。
      真实进程侧那一轮没实测，原因与替代判据见 §10.6 第一条。
- [x] `api` 侧只加了一个 Option 与一个字段，路由表与角色档位一字未改（`git diff api/server.go` 里
      `setupRoutes` 无变化），且没有新增 `auditActions` 行（§5.5 反证）。
- [x] 窄接口四项扩项后，`cmd/server` 的 `reload.go` 里**没有任何类型断言**
      （`grep -n "\.(\*\|assert" cmd/server/reload.go` 零命中）。
- [x] 冒烟表（§5.3）五行全部实测，逐行抄进 §10.4。第一行的"stdout 出现 debug 行"在默认功能面下
      无站点可产出，已就地换成"级别拧到 error 后 INFO 行不再出现"的可判形状（D-R0602）。
- [x] `go build ./...`、`go vet ./...`、`go test ./... -race -count=1` 全绿；
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

预期：第一条列出 §5.1 的 12 条；第二条列出 §5.2 的 4 条；第三条含"没有 `reload` 键"那条。
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

**新增的类型与函数**（`cmd/server/reload.go`，除注明外都是包内私有）：

| 名称 | 形状 | 谁用它 |
| --- | --- | --- |
| `reloadTargets` | 八个函数值（全部带 `error` 返回，见 §10.2 第 1 条） | `reloader` 唯一的下游视图 |
| `reloadStep` / `buildPlan` | 一步 = 标签 + 认领的键路径（精确或前缀）+ `run` + `available` + `unavailable` | `applyChange` |
| `reloadChain` | `Reload() (core.ReloadState, error)` / `Stop()` / `bindDebounce(configWatcherAPI)` | `run()` 与 §5.2 的替身 |
| `reloader` | `mu` + `applied` + `processEnabled` + `cfgPath` + `targets` + `logger` + `stopped` | `newReloader(reloadDeps)` |
| `reloadDeps` | `cfgPath` `applied` `store` `scheduler` `events` `audit` `applier` `executorsEnabled` `levelVar` `logger` | 装配方（`run()`） |
| `configWatcherAPI` | `Run` `Close` `SetDebounce` `State` `MarkWatcherError` | 链的 #2 落点 + 读口 + §5.2 替身 |
| `reloadStatusReader` | `attach` / `explain` / `State`，满足 `api.ReloadStateReader` | `api.WithReloadState` |
| `maxHotReloadWorkers = 4096` + `checkWorkerBound` | 链的 #6 步内联调用 | R03 交接的 D3 |
| 十一条键路径常量 | `keyLoggingLevel` … `keyReloadDebounce` | 分派表 + `TestReloadEveryHotKeyHasDispatchEntry` |

**`api` 侧新增**：`ReloadStateReader`（接口）、`ReloadStatus`（十字段，两个时间是字符串）、
`WithReloadState(r ReloadStateReader, enabled bool) Option`、`reloadStatusOf`、`formatReloadTime`；
`Server` 多两个字段（`reloadState` / `reloadEnabled`），`RuntimeResponse` 多一个指针字段。
`setupRoutes` 一字未改（DoD 倒数第三条按 `git diff` 核过）。

**窄接口扩项（§3.3 那张表的落地形态）**：

| 接口 | 新方法 | 真实实现 | 替身 |
| --- | --- | --- | --- |
| `schedulerAPI` | `ResizeWorkers(n int) error`、`SetRetryPolicy(p core.RetryPolicy)` | `*core.Scheduler`（R03、R02） | `spyScheduler` 两个方法都记进调用流水，另给 `concurrencyNow()` / `retryMaxDelay()` 两个读数口 |
| `eventLogAPI` | `SetRetention(count int, age time.Duration)` | `*sqlite.EventLog` | `eventLogStub` + `setRetentionCalls()` |
| `auditLogAPI` | `SetRetention(count int, age time.Duration)` | `*sqlite.AuditLog` | `auditLogStub` + `setRetentionCalls()` |
| `core.Store` | 消费 R02 的 `SetHistoryRetention` | `*core.JSONFileStore` | `stubStore` + `retentionPairs()` |

`runtimeDeps` 新增四个字段：`configPath`、`levelVar`、`newConfigWatcher`、`newReloadChain`、
`reloadStatus`（五个，前两个是链的输入，后三个是装配点）；`defaultRuntimeDeps` 的签名随之
加 `configPath` 与 `levelVar` 两个参数（既有四处调用点同步，见 §10.2 第 9 条）。

### 10.2 与本卡写法的差异（含既有断言被改动的逐条说明）

1. **`reloadTargets` 里五只"不返回错误"的落点统一带 `error`**（卡 §3.2 给的是
   `setRetry func(time.Duration)` 等五只无返回值）。不加它就做不到 §5.1 #3 那条
   "`setRetry` 的 undo 失败 ⇒ `degraded`"，而 `degraded` 是 DoD 第四条明令要"可达且可读"的。
   真实实现里这五只恒返回 `nil`，所以行为与卡面一致，只是形状统一。
2. **`WithReloadState` 多一个 `enabled` 参数**（卡 §3.5 只有读口一位）。D-R0503 要的就是这件事：
   `enabled` 只能来自进程配置，不能取自 `State()`。
3. **`RuntimeResponse.Reload` 的类型是本地 `*ReloadStatus` 而不是 `*core.ReloadState`**
   （卡 §3.5 写的是后者）。理由与设计文档 §5.4 的意图一致：`time.Time` 上的 `omitempty` 不生效，
   直接序列化会把零值时间暴露成 `0001-01-01T00:00:00Z`（R05 交给本卡的 D-R0502）。
   十个字段一一对应，只有两个时间换成 RFC3339Nano 字符串并且零值整个键不给。
4. **"读文件失败"的结论是 `rejected`**（与卡 §3.1 第 1 步、§5.1 #6 一致）。
   R02 那版冒烟具当时记的是 `failed` 加一条 `<配置文件读失败>` 占位键——那份形状在 `ReloadState`
   里根本没有对应的字段（没有 `failed_keys`），随 miniature 链一起删掉了。
5. **执行器未启用时不接档位落点**（卡 §3.1 第三条写的是"`ApplyConfig` 会返回错误，本卡特殊处理"）。
   实现把这条判定提到建链时：`newReloader` 只在 `applier != nil && executorsEnabled` 时才装 `setCommands`，
   于是"未启用"是一条计划期事实（`unavailableNotInEffect`），运行期不会真去调 `ApplyConfig`。
   结论、键清单（键留在 `applied_keys`）与那条 warn 文案都和卡面一致；实现者当时按卡面留下的
   `errTargetDisabled` 哨兵与 `errors.Is` 分支因此恒不可达，已删（缺陷表外的清理）。
6. **`ReloadState.Enabled` 钉在建链时那一份**（卡 §3.1 只说"取进程配置"）。
   第 7 步 `applied = candidate` 会把文件里那份新的 `reload.enabled` 一并带进权威（那正是"重启档
   不重复报一遍"要的形状，§5.1 #8 也按它断），所以链不能再从 `applied` 读这一位，
   否则一次改过 `reload.enabled` 的重载之后读数会翻成假。实现为 `reloader.processEnabled`，
   并在 §5.1 #8 里加了"连着走两次、第二次仍说 true"的断言。api 的读数不受影响（第 2 条）。
7. **`resolvedConfigPath` 在 `-config` 留空时试 `config.yaml` 与 `config.yml`**。
   卡面没提这件事，但 `core.LoadConfig` 走的是 viper 的名字查找（两种拼写都认），
   只试 `.yaml` 会让"用 config.yml 启动的部署"交回空串——热重载就此不接（有一条 warn，但那是"该接的没接"）。
   viper 实际还支持 `.json`/`.toml`/…，那几种本仓库的文档与示例从不承诺，登记不修（D-R0603）。
8. **冒烟具换成生产链**（README 的 R02 交接要求："接线后把 `applyCandidate` 换成生产重载链，场景清单留下"）。
   `cmd/server/config_reload_smoke_test.go` 里 `reloadOutcome` / `reloadFromFile` / `applyCandidate` /
   `hotEntries` / `applyHotKey` / `smokeConfig` 六个整体删除，换成 `newSmokeStack` 用 `newReloader`
   接真 store / 真调度器 / 真 SQLite 两张表。八条用例的去向：六条原样保留（主冒烟八键、拒绝档、
   重启档、无变化、坏文件、环境变量），两条换形状——`TestSmokeHotKeyWithoutEntryFailsLoud` 的
   "表上没入口"在生产链上已经不是可表达的状态（八个落点由 `reloadDeps` 一次给定），它的场景由
   §5.1 #13 `TestReload_MissingEntryFailsLoudly` 与 #17 的静态守卫承担；
   `TestSmokeEntryReturningErrorKeepsAppliedBack` 升级为
   `TestSmokeOverBoundWorkersRollBackEveryRealTarget`（R02 当时写明"半应用与回滚只能由 R06 用
   排在后面、又会失败的入口来断"，这条就是那条断言，且四个真下游逐项核对回滚）。
   现共 7 条 `TestSmoke*`。
9. **既有断言被改动的逐条**（全部是签名跟进，不改变语义）：
   `cmd/server/main_test.go` 两处、`cmd/server/profile_merge_test.go` 三处
   `defaultRuntimeDeps(cfg, logger)` → `defaultRuntimeDeps(cfg, "", nil, logger)`；
   `fileSpec` 加一个 `port` 字段（起点仍是 `"18080"`，`render` 的输出对既有用例逐字节不变）；
   `api/reload_state_test.go` 的 canary 用例改走"配置账号 + 登录拿 JWT"的通道
   （`/admin/runtime` 是 ops 档，静态 token 是 machine 档，直接请求会得到 401/403 而不是 200）。
   除这些之外 `api` 既有的 `handlers_admin_test.go` 三条 `decodeRuntime` 用例一字未动，
   `JSONEq` 类整对象断言这一包没有（DoD 第 5 条要求的"零既有断言改动"照 W07 先例如实记录为上一条）。

### 10.3 验证证据

命令都在终态字节上跑；全量验证期间没有任何源文件被并行改动（R05 的 D-R0515 口径）。

**§7 第一条** `go test ./cmd/server -run 'TestReload' -v -count=1` → 17 条 `--- PASS`、`ok 0.582s`：
#1–#11 与 #12（`Stop`）全在，另加 #13 `MissingEntryFailsLoudly`、#14 `NoConfigPathIsNotSilence`、
#15 `WorkerBoundBoundaryIsExact`（三个子档 4095/4096/4097）、#16 `TestReloadEveryHotKeyHasDispatchEntry`、
#17 `TestReload_AbsurdWorkerCountFailsAndRollsBack`。
（卡 §5.1 编号里 #12 与 #11 印反了顺序，且列了 11 项却写"12 条"，实际按功能分是 12 条 + 5 条附加。）

**§7 第二条** 卡面写的是 `go test ./cmd/server -run 'TestRun_Watcher' -v`，那条模式只匹配到三条
（`WatcherNotStartedWhenReloadDisabled` / `WatcherNotStartedWithoutConfigFile` /
`WatcherFailureDoesNotBlockStartup`），§5.2 的另外三条名字里没有 `Watcher`。按 §5.2 的清单改成
`-run 'TestRun_Watcher|TestRun_CloseOrder|TestRun_Wired|TestRun_ReloadReader'` 重跑：六条全 `--- PASS`、
`ok 0.266s`（多出的是 `TestRun_CloseOrder`、`TestRun_WiredReloadChainReachesEveryTarget`、
`TestRun_ReloadReaderFollowsTheWatcher`）。这是 §7 那条命令与 §5.2 编号对不齐，不是漏跑。

**§7 第三条** `go test ./api -run 'TestGetRuntime|TestRuntimeReadDoesNotWriteAudit' -v`：五条全 `--- PASS`、
`ok 0.444s`（§5.4 三条 + §5.5 一条 + `ReloadStateWithoutAnyAttempt`）。

**冒烟（测试内）** `go test ./cmd/server -run TestSmoke -count=1` → 七条全 `--- PASS`，`ok 1.149s`。

**§7 第四条** `go test ./cmd/server -race -count=5 -timeout 30m`：

```
ok  	godelayq/cmd/server	31.008s
```

**§7 第五条** `go build ./... && go vet ./...` 两条命令均无输出（BUILD_VET_CLEAN）。

**§7 第六条** `go test ./... -race -count=1 -timeout 30m`（复核与复复核的全部改动之后整轮重跑）：

```
ok  	godelayq/api          114.907s
ok  	godelayq/cmd/server     7.185s
ok  	godelayq/core          20.562s
ok  	godelayq/executor      22.248s
ok  	godelayq/store/sqlite   4.006s
（cmd/gensecret、cmd/hashpassword、examples/demo1、examples/demo2、web 无测试文件）
```

`api` 那一格的读数本卡内同一条命令四次分别是 149.019s / 147.181s / 103.878s / 114.907s——包里的 SQLite 与
超时用例跟本机负载强相关，这张表只能当"本次真实输出"读、不能当基线；判据是五格全 `ok`
（无 `--- FAIL`、无 data race 报告），每一次都成立。

`gofmt -l` 只报 `core/` 那五个既有文件（本仓 CRLF 的已知误报，见项目记忆"验证口径"），本卡新增与改动的文件都不在其中。

**`reload.go` 里没有类型断言**：`grep -n '\.(\*\|assert' cmd/server/reload.go` 零命中（DoD 倒数第二条）。

**变异反验证（四十个变异体，只改 `cmd/server/reload.go`、`cmd/server/main.go`、
`api/reload_state.go`、`api/handlers_admin.go`，每个跑完立刻从字节备份还原并核对 sha256）**：

| 编号 | 改哪里 | 结论 | 判红的用例 |
| --- | --- | --- | --- |
| R01 | 拒绝档不早退 | RED | `TestSmokeRejectedKeyAbortsWholeReload`（先红的那条；单元侧 `TestReload_RejectsCredentialChange` 同红） |
| R02 | 永远走 unchanged 分支 | RED | `TestSmokeHotReloadAppliesEveryHotKey` |
| R03 | 成功后不推进 applied | RED | `TestSmokeUnchangedConfigIsNotAReload`（单元侧 `ConcurrentReloadsSerialized` 同红） |
| R04 | 失败时不回滚 | RED | `TestSmokeOverBoundWorkersRollBackEveryRealTarget` |
| R05 | 回滚按正序 | RED | `TestReload_RollbacksInReverseOnFailure` |
| R06 | 失败那一步也被回滚（多撤一次） | RED | `TestReload_RollbacksInReverseOnFailure` |
| R07 | 分派表漏项被静默接受 | RED | `TestReloadEveryHotKeyHasDispatchEntry` |
| R08 | 应用顺序整个倒过来 | RED | `TestReload_AppliesEveryHotKey` |
| R09 | 去掉并发数上界 | RED | `TestReload_AbsurdWorkerCountFailsAndRollsBack` |
| R10 | 观测层未启用判成装配错误 | RED | `TestReload_ObservabilityKeysSkippedWhenDisabled` |
| R11 | 执行器未启用判成装配错误 | RED | `TestReload_CommandsWithExecutorsDisabled` |
| R12 | 档位键按精确名认领 | RED | `TestRun_WiredReloadChainReachesEveryTarget` |
| R13 | 没应用任何键也填 LastAppliedAt | RED | `TestSmokeRestartKeysAreReportedNotApplied` |
| R14 | 被跳过的键也算进 applied_keys | RED | `TestReload_ObservabilityKeysSkippedWhenDisabled` |
| R15 | `Stop` 不置 `stopped` | RED | `TestReload_StopWaitsInFlightChain` |
| R16 | `Stop` 之后仍照常走整条链 | RED | `TestReload_StopWaitsInFlightChain` |
| R17 | 故意引一个不存在的标识符（构建失败对照） | RED-构建失败 | 脚本能分辨"跑不到断言"与"断言判红" |
| S01 | 去上界（冒烟面） | RED | `TestSmokeOverBoundWorkersRollBackEveryRealTarget` |
| S02 | 不回滚（冒烟面） | RED | 同一条 |
| S03 | R14 的另一条命令面（`-run TestSmoke`） | 没判红 | **不是存活变异**：那一次改动里没有会被跳过的键，`hot` 与 `exceptKeys(hot, ignored)` 逐字节相同 ⇒ 等价变异。判红能力由 R14 提供，如实记在此处 |
| S04 | 事件写入器落点缺席 | RED | `TestSmokeHotReloadAppliesEveryHotKey` |
| S05 | 档位未启用被算进 ignored_keys | RED | `TestReload_CommandsWithExecutorsDisabled` |
| M01 | 关闭时不收口链 | RED | `TestRun_WatcherNotStartedWithoutConfigFile`（`stops` 计数） |
| M02 | 第一轮写成"删掉再插回原位" | 没判红 | **等价于没改**（新字节与旧字节同一位置）。第二轮换成 M02b |
| M02b | 链收口挪到 `scheduler.Stop` 之后 | RED | `TestRun_CloseOrder` |
| M03 | 不绑防抖落点 | RED | `TestRun_CloseOrder`（`binds` 计数） |
| M04 | 读口不接 watcher | RED | `TestRun_ReloadReaderFollowsTheWatcher` |
| M05 | `resolvedConfigPath` 只认 `.yaml` | RED | `TestResolvedConfigPath` |
| A01 | 零值时间被格式化出去 | RED | `TestGetRuntime_ReloadStateWithoutAnyAttempt` |
| A02 | `enabled` 取自 `State()` | RED | `TestGetRuntime_ExposesReloadState` |
| A03 | Option 不记 `enabled` | RED | 同一条 |
| A04 | 没注入读口也填 `reload` 对象 | RED | `TestGetRuntime_HasNoReloadFieldWhenNotInjected` |
| B01 | `maxHotReloadWorkers` 改成 12 | RED | `TestReload_WorkerBoundBoundaryIsExact` |
| B02 | 上界判定 off-by-one（`>` 改 `>=`） | RED | 同一条 |
| W01 | 档位落点 `available: t.setCommands != nil` 改成恒假 | RED | `TestRun_WiredReloadChainReachesEveryTarget`（新的超时读数判据） |
| W02 | 档位那一步的 `run` 换成空跑（不调 `ApplyConfig`） | RED | 同一条 |
| C01 | `unmatched` 分支交回的 `ignored_keys` 去掉重启档清单 | RED | `TestReloadEveryHotKeyHasDispatchEntry` 第三条 |
| A05 | `reloadStatusOf` 丢掉 `Error` 那一位 | RED | `TestGetRuntime_ExposesReloadState`（新加的正向判据） |
| A06 | `reloadStatusOf` 丢掉 `WatcherError` 那一位 | RED | 同一条 |
| A07 | 两个时间字段填同一个源头 | RED | 同一条（改成断逐字相等之后才红） |

其中 W01/W02 是复核第一轮之后补的：这两条变异在复核前**判不出来**（当时那条断言只判"键还在表里"，
而它启动注册时就在），补上取值判据之后才红——记在这里是为了说明"复核改了判据要连变异一起重跑"。
A05/A06/A07 同理：`error`、`watcher_error` 与两个时间在复核前**没有任何正向判据**，
把 `reloadStatusOf` 里那三行删掉全包都是绿的（复核抓到的一条 Important，见 §10.5c）。
B01/B02/C01 同理，是补 `TestReload_WorkerBoundBoundaryIsExact` 与两条对称性断言之后新造的判据。
四十条里三十八条判红；两条"没判红"的（S03、M02）在复跑时都确认是**等价变异**而不是漏判，
处置与证据记在上表最后一列。全部跑完后四个被改文件与备份逐字节相同。

### 10.4 端到端冒烟实录（§5.3 五行，逐行给命令与观察到的字段）

隔离目录 `%TEMP%\r06smoke`（`config.yaml` + `data/` 全在其中，端口 `18117` 先核过 IPv4/IPv6 都空闲），
`server.exe` 由 `go build -o … ./cmd/server` 在终态字节上现编。凭据全部是本次自造的假值
（静态 token、JWT 密钥、一个 ops 账号的 bcrypt 哈希），仓库的 `configs/config.yaml` 与 `data/` 一次都没被写。

启动即留下这一行，说明监听器真的在跑：

```
level=INFO msg="config hot reload enabled" path=C:\Users\xing\AppData\Local\Temp\r06smoke\config.yaml debounce=200ms
```

`GET /api/v1/admin/runtime`（ops 档，先 `POST /api/v1/auth/login` 拿 JWT）的起点读数：

```
{"result": "", "applied_keys": null, ..., "watched_path": "…\\config.yaml", "enabled": true,
 "workers": 4, "queue_capacity": 4}
```

即"开关开着、监听器在位、但从没尝试过"（`result` 空串是判据，两个时间键整个不给）。

| # | 改的键 | 命令 | 观察到的读数 | 判据 |
| --- | --- | --- | --- | --- |
| 1 | `logging.level: info→debug` | `sed -i 's/^  level: info$/  level: debug/' config.yaml`；等 1.5s；`python probe.py runtime` | `result="ok"`、`applied_keys=["logging.level"]`；日志 `msg="config reload applied" applied_keys=logging.level` | 达标。**行为侧换了判据**：默认功能面里全仓只有六处 `logger.Debug`（都要执行器/观测层/队列满等前提），任何 HTTP 动作都产不出 debug 行，所以改用"级别真的跟着拧"的反证：再把文件改成 `level: error` → 等 1.2s → 一次 `GET /api/v1/jobs` 之后 `level=INFO` 行数 16→16（不增）；改回 `info` → 同样一次请求 17→18（恢复）。见 §10.6 未覆盖项与 D-R0602 |
| 2 | `scheduler.workers: 4→12` | `sed -i 's/^  workers: 4$/  workers: 12/'` | `result="ok"`、`applied_keys=["scheduler.workers"]`、`scheduler.workers` 读数 **12**、`queue_capacity` 仍是 4 | 达标（R03 §3.4 的口径：并发读数变，队列通道不换） |
| 3 | 写一份含未知键的 YAML | 覆盖成 `logging: {level: info, level_not_a_key: true}` | `result="rejected"`、`error="重新读取配置文件失败，本次重载作废、现网继续按当前生效的取值运行（…config.yaml）：parse config failed: … 'logging' has invalid keys: level_not_a_key"`、`workers` 仍是 **12**、INFO 行仍照常写（说明 `logging.level` 仍是上一步的 `info`） | 达标：`applied` 没退，两条新值都还在现网 |
| 4 | 把该文件修好 | `cp config.good.yaml config.yaml` | `result="unchanged"`、`error=""`、`applied_keys=null`；日志 `msg="config reload found no change" note="取值以配置文件与环境变量的合并结果为准…"` | 达标（卡面接受 `ok` 或 `unchanged`；这里是 `unchanged`，因为坏文件从没推进过权威，修好就等于回到现网取值） |
| 5 | `server.auth.token` 改一个新值 | `sed -i 's/OLD-static-token-8f2b/NEW-static-token-5c9d/'` | `result="rejected"`、`rejected_keys=["server.auth.token"]`、`error="改动触碰了拒绝档（凭据与执行许可字段），整次作废、一项都没有应用：server.auth.token；这类改动只能改完文件再重启进程"`；`GET /api/v1/jobs` 带**旧** token → HTTP 200，带**新** token → HTTP 401 | 达标，也是最硬的一条：拒绝档确实什么都没动 |

整轮之后 `server.log` 里与热重载相关的行共八条（一次 enabled、三条 applied、两条 rejected 各配一条
`config reload returned error`、一条 found no change、一条拒绝档的 error），逐条已在 #7 的原文里。
临时目录在收口后删除；进程用 `taskkill /F` 收（Windows 侧无法把 SIGTERM 投给这种起法的控制台进程，
四步关闭顺序的判据在 §5.2 #14 的替身流水上，见 §10.6）。

**表外补的第六行（默认关闭那一档，DoD 第五条要求的真实进程证据）**：把同一份文件改成
`reload: {enabled: false}` 再起一次进程（其余一字未动），核对"接线之后不开关时行为零变化"：

```
grep -ci "reload" off.log      → 0
grep -ci "hot reload" off.log  → 0
GET /api/v1/admin/runtime      → 整份响应里没有 "reload" 这个键
                                 （读数取的是解出来的 map：reload 的十个字段全是 null/缺省，
                                  而 scheduler.workers=12 等其余字段照旧在位）
```

即：`api.WithReloadState` 没被调用（注入条件是 `cfg.Reload.Enabled`，见 `main.go` 的 `newServer` 闭包），
链与监听器一个都没建，`/admin/runtime` 的响应形状回到本系列之前。

**表外补的第七行（`reload.debounce` 在真实进程里改到了真实监听器）**：这一条把 R05 与 R06 接起来，
`§5.2` 的 #6 只能在替身 watcher 上判"值递过去了"，这里判的是"窗口真的变长了"。
起进程（`debounce: 8s`，起点 `logging.level: info`），存一次盘把级别改成 `warn`，随后每秒读一次
`/api/v1/admin/runtime`：

```
write at 17:12:03
+1s..+6s  result=""（还没应用）
+7s       result="ok"  applied_keys=["logging.level"]     ← 探针每次含一次 bcrypt 登录，
+8s..+11s 同上                                              真实间隔比标称秒数长，量到的是 ~8s 窗口
```

对照起点那份配置的 `debounce: 200ms`（前五行都是一存盘就在一秒内读到 `ok`）。
同一轮里还顺带读到 §3.2 把 `logging.level` 排在 #1 的那条理由在真实进程里成立：
级别换到 `warn` 之后，链自己那条 `msg="config reload applied"` 的 INFO 行不再出现在 stdout
（整份日志里与 reload 相关的只剩启动时那一条 INFO）——也就是**这一次重载自己的日志跟着新级别走了**。

### 10.5 缺陷

| 编号 | 严重度 | 现象 | 处置 |
| --- | --- | --- | --- |
| D-R0601 | Minor | 一次失败的重载在日志里留下**两条** `level=ERROR`：链自己按失败原因记一条（带 `hint`、步骤号），R05 的 `storeState` 又把 `ReloadFunc` 返回的 error 记一条（带 `path`）。按 error 行计数告警的部署会重复计数 | **登记不修**，归 R07 的文档同步：设计文档 §5.4/§8 的运维口径里写清"一次失败两行，二者归因不同（步骤级 / 进程级）"，要按事件计数请按 `path` + 时间窗去重 |
| D-R0602 | Minor | 卡 §5.3 第 1 行的判据"stdout 出现 debug 行"在默认功能面下**无法达成**：全仓六处 `logger.Debug` 分别要执行器池满、观测层启用且映射缺项、启动期装载等前提，普通 HTTP 动作一条都触发不了 | **已修**（就地把判据换成"级别拧到 error 后 INFO 不再出现、拧回后恢复"，实测见 §10.4 第 1 行），并把卡面那句话改正为可判的形状 |
| D-R0603 | Minor | `resolvedConfigPath` 在 `-config` 留空时只补 `config.yaml` / `config.yml` 两种拼写，而 `core.LoadConfig` 走 viper 的名字查找，理论上还认 `.json`/`.toml`/`.hcl`/… 十几种 | **登记不修**，归 R07：本仓的文档、示例与 `.gitignore` 只承诺 `configs/config.{yaml,yml}`（`configs/config.example.yaml` 是唯一样本），要为其余扩展名对齐得改 `LoadConfig` 的返回形状（交回"实际用了哪个文件"），那不是本卡的范围 |
| D-R0604 | Minor | 建链时 `applier == nil && executorsEnabled == true` 会被算成"本节未启用"（键留在 `applied_keys` + 一条 not-in-effect 的 warn），说的却是另一件事。真实装配到不了这一格（`main.go` 的构造条件在 `reload && executors.enabled` 时必建 Applier），只有手搓 `reloadDeps` 的测试能造出来 | **登记不修**，归 R07：要在 `newReloader` 里把它降级成"装配缺入口"需要先回答"Applier 建不出来时 executors 那一节算什么"，与本卡的三档结论冲突 |
| D-R0605 | Minor | 执行器未启用时那次档位改动留在 `applied_keys` 里，而 `LastAppliedAt` 也被填上——设计文档 §5.4 把这份清单写成"本次真的换上现网的键"，单看读数会以为档位换了（现场只有一条 warn 说没生效） | **登记不修**，归 R07 文档同步：要么把 §5.4 那句改成"已被本次重载处理、未生效的另有说明"，要么把这条键挪进 `ignored_keys` 并同时改卡 §3.1 第三条。本卡按卡面落地，不自己改口径 |
| D-R0606 | Important（卡面缺陷） | §5.4 第 3 条要的"把 canary 值塞进读口的返回值，再 `strings.Contains` 反证"在这条路上不成立：`reloadStatusOf` 是原样透出，塞进读口的取值**必然**出现在响应里，那是正确行为而不是泄漏。照字面写出来的用例会得到一个只会判"透出成功"的假绿 | **卡面已就地改正**（见 §5.4 第 3 条那条的改写），并把这个前提写进用例自己的注释。真正能守的是"api 这层不主动搬配置值"+ 取值不进状态的闸门在 `core`（R01 的 `ChangedKey` 只带 `Path`） |
| D-R0607 | Minor | 设计文档 §5.4 承诺 `error` 文本"不能含取值"，而 `core` 的若干校验错误串会回显**非凭据**取值（`reload.debounce ... got %v`、`duplicate name %q`、`executors.workspace %q`…），`watcher_error` 同样是调用方给的自由文本 | **登记不修**，归 R07 文档同步：把 §5.4 那句话收窄成"不含凭据取值"（凭据那一侧有更硬的机制：`core` 报带密字段只报空/长度，mapstructure 会洗掉解码错误里的取值），或者逐条整改产出方的文案。本卡不在这个范围里单方面改 |

### 10.5b 复核轮（第一轮，`cmd/server` 侧）

这一轮的复核没有抓到 Blocker/Important 级的正确性缺陷（三条不变量、关闭顺序、
上界位置、分派守卫的非空转都是读代码核过的），六条 Minor 的处置逐条如下：

| 复核发现 | 处置 |
| --- | --- |
| #1 应用循环里 `case unavailableFails:` 是死分支（前置检查第二条已经整次拦在前面） | **已修**：删掉那一支与随之永假的 `if failure != nil { break }`，换成一句"这里只可能是两种未启用脸色"的注释。留着的害处不是多四行，而是让人以为运行期还有第二条退路 |
| #2 档位那一步（#7）排最后，它的 undo 在生产顺序里永远弹不到 | **登记**，进 §10.6：卡 §3.2 说的"逆序重放安全"在档位这一项上只有 R04 自己的可重入用例背书，链上没有用例走到 |
| #3 执行器未启用时 `executors.commands` 留在 `applied_keys` 且 `LastAppliedAt` 被填，与设计 §5.4 那句"真的换上现网的键"读起来不一致 | **登记不修**，归 R07 文档同步（D-R0605）：这是卡 §3.1 第三条明令的形状（结论 ok + 键留在 applied + 一条 warn），要改的是设计文档那句话还是这张表，得一起定 |
| #4 `unmatched` 那条前置检查交回的 `ignored_keys` 少了重启档清单，与姊妹分支（缺入口）不对称 | **已修**：补成 `mergeKeys(changedPaths(change.Restart))`，并给 `TestReload_MissingEntryFailsLoudly` 与 `TestReloadEveryHotKeyHasDispatchEntry` 第三条各补一条断言；变异反验证里把这一处改回去确实判红（见 §10.3 的 C01） |
| #5 `TestRun_WiredReloadChainReachesEveryTarget` 的档位断言只判"exec.echo 还在表里"，而它启动注册时就在 | **已修**：改判**换进去的那一份取值**（起点 10s → 换完 3m），并在起点先核一次 10s 作对照。两个新变异体（`available: false` / `run` 空跑）改前都过、改后都红，见 §10.3 的 W01/W02 |
| #6 D-R0601 那条双 ERROR 行 | 复核独立发现，与本卡自己登记的同一条，不重复计 |

（`api` 侧那轮的发现与处置记在 §10.5c。）

### 10.5c 复核轮（第二轮，`api` 侧）

这一轮抓到的是**判据空转**那一类，四条 Important 全部落地。它同时核过并确认成立的前提也记在这里
（都是读代码/读标准库核的，不是推断）：`encoding/json` 的 `isEmptyValue` 没有 `reflect.Struct` 分支，
所以 D-R0502 那句"`time.Time` 上的 omitempty 不生效"在本工具链上成立；
`setupRoutes` 与 `api/audit.go` 的 diff 为零（两个文件各只有新增行）；
非测试源文件里没有新增任何写端点；凭据取值进不了响应这条有更硬的理由——
`core/config.go` 报带密字段只报"空/长度"、`executor/profile.go` 回显的是 env 的**键**、
而 mapstructure v2.4.0 会把 `ParseError`/`UnconvertibleTypeError`/strconv 错误里的取值洗掉。

| 复核发现 | 严重度 | 处置 |
| --- | --- | --- |
| I-1 `error` 与 `watcher_error` 在五条用例里**一次都没有正向断言**：把 `reloadStatusOf` 里那两行删掉，`api` 全包仍绿，而运维看不到失败原因（I3 在这层静默破掉） | Important | **已修**：`TestGetRuntime_ExposesReloadState` 改成十个字段全给非空取值、十个键必须全出现、两条文本按相等断；`TestGetRuntime_ReloadStateWithoutAnyAttempt` 补 `watcher_error` 的相等断（那一格正是真实进程里最常见的"开关开着、监听器没建"）。新造的 A05/A06 两条变异复跑判红 |
| I-2 卡 §5.4 第 3 条要的"把 canary 值塞进读口的返回值再反证"在这条路上是**反的**：读口给什么这一层就透出什么，塞进去必然出现在响应里。而该用例的前提断言只覆盖了 `watched_path` 一条通道、那段"清单里不许有取值"的循环读的是用例自己写的字符串，任何生产改动都判不出红 | Important | **卡面前提不成立，已就地改写卡面**（§5.4 第 3 条下面的 D-R0606）：这条用例真正能守的是"api 这层不主动把服务自己的配置值搬进读数"，canary 留在 `Security` 里判这一条；取值不进状态的闸门在产出方（`core.ChangedKey` 只带 `Path`）。空转的那段循环删掉前提依赖、改成"三份清单只许是键路径"的形状判据，并在注释里写清证得到/证不到什么 |
| I-3 `handlers_admin.go` 那句"这条路径上唯一的外泄风险是 Error 文本：它来自配置层，那里只带键名与路径"是**假的**：`core` 的校验错误串里有回显非凭据取值的地方（`reload.debounce ... got %v`、`duplicate name %q`、`executors.workspace %q` 等），且 `watcher_error` 是第二条同样敞开的文本通道 | Important | **已修（注释）**：改成陈述这一层真正的保证 + 指明那道闸门在写文本的那一侧。**登记 D-R0607** 归 R07：设计文档 §5.4 那句"不能含取值"要么收窄成"不含凭据取值"，要么逐条整改产出方的错误文案——两者都不是本卡能单方面定的 |
| I-4 `TestRuntimeReadDoesNotWriteAudit` 没注入读口，`GetRuntime` 里那段 `if s.reloadState != nil` 根本不执行，"读一次不写台账"测的是一条与本卡无关的路径 | Important | **已修**：`auditServer` 加可变参数透传给 `newSecurityServer`（既有调用点一字不动），该用例注入一份带 `error` 文本的读数，并先断响应里**确实有** `reload` 对象再断台账为空。它的正向对照（一次写留一行）复核确认为可靠 |
| M-1 注释说"按十个断"而 `wantKeys` 只有八个字面量，封闭判据又另写一份十字面量 | Minor | **已修**：一份包级 `reloadStatusKeys` 两处共用 |
| M-2 两个时间只判"能解析、非零"，把两个字段填反或都填同一源头都过 | Minor | **已修**：改判与 `Format(RFC3339Nano)` 逐字相等，解析判据保留在后面那个循环里 |
| M-3 `Result 是封闭枚举` 这句注释在本层没有实现，用例又是"注入 ok 期望 ok"的同义反复 | Minor | **已修**：用例改成先判"属于那六个取值之一"再判相等；注释改成"取值由产出方限定，这一层原样透出，不做第二份白名单" |
| M-4 `slices.Clone` 的理由写成"序列化之后还有人拿去打日志、比对"——不存在这样的消费者 | Minor | **已修**：改成真实理由（不把产出方的活数组递给编码器，D-R0514），并如实说明这条没有判据支撑（§10.6 那条未覆盖项保留） |
| M-5 `WithReloadState` 的契约漏了两条调用方规则（只在 `reload.enabled=true` 时注入；不许传包了 nil 指针的读口） | Minor | **已修（文档两行）**。代码不加反射式判空——那是给编程错误兜底，与本仓风格冲突 |
| M-6 "与台账 time 列的序列化口径一致"说过头：台账列是整数 `UnixMicro`，线格式是 `time.Time` 默认（微秒），这里是纳秒 | Minor | **已修**：改成"同一种格式家族，精度与存储形状都不同" |
| M-7 一处未检查的 `raw["reload"].(map[string]any)` 断言（键没了会 panic 而不是干净失败）；canary 用例把 helper 已经解开的 body 又解一遍 | Minor | **已修**：两处都换成 `require` 与 helper 的第一返回值 |
| M-8（复复核抓到）卡面"涉及文件"清单与真实改动不符：点名的 `api/handlers_admin_test.go` 一字未动，而实际新增/改动的 `api/reload_state_test.go`、`api/audit_test.go`、`cmd/server/config_reload_smoke_test.go` 三个都不在清单里 | Minor | **已修（卡面）**：清单按真实集合重写，并说明读面测试为什么落在新文件而不是 `handlers_admin_test.go`（§5.4 第 1 条要求绕开 `decodeRuntime`） |
| 工作树卫生：`api/executor_profile_record_test.go` 与 `api/handlers_executor_profiles_test.go` 在 `git status` 里是 M，但 `git diff --numstat` 报 0 行内容差异（只有 CRLF 归一化） | — | 本卡提交时**不入这两个路径**（`--include` 只挑自己改过的文件），并在 §10.6 记一句 |

### 10.5d 复复核轮（第二轮修复之后的独立复核）

两处修复各交一份 fresh-context 复核复判。结论：**§10.5b/§10.5c 的 11 项修复全部 CLOSED**，
三条不变量、导入边界（`go list -deps` 核过 `core` 不依赖 executor/store/sqlite/api，`api` 不依赖 cmd）、
"默认关闭零变化"与 `setupRoutes` 零差异都被重新读代码确认；`go build`/`go vet` 在那份字节上干净。
新留三条 Minor，逐条处置：

| 复复核发现 | 处置 |
| --- | --- |
| 应用循环那个 `switch step.unavailable` 只有两支、没有 `default`：**将来**给 `unavailableOutcome` 加第四种取值时，带着它的步骤会悄悄 `continue`（键留在 `applied_keys` 却没应用），而且不会编译失败 | **已修（注释）**：在那支 switch 前加一条维护提醒。零值这一侧经复核确认是安全的——`unavailableFails` 是 `iota` 的第一个取值，新加的分支若忘了标 `unavailable` 会落到"缺入口"那条前置检查上，是响亮失败而不是静默跳过 |
| 卡面"涉及文件"清单与真实改动不符 | **已修（卡面）**：清单重写，记为 §10.5c 的 M-8 |
| 工作树里八个与本卡无关的脏路径（五个纯 CRLF、三个带真实差异） | **已修（记录）**：§10.6 那条改成逐一列名的版本；本卡提交按具名路径 `git add`，不用 `-A`/`.` |

复复核里"无法在这轮重跑"的部分（四十个变异体的判红结论、§10.4 的真实进程实录、
`resolvedConfigPath` 改前改后的对照）已明确标为读取代码与结构推断，不当作已复现的证据。

### 10.6 未覆盖项

- **真实进程里的四步关闭顺序没有实测**：Windows 下这种起法收不到 SIGTERM，只能强杀。
  判据由两处替代：§5.2 #14 用替身流水钉住"链收口"的位次，R05 的用例钉住真 watcher 的 `Close` 契约。
  Linux 侧能真的送信号，那一轮的等价验证留给 R07 的 22 场景表。
- **`executors.commands` 的热更在冒烟具（测试内那七条）里没走**：真 Applier 要登记表、产物存储与
  脚本文件一整套，那一条由 §5.2 的 `TestRun_WiredReloadChainReachesEveryTarget` 用真 Applier +
  真处理函数表判；冒烟配置文件里 `executors.enabled: false`，且文件头写明为什么不开。
- **`reloadStatusOf` 里那三次 `slices.Clone` 的防御无法判别**：读侧复制的是马上要序列化的那份切片，
  改成直接引用不会产生任何可观察差异（对应 R05 的 D-R0514 那条共享底层数组的口径）。
- **canary 反证的通道限制**：`/admin/runtime` 要 ops 档，静态 token 是 machine 档，所以那条用例里的
  token 取值只作为"配置里存在过的值"参与负向断言，不参与请求头；若将来有人把请求头原样回显进读数，
  这条用例判不出来。
- **`degraded` 只有替身能造**：八个真实落点里没有任何一个在"应用成功之后、用旧值重放时"会失败
  （全是幂等的取值替换）。这是好事，但也意味着真下游上的 `degraded` 无法端到端复现，
  §5.1 #3 就是它的唯一判据。
- **两个"没判红"的变异体**（S03、M02）见 §10.3 表末列的等价性说明。
- **档位那一步（#7）的 undo 在生产顺序里弹不到**：它排在最后，后面没有会失败的步骤，
  所以"逆序重放到 `ApplyConfig` 这一项"在链的用例里从没被执行过（替身用例走到的是 #1–#4 那几只）。
  这条性质的现有背书只有 R04 自己那两条"`ApplyConfig` 可重入且幂等"的用例，卡 §3.2 的推断成立但链上无判据。
- **`api` 台账用例的读口注入靠的是 `auditServer` 新加的可变参数**：那条用例现在确实走到了
  `if s.reloadState != nil` 里面（先断响应里有 `reload` 对象再断台账为空），
  但"读一次不写台账"这件事在结构上由 `api/audit.go` 的 GET 早退还给保证——
  该用例的真实增量是回归网与动作词表那一条循环。
- **工作树里与本卡无关的脏文件一律不入本卡提交**（复复核逐条核过 `git diff --numstat`）：
  内容差异为 0、只是 CRLF 归一化的有 `api/api_test.go`、`api/shutdown_test.go`、`api/sse.go`、
  `api/executor_profile_record_test.go`、`api/handlers_executor_profiles_test.go`；
  带着**真实**差异且属于别处的有 `web/vite.config.ts`（dev 端口 5173→5177）、
  `data/jobs.json`（一条 `data_sync` 任务记录）、`data/groups.json`（未跟踪的新文件）。
  本卡的提交按具名路径逐个 `git add`，不用 `git add -A`/`.`，因此这八个路径都不会进去；
  它们留在工作树里等各自的处置，本卡不动。
- **Linux/macOS 未实跑**（全系列同一条）。
