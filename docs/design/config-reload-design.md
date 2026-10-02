# 配置热重载设计（改文件即生效，不必重启进程）

> 状态：**设计已评审，未实施**。评审拍板的四条口径标 ★，见 §2。
> 冲突处理：本文与 `web-profile-design.md` 在"档位怎么生效"上同源（共用 `executor.Applier` 那条链），
> 与 `executor-design.md`、`sqlite-observability-design.md` 不冲突——本文只改"配置取值什么时候被读"，
> 不改任何执行侧语义。
> 引用约定：正文只写 `文件`、`文件:行号` 与既有文档的 `§`，不写"上面那段"这类无法定位的指代。
> 行号按 2026-10-02 的代码基线核过。

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
| R2 ★ | **fsnotify 监听 + 防抖 + 内容哈希去重，不引入手动触发端点** | 与"改文件即生效"的诉求直接对应，且 `core/load.go:501` 已经在用同一套依赖、`loaderDebounceInterval`（`core/load.go:94`）已经确立了合并写入事件的写法。多一个 REST 触发口就多一份"谁有权改配置"的判定，本期不要 |
| R3 ★ | **全量校验后原子生效；拒绝档变化则整次作废** | 照抄 `executor.Registry.ApplyStore` 的既有承诺："失败时旧表原样不动，不会留下删了一半的中间态"（`executor/registry.go:143-153`）。重载不是逐键尝试，而是先把新配置整个读通（`LoadConfig` 的 `UnmarshalExact` + `Validate`，`core/config.go:806-813`），再过一遍拒绝档，然后逐项应用 |
| R4 ★ | **功能整体由 `reload.enabled` 控制，默认 false** | 与 `executors.enabled`、`observability.enabled`、`executors.web_enabled` 同一口径：打开前进程行为与本设计之前一字不差。热重载把"改文件要重启"这条人工防线撤掉了一道，而本系列的范围含 `executors.commands`（能执行什么的一部分），所以必须由部署方显式表态 |
| R5 | **`scheduler.workers` 做运行期扩缩，队列容量不做** | 通道容量在 `Start` 里一次定下（`core/scheduler.go:1141-1146`），投递方阻塞在 `s.workCh <- job` 上（`core/scheduler.go:1327-1332`）。换通道会让正在阻塞的投递永远等不到结果。扩 worker 不碰通道：容量本来就与 worker 数解耦 |
| R6 | **缩容用温和式：多出来的 worker 在下一次循环开头自己退出** | 与既有 `Stop` 的姿态一致（`core/scheduler.go:1206-1228`：不再投新任务、等在途收尾）。一次改配置不该取消一批正在执行的任务——执行器任务的外部副作用结果未知，这条判断 `executors.restore_policy: pause` 已经替本仓表过态 |
| R7 | **`executors.commands` 热更不要求 `executors.web_enabled`** | 两条链在 `Registry`/`Applier` 里本来就分得开：`ApplyStore` 只管 store 来源（`executor/registry.go:143`），config 来源是 `NewRegistry` 那一批（`:99-141`）。热更 commands 需要的是一次覆盖两批的整表替换，见 §6.5；不必为了复用同步器而把 web 写端点一起打开 |
| R8 | **执行许可字段进拒绝档**（档位内的 `deny_private_ranges` 等；顶层的 `executors.workspace`/`runtime_allow`/`env_allow` 在重启档，见 §6.4） | 处理函数把归一化后的 `ExecutorsConfig` 冻结在闭包里（`executor/register.go:87`、`executor/applier.go:175-183`），要让顶层许可字段对既有档位生效就得连它们一起重登记，"能执行什么"从此变成免重启通道。热重载范围已明确到"改档位内容"为止，改许可边界请重启 |
| R9 | **重载结果只加进 `GET /api/v1/admin/runtime`，不开新端点** | 该端点已是 ops 档专用（`api/server.go:293-299`、`api/handlers_admin.go:15-45`），且已经承载"这次运行到底什么状态"的读数（调度器占用、事件缓冲占用）。新增的是一份 `reload` 对象：上次重载时间、结论、错误文案、被忽略的重启档键名 |
| R10 | **`logging.level` 热更靠 `slog.LevelVar`，格式不改** | `parseLogLevel`（`core/logging.go:48-61`）已经在解析成 `slog.Level`，把 `HandlerOptions.Level` 从常量换成 `*slog.LevelVar` 即可运行期改级别，且不动 handler、不换 writer。`logging.format` 要换 handler 类型，牵连已在手的 logger 实例，归入重启档 |

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
Reload core.ReloadState `json:"reload,omitempty"` // reload 未启用时整个字段缺省
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
    // Result: ok | unchanged | rejected | failed | degraded
    Result        string   `json:"result"`
    Error         string   `json:"error,omitempty"`
    AppliedKeys   []string `json:"applied_keys,omitempty"`   // 本次真正生效的热更键
    IgnoredKeys   []string `json:"ignored_keys,omitempty"`   // 本次改了但需要重启的键
    RejectedKeys  []string `json:"rejected_keys,omitempty"`  // 本次导致整次作废的键
    WatcherError  string   `json:"watcher_error,omitempty"`  // 监听器自己的故障
}
```

三条约束：不含任何凭据内容（键名可以有，值不能有，对照 `store/sqlite/audit.go` 台账
"不存请求体与 error 原文"的同一取向）；`Result` 是封闭枚举；这个对象是**进程内**状态，
不入库——观测层的 `write_audit` 只记写操作，重载不是写操作。

## 6. 分档表（本设计的核心交付）

判定单位是叶子键；`executors.commands` 整段作为一个热更单元（它内部字段的变更另按拒绝档判定，见 6.4）。

### 6.1 热更（重载后立即生效）

| 键 | 生效方式 | 落点 |
| --- | --- | --- |
| `logging.level` | `slog.LevelVar.Set` | `core/logging.go` 改造 + `core.SetLogLevel(logger, level)` |
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

- **允许热更的字段**：`timeout`、`args`/参数定义、`env` 键值、`name`/`description` 一类展示字段、
  条目的增删。
- **触发拒绝的字段**：档位内的 `deny_private_ranges`（http 档位），以及任何扩大执行能力的写法。
  顶层的 `executors.workspace`/`runtime_allow`/`env_allow` 本来就在重启档（§6.2），而 §7.5 的加载固定用
  启动时那份归一化配置，所以它们改了什么都不会进运行期——拒绝档在这里的作用是**给未来留字段余地**：
  `Diff` 对新旧两份配置各跑一次 `executor.LoadProfiles`（`executor/profile.go`），
  比较两份归一化结果里"执行许可相关字段"的投影，投影不同即拒绝。
  投影的构造必须显式列字段，将来新增字段默认落在"未列"的一侧，而"未列"的一侧由 §8 的守卫测试挡住
  ——这样加字段不会因为忘记归档而变成免重启的口子。

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
// 与 ApplyStore 同一套承诺：整批替换、失败时旧表不动、同批内重复键拒绝。
func (r *Registry) ApplyConfig(entries []*Profile) error
```

`Applier` 新增 `ApplyConfig(old, new core.ExecutorsConfig) (ApplyResult, error)`：

1. 用**启动时那一份**归一化 `ExecutorsConfig` 调 `LoadProfiles` 加载新 commands
   （R8：许可字段属拒绝档，因此这一份实例不会与文件里的许可字段脱节）。
2. 逐条 `Probe`，探测失败仍入表（与 `NewRegistry` 同一口径，`executor/registry.go:126-137`）。
3. 与现存 store 来源条目做撞名判定：新配置的某条与页面建的某条同名时，按 §5.2 的既有方向处理——
   config 赢、store 那条标 Degraded（复用 `MergeStoreProfiles`，`executor/merge_profiles.go:39`）。
4. `ApplyConfig` 整表替换 → 重登记**全部** `exec.` 前缀处理函数（config 与 store 两批都重登记，
   因为 config 那批的闭包按 §3 盘点过的方式建出来，整批重登记才能保证"生效表与调度器同一批"）
   → 摘除新表里已不存在的键。摘除与登记顺序照抄 `Applier.Apply`（先登记后摘除，`executor/applier.go:172-198`）。
5. 任一步失败：不碰表、不碰调度器，返回错误，由重载方回滚。

`cmd/server/main.go` 只有在 `executors.enabled=true` 时才有 `Applier`（`:596-608` 的分岔），
而 `executors.enabled` 在重启档，所以热更 commands 时 `Applier` 必然已在手；
`web_enabled=false` 时它也在——本设计把它从"只在 web_enabled 时构造"改成
"reload 需要或 web_enabled 需要时构造"，`core.ExecutorProfileStore` 的打开条件同步放宽，
两个写端点的 503 判定仍只看 `web_enabled`（§7.7），不受影响。

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
// 调用方（重载链）在 candidate 已 Normalized 的前提下使用它；生效成功后整体换入 candidate。
func Diff(applied, candidate Config) (ConfigChange, error)
```

扁平化复用 `core/config_test.go:437-444` 那段 `configKeys` 的做法：用 Viper 把两份 `Config`
各摊平成叶子路径 map 再逐键比。这个函数从测试文件提到 `core` 内部（同名，去掉测试专用假设），
测试与 `Diff` 共用一份，避免"测试认得的键"与"Diff 认得的键"两套。

### 7.7 `core/watch.go`（新增）：监听器

```go
// ConfigWatcher 盯一份 YAML，变化后按防抖窗口触发一次 onReload，并保存重载结果供读端点取用。
// 它不认识任何业务键，也不自己应用配置：应用归装配方，失败也归装配方。
func NewConfigWatcher(path string, debounce time.Duration,
    onReload func() error, logger *slog.Logger) (*ConfigWatcher, error)
func (w *ConfigWatcher) Run(ctx context.Context)
func (w *ConfigWatcher) SetDebounce(d time.Duration)
func (w *ConfigWatcher) State() ReloadState
// SetState 由重载链在每次尝试结束时调用（成功、无变化、作废、失败都调），
// 状态的产生方是那五个 setter 的调用方，不是 watcher——watcher 只负责保存与并发可读。
func (w *ConfigWatcher) SetState(ReloadState)
```

- 监听**文件所在目录**，只认目标文件名的 / 路径匹配的事件；`Rename` 按"文件被换掉"处理，
  处理后重新登记监听。与目录加载器的监听（`core/load.go:501` 起）共用依赖但不复用代码：
  那边的合并窗口是 100ms 且按"每个文件各处理一次"记账（`core/load.go:94`、`:271`、`:446`），
  这边要的是"一份文件、比内容、失败也要留痕"，形状不同。
- 事件类型不做筛选（Write/Chmod/Create/Rename 全触发一次防抖），靠内容哈希决定要不要真重载：
  编辑器与杀软在 Windows 上的事件形状不可预测，筛事件不如比内容。
- `Run` 内所有错误都记进 `State()` 的 `watcher_error` 并继续存活；
  watcher 彻底死亡（channel 关闭）时记 error，让"进程停在旧配置"这件事可见（I3）。

### 7.8 `cmd/server/main.go`：接线

在 `runtimeDeps` 里加一组**函数值字段**（沿用既有风格，`cmd/server/main.go:128-173`）：

```go
applyReload func(core.Config, core.ConfigChange) (appliedKeys []string, err error)
newWatcher  func(path string, debounce time.Duration, onReload func() error, logger *slog.Logger) (*core.ConfigWatcher, error)
```

`defaultRuntimeDeps` 里给出的 `applyReload` 就是 §4 那条链的逐键实现：
按 `change.Hot` 里的键名分派到 §7.1~§7.5 的五个 setter，并在同一步记录旧值用于回滚。
`run()` 在 `scheduler.Start()`、`server.Start()` 之后建 watcher、`go watcher.Run(ctx)`，
在收到 SIGINT/SIGTERM 时先停 watcher 再 `server.Stop`（顺序原因：重载可能重新登记档位，
而优雅关闭期间不该有配置变化挤进来）。

集成测试的 `spyScheduler`/假 watcher 通过这两个字段注入，与 `main_integration_test.go`
既有替身同一套路。

## 8. 验收清单

- [ ] `config.example.yaml` 与 `config.yaml` 都含 `reload` 一节，键一一对应
      （`TestExampleConfigMatchesLocal` 跑绿）。
- [ ] `TestEveryLeafKeyIsClassed`：从 `Config` 结构体摊平出的**每一个**叶子键都在 `configClasses` 里有且
      只有一档；`TestHotKeysAreEffective`：热更档的每个键都有一个对应的 `applyReload` 分派分支，
      漏一个即失败（防止"归档成热更、但没人应用"）。
- [ ] `reload.enabled=false`（默认）时：不建 watcher、`/admin/runtime` 无 `reload` 字段、
      全部行为与本设计之前一致。
- [ ] 改 `logging.level` → 日志级别立刻变；改 `logging.format` → 记入 `ignored_keys`，级别不受影响。
- [ ] 改 `scheduler.workers` 8→32→4：`/pools` 与 `/admin/runtime` 的 `Workers` 读数随之变；
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
- [ ] 同一次存盘触发多个事件 → 只重载一次；内容与 `applied` 等价 → `result=unchanged`，不重登记档位。
- [ ] 连续两次重载并发到达 → 被一把锁串行化，第二次能看到第一次的落盘结果。
- [ ] watcher 出错（监听器关闭）→ `watcher_error` 有值且记 error，进程不停摆。
- [ ] 优雅关闭顺序：先停 watcher，再关 server，再停调度器；关闭期间不再有注册动作。
- [ ] 文档同步：`configs/config.example.yaml` 注释、`docs/design/executor-design.md` 与
      `web-console-design.md` 里"改配置需重启"的表述加一句指向本文（哪些仍要重启、哪些不必）。

## 9. 安全模型

- **默认关**：`reload.enabled=false` 时本设计一个 goroutine 都不建（R4）。
- **打开之后的新威胁面**：能在本机改 `configs/config.yaml` 的人，从此不必重启就能改变并发数、
  留痕策略与**可执行档位的内容**。这条边界本来由"改配置的人 = 有部署权限的人" implicit 保证，
  热重载把它压缩成"改文件的人"。因此拒绝档必须严格执行：凭据与执行许可字段（§6.3、§6.4）
  仍然只能靠重启变更。
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
| N2 | `executors.concurrency/required_role/default_timeout/max_timeout` 热更 | `concurrency` 属于 N1（要能建池才有意义）；`required_role` 与两个 timeout 都固化在 `Registry` 构造时的字段与那份冻结进闭包的 `ExecutorsConfig` 里（`executor/registry.go:102-110`），而提交期与执行期的超时合成又分别读这两处（合成规则见 `core/config.go:230-236` 的注释与
`executor.Profile.timeoutWithin`），热更它们要同时改 `Registry` 快照、全部档位闭包与生效超时的合成入口，一处漏改就是"接口显示新上限、执行按旧上限"。本期由重启档明确挡住 |
| N3 | `store.flush_interval`、`observability.flush_interval/queue_capacity` 热更 | 换 ticker 与换有界通道都涉及正在等待的一方 |
| N4 | 鉴权运行期热更（users/token/jwt.secret） | §6.3 |
| N5 | 配置重载的专门读端点（`GET /api/v1/config`）与手动触发端点 | R2、R9：本期只要日志与 `/admin/runtime` |
| N6 | 配置变更台账（谁在什么时候把哪个键改成了什么）入 SQLite | 需要新表与新审计口径，且磁盘文件本身没有作者信息 |
| N7 | 目录加载器的防抖与本 `reload.debounce` 合并成一个 knob | 两者的失败代价不同（§5.1），合并会让人误以为改一处两处都变 |

## 11. 实施计划

按本仓既有系列逐卡交付，卡内改动可独立验证，卡间不共享未交付的能力：

| 卡 | 内容 | 依赖 |
| --- | --- | --- |
| TASK-R01 配置节与分档表 | `reload` 一节（两份 YAML + `DefaultConfig` + `Validate` + 环境变量绑定表）、`core/config_reload.go` 的 `ConfigClass`/`configClasses`/`Diff`、两条守卫测试、`configKeys` 从测试提到包内 | — |
| TASK-R02 轻量 setter | `SetLogLevel`（含 LevelVar 改造）、`Scheduler.SetRetryPolicy`、`JSONFileStore.SetHistoryRetention`、两个 sqlite 写入器的 `SetRetention`，各配单测 | R01 |
| TASK-R03 调度器运行期扩缩 | `ResizeWorkers` + target 计数 + `RuntimeStats.Workers` 读数口径，含 §8 里那两条并发测试 | R02 |
| TASK-R04 档位配置整表替换 | `Registry.ApplyConfig`、`Applier.ApplyConfig`、`main.go` 的 Applier 构造条件放宽 | R01 |
| TASK-R05 监听器 | `core/watch.go`（目录监听 + 防抖 + 哈希去重 + 状态），watcher 单测用真实临时文件 | R01 |
| TASK-R06 接线 | `runtimeDeps` 两个新字段、`applyReload` 分派表与回滚、`/admin/runtime` 的 `ReloadState`、关闭顺序 | R02-R05 |
| TASK-R07 端到端验证与文档收口 | 集成用例（真文件驱动各类成功/失败路径）、`gofmt`/`go vet`/`-race -count=5 -timeout 30m`、`-tags dashboard` 内嵌形态冒烟、既有文档的表述同步与偏离标注 | R06 |

七张卡，R02/R03/R04/R05 可在 R01 后并行。

## 12. 风险与后续演进

| 风险 | 说明与对策 |
| --- | --- |
| Windows 事件形状不可预测 | 靠"内容哈希决定是否真重载"兜住重复事件；半截文件靠一次失败的重载 + 下一次事件恢复，不靠猜窗口长度 |
| 回滚链比应用链更长 | 每个 apply 分支必须自带旧值与反向操作，`applyReload` 的形态是"一张表 + 逐项 undo"，而不是顺序 if；R06 的验收要求逐键断言回滚 |
| 重载与页面档位写入撞上 | 两条链都要走 `Applier` 的同一把写锁（`executor/applier.go:116-117`）；R04 必须复用该锁而不是新建一把 |
| 读数与真值短期不一致 | 缩容退场窗口内 `Running` 可能暂时高于 `Workers`；`RuntimeStats` 的注释要写明"Workers 是期望并发、Running 是瞬时值" |
| 环境变量与文件谁赢造成困惑 | §5.3：本期只在文案提示，不新增来源判定；若后续要做，方向是在 `ReloadState` 里带一份"哪些键被环境覆盖"的只读清单 |

## 13. 待拍板（写卡时按推荐值落的，执行前请复核）

| # | 问题 | 本设计的取值 | 另一选择 |
| --- | --- | --- | --- |
| P1 | `Store` 接口是否加 `SetHistoryRetention`（会让所有测试替身一起实现） | 加，与既有 `SetExecConcurrency` 一组接口同法 | 只在 `*JSONFileStore` 上提供，`applyReload` 里做类型断言 |
| P2 | `Applier` 构造条件放宽后，`web_enabled=false` 但 `reload.enabled=true` 时是否仍不打开档位文件 | 打开（`ApplyConfig` 需要 store 那一批参与撞名判定与整表重登记） | 只处理 config 批次、撞名判定推迟到下次启动 |
| P3 | `reload.debounce` 默认值 | 500ms（比目录加载器的 100ms 宽：配置改错的代价高于多等 400ms） | 100ms，与 `core/load.go:94` 对齐 |
| P4 | 被环境变量压住的键是否在日志里单列 | 不单列，统一走"本次无变化"文案（§5.3） | 单列一份 `overridden_by_env` 清单，需要在 `LoadConfig` 侧新增来源信息 |
