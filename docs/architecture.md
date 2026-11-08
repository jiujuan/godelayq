# 架构说明：分层、模块、依赖与主业务流程

> 状态：基于 main 分支（6bb34b3）的代码走读整理，2026-11-08。
> 引用约定：按**符号**锚定代码（如 `core/scheduler.go` 的 `scheduleLoop`），不写行号——行号会漂移，符号不会。
> 本文件回答"代码长什么样、为什么这么分层、一条任务从提交到终态经过哪些步骤"。
> 面向使用者的配置与接口说明见 `docs/deployment.md` 与 `docs/api.md`；各能力的立项与取舍见 `docs/design/` 下的设计文档。

---

## 1. 总体分层

整体是**洋葱式分层 + 组合根装配**，依赖全部单向朝下：

```
┌──────────────────────────────────────────────────────────┐
│  cmd/server          组合根（装配层）                       │
│  main.go 的 run / reload.go：不写业务，只做"接线"             │
│  runtimeDeps = 一组构造闭包 + 消费方接口（schedulerAPI 等）    │
└──────┬───────────────────────────────────────────────────┘
       │ 组装并注入
┌──────▼─────────────┐     ┌──────────────────────────────┐
│  internal/api      │     │  internal/web                 │
│  HTTP 接入层        │     │  前端 dist 嵌入（embed）        │
│  Gin REST/WS/SSE   │     │  零依赖，-tags dashboard 才有内容│
│  JWT/RBAC/限流/审计 │     └──────────────────────────────┘
└──────┬─────────────┘
       │
┌──────▼───────────────────────────────────────────────────┐
│  internal/executor   执行器层（可选，executors.enabled）      │
│  Registry 档位登记表 / proc+http 执行器 / ArtifactStore     │
│  / Applier 档位同步器 / adhoc 临时执行                       │
└──────┬───────────────────────────────────────────────────┘
       │
┌──────▼───────────────────────────────────────────────────┐
│  internal/core   核心域（零内部依赖，最底层）                  │
│  Scheduler / QuaternaryHeap(四叉堆) / Job / EventBus       │
│  Store、UserStore、GroupStore、ExecutorProfileStore 接口    │
│  Cron 解析 / 重试策略 / Config / 目录加载器 / 共享词汇          │
└──────▲───────────────────────────────────────────────────┘
       │ 隐式实现接口
┌──────┴───────────────────────────────────────────────────┐
│  internal/store     基础设施适配层                          │
│  engine  ── 全仓唯一 import 驱动的包（bun + modernc/sqlite    │
│             + pgx），方言可插拔（sqlite 默认 | postgres）      │
│  jobs    ── 任务快照库（store.type=sqlite 时替代 JSON）       │
│  mgr     ── 管理库（账号/分组/档位，mgrstore.enabled）        │
│  observe ── 观测库（job_events/artifact_index/write_audit） │
│  （JSON 文件存储实现在 core 内，作为默认后端）                  │
└───────────────────────────────────────────────────────────┘
```

## 2. 分层的两条支柱惯例

### 2.1 接口定义在消费方

这是全仓库统一的红线，各包 `doc.go` 都写明了。**实现方只做隐式实现，不导出"供某某使用"的接口类型**；是否满足接口由装配方（`cmd/server`）在编译期把关。

| 接口 | 定义在 | 实现在 |
| --- | --- | --- |
| `Store` / `UserStore` / `GroupStore` / `ExecutorProfileStore` | core（`core/store.go`、`core/user_store.go`、`core/group_store.go`、`core/executor_profile_store.go`） | core 的 JSON 实现 + `store/jobs` + `store/mgr` |
| `schedulerAPI` / `serverAPI` / `mgrAPI` / `eventLogAPI` / `auditLogAPI` / `observabilityDB` | `cmd/server/main.go` | `core.Scheduler`、`api.Server`、`mgr.Handle`、observe 各写入器 |
| `executor.ArtifactIndexer` | `internal/executor`（`artifact_index.go`） | `store/observe` |
| 事件/审计读取窄接口 | `internal/api` | `store/observe` |

好处：api 与 observe 各自依赖 core、**互不相认**（共享词汇 `Event`、`AuditEntry` 等都放 core）；store 包不需要知道任何调用方的存在；"句柄可替换"也让关闭顺序这类时序断言在集成测试里可观察（真实句柄换成记录调用流水的假句柄）。

### 2.2 依赖收敛：数据库驱动只有一个入口

`store/engine` 是全仓唯一 import 数据库驱动的包（`modernc.org/sqlite` 纯 Go 无 CGO；`pgx/v5` 走 PostgreSQL；查询层统一经 bun 查询构建器生成方言 SQL），且它对业务包**零依赖**——只提供四种机制：按方言打开连接（SQLite 的 PRAGMA DSN / PG 的 DSN）、连接参数读数、版本化迁移框架（`Migrate`，迁移 DDL 可按方言分支）、`*bun.DB` 句柄，不认识任何一张业务表。三个业务库各自用自己的迁移表名调用它。

这保证了：作为库引入 godelayq 的人（只用 core，见 `examples/demo1`）不会被任何驱动依赖传染；core、api、executor 与 examples 都不许引入驱动（各包 `package_guard_test.go` 在测试期钉住这条红线）。存储后端由配置节的 `dialect` 键切换（`sqlite` 默认 | `postgres`），同一套查询代码两个方言通用，方言分歧只允许存在于迁移 DDL 与打开路径。

## 3. 模块清单

| 模块 | 职责 | 关键符号 |
| --- | --- | --- |
| `cmd/server` | 组合根：配置 → 闭包装配 → 按序启动/关停 | `main`、`run`、`defaultRuntimeDeps`、`registerHandlers`、`installRestoreGuard`；`reload.go` 的热重载链 |
| `cmd/gensecret`、`cmd/hashpassword` | 运维小工具：生成 JWT 密钥 / bcrypt 哈希 | — |
| `internal/core` | 调度核心 + 领域词汇 | `scheduler.go` 的 `Scheduler`、`scheduleLoop`、`executeJob`；`heap.go` 的 `QuaternaryHeap`；`job.go` 的 `Job`/`JobSnapshot`；`event.go` 的 `EventBus`；`cron.go`、`retry.go` 的 `ExponentialBackoffRetry`、`config.go` 的 `LoadConfig`、`load.go` 的目录加载器、`store.go` 的 JSON 实现 |
| `internal/api` | HTTP 接入层 | `server.go` 的 `Server`/`NewServer` 与 Option 注入；`handlers_*.go` 各域端点；`sse.go`/`websocket.go`；`ratelimit.go`；`security.go` |
| `internal/executor` | 执行器档位（可选层） | `registry.go` 的 `Registry`；`register.go` 的 `Register`/`Handler`；`profile.go` 的 `Profile`；`proc.go`（进程执行 + 整树终止）、`http.go`；`artifact.go` 的 `ArtifactStore`；`applier.go` 的 `Applier`；`adhoc.go`；`merge_profiles.go` 的 `MergeStoreProfiles` |
| `internal/store/engine` | 公共访问底座：双方言打开 + 连接参数 + 迁移框架 + bun 句柄 | 唯一驱动入口；`engine.Open`、`Migration` |
| `internal/store/jobs` | 任务快照库（`store.type=sqlite`） | `jobs.Open`、`ImportOnce` |
| `internal/store/mgr` | 管理库：账号/分组/档位三张表 | `mgr.Open`、`ImportOnce`、`NewUserStore`、`NewGroupStore`、`NewExecutorProfileStore` |
| `internal/store/observe` | 观测库：运行事件/产物索引/写操作审计 | `observe.Open`、`NewEventLog`、`NewAuditLog`、`NewArtifactIndex` |
| `internal/web` | 控制台静态资源嵌入 | `Dist`（不带 `-tags dashboard` 时恒为 nil） |
| `examples/demo1,2` | 作为库使用的示例（只 import core） | — |

## 4. 依赖关系

实测 import 图（生产代码，不含测试）：

```
cmd/server ──→ api, core, executor, store/{jobs,mgr,observe}, web
api        ──→ core, executor（只读接口：Registry / ArtifactStore / ArtifactIndexer）
executor   ──→ core
store/jobs ──→ core, store/engine
store/mgr  ──→ core, store/engine
store/observe ──→ core, executor(为实现 executor.ArtifactIndexer), store/engine
store/engine  ──→ （无内部依赖，唯一驱动入口：bun + modernc/sqlite + pgx）
core       ──→ （无内部依赖）
web        ──→ （无内部依赖，仅 embed）
```

规则归纳为两条：

1. **向下依赖单向、禁止旁路**：api 不碰 sqlite；core 不知道任何人存在；observe 与 api 通过 core 的共享词汇解耦。
2. **接口在消费方、实现在供给方**：依赖箭头永远指向"定义词汇的那个包"（core），而不是"实现功能的具体类型"。

## 5. 术语澄清：档位（Profile）与任务类型

**对外表现上，一个档位就是"一类可执行的任务"；但术语上它比"任务类型"多一层含义。**

`executor/profile.go` 的 `Profile` 结构存的是**"怎么执行"的完整规格**，不是一个类型名：

| 档位种类 | 定义内容 |
| --- | --- |
| `script` | 解释器（`bash`/`python`/`node`…）+ 工作区内的脚本路径 |
| `binary` | 预编译程序路径 + 固定参数 |
| `http` | Method + URL 模板 + 白名单 host + 期望状态码 |

外加一套执行约束：参数声明（`Args []ArgSpec`）、环境变量白名单、`Timeout`/`MaxTimeout`、`MaxParallel`、`RetryOnExit` 等。

**它与任务类型的关系**：注册之后，一个档位就成为一个任务类型——

```
档位 profile（配置声明） ──注册──▶ 注册表里的 handler key "exec.<name>"
                                      ▲
                              Job 的 Type/Name 引用它
```

- 调度器视角：档位任务与 Go 代码注册的任务在**同一张注册表**里（`api.Server.RegisterJobHandler` 就是调度器的转发），区别只在盖的章不同——档位是 `core.JobClassExec`（进独立执行池），代码注册的示例任务走默认池。
- 调用方视角：提交 Job 时填 `type: exec.<name>`、payload 带参数，与提交普通任务无异。

**为什么单独叫"档位"而不是"任务类型"**：本项目的任务类型有两种来源，档位只是其中一种，区分的焦点在**安全边界**：

| | 代码注册的任务类型 | 执行器档位 |
| --- | --- | --- |
| 定义方式 | Go 函数，编译期写死（`registerHandlers` 里的示例） | 配置/管理页声明，不改代码不重编译 |
| 任务类型的本质 | 一个函数 | 一条白名单执行规格 |
| payload 能干什么 | 函数自己解析 | 只能引用档位名 + 填受约束的参数 |

`docs/design/executor-design.md` 的 D1 决策写明：**只有 profile 白名单，不提供 raw command 字符串**——payload 里不能塞命令，只能引用档位名并按 `ArgSpec` 校验参数。这把远程代码执行面从"任何能 POST 的人"收窄到"能改配置并重启的人"。**档位的本质是一道 RCE 白名单；"任务类型"只是它注册后呈现出来的外形。**

"档位"对应英文 profile（预设规格，类似相机的档位——档位定好光圈快门，拍照的人只管按快门），源自 web-console-design §5.7 的"角色档位"，后推广为执行器层的统一叫法。

## 6. 主业务流程

### 6.1 启动装配（`cmd/server/main.go` 的 `run`，顺序即正确性）

1. `core.LoadConfig` → 建 logger + `LevelVar`（为 `logging.level` 热更留载体）→ `defaultRuntimeDeps` 建闭包集合。
2. **依赖完整性检查**：开关打开而闭包缺失直接拒启。例：`observability.enabled && observability.events.enabled` 但没装配 `newEventLog`，后果是"三张表里事件那张永远空着"，必须在启动期报错，不能让"没记"与"没发生"分不清。
3. `newStore`：`json` 或 `sqlite`（sqlite 首次启动对旧 JSON 文件做一次 `ImportOnce` 整文件导入，之后以库为准）。
4. `newScheduler` + 设置两个池的规模（执行器未启用时传 0：连队列和协程都不建，进程与拆池之前一字不差）。
5. 管理库（若 `mgrstore.enabled`）：`mgr.Open` → `ImportOnce`（config users / groups.json / profiles.json → 三张表，之后配置与 JSON 不再被读取）。
6. 档位：`profileStore` 读取 → `executor.NewRegistry`（加载 + 探测）→ `MergeStoreProfiles` 合并（单条不合法不连坐、撞名降级）→ `ApplyStore` 注册。**合批必须早于 `registerHandlers`**：崩溃恢复守卫按注册表里的类别改判，晚注册的档位在恢复现场查不到类别。
7. 产物存储（仅 `executors.enabled`）→ 观测层三件套：`NewEventLog` 挂上 `EventBus`（**必须在 `scheduler.Start` 之前**：Restore 会重新发布一批事件，挂晚了这批静默不进库）、`NewArtifactIndex` 挂上 `ArtifactStore`、`NewAuditLog` → 产物清理协程（在索引挂好之后起，第一轮扫描要做索引对账）。
8. `newServer`：按开关逐个注入 Option（GroupStore / UserStore / Console / EventLog / AuditLog / ExecutorProfileStore+Applier / ReloadState）。未启用的能力靠"不注入 = nil = 明确的 503/默认路径"表达，不在业务包里散落 `if enabled`。
9. `registerHandlers`：示例 handler 走 `server.RegisterJobHandler`，档位走 `executor.Register` → `installRestoreGuard`（**必须在 `Start` 之前**）→ 建热重载链。
10. `scheduler.Start()`（第一步是 `Restore`）→ `server.Start()` → watcher 开始盯配置文件 → `<-quit` 等信号。
11. **关停四步**：`watcher.Close → chain.Stop → server.Stop(ctx) → scheduler.Stop`；之后 defer 逆序执行：事件/审计写入器 → 观测库 → 管理库 → 任务存储。顺序反过来就是往已关闭的连接上写（`chain.Stop` 不可省：`watcher.Close` 不等在途的那次 ReloadFunc）。

### 6.2 延迟任务主链路（提交 → 到期 → 执行 → 终态）

```
POST /api/v1/jobs
  → 中间件链：JWT 认证 → RBAC(operator) → 限流 → 审计
  → api.CreateJob 校验 → scheduler.Schedule(job)
      · 四叉堆 PushItem + store.Save 落盘 + notifyNewJob 唤醒调度循环
  → scheduleLoop:
      · heap.PopIfDueWhere(now, dispatchable) —— 原子取"到期且目标池有空位"的任务
        （dispatchable 在堆写锁内只读 channel len/cap 与任务类别，刻意不查注册表，
          避免"先堆锁、后调度器锁"的死锁顺序）
      · dispatch：JobClassExec 走 execCh（非阻塞，满了塞回堆等空位信号）
                 普通任务走 workCh（满了阻塞，背压交给 worker 池）
  → worker → executeJob:
      · 发布 job.started 事件
      · 按注册表恢复 Handler（Type 优先，回退 Name；显式绑定优先）
      · 建 cancelCtx + 单次超时；store.Update(running) 落盘运行态
      · handler(ctx, job)
      · 成功 → handleSuccess：Cron 任务算 next 重新 Schedule（同 ID 覆盖），
               否则落 success 终态快照（是否长期保留由存储保留策略决定）
      · 失败 → handleFailure 三分支判定（顺序即口径）:
             ① parkForcedPause：用户强制暂停优先于一切，不消耗重试次数、不再排期
             ② 永久失败（error 带 Permanent）→ markFailed，重跑只会重现同一错误
             ③ RetryCount < MaxRetries → retryPolicy.NextRetry（指数退避）
                → job.CloneForRetry → 重新 Schedule（发布 job.retrying 事件）
             否则 markFailed 落终态
      · ctx.Canceled（用户 Cancel / 优雅关停）→ handleInterrupted 落回 pending，重启后照常重跑
```

调度循环有一个细节（`scheduleLoop`）：有到期任务但执行器池满时，不能按 `TriggerAt` 继续等（那是过去的时刻，等于零时长定时器忙等），改为等 `execSlotFreed` 空位信号 + 500ms 兜底唤醒（`execBlockedRetry`）。

### 6.3 Cron 周期任务

`Job.IsRepeat && CronExpr != ""` 时，`handleSuccess` 用 `core.cronParser.Next` 算下次触发时刻，构造同 ID 的新任务重新 `Schedule`（同 ID 覆盖旧快照，保证一个周期任务只有一个实例）。下一轮不带上一次的 `Exec` 字段——新的一次执行不该在跑完之前显示旧结论。

### 6.4 执行器档位任务（进程 / HTTP 执行）

1. 档位来源合并：`executors.commands`（配置）+ 档位存储（管理库或 JSON 文件，页面写入）→ 单条校验失败不连坐、撞名降级（`MergeStoreProfiles`）。
2. `Registry` 探测可用性（`ProbeResult`）→ `executor.Register` 把 `Profile` 包成真实 handler 注册进调度器，盖 `JobClassExec` 章（handler key 以 `exec.` 前缀，`core.IsExecHandlerKey` 识别）。
3. 到期后进**独立执行池**（`execCh`，与普通 worker 分池）：队列满不拖累普通任务按时投递——执行器池的隔离点。
4. `proc.go` 起进程（取消时杀**整棵进程树**，防孤儿）或 `http.go` 调 HTTP（白名单 host、拒内网地址可选）→ stdout/stderr 流式写 `ArtifactStore`（MaxBytes 限额、TTL、孤儿清理）→ 退出码/时长/尾部预览组成 `ExecMeta` 进事件；快照只放摘要，全量输出按 `job_id/attempt` 落产物目录（D4：JSON 存储是整文件重写，128KB 输出进快照会把 flush 放大 128MB）。
5. 单次超时是硬约束：`Job.Timeout == 0` 表示不限，一个挂死的 shell 会永久占住 worker，因此档位级 `default_timeout`/`max_timeout` 是上限（`Registry.EffectiveTimeout`）。

### 6.5 崩溃恢复（Restore + RestoreGuard）

`scheduler.Start` 第一步执行 `Restore`：`store.LoadAll` 取全部快照，非终态任务回堆重新排期。**`running` 状态的快照**含义是"进程确实起来过、结果未知"（被强杀的进程来不及写任何结论），交给 `RestoreGuard` 钩子改判：

- `cmd/server` 的 `pauseRunningExecOnRestore` 只拦"`running` 且类别是 `JobClassExec`"的快照，改判为 **paused** 等人确认——重放可能重复外部副作用；
- `executors.restore_policy=replay` 时守卫不装（部署方明确承担重复执行后果）；
- 优雅关闭不走守卫：`handleInterrupted` 已把在途任务落成 pending，重启后照常重跑。

### 6.6 配置热重载（reload.go + `core/config_reload.go`）

`fsnotify` **只盯启动时真正读到的那份配置文件**（`resolvedConfigPath`：`-config` 优先，否则按约定路径找，都找不到就不建监听器）→ debounce → `reloadChain.Reload` 串行执行级联应用：

- `ResizeWorkers` / `SetRetryPolicy`（运行期真正生效的那一对）
- 事件/审计 `SetRetention`（下一个批量周期用新值，不重启不换连接）
- `SetLogLevel`（换 `LevelVar` 载体，不换 logger 实例）
- 档位 `Applier.ApplyConfig`（config 侧整表替换）
- 结论写入 `reloadStatus`，`GET /admin/runtime` 可读

设计取向：**热重载是便利，不是承重结构**——watcher 起不来只留 warn + 读数，不拒启；关停时 `chain.Stop()` 要等在途那次 Reload 走完（`watcher.Close` 明确不等，理由见关停四步）。

### 6.7 观测与实时推送

`EventBus`（core，进程内 Pub/Sub）有两个订阅者：

- **api 内存缓冲** → WS（`/ws`，凭 `/auth/ws-ticket` 短票连接）+ SSE（`/sse/events`）+ `GET /events`；
- **observe.EventLog** → 批量周期写 SQLite `job_events`（队列满丢弃但计数 `Dropped`，关停时打进日志——库里缺页是读不出来的，必须留痕）。

审计：写操作中间件 → `AuditLog` → `write_audit` 表；写入方与查询方是同一个句柄（`GET /admin/audit`）。产物清理协程每轮做"孤儿清理 + TTL 清理 + 与 `artifact_index` 对账"。

### 6.8 认证与账号

JWT：`/auth/login|refresh|logout|me|ws-ticket`；RBAC 四角色 reader / operator / ops / admin（`core.RoleAdmin` 等）。鉴权覆盖全部端点，含 `/ws` 与 `/sse/events`。账号存储双态：`mgrstore.enabled` 时走管理库（改账号即时生效，`/admin/users` 端点启用）；否则退回 config 里的 users 快照（遗留模式，管理端点不存在）。

## 7. 设计要点小结

1. **组合根很重**：所有"开关组合出的部署形态"（执行器 × 观测层 × 管理库 × 热重载）都收敛到 `cmd/server` 的装配层，业务包里不出现散落的 `if enabled`；未启用的行为靠"不注入 = nil = 明确的 503/默认路径"表达。
2. **消费方接口 + 隐式实现**让依赖倒置落地成本极低，每个窄接口都带一条"为什么定义在消费方"的注释（通常是：只有句柄可替换，关闭顺序才断言得出来）。
3. **顺序即正确性**：启动/关停每一步顺序都有注释论证（Restore 前必须装守卫、EventLog 必须挂在 Start 前、defer 位次决定关闭顺序），并有测试钉住。
4. **失败口径统一**：配置非法 / 库建不起 / 文件读不出 = 拒启；热重载起不来 = 降级继续服务 + 留痕。哪些错误拦启动、哪些只记日志，每处都写明了理由。
