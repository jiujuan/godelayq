# 配置热重载设计（改文件即生效，不必重启进程）

> 状态：**已实施（TASK-R01…R07 落地，2026-10-03 收口）**。评审拍板的四条口径标 ★，见 §2；
> 实施后的落地位置在 §2 末尾，实现与本文明确不同的地方集中在 §14（含 22 个真实进程场景的实测结论）。
> 冲突处理：本文与 `web-profile-design.md` 在"档位怎么生效"上同源（共用 `executor.Applier` 那条链），
> 与 `executor-design.md`、`sqlite-observability-design.md` 不冲突——本文只改"配置取值什么时候被读"，
> 不改任何执行侧语义。
> 引用约定：正文只写 `文件`、`文件:行号` 与既有文档的 `§`，不写"上面那段"这类无法定位的指代。
> 行号按 2026-10-02 的代码基线核过；R01–R07 落地后行号整体下移，现行位置按 §14 与卡片 §10.1 的符号名定位。

## 1. 背景与目标

`configs/config.yaml` 目前只在进程启动时读一次（`cmd/server/main.go:318` → `core.LoadConfig`，
`core/config.go:717`），之后每个子系统各持有一份启动时的取值快照：
日志级别固化在 handler 里（`core/logging.go:66-81`），调度器的六个 setter 在 `Start` 之后调用会被忽略
（`core/scheduler.go:153-254`），存储的保留策略在构造时写进结构体字段（`core/store.go:80-93`），
观测层写入器同理（`store/sqlite/events.go:93-107`、`store/sqlite/audit.go:95-101`）。
结果是：调一个并发数、改一条档位、把日志临时开成 debug，都要走一次"改文件 → 重启进程 → 等恢复"。

本设计要交付四件事：

1. **一条自动生效路径**：配置文件变化后，进程自己读回来、校验、生效，不需要人重启也不需要调端点。
2. **一张分档表**：每个配置项明确属于"热更 / 重启 / 拒绝"三档中的哪一档，且只有一个定义处。
3. **一次可解释的失败**：任何一次重载要么全部生效，要么旧取值原样保留，并在日志与 `/admin/runtime` 里
   说清原因；不允许出现"改了一半"而不告诉任何人。
4. **调度器的运行期扩缩容**：`scheduler.workers` 可以热更，扩容立即起 worker，缩容让多出来的 worker
   自己退出，在途任务不受打断。

不在本设计范围内：把配置写回磁盘、鉴权凭据的在线更换、监听端口/CORS 的热更、
以及任何"新增一套配置端点"的读面。理由逐条写在 §9。

## 2. 决策摘要

| # | 决策 | 取舍与理由 |
| --- | --- | --- |
| R1 ★ | **每个叶子键显式归入热更 / 重启 / 拒绝三档，表只有一张** | 本仓最不能接受的是"改了没生效，也没人说什么"。`logging.format`、`observability.events.enabled` 这类进重启档（改了这次不算，下次重启算，日志逐条列出），凭据类进拒绝档（改了整次作废并记 error）。分档表在 `core/config.go`，与 `LoadConfig` 的环境变量绑定表（`core/config.go:734-794`）并置，两张表都由测试守着 |
| R2 ★ | **fsnotify 监听 + 防抖，不引入手动触发端点；"有没有真的变"由重载链判** | 与"改文件即生效"的诉求直接对应，且 `core/load.go:501` 已经在用同一套依赖、`loaderDebounceInterval`（`core/load.go:94`）已经确立了合并写入事件的写法。多一个 REST 触发口就多一份"谁有权改配置"的判定，本期不要。内容等价的判定放在 `Diff`（它握着 `applied`）而不是放在 watcher：见 §7.7 |
| R3 ★ | **全量校验后原子生效；拒绝档变化则整次作废** | 照抄 `executor.Registry.ApplyStore` 的既有承诺："失败时旧表原样不动，不会留下删了一半的中间态"（`executor/registry.go:143-153`）。重载不是逐键尝试，而是先把新配置整个读通（`LoadConfig` 的 `UnmarshalExact` + `Validate`，`core/config.go:806-813`），再过一遍拒绝档，然后逐项应用 |
| R4 ★ | **功能整体由 `reload.enabled` 控制，默认 false** | 与 `executors.enabled`、`observability.enabled`、`executors.web_enabled` 同一口径：打开前进程行为与本设计之前一字不差。热重载把"改文件要重启"这条人工防线撤掉了一道，而本系列的范围含 `executors.commands`（能执行什么的一部分），所以必须由部署方显式表态 |
| R5 | **`scheduler.workers` 做运行期扩缩，队列容量不做** | 通道容量在 `Start` 里一次定下（`core/scheduler.go:1141-1146`），投递方阻塞在 `s.workCh <- job` 上（`core/scheduler.go:1327-1332`）。换通道会让正在阻塞的投递永远等不到结果。扩 worker 不碰通道：容量本来就与 worker 数解耦 |
| R6 | **缩容用温和式：多出来的 worker 在下一次循环开头自己退出** | 与既有 `Stop` 的姿态一致（`core/scheduler.go:1206-1228`：不再投新任务、等在途收尾）。一次改配置不该取消一批正在执行的任务——执行器任务的外部副作用结果未知，这条判断 `executors.restore_policy: pause` 已经替本仓表过态 |
| R7 | **`executors.commands` 热更不要求 `executors.web_enabled`** | 两条链在 `Registry`/`Applier` 里本来就分得开：`ApplyStore` 只管 store 来源（`executor/registry.go:143`），config 来源是 `NewRegistry` 那一批（`:99-141`）。热更 commands 需要的是一次覆盖两批的整表替换，见 §6.5；不必为了复用同步器而把 web 写端点一起打开 |
| R8 | **执行许可字段进拒绝档**（档位内的 `deny_private_ranges` 等；顶层的 `executors.workspace`/`runtime_allow`/`env_allow` 在重启档，见 §6.4） | 处理函数把归一化后的 `ExecutorsConfig` 冻结在闭包里（`executor/register.go:87`、`executor/applier.go:175-183`），要让顶层许可字段对既有档位生效就得连它们一起重登记，"能执行什么"从此变成免重启通道。热重载范围已明确到"改档位内容"为止，改许可边界请重启 |
| R9 | **重载结果只加进 `GET /api/v1/admin/runtime`，不开新端点** | 该端点已是 ops 档专用（`api/server.go:293-299`、`api/handlers_admin.go:15-45`），且已经承载"这次运行到底什么状态"的读数（调度器占用、事件缓冲占用）。新增的是一份 `reload` 对象：上次重载时间、结论、错误文案、被忽略的重启档键名 |
| R10 | **`logging.level` 热更靠 `slog.LevelVar`，格式不改** | `parseLogLevel`（`core/logging.go:48-61`）已经在解析成 `slog.Level`，把 `HandlerOptions.Level` 从常量换成 `*slog.LevelVar` 即可运行期改级别，且不动 handler、不换 writer。`logging.format` 要换 handler 类型，牵连已在手的 logger 实例，归入重启档 |

**落地位置**（TASK-R01…R07 实施后补写，按决策找代码；一律用符号名定位，行号会随后续改动漂移）：

- R1 三档表：`core/config_reload.go` 的 `configClasses`（热更 11 条 + 重启 39 条的显式表）、
  `rejectPrefixes`（三条凭据前缀）、`permissionCommandFields`（15 项执行许可字段）与
  `hotCommandFields`（11 项可调字段）两份对偶清单、`classify` 与 `classifyChange`（后者管条目增删
  与重命名那条 R01 卡面缺的判据）、`Diff`/`flattenLeaves`、`ReloadState`/`ReloadResult`。
  守卫用例 `TestEveryLeafKeyIsClassed` 双向跑：每个叶子键必须有档、表里不许有摊不出来的键名。
- R2 触发时机：`core/watch.go` 的 `ConfigWatcher`（`NewConfigWatcher` 只收绝对路径并要求文件存在、
  `Run` 事件循环、`Close` 只等事件循环不等在途重载、`SetDebounce`、`State`、`MarkWatcherError`），
  事件合并计时器在 `timers`/`debounceSlot`，`ReloadFunc` 的两条契约写在 `core/watch.go` 的注释上
  （串行靠 `reloadMu`，不靠调用点）。默认值与下界在 `core/config.go`：`DefaultReloadDebounce = 500ms`、
  `Validate` 里 50ms 的下界。手动触发端点一条都没加（本文与 R07 场景实测都核对过 404）。
- R3 原子生效与逆序回滚：`cmd/server/reload.go` 的 `reloader.Reload`（八步固定顺序，第 0 步是 R07 补的那道问）、
  `reloadStep`/`buildPlan`（键→落点的分派表，`claims` 支持精确名与前缀认领两种形态）、
  `applyChange`（应用前压 undo 栈、失败逆序重放、回滚再失败才 `degraded`）、`unavailableOutcome`
  （某节没启用时算 `ignored` 还是算失败）。"没有取值"那一判据在
  `configFileCarriesNoValues` + `yamlCarriesNoValue`（按 YAML 结构判，BOM 与 UTF-16 由解析器自己认，
  见 §14 的 D-R0702、D-R0709、D-R0711 与 D-R0715）。
- R4 总开关默认关：`core/config.go` 的 `ReloadConfig` 与 `DefaultConfig`（`Enabled: false`）；
  装配在 `cmd/server/main.go` 的 `if cfg.Reload.Enabled { ... }` 块，`api.WithReloadState` 的注入
  条件与它同源。关闭时 `run()` 一个字节都不执行，`/admin/runtime` 连 `reload` 键都不给。
- R5/R6 worker 扩缩：`core/scheduler.go` 的 `ResizeWorkers`（温和缩容：多出来的 worker 在循环开头
  自己退场，在途任务不打断）、`targetWorkers`（`atomic.Int32`，`RuntimeStats.Workers` 读的就是它）。
  `ResizeWorkers` 只有下限、没有上限，上限由重载链补：`cmd/server/reload.go` 的
  `maxHotReloadWorkers = 4096` 与 `checkWorkerBound`（R03 交接的 D3）。队列容量仍是重启档。
- R7 档位热更不要求 `web_enabled`：`executor/applier.go` 的 `ApplyConfig(candidate core.Config)`
  （整表替换 config 批次并返回 `ApplyResult`）；`Applier` 的构造条件在 `cmd/server/main.go`
  放宽成 `web_enabled || (reload.enabled && executors.enabled)`，而档位文件的打开条件一字未改。
- R8 执行许可字段进拒绝档：判定在 `core/config_reload.go` 的 `permissionCommandFields`；
  顶层三份白名单在 `configClasses` 里是重启档，`ApplyConfig` 加载新批次固定用启动时那份归一化配置
  做 `LoadProfiles` 校验（R04），所以改顶层许可不会给新档位开出通路。
  ⚠️ 全局 `executors.env_allow` 从不进 `LoadProfiles`（唯一读取点是执行期的 `executor/env.go`），
  本文 §12 最后一条原来的说法已按实测收窄，见 §14 的 D-R0404。
- R9 读数：`api/reload_state.go` 的 `ReloadStateReader`、`ReloadStatus`（十个字段，两个时间是
  `api` 自己格式化的 RFC3339Nano 字符串，零值不给键）、`WithReloadState`、`reloadStatusOf`、
  `formatReloadTime`；`api/handlers_admin.go` 的 `RuntimeResponse.Reload *ReloadStatus`。
  链那侧的读数出口是 `cmd/server/reload.go` 的 `reloadStatusReader`（`attach`/`explain`/`State`）。
  没有新增端点，读端点也不进写操作台账（`TestRuntimeReadDoesNotWriteAudit` 钉住）。
- R10 级别热更：`core/logging.go` 的 `NewLoggerWithLevelVar`（把 `*slog.LevelVar` 交回装配方）与
  `SetLogLevel(levelVar, level)`；`levelVar` 由 `cmd/server/main.go` 建 logger 时留住并传给重载链，
  `logging.format` 仍在重启档。

## 3. 现状盘点（规划时基线，已核实）

| 事实 | 证据 |
| --- | --- |
| 配置只读一次，无全局单例，`Config` 按值传给各构造闭包 | `cmd/server/main.go:313-338`、`:176-311` |
| 加载用 Viper，未知键直接报错，随后 `Validate` 与 `Normalized` | `core/config.go:717-816`、`:819`、`:907` |
| 环境变量绑定是一张显式键名表，漏一项该键的环境变量就静默无效 | `core/config.go:734-794` |
| 两份配置文件键必须一一对应，有守卫测试 | `core/config_test.go:434`（`TestExampleConfigMatchesLocal`）、扁平化工具 `:437-444` |
| 调度器全部容量类 setter 在 running 时 warn 并忽略 | `core/scheduler.go:153-254` |
| worker 数与通道在 `Start` 里一次建好，`Stop` 靠 `wg` 等全部退出 | `core/scheduler.go:1128-1196`、`:1206-1228` |
| 普通池投递是阻塞发送（带 `stopCh` 分支），执行器池投递非阻塞且有 500ms 兜底 | `core/scheduler.go:1327-1332`、`:1276-1282` |
| 重试延迟在每次执行时才读，不在启动时算死 | `core/scheduler.go:1686` → `core/retry.go:41-48` |
| 存储的保留条数/时长按每次写入时的字段值判定 | `core/store.go:161-189`（读 `historyLimit`/`historyTTL`） |
| 存储的合并落盘周期是一个 ticker | `core/store.go:204-222` |
| 观测层两个写入器的 retention 是结构体字段，淘汰在批量周期里执行 | `store/sqlite/events.go:62-63`、`:227-234`；`store/sqlite/audit.go:73-74` |
| 日志级别进 handler 常量，改级别要换 handler | `core/logging.go:66-81` |
| 档位登记表已支持运行期整表替换，但只覆盖 store 来源那一批 | `executor/registry.go:85`（`atomic.Pointer`）、`:143-153` |
| 配置来源的档位没有运行期入口，`Register` 键冲突即返回错误且只跑一次 | `executor/register.go:52-108` |
| 在线生效链已有"读文件 → 校验/探测 → 整表替换 → 同步处理函数"，全程一把写锁 | `executor/applier.go:106-203`、`:116-117` |
| 全仓没有配置监听：`fsnotify` 只用于目录任务加载器 | `core/load.go:501`、`watcher` 字段 `:119` |
| `SIGHUP` 只作为子进程信号名出现，进程自身只处理 SIGINT/SIGTERM | `executor/proc_unix.go:68`、`cmd/server/main.go:627-629` |
| 执行器开关与产物存储的装配都在启动期分岔，关掉时对象根本不建 | `cmd/server/main.go:395-401`、`:596-608` |

## 4. 总体架构

```
            ┌─ 启动（不变）──────────────────────────────────────────┐
config.yaml ┤ LoadConfig → NewLogger → defaultRuntimeDeps → run()    │
            └──────────────────────┬─────────────────────────────────┘
                                   ↓ applied 快照（当前生效的那份配置，值类型）
                     reload.enabled=true 时挂上 ConfigWatcher
                                   ↓ fsnotify 事件（Write/Rename/Chmod/Create）
                     防抖窗口合并 → LoadConfig(path) 全新读通
                                   ↓
                     Config.Diff(applied, candidate) → hot / restart / reject
                                   ↓
        reject 非空 或 读通失败 ──→ 整次作废：applied 不变，记 error，状态置 rejected
                                   ↓ 否则逐项应用 hot（每项先记录旧值）
        任一项失败 ─────────────→ 用旧值逐项回滚，applied 不变，记 error，状态置 failed
                                   ↓ 全部成功
                     applied = candidate；记 info（改了哪些键、忽略哪些键）
                     状态置 ok → GET /api/v1/admin/runtime 的 reload 对象
```

三个不变量：

- **I1 单一权威**：`applied` 是"现在跑的到底哪套值"的唯一答案，重载成功才换。子系统继续持各自的取值快照，
  本文不把任何子系统改成"实时读配置"。
- **I2 要么全变要么没变**：热更项按依赖顺序应用，中途失败逐项链式回滚。回滚本身失败时进程进入
  degraded 状态（内存里可能半新半旧），必须在 `/admin/runtime` 里看得见，不能只留一行日志。
- **I3 沉默是 bug**：任何"这次没生效"都要有归宿——重启档进 `ignored_keys`，拒绝档与失败进
  `error`，监听器自己坏了进 `watcher_error`。

## 5. 数据契约

### 5.1 新增配置节

```yaml
# 配置热重载：改文件即生效，不必重启进程。默认关闭，关闭时一个监听器都不建，
# 进程行为与本节不存在时一致。
# 设计依据见 docs/design/config-reload-design.md。
# 环境变量：GODELAYQ_RELOAD_ENABLED=true
reload:
  # 总开关。打开后进程会盯着启动时实际读到的那个文件（见 §5.2）。
  # 这一项本身改了不生效：运行期关掉监听只能重启，所以它属于重启档。
  enabled: false
  # 静默窗口：同一文件在这段时间内的多次写入合并成一次重载。
  # 编辑器存盘往往拆成 Create + 多次 Write，逐次读会读到半截内容。
  # 留空/0 用默认 500ms；低于 50ms 会被拒（那样等于每次存盘读好几遍）。
  # 这一项可以热更：改它下一次事件就用新窗口。
  debounce: 500ms
```

两条配套口径：

- `reload.debounce` 与 `core/store.go:204-222` 的 ticker 无关，它只管事件合并；
  与目录加载器的 `loaderDebounceInterval`（常量 100ms，`core/load.go:94`）是两处独立取值，
  本文不去合并它们——任务文件读到半截与配置读到半截的代价不同。
- 两份配置文件都要加这一节：`configs/config.example.yaml` 与 `configs/config.yaml`，
  `TestExampleConfigMatchesLocal` 会盯着（`core/config_test.go:434`）。

### 5.2 盯的是哪个文件

`-config` 显式给的路径优先；留空时按 `core.DefaultConfigPath`（`core/config.go:12`）自动查找，
找到了就盯它，没找到就不建监听器并记一条 warn（"当前用默认值运行，无文件可盯"）。

规则：**只盯启动时真正读到的那一份文件**。启动后凭空创建一个 `configs/config.yaml` 不会触发重载——
那样会让"往目录里扔个文件"变成配置来源，与显式 `-config` 的部署冲突。

### 5.3 环境变量覆盖的口径

重载仍走 `LoadConfig`，因此 `GODELAYQ_*` 的覆盖照旧逐项重新生效
（绑定表 `core/config.go:734-794`）。这带来一条要说清的行为：进程环境里有一个常驻的
`GODELAYQ_SCHEDULER_WORKERS=32` 时，把 YAML 里的 `scheduler.workers` 从 8 改成 16 不会改变运行值
（环境变量赢），`Diff` 看到 8→8，日志记"本次无变化"。这是既有优先级的正常结果，不是重载的缺陷，
但日志必须把这个词说对：区分"文件没变"与"变了但被环境变量压住"要靠 `candidate` 的原始来源，
本期只在文案里提示"存在环境变量覆盖时取值以环境为准"，不新增来源判定。

### 5.4 重载状态对象

```go
// api/handlers_admin.go 的 RuntimeResponse 新增一个字段
// 用指针而不是值类型：值类型会让未启用热重载的部署回出一个 {"result":""} 的空对象，
// 那会被读成"启用过但从没重载"。
Reload *core.ReloadState `json:"reload,omitempty"` // reload 未启用时整个字段缺省
```

`ReloadState` 定义在 `core/config_reload.go`（不是 `api`）：产出它的一方是 `core.ConfigWatcher`
与 `cmd/server` 的重载链，`api` 只是把它转写进响应——结构体必须落在依赖方向的下层，
否则 `core` 要反向 import `api`。

```go
// core/config_reload.go
type ReloadState struct {
    Enabled       bool      `json:"enabled"`
    WatchedPath   string    `json:"watched_path,omitempty"`
    LastAttemptAt time.Time `json:"last_attempt_at,omitempty"`
    LastAppliedAt time.Time `json:"last_applied_at,omitempty"`
    // Result 是封闭枚举：ok | unchanged | rejected | failed | degraded
    Result        ReloadResult `json:"result"`
    Error         string   `json:"error,omitempty"`
    AppliedKeys   []string `json:"applied_keys,omitempty"`   // 本次真正生效的热更键
    IgnoredKeys   []string `json:"ignored_keys,omitempty"`   // 本次改了但需要重启的键
    RejectedKeys  []string `json:"rejected_keys,omitempty"`  // 本次导致整次作废的键
    WatcherError  string   `json:"watcher_error,omitempty"`  // 监听器自己的故障
}
```

三条约束：不含任何**凭据**取值（键名可以有；`error` 与 `watcher_error` 是自由文本，其中可能回显
非凭据的取值——写错的时长、撞名的档位名——那道闸门在产出文案的一侧，不在读口）；`Result` 是封闭枚举；
这个对象是**进程内**状态，不入库——观测层的 `write_audit` 只记写操作，重载不是写操作（R07 实测：
`GET /api/v1/admin/runtime` 三次不产生台账行，`rejected_keys` 与 `applied_keys` 都只在读数与日志里）。

实施后的两处口径修正（详见 §14）：

1. `AppliedKeys` 的注释写的是"本次真正生效的热更键"。执行器那一节有一种情况不是"生效"而是
   "处理过但未生效"：`executors.enabled: false` 时改 `executors.commands`，链会把键记进
   `AppliedKeys`、`LastAppliedAt` 也推进，另有一条 warn 说明"这一节没打开，本次不会生效"
   （真实进程实测见 R07 场景 13）。读数里的 `applied_keys` 因此应当读作"本次重载处理过的键"。
2. 一次失败的重载在日志里留下**两条** error 级记录：重载链按失败原因记一条（带步骤与 `hint`），
   监听器把 `ReloadFunc` 返回的错误再记一条（带 `path`）。两者归因不同（步骤级 / 进程级），
   按 error 行数告警的部署会重复计数，要按 `path` + 时间窗去重。

## 6. 分档表（本设计的核心交付）

判定单位是叶子键；`executors.commands` 整段作为一个热更单元（它内部字段的变更另按拒绝档判定，见 6.4）。

### 6.1 热更（重载后立即生效）

| 键 | 生效方式 | 落点 |
| --- | --- | --- |
| `logging.level` | `slog.LevelVar.Set` | `core/logging.go` 改造（`NewLoggerWithLevelVar`）+ `core.SetLogLevel(levelVar, level)` |
| `scheduler.workers` | 运行期扩缩 worker | `core.Scheduler.ResizeWorkers(n)`（§7.1） |
| `scheduler.max_retry_delay` | 换重试策略实例 | `core.Scheduler.SetRetryPolicy(p)` |
| `store.history_limit` | 下一次写入的 trim 用新值 | `core.Store.SetHistoryRetention(limit, ttl)` |
| `store.history_ttl` | 同上 | 同上（两项必须一起给，见 §7.2） |
| `observability.events.retention_count` / `.retention_age` | 下一个批量周期的淘汰用新值 | `sqlite.EventLog.SetRetention` |
| `observability.audit.retention_count` / `.retention_age` | 同上 | `sqlite.AuditLog.SetRetention` |
| `executors.commands` | 整表替换 + 重登记处理函数 | `executor.Applier.ApplyConfig`（§7.5） |
| `reload.debounce` | 下一次事件起用新窗口 | `core.ConfigWatcher.SetDebounce` |

"生效时机"这一列是验收依据：热更不等于瞬时，`store.history_ttl` 要等下一次写入触发的 trim，
`observability.*.retention_*` 要等下一个批量周期。每张任务卡的验收都得按这一列写。

### 6.2 重启（重载接受但不应用，逐条记入 `ignored_keys`）

`server.port`、`server.cors.allow_origins`、`server.cors.allow_credentials`、
`server.auth.jwt.access_ttl`、`server.auth.jwt.refresh_ttl`、
`scheduler.queue_capacity`、`scheduler.shutdown_timeout`、
`store.type`、`store.path`、`store.groups_path`、`store.flush_interval`、
`logging.format`、
`observability.enabled`、`observability.path`、`observability.flush_interval`、
`observability.queue_capacity`、`observability.busy_timeout`、`observability.synchronous`、
`observability.events.enabled`、`observability.artifacts.enabled`、`observability.audit.enabled`、
`executors.enabled`、`executors.required_role`、`executors.concurrency`、`executors.queue_capacity`、
`executors.default_timeout`、`executors.max_timeout`、`executors.restore_policy`、
`executors.loader_allow`、`executors.web_enabled`、`executors.profiles_path`、
`executors.output.*`、`reload.enabled`。

归类的三条理由：

- **通道/ticker/连接类**（两个 `queue_capacity`、`store.flush_interval`、`observability.flush_interval`、
  `observability.queue_capacity`、`busy_timeout`）：绑着在 Start 期建立的通道与 ticker
  （`core/scheduler.go:1141-1159`、`core/store.go:208`、`store/sqlite/batch.go`），
  换它们等于换掉正在阻塞的等待方。
- **路径与开关类**（`store.path`、`observability.path`、`executors.output.dir`、各 `enabled`）：
  换文件就是换一份数据或换一个订阅关系，属于重新装配，不属于改取值。
- **改动会扩大执行能力的**（`executors.concurrency`、`default_timeout`、`max_timeout`、`required_role`）：
  `concurrency` 要走执行器池的运行期扩缩（§7.1 末段说明的"从无到有建池"问题，登记在 §10 的 N1）；
  其余三项固化在 `Registry` 构造时的取值与冻结进闭包的那份配置里，理由与改动面见 §10 的 N2。

### 6.3 拒绝（变化即整次作废）

`server.auth.token`、`server.auth.users`、`server.auth.jwt.secret`。

理由：这三项是身份边界。`api.Server` 在构造时用 `NewAuthenticator(sec.Auth)` 把账号表固化
（`api/server.go:176-184`），配置文件里也明写了"增删账号需改本文件并重启进程"
（`configs/config.example.yaml:33`）。把它们放进重启档会变成"改了静默不生效"，
放进热更档要运行期换 Authenticator 且换密钥会让全部已签发令牌瞬间失效。
放进拒绝档的代价是"改凭据会让整次重载失败"，而这个失败会带着明确文案记 error，
运维当场就知道要重启。

### 6.4 `executors.commands` 的两个方向

档位条目里的字段按"改它等于换身份、换目标、换可执行体或换凭据吗"分两侧：

- **允许热更**：`name`（重命名等价于删一条加一条）、`body`（只决定 payload 的 body 按 json/raw/none
  解释，既不换可执行体也不换目标主机，见 `executor/http.go:372` 的 `requestBody`）、`timeout`、`max_parallel`、
  `retry_on_exit`、`args`/`args_render`/`positional`（payload 参数声明）、`expect_status`、
  `capture_response`、`max_body_bytes`，以及条目的增删。
- **触发拒绝**：`kind`、`runtime`、`script`、`program`、`fixed_args`、`cwd`、`env`、`env_allow`、
  `method`、`url_template`、`allowed_hosts`、`headers`、`header_allow`、`deny_private_ranges`、
  `max_redirects`。这十五项是"跑哪个可执行体、以什么身份、把请求发到哪里"的身份、目标与凭据字段，
  其中 `env` 装的是固定注入的凭据材料（控制台因此从不回显它的取值），
  改它等于换掉一次执行所凭的身份；`kind`（`script`/`binary`/`http`）与 `runtime`/`script`/`program`
  同属"哪一个可执行体"的身份而不是可调参数——换了它，同一条任务类型上周跑脚本、这周发 HTTP，
  而任务留痕里看不出来。这与档位在线管理已拍板的 D7（`docs/design/web-profile-design.md:38`：
  修改档位不允许改 `kind` 与 `script`/`program`，要换就删了重建）同向，落地口径见
  `immutableFieldChange`（`api/handlers_executor_profiles.go:514`）：在线 PUT 那侧本来就是改 `kind`
  直接回 400，热重载这侧若允许原地热更就是两条相反的口径。
- 顶层的 `executors.workspace`/`runtime_allow`/`env_allow` 本来就在重启档（§6.2），而 §7.5 的加载固定用
  启动时那份归一化配置，所以它们改了什么都不会进运行期——拒绝档在这里的作用是**给未来留字段余地**：
  `Diff` 只用 `core` 自己看得见的信息判定（`core.ExecutorCommand` 的字段清单，实现上是两份显式的
  清单 `permissionCommandFields` 与 `hotCommandFields`），不借 `executor.LoadProfiles` 的归一化结果——
  `core` 不许 import `executor`（这条依赖方向红线在本仓是硬约束，`core/executor_profile_store.go`
  的存在就是为它服务的）。两份清单合起来恰好覆盖 `ExecutorCommand` 的每一个字段名，`classify` 的
  档位字段分支是**三态**：命中拒绝清单→拒绝、命中热更清单→热更、两边都不在→未归档（`ok=false`）。
  将来新增字段必须显式选一份清单登记，否则 §8 的守卫用例红——这样加字段不会因为忘记归档而静默
  落进"允许热更"的一侧变成免重启的口子。
- 上条只说"原地改档位内的字段"这一侧；新增或重命名一条档位属于本次已拍板的热更范围，其约束来自顶层
  许可字段（重启档）与 §7.5 的 workspace 越界拒绝。这一后果的完整口径见 §9。

## 7. 后端改动方案（逐文件）

### 7.1 `core/scheduler.go`：运行期扩缩 worker

```go
// ResizeWorkers 在运行期把普通池的 worker 数调到 n（>=1）。
// 与 SetConcurrency 的分别：那个只在 Start 前有效（既有的 warn 分支原样保留），
// 这个专供重载路径。队列容量不动——通道换不得（见设计 §5）。
func (s *Scheduler) ResizeWorkers(n int) error
```

实现要点：

- 新增 `targetWorkers atomic.Int32`，`Start` 里置为 `s.concurrency`。
- **扩容**：起 `n-target` 个 worker 协程，`wg.Add` 同步计入；每个协程启动时取自己的序号 `idx`，
  `idx >= target` 即退场。缩容不做主动通知，正在跑任务的 worker 跑完回到循环开头自己判断，
  这正好是 R6 要的温和语义。
- **缩容**：先把 `target` 降下去，多出来的 worker 自行退场；`RuntimeStats.Workers`
  读 `target`（读数口径是"期望并发"，与 `Running` 的瞬时口径区分开，注释里写明）。
- 通道不重建：`deliver` 的阻塞发送（`core/scheduler.go:1327-1332`）在退场窗口里必然有空位被腾出，
  不存在投递方被永久挂住的窗口——这条要靠测试证明，不靠推理（§8）。
- `Stop` 不变：它靠 `wg.Wait()` 收敛，新增的 worker 都算进了 `wg`。
- 执行器池不在本节范围：`execCh` 为 nil 时（未开执行器）没有池可扩，"从无到有建池"另议（§10）。

### 7.2 `core/store.go`：保留策略 setter

`JSONFileStore` 的 `historyLimit`/`historyTTL` 改成 `atomic.Int64` + `atomic.Int64`（时长存纳秒），
`trimTerminalLocked`（`core/store.go:161-189`）与 `trimAfterWriteLocked` 读这两个原子值。

```go
// SetHistoryRetention 运行期调整终态留痕的条数与时长上限。
// 两项必须一起给：分开调会出现"新条数配旧时长"的中间态，
// 而 store.history_limit=-1（不留痕）与 history_ttl 的组合语义只在成对时说得清。
func (s *JSONFileStore) SetHistoryRetention(limit int, ttl time.Duration)
```

`Store` 接口要不要加这个方法：加。`api`/`cmd` 侧只通过接口用它，测试替身需同步实现——
这是本仓既有接口的扩张方式（对照 `schedulerAPI` 上那组 `SetExecConcurrency`，
`cmd/server/main.go:34-40`，替身在 `cmd/server/main_integration_test.go:1887` 附近）。

### 7.3 `core/logging.go`：可变级别

`NewLogger`（`core/logging.go:66-81`）的签名与行为保持不变——它被 `cmd/server/main.go:324`、
各示例与测试大量调用，改返回值会波及一片。做法是新增一个并列构造函数，把级别载体交回调用方：

```go
// NewLoggerWithLevelVar 与 NewLogger 同一套解析与 handler 构造，额外交回级别的载体。
// reload 路径需要它；其余调用方继续用 NewLogger，拿不到也不需要拿到。
func NewLoggerWithLevelVar(level, format string, w io.Writer) (*slog.Logger, *slog.LevelVar, error)

// SetLogLevel 解析级别名并写入载体。格式、handler、writer 一概不动。
func SetLogLevel(v *slog.LevelVar, level string) error
```

- `parseLogLevel`（`core/logging.go:48-61`）原样复用，`NewLogger` 内部改成"建一个 LevelVar 再走同一条
  handler 构造"，两个入口共享那一段，避免出现第二份级别解析。
- `main()` 里把 `NewLogger` 换成 `NewLoggerWithLevelVar`，LevelVar 存进 `runtimeDeps`，
  `slog.SetDefault(logger)`（`cmd/server/main.go:329`）无需重调：换级别不换 logger 实例，
  经由 `slog.Default()` 的包级调用（第三方库、示例 handler）一起跟着变——这是本节的预期行为，
  不是副作用，注释里要写明。
- `logging.format` 要换 handler 类型，牵连已在手的 logger 实例，留在重启档（§6.2）。

### 7.4 `store/sqlite/{events,audit}.go`：retention setter

两个写入器的 `retentionCount`/`retentionAge` 改成原子字段，新增

```go
func (e *EventLog) SetRetention(count int, age time.Duration)
func (a *AuditLog) SetRetention(count int, age time.Duration)
```

淘汰 SQL（`store/sqlite/events.go:227-234` 及 audit 对应处）读原子值。零值口径逐字沿用
现有 `New*` 里那段（`<=0` 回默认、`age<0` 按 0），不许在 setter 里另立一套。

### 7.5 `executor/registry.go` + `applier.go`：配置来源的整表替换

`ApplyStore` 的语义是"只动 store 那一批，config 条目一律保留"（`executor/registry.go:143-153`），
热更 commands 需要的是反过来，所以新增一个方法而不是改它：

```go
// ApplyConfig 用给进来的这一批档位重建登记表的 config 部分，store 来源的条目原样保留。
// 与 ApplyStore 镜像对称、同一套承诺：整批替换、失败时旧表不动、同批内重复键拒绝；
// 多一条 ApplyStore 没有的规则——新 config 条目与现存 store 条目撞名时 config 赢，
// 那条 store 条目移入降级展示面（与启动合并同方向，§5.2）。
func (r *Registry) ApplyConfig(profiles []*Profile) error
```

`Applier` 新增 `ApplyConfig(candidate core.Config) (ApplyResult, error)`
（收整份新配置而不是只收 `ExecutorsConfig`：调用方手里就是 `LoadConfig` 的返回值，
让它先取出 `Commands` 再合成一份配置是同一件事在两处各写一遍）：

1. 把 `candidate.Executors.Commands` 接到**启动时那一份**归一化 `ExecutorsConfig` 上
   （其余字段一律用冻结值，R8：许可字段属重启档与拒绝档），调 `LoadProfiles` 加载新 commands。
   这一步是整条链上唯一必须说清的地方：用整份 candidate 去校验会让新增的档位按**新的**
   `workspace`/`runtime_allow` 建出来，而既有档位仍按旧值跑，
   那是"同一台机器上两套许可"，比不改还糟。
2. 逐条 `Probe`，探测失败仍入表（与 `NewRegistry` 同一口径，`executor/registry.go:126-137`）。
3. 与现存 store 来源条目做撞名判定：新配置的某条与页面建的某条同名时，按 §5.2 的既有方向处理——
   config 赢、store 那条标 Degraded（复用 `MergeStoreProfiles`，`executor/merge_profiles.go:39`）。
4. `ApplyConfig` 整表替换 → 重登记**全部** `exec.` 前缀处理函数（config 与 store 两批都重登记，
   因为 config 那批的闭包按 §3 盘点过的方式建出来，整批重登记才能保证"生效表与调度器同一批"）
   → 摘除新表里已不存在的键。摘除与登记顺序照抄 `Applier.Apply`（先登记后摘除，`executor/applier.go:172-198`）。
5. 任一步失败：不碰表、不碰调度器，返回错误，由重载方回滚。

`cmd/server/main.go` 只有在 `executors.enabled=true` 时才有 `Applier`（`:596-608` 的分岔），
而 `executors.enabled` 在重启档，所以热更 commands 时 `executors.enabled` 必为真；
本设计把 `Applier` 的构造条件从"只在 `web_enabled` 时"放宽到"`web_enabled` 或 `reload.enabled` 时"，
但**不为 `reload` 打开档位文件**：`web_enabled=false` 的部署今天连 `profiles_path` 的父目录都不碰
（W01 的冒烟证据），为了热更档位而去读它，等于把"关闭时惰性"这条口径改掉。
做法是给 `Applier` 一个不依赖 store 的构造入口（`executor.Applier.Apply` 那条 store 路径返回明确错误，
`ApplyConfig` 照常可用），登记表的 store 侧条目在那种部署里本来就不存在，
撞名判定只看 config 批次，与今天启动后的状态一致。两个写端点的 503 判定仍只看 `web_enabled`
（§7.7），不受影响。

### 7.6 `core/config_reload.go`（新增）：分档表 + Diff + applied 快照

```go
// ConfigClass 是一个叶子键在重载里的归属。
type ConfigClass int

const (
    ClassHot ConfigClass = iota   // 重载时应用
    ClassRestart                  // 重载时忽略，日志逐条列出
    ClassReject                   // 变化即整次作废
)

// configClasses 是唯一权威表：叶子路径 → 档位。
// 新增配置键必须在这里登记，TestEveryLeafKeyIsClassed 会挡住漏登记的键。
var configClasses = map[string]ConfigClass{ /* ... */ }

// Diff 返回 candidate 相对 applied 三份清单：待应用的键与值、需重启的键名、导致作废的键名。
// 没有失败可报告：比较两份内存里的结构体不会出错，所以不返回 error。
// 前提是两侧都已 Normalized()——否则 0 值与默认值的差别会被当成一次改动。
// candidate 被换入 applied 由调用方负责，Diff 不改动任何一方。
func Diff(applied, candidate Config) ConfigChange
```

扁平化是 `core` 内部一份新的 `reflect` 实现（R01 §3.2），**不复用**
`core/config_test.go:437-444` 的 `configKeys`：那份从 YAML 文件读键名、只给路径不给值、
且要求文件存在，而这里要比较两份内存配置的取值；`executors.commands` 还必须按元素摊成
`executors.commands.<name>.<field>` 才能逐字段归档（§6.4）。`configKeys` 保持原样，
它服务的是"两份 YAML 的键名集合"这条守卫，与"两份内存配置的取值"是两件事。

### 7.7 `core/watch.go`（新增）：监听器

```go
// ReloadFunc 是一次重载的执行体：读配置、比对、应用，并把结论交回来。
// 串行由实现方自己保证（重载链内部一把锁，另加与档位写链共用的那一把）。
type ReloadFunc func() (ReloadState, error)

// ConfigWatcher 盯一份 YAML，变化后按防抖窗口触发一次 onReload，并保存重载结果供读端点取用。
// 它不认识任何业务键，也不自己应用配置：应用归装配方，失败也归装配方。
// 状态的归属说清：ReloadState 由 ReloadFunc 产出、由 watcher 保存（一次 atomic.Pointer 换），
// 唯一的例外是 WatcherError——那是 watcher 自己的字段，保存新状态时保留旧值不被覆盖。
// api 的读端点注入的就是 watcher（它满足 State() core.ReloadState 这一个方法）。
func NewConfigWatcher(path string, debounce time.Duration,
    onReload ReloadFunc, logger *slog.Logger) (*ConfigWatcher, error)
func (w *ConfigWatcher) Run(ctx context.Context)
func (w *ConfigWatcher) SetDebounce(d time.Duration)
func (w *ConfigWatcher) State() ReloadState
func (w *ConfigWatcher) Close() error
func (w *ConfigWatcher) MarkWatcherError(msg string)
```

- 监听**文件所在目录**，只认目标文件名的 / 路径匹配的事件；`Rename` 按"文件被换掉"处理，
  处理后重新登记监听。与目录加载器的监听（`core/load.go:501` 起）共用依赖但不复用代码：
  那边的合并窗口是 100ms 且按"每个文件各处理一次"记账（`core/load.go:94`、`:271`、`:446`），
  这边要的是"一份文件、比内容、失败也要留痕"，形状不同。
- 事件类型不做筛选（Write/Chmod/Create/Rename 全触发一次防抖）。**watcher 不比较内容**：
  判"内容有没有真的变"需要知道上一次真正生效的那份配置，而那份权威（`applied`）在重载链手里，
  不在 watcher 手里——让两个类型共享同一个概念比省一次读文件更贵。
  等价内容导致的那次重载由重载链自己给出 `unchanged` 结论（§8 那条验收就是它）。
- `Run` 内所有错误都记进 `State()` 的 `watcher_error` 并继续存活；
  watcher 彻底死亡（事件通道关闭）时记 error 并留下 `watcher_error`，
  且这个字段不被后续成功的重载抹掉，让"进程停在旧配置"这件事可见（I3）。

### 7.8 `cmd/server`：接线

新增 `cmd/server/reload.go`（一个 `reloader` 类型而不是 `run()` 里的一串闭包：这条链的状态
——`applied`、串行锁、待回滚的旧值——跨多次重载存活），并在 `runtimeDeps` 里加一个函数值字段
（沿用既有风格，`cmd/server/main.go:128-173`）：

```go
newConfigWatcher func(path string, debounce time.Duration,
    onReload core.ReloadFunc, logger *slog.Logger) (*core.ConfigWatcher, error)
```

`reloader.Reload` 就是 §4 那条链的逐键实现：按 `change.Hot` 里的键名分派到 §7.1~§7.5 的 setter，
并在同一步记录旧值用于回滚。`run()` 在 `scheduler.Start()`、`server.Start()` 之后建 watcher、
`go watcher.Run(ctx)`；收到 SIGINT/SIGTERM 之后、`server.Stop` 之前显式 `watcher.Close()`
（不是 `defer`——`defer` 会晚于 `server.Stop` 执行）。顺序原因：重载可能重新登记档位，
而优雅关闭期间不该有配置变化挤进来。

集成测试的 `spyScheduler` 与假 watcher 通过 `runtimeDeps` 的字段注入，与
`main_integration_test.go` 既有替身同一套路；`/admin/runtime` 的读口注入的是 watcher
（它满足 `State() core.ReloadState`），`api` 侧不需要知道 `reloader` 的存在。

## 8. 验收清单

- [ ] `config.example.yaml` 与 `config.yaml` 都含 `reload` 一节，键一一对应
      （`TestExampleConfigMatchesLocal` 跑绿）。
- [ ] `TestEveryLeafKeyIsClassed`：从 `Config` 结构体摊平出的**每一个**叶子键都在 `configClasses` 里有且
      只有一档；`TestHotKeysAreEffective`：热更档的每个键都有一个对应的 `reloader` 分派分支，
      漏一个即失败（防止"归档成热更、但没人应用"）。
- [ ] `reload.enabled=false`（默认）时：不建 watcher、`/admin/runtime` 无 `reload` 字段、
      全部行为与本设计之前一致。
- [ ] 改 `logging.level` → 日志级别立刻变；改 `logging.format` → 记入 `ignored_keys`，级别不受影响。
- [ ] 改 `scheduler.workers` 8→32→4：`/admin/runtime` 的 `scheduler.workers` 读数随之变
      （R07 实测：`GET /api/v1/pools` 这个端点在本仓从来没有过，见 §14 的 D-R0701）；
      缩容期间在途任务不被取消；`-race -count=5 -timeout 30m` 跑绿。
- [ ] 缩容窗口内 `deliver` 仍能投递（专门一条测试：target 已降、旧 worker 正在跑任务，新任务照样被取走）。
- [ ] 改 `store.history_limit`/`history_ttl` → 下一次写入触发的 trim 用新值；改 `store.flush_interval` →
      进 `ignored_keys`。
- [ ] 改观测层四个 retention 键 → 下一个批量周期的淘汰用新值；改 `observability.flush_interval`/
      `queue_capacity` → 进 `ignored_keys`。
- [ ] 改 `executors.commands` 增删条目 → `GET /api/v1/executors` 与 `/job-types` 随之变，
      旧键的处理函数被摘除，同名撞页面档位时按 config 赢、store 那条标 Degraded。
- [ ] 改 `executors.commands` 里的 `deny_private_ranges`（http 档位）→ 整次作废、
      生效表原样不动、`rejected_keys` 与 `error` 有内容。改顶层 `executors.runtime_allow` →
      进 `ignored_keys`，且既有档位的处理函数一律不变。
- [ ] 改 `server.auth.token` / `users` / `jwt.secret` → 整次作废，旧凭据继续可用，记 error。
- [ ] 坏 YAML / 未知键 / `Validate` 不过 → 旧配置原样、`result=rejected`、`error` 带原因；
      修好文件后下一次事件自动恢复。
- [ ] 文件被删或读不到 → 视为一次失败的重载，绝不退回默认值。
- [ ] 文件不再表达任何取值——只剩空白/注释/文档分隔符（**含带 BOM 与 UTF-16 写出来的同一种**）、
      只有键名没有值（`logging:`、`workers: null`、`logging: {}`、顶层 `null`）、或值写在第二份文档里
      （viper 只读第一份）→ 同样作废、现网取值一字不动，`error` 给专门文案。
      （R07 场景 16/16E 实测发现这里原本会静默退回默认值；判据改了三轮才修到底，
      见 §14 的 D-R0702、D-R0709、D-R0711、D-R0715 与 R07 卡 §10.4 的 16F/16G/16H。）
- [ ] 同一次存盘触发多个事件 → 防抖窗口内只重载一次；内容与 `applied` 等价 → `result=unchanged`，一个 setter 都不调（不重登记档位）。
- [ ] 连续两次重载并发到达 → 被一把锁串行化，第二次能看到第一次的落盘结果。
- [ ] watcher 出错（监听器关闭）→ `watcher_error` 有值且记 error，进程不停摆。
- [ ] 优雅关闭顺序：先停 watcher，再关 server，再停调度器；关闭期间不再有注册动作。
- [ ] 文档同步：`configs/config.example.yaml` 注释、`docs/design/executor-design.md` 与
      `web-console-design.md` 里"改配置需重启"的表述加一句指向本文（哪些仍要重启、哪些不必）。

## 9. 安全模型

- **默认关**：`reload.enabled=false` 时本设计一个 goroutine 都不建（R4）。
- **打开之后的新威胁面**：能在本机改 `configs/config.yaml` 的人，从此不必重启就能改变并发数、
  留痕策略与**可执行档位的内容**。这条边界本来由"改配置的人 = 有部署权限的人" implicit 保证，
  热重载把它压缩成"改文件的人"。这条压缩的确切成立范围要说清，不要藏：
  - **原地改一条既有档位或凭据的字段**：拒绝档严格执行——`Diff` 只要检测到身份/目标/凭据字段
    （§6.3、§6.4 的 `permissionCommandFields` 与凭据前缀）变化就整次作废、一项都不应用，
    这类变更仍然只能靠重启生效。
  - **新增一条档位、或把一条既有档位改名再配新 `script`**：这属于本次已拍板的热更范围（§6.4 把
    "条目增删""重命名"明列在允许热更一侧），**可以在本机免重启新增一条档位**。它的约束不来自拒绝档，
    而来自两处：顶层许可字段 `executors.workspace`/`runtime_allow`/`env_allow` 是重启档（改了不生效，
    运行期用的仍是冻结在启动那份配置里的值），以及 §7.5/R04 加载新批次时固定用这份冻结值做
    `executor.LoadProfiles` 的越界拒绝——新档位若越出冻结 workspace 或 `runtime` 不在冻结
    `runtime_allow` 里，整次失败。因此"改文件的人免重启新增档位"这件事被限定在**既有许可范围内**，
    不能借热更扩边界。兜底位置在 R04 的 `ApplyConfig`，其验证面见 `task-r04` §5.2 新增用例。
- **谁有权触发**：只有文件系统。没有新端点，因此没有新增鉴权面；`/admin/runtime` 的读权限不变。
- **审计**：重载本身不写 `write_audit`（那张表记的是 API 写操作，口径见
  `docs/design/sqlite-observability-design.md` §6.3），它的痕迹在日志与 `/admin/runtime`。
  如果后续要做"配置变更台账"，那是新表新卡（§10）。
- **不做的事**：不回写配置文件（全仓至今没有配置写回代码，见 §3 末段那条盘点的延伸——本设计继续保持）、
  不热更凭据、不热更监听地址。

## 10. 明确不做（登记为后续卡）

| # | 项目 | 为什么本期不做 |
| --- | --- | --- |
| N1 | 执行器池的运行期扩缩与"从无到有建池" | `execCh` 为 nil 时建池要同时改 `SetExecConcurrency` 的 Start 前置约定、`execPoolEnabled` 的判定方式（`core/scheduler.go:1202-1204`）与产物存储的装配分岔，改动面大于本文其余部分之和 |
| N2 | `executors.concurrency/required_role/default_timeout/max_timeout` 热更 | `concurrency` 属于 N1（要能建池才有意义）；`required_role` 与两个 timeout 都固化在 `Registry` 构造时的字段与那份冻结进闭包的 `ExecutorsConfig` 里（`executor/registry.go:102-110`），而提交期与执行期的超时合成又分别读这两处（合成规则见 `core/config.go:230-236` 的注释与`executor.Profile.timeoutWithin`），热更它们要同时改 `Registry` 快照、全部档位闭包与生效超时的合成入口，一处漏改就是"接口显示新上限、执行按旧上限"。本期由重启档明确挡住 |
| N3 | `store.flush_interval`、`observability.flush_interval/queue_capacity` 热更 | 换 ticker 与换有界通道都涉及正在等待的一方 |
| N4 | 鉴权运行期热更（users/token/jwt.secret） | §6.3 |
| N5 | 配置重载的专门读端点（`GET /api/v1/config`）与手动触发端点 | R2、R9：本期只要日志与 `/admin/runtime` |
| N6 | 配置变更台账（谁在什么时候把哪个键改成了什么）入 SQLite | 需要新表与新审计口径，且磁盘文件本身没有作者信息 |
| N7 | 目录加载器的防抖与本 `reload.debounce` 合并成一个 knob | 两者的失败代价不同（§5.1），合并会让人误以为改一处两处都变 |

## 11. 实施计划

按本仓既有系列逐卡交付，卡内改动可独立验证，卡间不共享未交付的能力：

| 卡 | 内容 | 依赖 |
| --- | --- | --- |
| TASK-R01 配置节与分档表 | `reload` 一节（两份 YAML + `DefaultConfig` + `Validate` + 环境变量绑定表）、`core/config_reload.go` 的 `ConfigClass`/`configClasses`/`Diff`/`ReloadState`、两条守卫测试 | — |
| TASK-R02 轻量 setter | `SetLogLevel`（含 LevelVar 改造）、`Scheduler.SetRetryPolicy`、`JSONFileStore.SetHistoryRetention`、两个 sqlite 写入器的 `SetRetention`，各配单测 | R01 |
| TASK-R03 调度器运行期扩缩 | `ResizeWorkers` + target 计数 + `RuntimeStats.Workers` 读数口径，含 §8 里那两条并发测试 | R02 |
| TASK-R04 档位配置整表替换 | `Registry.ApplyConfig`、`Applier.ApplyConfig`、`main.go` 的 Applier 构造条件放宽 | R01 |
| TASK-R05 监听器 | `core/watch.go`（目录监听 + 防抖 + 状态保存），watcher 单测用真实临时文件；不比内容，等价由 `Diff` 判 | R01 |
| TASK-R06 接线 | `cmd/server/reload.go` 的 `reloader`（分派表 + 逆序回滚）、`runtimeDeps` 的新字段、`/admin/runtime` 的 `reload`、关闭顺序 | R02-R05 |
| TASK-R07 端到端验证与文档收口 | 集成用例（真文件驱动各类成功/失败路径）、`gofmt`/`go vet`/`-race -count=5 -timeout 30m`、`-tags dashboard` 内嵌形态冒烟、既有文档的表述同步与偏离标注 | R06 |

七张卡：R01 先行（判据），R02–R05 都是落点、可并行（R03/R04/R05 只依赖 R01 的分类表，不依赖 R02），R06 收拢成一条链，R07 收口。卡间依赖以 `docs/design/tasks/config-reload/README.md` 的执行顺序表为准。

## 12. 风险与后续演进

| 风险 | 说明与对策 |
| --- | --- |
| Windows 事件形状不可预测 | 靠"防抖窗口 + `Diff` 判等价（等价内容回 `unchanged`）"兜住多余事件；半截文件靠一次失败的重载 + 下一次事件恢复，不靠猜窗口长度 |
| 回滚链比应用长 | 每个 apply 分支必须自带旧值与反向操作，`reloader` 的形态是"一张表 + 逐项 undo"，而不是顺序 if；R06 的验收要求逐键断言回滚 |
| 重载与页面档位写入撞上 | 两条链都要走 `Applier` 的同一把写锁（`executor/applier.go:116-117`）；R04 必须复用该锁而不是新建一把 |
| 读数与真值短期不一致 | 缩容退场窗口内 `Running` 可能暂时高于 `Workers`；`RuntimeStats` 的注释要写明"Workers 是期望并发、Running 是瞬时值" |
| 环境变量与文件谁赢造成困惑 | §5.3：本期只在文案提示，不新增来源判定；若后续要做，方向是在 `ReloadState` 里带一份"哪些键被环境覆盖"的只读清单 |
| 免重启新增档位是"改文件的人"绕开重启的一条口 | §6.4 已拍板"条目增删/重命名可热更"，因此能改本机配置文件的人可以免重启新增一条档位。兜底不在 `Diff` 的拒绝档（原地改字段才进拒绝档），而在 R04：`ApplyConfig` 加载新批次固定用冻结在启动那份配置里的顶层 `workspace`/`runtime_allow`/`env_allow` 做 `LoadProfiles` 越界拒绝，新档位越界即整次失败、生效表与处理函数一字不动。验证面见 `task-r04` §5.2 新增用例；完整口径见 §9 |

## 13. 待拍板（写卡时按推荐值落的，执行前请复核）

| # | 问题 | 本设计的取值 | 另一选择 |
| --- | --- | --- | --- |
| P1 | `Store` 接口是否加 `SetHistoryRetention`（会让所有测试替身一起实现） | 加，与既有 `SetExecConcurrency` 一组接口同法 | 只在 `*JSONFileStore` 上提供，`reloader` 里做类型断言 |
| P2 | `Applier` 构造条件放宽后，`web_enabled=false` 但 `reload.enabled=true` 时是否仍不打开档位文件 | 构造 `Applier`，但**不打开档位文件**：给它一个不依赖 store 的构造入口，`Apply`（store 路径）在那种部署里返回明确错误、`ApplyConfig` 照常可用。选这条的理由是 W01 的冒烟证据——`web_enabled=false` 的部署今天连 `profiles_path` 的父目录都不碰，为热更档位而去读它会破掉"关闭即惰性"。撞名判定在那种部署里只看 config 批次，与今天的运行态一致 | 只处理 config 批次、撞名判定推迟到下次启动 |
| P3 | `reload.debounce` 默认值 | 500ms（比目录加载器的 100ms 宽：配置改错的代价高于多等 400ms） | 100ms，与 `core/load.go:94` 对齐 |
| P4 | 被环境变量压住的键是否在日志里单列 | 不单列，统一走"本次无变化"文案（§5.3） | 单列一份 `overridden_by_env` 清单，需要在 `LoadConfig` 侧新增来源信息 |

## 14. 与实现的偏离（R07 收口时逐条核对，2026-10-04）

本节只记两类事：设计文本与落地代码不同的地方，以及任务卡的前提与实测不同的地方。
行文里的行号是 2026-10-04 复核时的位置，之后的改动会让它漂移，所以每条都同时给了符号名。

### 14.1 待拍板 P1–P5 的实际落地答案

| # | 设计里的推荐值 | 落地答案 | 证据 |
| --- | --- | --- | --- |
| P1 | `Store` 接口加 `SetHistoryRetention` | **照推荐落地**：接口方法在 `core/store.go` 的 `Store` 里（`SetHistoryRetention(limit, ttl)`），实现是 `(*JSONFileStore).SetHistoryRetention`；测试替身按拍板时预见的代价一起补上了实现（`core/scheduler_test.go` 的 `mockStore`） | R02 卡 §10；R07 场景 8 实测（`store.history_limit` 1000→5 之后 `GET /api/v1/jobs?status=success` 的条数与 `/stats` 的 completed 同时收到新上界） |
| P2 / README P5 | 构造 `Applier` 但不打开档位文件 | **照推荐落地**：新增不依赖 store 的构造入口 `executor.NewConfigApplier`，`Applier.Apply()`（store 路径）在那种部署里返回明确错误、`ApplyConfig` 照常可用；`cmd/server/main.go` 的构造条件是 `web_enabled \|\| (reload.enabled && executors.enabled)`，档位文件的打开条件一字未改 | R04 卡 D-R0403 登记了"`Apply()` 恒返回错误"这一后果并明确不修；R07 场景 13 在 `executors.enabled=false` 的部署里实测到档位改动只记不生效 |
| P3 | 默认 500ms | **照推荐落地，并另加一条下界**：`core/config.go` 的 `DefaultReloadDebounce = 500 * time.Millisecond` 与 `minReloadDebounce = 50 * time.Millisecond`，后者由 `Validate` 拦（R07 场景 2 实测：`debounce: 10ms` 让进程启动即失败，错误文案同时给出下界与省略本项时的取值） | R01 卡；R07 场景 2 |
| P4 | 不单列 | **照推荐落地**：全仓没有 `overridden_by_env` 这个名字（代码与文档一起 grep 过），环境变量压住的键走"本次无变化"那条结论。R07 场景 4A 另外测到一个更强的事实：**没有配置文件时环境变量整体不生效**（`LoadConfig` 只在读通文件那一支调 `UnmarshalExact`），所以这条拍板在今天那种部署里连"被压住"的机会都没有，见 D-R0706 | R07 场景 4A |

### 14.2 落地形状与设计文本不同的地方

1. **§5.4 的响应字段类型**：设计写的是 `RuntimeResponse.Reload *core.ReloadState`，落地是 `api` 自己声明的
   `*ReloadStatus`（`api/reload_state.go`）。原因是 R05 交给 R06 的 D-R0502——`time.Time` 上的
   `omitempty` 不生效，直接序列化 `core.ReloadState` 会把"从没应用过"落成 `"0001-01-01T00:00:00Z"`。
   现在两个时间字段是 `api` 侧自己格式化的 RFC3339Nano 字符串（`formatReloadTime`），零值给空串并整个键缺省。
   `core.ReloadState` 仍然留在 `core`，`api` 靠本地的 `ReloadStateReader` 接口拿它，依赖方向没变。
2. **§5.4 的 `enabled` 出处**：落地是"建链时钉一次"的进程配置值（`WithReloadState(reader, enabled)` 的第二个
   参数），不是 `State().Enabled`（D-R0503 的具体处置）。注入条件与"配置里开没开"绑，不与"watcher 建没建起来"绑。
3. **§7.8 的关闭顺序**：设计写的是"SIGINT 之后、`server.Stop` 之前显式 `watcher.Close()`"三步。
   落地是四步：`watcher.Close() → chain.Stop() → server.Stop(ctx) → scheduler.Stop()`（`cmd/server/main.go`
   末段，注释就在那儿）。多出来的 `chain.Stop()` 拿一次重载链的串行锁，等的是在途那次重载走完——
   R05 的 `Close` 明确不等 `ReloadFunc`，少这一步就会出现"关闭过程中档位还在注册"的交错。
4. **§7.5 的签名核对**：`Registry.ApplyConfig(profiles []*Profile) error`（`executor/registry.go`）与
   `Applier.ApplyConfig(candidate core.Config) (ApplyResult, error)`（`executor/applier.go`）与设计文本一字不差；
   落地多出来的是构造入口（见 14.1 的 P2）与"冻结值取自哪一份"的注释修正，不是签名。
5. **§7.6 的扁平化实现位置**：摊平在 `core/config_reload.go` 里，不在 `LoadConfig`——
   `flattenLeaves` 是入口，`flattenStruct`（递归结构体）与 `flattenCommands`/`commandEntryName`（档位列表按
   `executors.commands.<名字>.<字段>` 摊平）是两支，`splitCommandLeaf` 反过来把路径拆回名字与字段，
   `Diff` 拿两份叶子表比对、`classifyChange` 决定归属档位。
   "每个叶子键都必须有且只有一档"由 `TestEveryLeafKeyIsClassed` 守着（§8 第二条）。
6. **§9 与 §12 关于 `env_allow` 的说法已按实测收窄**（R04 登记的 D-R0404）：`LoadProfiles` 只看每条档位自己声明的
   `env_allow`，顶层那份全局 `executors.env_allow` 从不进那次校验，它的唯一读取点是执行期的 `executor/env.go`。
   免重启新增档位真正的兜底是两条：`workspace`/`runtime_allow`/两个 timeout 由 `ApplyConfig` 用冻结值校验，
   而 `env_allow` 靠处理函数闭包钉住（`syncHandlers` 传的是 `executorsNow()`）。本文 §2 落地块 R8 那条已改写成这个形状。
7. **§8 验收清单里的 `/pools` 端点在本仓不存在**（D-R0701）：那条判据改取
   `GET /api/v1/admin/runtime` 的 `scheduler` 对象——同一份 `RuntimeStats`，`workers`/`queue_capacity`/
   `exec_workers`/`exec_queue_capacity` 都在。§8 那一行已就地改正。

### 14.3 场景实测暴露出来的读数语义

1. **`applied_keys` 要读作"本次重载处理过的键"**（D-R0605 的处置）：`executors.enabled=false` 时改档位，
   键仍然进 `applied_keys`、`last_applied_at` 也会推进，随行的是一条 warn 说明这一节没打开（场景 13 实测）。
   §5.4 已经按这个口径补了说明。
2. **档位改动的键名是逐字段摊平的**（D-R0704）：加/删一条档位时 `applied_keys` 给的是
   `executors.commands.<名字>.<字段>` 一串，不是卡面写的裸键 `executors.commands`；
   只有整列表被清空那种才会看到容器路径本身。判据要按"含这一族前缀"写。
3. **`rejected_keys` 是那一次重载里"与现网不同的全部拒绝档键"，是累计的**（D-R0705）：场景 15 连着改三次凭据，
   第二次的清单是 `server.auth.token, server.auth.users`，第三次是三条全在。读它的人要知道这解释的是
   "本次作废的原因"，不是"你刚改的那一条"。
4. **`last_applied_at` 只属于成功的那一次尝试**（D-R0707）：一次 `ok` 之后再遇到 `rejected`/`failed`，
   读数里这个键会消失（状态是按次替换的整份快照）。它不是"历史上最后一次应用"的台账，运维要看历史得翻日志。
5. **防抖语义**（场景 22）：`reload.debounce` 可以热更，`core.ConfigWatcher.SetDebounce` 对**下一次排期**生效，
   正在计时的这一次不改窗口（`core/watch.go` 里那条注释）。实测形状：800ms 窗口把间隔 400ms 的两次写入合并成
   一次重载（`last_attempt_at` 只推进一次），同一个 400ms 间隔在 200ms 窗口下给出两次——所以"合并与否"判据
   取的是窗口长度与写入间隔的大小关系，不是"两次写入是否挨着"。
6. **不再表达任何取值的文件不会把现网换成默认值**（D-R0702，本卡的代码改动；BOM/UTF-16 那一半是 D-R0709、
   标记与流式写法那一半是 D-R0711、"有键名没取值"与多文档那一半是 D-R0715）：
   重载链在第 1 步读文件之前先做"这份文件还表达任何取值吗"的判断
   （`cmd/server/reload.go` 的 `configFileCarriesNoValues` + `yamlCarriesNoValue`：**按 YAML 结构判**——
   用 viper 同一族解析器把整份解析成 `any`，顶层是 nil、或映射里每个叶子都是 nil / 空映射，就算没表达；
   标量与序列（哪怕 `commands: []`）算作者写下的取值；解析失败或文件读不出都**不判**，交回第 1 步那条统一口径）。
   前两版是逐行扫文本，判不到这一族，第三轮复核才换到结构层。场景 16/16E 实测：
   现网 `scheduler.workers` 停在文件里写的 7（代码默认 100），级别维度的证据是随后整份重写正常配置时
   `result=ok` 且读数按新值换掉（说明监听器活着）。
   配套用例是 `cmd/server/reload_test.go` 里那七条（链上五条 + 正向对照一条 + 直接量判据的一条，
   合计 54 条可计数用例，清单在 R07 卡 §10.3）；判红点由七条变异 M1–M7 给出、每条的原始输出留档，
   还原后 `reload.go` 的 sha256 与备份逐字一致。真进程侧证据是卡 §10.4 的 16F / 16G / 16H。
7. **只写一部分键的文件仍会把没写的键退回默认值**（D-R0703，登记不修）：这是"整份文件是唯一真相"加上
   R04 整表替换语义的必然结果，不是这条守卫能管的范围——它只认"一个取值都没有"。
   `docs/deployment.md` 的运维提示里写了"删掉一个键等于把它改回默认值，要退回默认得显式写出每一项并重启"。

### 14.4 R05/R06 交给 R07 的携带项的处置

| 来源 | 内容 | R07 的处置 |
| --- | --- | --- |
| R02 的 D-R0203 | "设计 §6.1 把 `store.flush_interval` 列为热更键，与代码冲突，R07 要把它改到重启档" | **前提不成立**：初稿提交 `9909962` 的 §6.1 表就只有那 9 行热更键，`store.flush_interval` 从初稿起就在 §6.2 重启档那一串里（`git show 9909962:docs/design/config-reload-design.md` 可查）。没有任何文本要改，代码现状（`ClassRestart`）本来就与设计一致 |
| R05 的 D-R0502 | 时间字段序列化形状 | R06 已收口（`api.ReloadStatus` 自格式化），本文 §5.4 的代码草图保留原样并在 14.2 第 1 条标注偏离 |
| R05 的 D-R0503 | `enabled` 的出处 | R06 已收口（`WithReloadState` 的第二参数），见 14.2 第 2 条 |
| R06 的 D-R0601 | 一次失败的重载留两条 error 级日志 | 已写进 §5.4 的约束段与 `docs/deployment.md` 的读数一节：两条归因不同（步骤级 / 进程级），按 `path` + 时间窗去重 |
| R06 的 D-R0602 | 卡面"stdout 出现 debug 行"的判据在默认功能面达不成 | R06 期间已就地换成"级别拧到 error 后 INFO 行不再增长、拧回恢复"，R07 场景 5 用同一形状复测通过 |
| R06 的 D-R0603 | `-config` 留空时只补 `.yaml`/`.yml` 两种拼写，viper 实际还认 `.json`/`.toml`/… | **维持登记不修**：本仓文档、示例与 `.gitignore` 只承诺 `configs/config.{yaml,yml}`。R07 场景 4B 实测到那条盲区的现场形状：目录里只放 `config.json` 时进程按"没读到配置文件"走，`reload` 读数给出 `watcher_error` 而不是静默失灵；已把这一条写进 `docs/deployment.md` 的运维提示 |
| R06 的 D-R0604 | 手搓 `reloadDeps` 时"没有 Applier 但 executors 开着"会被算成"本节未启用" | **维持登记不修**：真实装配走 `cmd/server/main.go` 的构造条件到不了这一格（14.1 的 P2 那条条件必建 Applier），要改得先回答"Applier 建不出来时 executors 那一节算什么"，与本系列的三档结论冲突 |
| R06 的 D-R0605 | 执行器未启用时档位改动仍进 `applied_keys` | 已按"读数读作处理过的键"收口（§5.4 说明段 + 14.3 第 1 条 + `docs/api.md` 的字段表），代码不改 |
| R06 的 D-R0607 | §5.4 承诺 `error` 不含取值 | 已收窄成"不含**凭据**取值"（§5.4 约束段），非凭据取值（写错的时长、撞名的档位名）由产出方回显，`watcher_error` 同样是自由文本 |

### 14.5 R07 新登记的缺陷

| 编号 | 现场 | 处置 |
| --- | --- | --- |
| **D-R0701** | 卡 §3.1 与 §3.4 的判据写的是 `GET /api/v1/pools`，本仓没有这个端点（实测 404；`api/pools_stats_test.go` 测的是池的概念，不是路由） | **卡面就地改正 + 文档同步**：判据改取 `/admin/runtime` 的 `scheduler` 对象；§8 那一行同步；设计文档 §3 的现状盘点不受影响（它没提过这个端点） |
| **D-R0702** | 配置文件不再表达任何取值时，重载链把"整份读通"当成一份全默认配置接受，`result=ok` 并把现网取值换回代码默认——有凭据的部署只是碰巧被拒绝档挡住，没凭据的部署（场景 16E 那种）会静默丢掉并发数、级别与档位表 | **本卡已修，改了三轮才修到底**：`cmd/server/reload.go` 的 `configFileCarriesNoValues` + `yamlCarriesNoValue`，在第 1 步之前**按 YAML 结构**判"还表达取值吗"，判据侧七条用例、判红点七条变异（R07 卡 §10.3，每条留原始输出），真进程复测见卡 §10.4 的 16/16E/16F/16H。第一版按文本判，留下 D-R0709（BOM/UTF-16）、D-R0711（标记与流式写法）、D-R0715（只有键名 / null / 空映射 / 多文档）三条，三条都记在本表里 |
| **D-R0709** | D-R0702 第一版判据自身的漏口（第一轮 fresh-context 复核发现）：`strings.TrimSpace` 不认 U+FEFF，而 YAML 解析器容忍 BOM 与 UTF-16，所以"带 BOM 的只有注释"与 Windows PowerShell 5.1 的 `>` / `Out-File` 默认写出的带 BOM UTF-16LE 空文件会被判成"有内容"放过——守卫注释里举的那个现场正好从它脚边漏过去 | **本卡已修，修法后被 D-R0715 的重写吸收**：当时补的是 `configFileText`（自己还原三种 BOM/UTF-16 前缀 + 分隔符行按前缀认），终态里这个函数已删除——判据改用解析器之后这些编码由解析器自己认。这一族的复测留在 16F 与单元层的 BOM/UTF-16 四格 |
| **D-R0703** | 只写一部分键的文件会把没写的键退回默认值 | **登记不修**：整份文件是唯一真相 + R04 整表替换的必然形状，动它等于改本系列的语义基线。处置是文档：`docs/deployment.md` 运维提示第 3 条 |
| **D-R0704** | 卡 §3.1 场景 11/12/13 的判据"`applied_keys` 含 `executors.commands`"与落地不符：档位改动按逐字段摊平给出一串 `executors.commands.<名>.<字段>` | **卡面判据改正**（按前缀族判），并写进 14.3 第 2 条。不是代码缺陷：R01 的 `Diff` 判定单位就是叶子键（§6 开头那句） |
| **D-R0705** | 卡 §3.1 场景 15 的判据"`rejected_keys` 各自给出对应键名"与实测不符：清单是那一次里全部与现网不同的拒绝档键，累计给出 | **卡面判据改正** + 写进 `docs/api.md` 的字段表与 14.3 第 3 条。不是代码缺陷：`Diff` 就是按"applied vs candidate 的整份差"判拒绝档 |
| **D-R0706** | 卡 §3.1 场景 4 的"默认值部署"想用环境变量打开 `reload.enabled`，实测环境变量在**没有配置文件**时整组不生效（`LoadConfig` 只在 `ReadInConfig` 成功那一支走 `UnmarshalExact`） | **登记不修 + 文档**：这是 `LoadConfig` 的既有行为、不属于本系列范围；场景 4 拆成 4A（无文件：环境变量与开关都无从生效，进程按代码默认跑）与 4B（有文件但拼写不是 `.yaml`/`.yml`：走到 R06 §5.2 #13 那条 warn）。`docs/deployment.md` 的运维提示里补了同一件事 |
| **D-R0707** | `docs/api.md` 的字段表把 `last_applied_at` 写成"最近一次应用成功的时刻；只在 `result=ok` 时推进"，读起来像历史值；实测一次失败的之后这个键消失 | **文档已改**：写成"本次这一次尝试里应用成功的时刻；随后的失败重载不再给这个键，要历史请翻日志"，并登记在 14.3 第 4 条。代码不改（按次替换整份快照是 R05/R06 定下的形状，改成历史台账需要新的字段与新的口径） |
| **D-R0708** | 三处测量工具自己的错（不是产品行为）：`/api/v1/events` 的响应形状是 `{count,items,note}`、没有 `total`，场景 9 的第一版判据却拿 `total` 比；harness 里 `SetConsoleCtrlEvent` 在 Win32 根本不存在（投递 Ctrl+C 的那个 API 叫 `GenerateConsoleCtrlEvent`，前者是 pywin32 的包装名，ctypes 取不到）；场景 21 用 API 去读强杀之后的状态，而那时进程已经没了 | **改判据重跑**：事件按 `count`、台账按 `total`；Ctrl+C 换成 `GenerateConsoleCtrlEvent` 并打出现场三个返回值（结论见卡 §10.6 乙：这台机器投不进去）；场景 21 改成直接读 `data/jobs.json` 并把整数状态按 `core/job.go` 的 iota 翻回名字 |
| **D-R0710** | D-R0702 那道守卫自己的注释过度承诺：写着"两次读之间被截断由 `Diff` 与拒绝档兜住、所以这里不加锁"，而空文件读出的全默认配置与生效那份必然不一致，`Diff` 只会把它当一批热更键应用掉——注释描述了实现没有提供的保证（第二轮 fresh-context 复核发现） | **本卡已修（只改注释）**：如实写成"窗口微秒级、撞上那一次本判据补不回来、下一次事件再判一次；真要收口得让 `core.LoadConfig` 接受内容入参"，那是 `core` 的接口改动，超出 R07「零生产代码变化」的目标 |
| **D-R0711** | D-R0709 的"前缀匹配"改出了过头判据：`strings.HasPrefix(trimmed, "---")` 会把 `--- {logging: {level: debug}}` 这种标记后紧跟真取值的整行连内容一起跳掉，于是**一份有内容的单行流式配置**被判成"没有取值"而 `rejected`——守卫反过来误伤合法配置 | **本卡已修，同样被 D-R0715 的重写吸收**：当时补的是 `cutDocumentMarker`（剥掉标记后再看剩下的是什么），终态里这个函数已删除——标记、流式写法、多文档这些边界本来就该由结构解析回答。保留的形状搬进了 `TestConfigFileCarriesNoValuesShapes`，真进程侧是 16G |
| **D-R0712** | 记录侧三条：变异名单只记数不记红名单、旧编号在终态字节上重跑时没写映射；卡内抄的验收 grep 命中清单按行号引用（本卡每编辑一次就漂一次）并漏记本卡自己的自命中；§10.3 的轮次说明留着占位读数 | **本卡已修（记录）**：grep 命中清单改成按内容引用、自命中逐条列出；轮次说明换成真实读数。变异那一条随判据重写作废——文本判据那套 M1/M2/M3b/M5/M6 整体不再有意义，终态名单是 M1–M7，红名单与原始输出一起留档（见 D-R0716） |
| **D-R0713** | 流程侧：第三轮全量验证（00:59:38 起跑）与一条变异的还原过程重叠，无法证明那一轮编译读的是终态字节；同时验证脚本每轮截写同一个输出文件名，把最早那一轮的读数覆盖掉了 | **本卡已修**：判据重写之后在"已还原并核过 `sha256`"的字节上整轮再跑一遍（第五轮），卡与状态表只抄这一轮；每轮一个新文件名不再复用。口径同 R05 的 D-R0515：**变异与全量验证必须串行**，并行时任何一轮都不能当终态证据 |
| **D-R0714** | 真进程的 22 条场景与 16E 跑在 00:44 那次构建的 `server.exe` 上，而第二、三轮又改过 `cmd/server/reload.go`，那一批读数字面上不是"终态字节"下的读数 | **登记 + 补测**：卡 §10.4 现场段如实写明新旧边界；01:45 用终态字节重建 `server.exe` 后重跑 16F、16G 并新增 16G 之外的 16H（专测 D-R0715 那一族）。1–22 与 16E 没有全批重跑，理由写在同一节：重写只改变"哪些文件被判成没取值"，那一批里只有 16 那一族走这条判据 |
| **D-R0715** | 第三轮 fresh-context 复核发现：文本层的判据**判不到根**。`logging:` + `level:`（只有键名）、`scheduler: {workers: null}`、`logging: {}`、顶层 `null`、层层都是空键、以及"值写在第二份文档里"这六族，文本上有内容、`core.LoadConfig` 那里一律是一份全默认配置，链照走、`result=ok`、现网被换成代码默认——D-R0702 原样复现。复现方式：临时判据用 `go test -overlay` 挂进 `cmd/server`（不落仓库文件），同时问判据、`LoadConfig` 与整条链。判据侧另有三个洞：`configFileCarriesNoKeys` 返回 error 那一支零覆盖、守卫的日志与 `hint` 两行没有逐字断言、"该放过"的正向半边不覆盖空序列与标量 | **本卡已修**：判据换成按 YAML 结构判（用的就是 viper 那一族解析器，所以第 0 步与第 1 步答的是同一个问题），`configFileText` / `cutDocumentMarker` 一并删除，`gopkg.in/yaml.v3` 从 indirect 提到直接依赖（`go.sum` 未变，`go mod verify` + 交叉构建都跑过）。判据侧补三条：链上的 `TestReload_ValuelessFileIsSameConclusion`（7 格，每格先核 `LoadConfig` 真能读通）、`TestReload_StepOneShapesKeepTheirOwnWording`（Tab 缩进 / `----` 开头 / 不带 BOM 的 UTF-16 / 文件被删，判"两种脸色不串门"）、单元层的"文件不存在交回错误"那一格；守卫的日志与 `hint` 改成逐字断言 |
| **D-R0716** | 记录不可复算（本轮复核发现）：卡说"四轮读数在四个文件里"，实际脚本每轮截写同名文件、最早一轮已被覆盖，两份文件是同一轮；变异名单说"脚本在 mut5.py / mut6.py"，而 mut5.py 的锚点在终态字节里不存在（它跑的是第一轮那份备份）、mut6.py 的循环是 `MUTS[1:]`（M1 从没被它跑过），七条变异都没有原始输出留档；另有三处小口径不一致（`0.333s` 与留档的 `0.258s`、把"工作树是 CRLF"的提醒用在这三个 LF 文件上、丁 那条补充 grep 只举三处命中而实际二十来行） | **本卡已修（记录）**：验证每轮一个新文件名、只认有留档的那一轮；变异在终态字节上整批重跑成 M1–M7，每条的原始输出留档在 `r07smoke/mut_final/M<n>.txt`，红/绿数与首个红消息由脚本从原始输出算出；三处小口径逐条改成与留档一致。旧的那套文本判据变异随实现一起作废 |
| **D-R0717** | 跨文档矛盾（本轮复核发现）：设计文档 §2 落地位置 R3 仍写"七步固定顺序"（代码是八步）、§14.3/§14.5 写"三条用例 + 四个变异体"（终态是七条用例 + 七条变异）、R07 卡 DoD 里"设计文档 §14.5 每行都带前缀"不成立（那张表本来就是裸编号，前缀规则管的是卡内缺陷表）、本目录 README 的 R07 行写"两条补测 16E、16F"（终稿是三条，含 16H） | **本卡已修（文档）**：四处逐条对齐到终态；DoD 那句改成"卡内缺陷表每行带系列前缀，设计文档 §14.5 的编号本身已含系列号 R07"；`docs/deployment.md` 与 `docs/api.md` 里凡引用旧文案的地方一起换成终态那句 |
