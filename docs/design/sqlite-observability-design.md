# SQLite 观测层设计：运行事件、产物索引与写操作审计

- 状态：待实施（卡片见 `tasks/sqlite/`）
- 日期：2026-09-30
- 关联文档：`executor-design.md`（执行层，已实现）、`web-console-design.md` §5.6 与 §5.7.6（本文实现它登记的两处"二期"）
- 前置事实：任务快照与分组仍存 JSON 文件，本设计不改动 `core.Store` 与 `JSONFileStore` 的任何行为

## 1. 背景与目标

系统当前的可观测数据分三类存放，其中两类有明确缺陷：

| 数据 | 现状 | 缺陷 |
| --- | --- | --- |
| 任务快照 | `data/jobs.json`，200ms 合并全量重写 | 正常。本设计不动它 |
| 运行事件（时间线） | `api/history.go` 的内存 LRU | 重启即清空。该文件注释自己写着"不是审计日志""需要持久化的运行历史属于二期"（`api/history.go:23-25`） |
| 产物文件生命周期 | 靠 `os.ReadDir` 扫目录 + `store.LoadAll()` 判活 | 无索引。产物是否已被清理只能通过"读文件失败"发现（`api/handlers_executors.go:183`） |
| 写操作审计 | 只输出结构化日志 | 不可查询。`web-console-design.md:607` 登记"写操作审计本期只输出结构化日志（§5.7.6）；`/admin/audit` 查询端点留二期" |

目标：用一块本地 SQLite 补上这三块，同时满足两条约束。

1. **改动面最小**：不引入新的存储后端切换，不改任务快照的存放位置与落盘节奏。
2. **默认关闭**：`observability.enabled: false` 时进程行为与本设计之前完全一致，包括不创建任何文件。

非目标：不做集群、不做多进程共享、不做分析型数仓查询、不把 JSON 存储换成 SQLite（那一条的选型结论与代价记在 §4.3，供后续决策参考）。

## 2. 决策摘要

| # | 决策 | 理由 |
| --- | --- | --- |
| D1 | 只新增三张表，不搬既有数据 | 事件与审计在库里本来不存在（零搬迁）；产物索引只索引、不搬正文。任务快照继续走 JSON |
| D2 | 驱动用 `modernc.org/sqlite`（纯 Go，无 CGO） | 仓库当前零 CGO 依赖，`examples/demo1`、`demo2` 要能 `go run` 就跑；Windows 开发机与交叉编译都不能要求 C 工具链 |
| D3 | 实现放独立包 `store/sqlite/`，`core` 与 `api` 不 import 驱动 | `core` 是零重型依赖的调度库。接口定义在消费方（`api`、`executor`），由 `store/sqlite` 隐式实现，`cmd/server` 负责装配 |
| D4 | 写入采用有界队列 + 周期批量事务（默认 200ms） | 与 `core.JSONFileStore` 的 `flush_interval` 同一口径（`core/store.go:17-19`）。关键约束：**绝不回压 EventBus**——`core/event.go:176` 的 `Publish` 是非阻塞的，丢掉一条记录不能变成阻塞调度 |
| D5 | 事件读取走"配了库就只读库，没配库就读内存" | 见 §8.1。时间线的首屏是历史回看，实时增量走 WebSocket 推送，因此批量写入的可见性延迟（≤ `flush_interval`）不影响正确性 |
| D6 | 审计表按 §5.7.6 已定的字段形状做**宽表**，覆盖全部写操作 | 不另起一张只记 `exec.*` 的窄表：会出现两张表覆盖同一请求。执行器特有的判定结论作为本表的补充列，由 `gateExecutorSubmission` 写入 |
| D7 | 审计只存**封闭枚举**的拒绝码，不存 error 原文 | 参数值可能出现在校验错误文本里，而 `proc.go:320` 的既有规范是"档位名可以外泄，参数值不可以"。人类可读说明继续走 slog |
| D8 | `artifact_index` 定位为"加速器"而非"账本" | 库写失败时文件仍是权威；最终判定仍以文件存在与否为准。这条定位不写死，后续会有人拿它做删除决策 |
| D9 | 保留策略各自独立，不复用 `store.history_limit` | `store.history_limit: -1` 的部署不留终态快照，同一份配置已经造成过产物被误判为孤儿（`executor/artifact.go:558-560`），事件保留不能再绑上去 |

## 3. 现状盘点（规划时基线，已核实）

- `EventBus` 的订阅能力是公开的：`SubscribeAll()`（`core/event.go:109`）返回 `(subID, <-chan Event)`；`Scheduler.GetEventBus()`（`core/scheduler.go:1641`）是拿到总线的入口。`api.NewServer` 已经用这个组合挂了第一个订阅者（`api/server.go:142`）。**新增一个订阅者不需要改 `core` 的任何一行。**
- 内存缓冲的三档上限是常量：每任务 100 条、LRU 2000 个任务、全局 500 条（`api/history.go:12-19`）。注释说明"先用常量，真有人抱怨再开配置项"。
- 两个事件端点当前只读内存，响应体带一句 `Note`：`"in-memory buffer, cleared on restart"`（`api/handlers_events.go:17`、`:22`），且该字符串被前端直接引用。返回顺序是**按时间升序**。
- 产物写入侧的收口点是 `writer.Close()`：`executor/proc.go:167` 与 `executor/http.go:184` 各一处，返回的 `ArtifactInfo`（`executor/artifact.go:256`）已含 `JobID/Attempt/OutPath/ErrPath/OutBytes/ErrBytes/Truncated`，与索引表列几乎一一对应。
- 产物删除侧两处：`PurgeExpired`（按目录时间）与 `PurgeOrphans`（`executor/artifact.go:561`），后者依赖 `cmd/server/main.go:267` 的 `liveJobIDs` → `store.LoadAll()` 全量拉取。
- 执行器提交期的判定集中在一个函数：`gateExecutorSubmission`（`api/handlers_executors.go:537`），依次走档位角色 → 可用性 → 超时上限 → payload；越权拒绝另有统一出口 `logAccessRejection`（`api/security.go:308`）。
- 配置侧有两个不同的机制，容易混为一谈（本设计早期草稿就把它们搞错过一次）：
  - **未知键报错**来自 `v.UnmarshalExact(&cfg)`（`core/config.go:605`），判据是 `Config` 结构体里有没有对应字段。新增配置节只要加了结构体字段，YAML 里写这些键就能被接受；写错的键会被拒。
  - **`core/config.go:554-595` 那个字符串列表只做 `BindEnv`**，决定的是"哪些键可以被 `GODELAYQ_*` 环境变量覆盖"。列表里没有的键不会导致启动失败，只是环境变量对它无效。`server.auth.users` 与 `executors.commands` 就是**故意不加**的两个（嵌套列表无法用逗号分隔 hook 表达，且把凭据或可执行内容塞进环境变量容易被旁路读到）。
  - `configs/config.example.yaml` 顶部承诺"每一项都可被环境变量覆盖"，因此本节的 13 个标量键全部加入 `BindEnv` 列表，与 `executors.output.dir` 这类路径项同样可覆盖（路径不是凭据）。

## 4. 范围

### 4.1 进入 SQLite 的数据

| 表 | 数据来源 | 现状 | 收益 |
| --- | --- | --- | --- |
| `job_events` | EventBus 广播的 8 种 `job.*` 事件 | 纯内存，重启清空 | 时间线跨重启可查；可按类型/时间聚合统计 |
| `artifact_index` | 每次执行的输出文件属性 | 无索引，靠扫目录 | 按 attempt 列出输出尺寸与截断状态；清理变成条件删除；"产物已清理"从被动发现变主动标注 |
| `write_audit` | 全部 POST/PUT/DELETE 请求 | 只有 slog 文本 | 可查询的执行台账：谁、什么身份、做了什么、成没成、多快 |

### 4.2 明确不搬（附理由）

| 数据 | 不搬的理由 |
| --- | --- |
| 任务快照 `jobs.json` | 见 §4.3 的完整分析。本设计的范围决定不动它 |
| **产物正文**（`a<attempt>.out/.err`） | 写入器是直接挂给 `exec.Cmd.Stdout` 的 `io.Writer`（`executor/proc.go:210`），改成写库要引入缓冲层；单流上限 256KB × 终态留痕 1000 条会把库推到几百 MB，且 `Tail` 的反向读会退化 |
| 登录态（refresh 表 / jti 拒绝表 / 一次性 ticket） | 进程内语义是刻意的：重启即全员登出、ticket 一次性使用。落库等于把一条安全属性改成一条持久化保证，与本设计目标无关 |
| `loginLimiter` 失败计数 | 短期状态，跨重启累计会改变锁定行为 |
| `configs/config.yaml`、账号 bcrypt 哈希 | 配置是启动输入不是运行数据；账号入库会绕过"改账号需重启"这条既有口径（`configs/config.example.yaml` 明写） |
| 调度堆 `QuaternaryHeap` | 堆的价值在内存结构与按 ID 的 O(1) 定位（`core/heap.go:24-29`）。SQLite 是它的恢复源，不是替代 |
| 档位 `Profile` / 探测结论 | 配置派生的只读物，启动时确定 |

### 4.3 关于"把任务快照也换成 SQLite"的结论（本期不做，留档）

结论：**接缝已经留好，但收益不在本期，风险却会立刻出现。**

- 有利的一面：`Store` 是六方法接口（`core/store.go:25-37`），`store.type` 配置项与 `unsupported store.type %q, only "json" is implemented` 的校验分支（`core/config.go:637-641`）已经预留；全仓只有 `cmd/server/main.go:75` 一个构造点。忠实实现 `LoadAll()` 的前提下，11 个读取点与 13 个写入点可以一字不改。
- 不利的一面：`JSONFileStore` 是"内存 map 缓存 + 周期合并重写"，运行期的读实际发生在内存。换成"每请求一次全表查询"不会更快。要拿到收益必须同时把 `api.ListJobs`（`api/handlers.go:169`）、`GetStats`（`:481`）、`RetryJob`（`:438`）、`scheduler.findSnapshot`（`core/scheduler.go:661-664`，其注释已在等 `Store` 提供按 ID 取快照的方法）改成条件下推，那是独立的一轮改动。
- 因此本期把这三张观测表建起来，快照后端的选型另立设计。两件事各自可验证，混在一起会让"表建错"和"后端换错"难以区分。

## 5. 总体架构

```
                      ┌──────────────── core.EventBus ────────────────┐
                      │  Publish 非阻塞；缓冲区满丢弃（口径不变）      │
                      └───────┬──────────────────────────┬────────────┘
                    SubscribeAll│                  SubscribeAll│
            ┌───────────────────▼──────┐        ┌─────────────▼──────────────┐
            │ api.EventHistory（保留）  │        │ sqlite.EventLog（新增）     │
            │ 内存 LRU，实时            │        │ 有界队列 → 200ms 批量事务   │
            └───────────┬──────────────┘        └─────────────┬──────────────┘
             未启用库时读这里             启用库时读这里（D5）
                        └────────── api/handlers_events.go ──────────┐
                                             GET /jobs/:id/events · GET /events

api.Server（新增三个窄接口，都在 api 侧定义）
  ├ eventRecorder / eventReader  ← Option 注入；nil = 走内存路径
  └ auditRecorder                ← gin 中间件写入；nil = 只记 slog（即现状）

executor.ArtifactStore
  └ ArtifactIndexer（可选）      ← Runner 在 Close 之后 Record 一行；Purge* 删行；nil = 只扫目录（现状）

cmd/server/main.go（唯一装配点）
  observability.enabled ─▶ store/sqlite.DB ─▶ EventLog / ArtifactIndex / AuditLog
                                                └─ api.WithEventLog / WithAuditLog
                                                └─ artifacts.SetIndex

依赖边界（单向）
  core            ：不新增任何依赖，不认识 SQLite
  api / executor  ：只定义并消费窄接口，不 import 驱动
  store/sqlite    ：全仓唯一 import modernc.org/sqlite 的包
  cmd/server      ：装配上述全部
```

## 6. 数据模型

三张表各自带一个自增主键 `seq`：它既是分页游标，也是 `Timestamp` 精度之外的定序依据（一次执行会在同一毫秒内连发 `job.scheduled` 与 `job.started`，只按时间排不出因果）。

### 6.1 `job_events`

```sql
CREATE TABLE job_events (
  seq      INTEGER PRIMARY KEY AUTOINCREMENT,
  ts_us    INTEGER NOT NULL,               -- core.Event.Timestamp，Unix 微秒
  type     TEXT    NOT NULL,               -- job.scheduled|started|completed|failed|cancelled|retrying|paused|resumed
  job_id   TEXT    NOT NULL,
  job_name TEXT,
  status   INTEGER NOT NULL,               -- core.JobStatus 的 int，与 JobSnapshot.Status 同一口径
  data     TEXT,                           -- core.Event.Data 原文（JSON），可空
  metadata TEXT                            -- core.Event.Metadata 序列化为 JSON
);
CREATE INDEX idx_events_job  ON job_events(job_id, seq);
CREATE INDEX idx_events_ts   ON job_events(ts_us);
CREATE INDEX idx_events_type ON job_events(type, ts_us);
```

不存 `Event.JobID` 为空的事件：内存缓冲同样忽略它们（没有归属的 `heap.updated` 放进任何任务的时间线都是噪音，见 `web-console-design.md` §5.6）。

### 6.2 `artifact_index`

```sql
CREATE TABLE artifact_index (
  job_id    TEXT NOT NULL,
  attempt   INTEGER NOT NULL,
  kind      TEXT NOT NULL,                 -- script|binary|http
  profile   TEXT,                          -- 档位名，不含 exec. 前缀
  out_rel   TEXT, err_rel  TEXT,           -- 相对 executors.output.dir；不存绝对路径
  out_bytes INTEGER NOT NULL DEFAULT 0,
  err_bytes INTEGER NOT NULL DEFAULT 0,
  truncated INTEGER NOT NULL DEFAULT 0,
  state     TEXT NOT NULL,                 -- available|purged
  created_at INTEGER NOT NULL,
  PRIMARY KEY (job_id, attempt)
);
CREATE INDEX idx_artifact_state ON artifact_index(state, created_at);
```

- 主键 `(job_id, attempt)` 与文件布局一一对应（分 attempt 存文件的理由：重试副本沿用同一任务 ID，`executor/artifact.go:163-166`）。
- 只存相对路径，与 `Profile.ProgramDisplay()` 同一取向：不把服务器目录结构透给前端。
- `exit_code` / `http_status` **不进这张表**：它们属于执行结论，已经在 `JobSnapshot.Exec`（`core.ExecMeta`）里，重复存一份会造成两个真值来源。

### 6.3 `write_audit`

字段形状照 `web-console-design.md` §5.7.6 已定的 `who / role / action / method / path / status / latency`，再补执行器路径的结论列。

```sql
CREATE TABLE write_audit (
  seq       INTEGER PRIMARY KEY AUTOINCREMENT,
  ts_us     INTEGER NOT NULL,
  actor     TEXT,                          -- 账号名；静态凭据记 "machine"；未认证记空
  actor_kind TEXT NOT NULL,                -- user|machine|anonymous
  role      TEXT,                          -- viewer|operator|admin|ops|machine
  action    TEXT NOT NULL,                 -- 封闭集：job.create|job.cancel|job.pause|…（见 §9.5 映射表）
  method    TEXT NOT NULL,                 -- POST|PUT|DELETE
  route     TEXT NOT NULL,                 -- gin 路由模板，如 /api/v1/jobs/:id/pause；不存原始 URL 与查询串
  status    INTEGER NOT NULL,              -- HTTP 状态码（来自 c.Writer.Status()）
  latency_us INTEGER NOT NULL,
  verdict   TEXT NOT NULL,                 -- ok|denied|bad_request|not_found|conflict|error（按 status 派生）
  exec_verdict TEXT,                       -- 仅执行器任务：accepted|role_denied|profile_unavailable|payload_rejected|timeout_rejected
  exec_reason_code TEXT,                   -- 封闭枚举，见 D7；不存 error 原文
  handler_key TEXT,                        -- 仅执行器任务：注册键 exec.<name>
  profile   TEXT,                          -- 仅执行器任务：档位名
  job_id    TEXT,                          -- 有归属时填；批量请求填请求级 ID 留空
  remote_ip TEXT,
  user_agent TEXT
);
CREATE INDEX idx_audit_ts      ON write_audit(ts_us);
CREATE INDEX idx_audit_actor   ON write_audit(actor, ts_us);
CREATE INDEX idx_audit_action  ON write_audit(action, ts_us);
CREATE INDEX idx_audit_verdict ON write_audit(verdict, ts_us);
```

- **`route` 存路由模板而不是 `c.Request.URL.Path`**：一次解决两个问题——查询串里可能带 `?token=`，任务 ID 会让列值无界膨胀。
- **一行对应一个 HTTP 请求**，不对应一个条目：`POST /jobs/batch` 与 `/jobs/batch-ops` 各记一行，`exec_verdict` 只在单条创建路径上有值。批量里的逐条结果继续走响应体与 slog。
- 只审计 `POST/PUT/DELETE`。读端点不进台账：否则这张表会被前端轮询写满，而它记录的内容没有任何权限含义。

## 7. 写入路径与并发

### 7.1 批量事务

每个写入器内部都是同一个形状，三张表共用一份实现（放在 `store/sqlite` 的未导出类型里，不抽到 `core`）：

```
Append(item)      → 有界队列入队；队满则丢弃并累计 dropped，返回 nil
run()             → ticker(flush_interval) 触发一次 BEGIN; 批量 INSERT/UPSERT; COMMIT
Flush()           → 立即落盘；关停路径与测试用
Close(ctx)        → 撤订阅 → 最后一次 Flush → 关连接；幂等
```

- 队列容量默认 4096 条。丢弃时记一条 warn 并把累计值挂到 `/admin/runtime` 的观测输出里——**丢了多少必须是可查的**，否则这张表看起来完整、实则缺页。
- 事务失败（含 `SQLITE_BUSY`）时整批重新入队一次，二次失败则丢弃并记 error。不无限重试：磁盘满这类故障下无限重试会把队列变成内存泄漏点。
- `dropped` 只记写入器自己这一段的丢弃数。总线给每个订阅者的通道同样是非阻塞投递（`core/event.go` 的 `select/default`，缓冲 `core.NewEventBus` 的 `bufferSize`），到不了写入器的事件不计在这里：`dropped` 加落库条数因此小于等于发布条数，差额属于总线自身的过载，与 `api.EventHistory` 那一份内存缓冲看到的是同一件事。写用例时要么把发布条数控制在一个订阅缓冲以内，要么只断言下界。
- 每条语句都是 `INSERT`，`artifact_index` 用 `INSERT ... ON CONFLICT(job_id,attempt) DO UPDATE`（清理与重复读结果会二次触达同一行）。

### 7.2 连接设置

`sql.DB` 显式 `SetMaxOpenConns(1)`：本设计的写入是批量单点，读是短查询，一个连接足够，同时消除 `SQLITE_BUSY` 的主要来源。加 `PRAGMA journal_mode=WAL`、`synchronous=NORMAL`、`busy_timeout`（取配置，默认 2s）。

`NORMAL` 而不是 `FULL`：WAL + `synchronous=NORMAL` 下断电只丢最后若干次已提交事务，与 `JSONFileStore` 现有的"崩溃最多丢一个合并周期"（`core/store.go:17-19`）是同一量级的保证。这个取舍必须写进配置注释，让部署方能改。

### 7.3 关闭顺序

```
SIGTERM → api.Server.Stop(ctx) → Scheduler.Stop() → stopObservability()（撤订阅 + Flush + 关连接）→ store.Close()
```

顺序不可颠倒的理由与 `executor/artifact.go` 的清理协程那条完全同构：先撤订阅读者、再关写入端，最后关连接；否则可能出现向已关闭连接写入、或在存储关闭后再读一次任务集合。`cmd/server/main.go` 里以 `defer` 的声明顺序表达，注释指明。

## 8. 读取路径

### 8.1 "DB 优先、内存兜底"的确切含义

配置了 SQLite 时，两个事件端点**只读库**；未配置时**只读内存**。不做两路合并。

- 为什么可以只看库：批量写入的可见性延迟上界是 `flush_interval`（默认 200ms）。`GET /jobs/:id/events` 的用途是详情页与 Dashboard 的首屏回灌（`api/handlers_events.go:36-39`），实时增量由 WebSocket 推送承担，因此这 200ms 不会让人看到缺页的时间线。
- 为什么不合并：合并需要跨两个来源去重与定序（同类型事件在重试链里会重复出现），换来的是 200ms 的新鲜度。这个代价不对等。
- 响应里的 `Note` 必须跟着分岔：走库时改成说明"持久化事件库，最新一条可能有 `flush_interval` 的写入延迟"，走内存时保留原句。该字符串前端直接引用，不新增字段、不换类型。
- 排序契约不变：**按时间升序**返回，即库内 `ORDER BY seq DESC LIMIT n` 之后整体反转。
- `?limit=` 上界分岔：内存路径仍受 `historyPerJobLimit`(100) 限制；库路径放开到常量 1000。这条是**响应能力的扩大**（同 limit 值在启用库后能拿到更多历史），必须写进 `docs/api.md`，不能被当成回归。

### 8.2 产物索引的读端点

`GET /api/v1/jobs/:id/artifacts`（viewer 档）返回该任务各次尝试的输出尺寸、是否截断、`state`。

理由：一张没有读取方的表无法演示、也无法验证写入是否漏了。同时它补掉了现在的一个真实缺口——`GET /jobs/:id/result` 只能按 `snapshot.Attempts` 猜哪一次尝试有输出（`api/handlers_executors.go:99`），而排障最常问的恰是"第一次为什么失败"。

未注入索引时该端点返回 503，与 `requireArtifacts()` 的既有做法一致（`api/handlers_executors.go:449`）：区分"没装配"与"没有记录"。

### 8.3 审计的读端点

`GET /api/v1/admin/audit`（ops 档）：`?limit=&offset=&actor=&action=&verdict=&since=&until=`，按 `seq DESC`（最新在前）返回。路由名沿用 §5.7.6 预留的 `/admin/audit`，落在既有 `admin` 组里（`api/server.go:256` 已要求 ops 档）。

本期只做这一个只读端点，控制台页面另立卡片（`tasks/sqlite/task-s08-console-audit-page.md`）。

## 9. 后端改动方案（逐文件）

### 9.1 配置（`core/config.go`、两份 YAML）

```yaml
observability:
  enabled: false
  path: ./data/observe.sqlite
  flush_interval: 200ms     # 与 store.flush_interval 同一口径；也是事件端点的可见性延迟上界
  queue_capacity: 4096      # 有界队列；满则丢弃并计数
  busy_timeout: 2s
  synchronous: normal       # normal|full
  events:
    enabled: true           # 关掉则只建表不写入（保留读端点的 503 行为）
    retention_count: 200000 # 超出按 seq 淘汰最旧
    retention_age: 720h     # 0 表示不按时间淘汰
  artifacts:
    enabled: true
  audit:
    enabled: true
    retention_count: 500000
    retention_age: 2160h    # 90 天：安全台账的价值在事后追溯
```

改动清单：`Config` 加 `Observability ObservabilityConfig`；默认值表（参照 `core/config.go:499` 的写法）；`Normalized` 补齐；`Validate`；**环境变量绑定列表**（`core/config.go:554-595`，13 个标量键逐个加入，否则它们无法用 `GODELAYQ_*` 覆盖——注意这不是未知键拒绝的来源，见 §3 最后一条）；`configs/config.example.yaml` 与本机 `configs/config.yaml` 两份同步（守卫测试 `TestExampleConfigMatchesLocal` 会比对两份的键集合）。

独立于 `store:` 一节：它不改任务快照的存放位置，放进 `store` 会让人误以为与 `store.type` 有关。

### 9.2 `store/sqlite/`（新增包）

```go
type DB struct{ ... }
func Open(cfg core.ObservabilityConfig, logger *slog.Logger) (*DB, error)  // 建目录、PRAGMA、建表、迁移版本表
func (d *DB) Close() error
func (d *DB) Stats() (Stats, error)                                        // 三张表的行数与 dropped 计数

type EventLog struct{ ... }
func NewEventLog(bus *core.EventBus, db *DB, opts EventLogOptions) (*EventLog, error)
func (e *EventLog) Events(jobID string, limit int) ([]core.Event, error)   // 升序
func (e *EventLog) Recent(limit int) ([]core.Event, error)                 // 升序
func (e *EventLog) Flush() error
func (e *EventLog) Close() error

type ArtifactIndex struct{ ... }   // 实现 executor 侧的 artifactIndexer
type AuditLog struct{ ... }        // 实现 api 侧的 auditRecorder
```

构造函数一律 `New*`/`Open`（`Open` 属于既有例外名单：与 `os.Open` 同义的资源获取）。驱动只在本包 import。

### 9.3 事件（`api/`）

- `api/server.go`：新增 `WithEventLog(...)` Option 与 `eventLog` 字段（nil 表示未装配），窄接口定义在这里。
- `api/handlers_events.go`：两个端点各加一个分支，`Note` 文案分岔，`limit` 上界分岔。
- 保留 `EventHistory` 的构造与订阅不变：它承担未启用库时的读路径，也是启用库时的对照实现。

### 9.4 产物索引（`executor/`）

- `executor/artifact.go`：`SetIndex(ArtifactIndexer)`（nil 安全）与 `Index()` 访问器；`PurgeExpired`/`PurgeOrphans` 每次 `os.RemoveAll` 成功之后同步删行（顺序固定为先删目录、再删行）。
- `executor/proc.go`、`executor/http.go`：`writer.Close()` 之后写一行索引，紧跟现有的 `result.Meta.Artifact = core.ArtifactAvailable`（`proc.go:171`）。
  **索引登记由 Runner 而不是 ArtifactStore 发起**：`Kind` 与 `Profile` 只有 Runner 拿得到（`r.profile`），而 `ArtifactStore.Open(jobID, attempt)` 不认识档位。因此 `ArtifactInfo` 结构不加字段（它还要原样写进 `meta.json`），`Kind`/`Profile` 走 `IndexRecord` 带上；`store/sqlite` 侧在 `Record` 里把 `OutPath`/`ErrPath` 折成相对 `output.dir` 的写法入库。
- `api/handlers_executors.go`：`markArtifactPurged`（`:183`）里补一次索引标注——这条路径的存在本身就说明"文件消失是被读出来的，不是被告知的"，索引表正是补这个短板。
- 新增 `GET /jobs/:id/artifacts`（§8.2）。

### 9.5 审计（`api/`）

- `api/audit.go`（新增）：`auditMiddleware(s *Server) gin.HandlerFunc`，注册在 `setupMiddleware` 的鉴权之后（`api/server.go:184` 之后，Principal 必须已知），只处理 `POST/PUT/DELETE`。
- `route` 与 `status` 都在 `c.Next()` **之后**读，取路由匹配与状态码的最终值。
- `action` 由路由模板映射到封闭集：`/api/v1/jobs` + POST → `job.create`；`/api/v1/jobs/:id/pause` → `job.pause`；`/api/v1/groups/:name` + DELETE → `group.delete`；`/api/v1/admin/*` → `admin.*`；未匹配（`c.FullPath()` 为空）→ `unmatched`。映射表写在 `api/audit.go` 的一个 `map[string]string` 里，与 `setupRoutes` 一处对照维护。
- `api/handlers_executors.go`：`gateExecutorSubmission`（`:537`）在各 return 点用 `c.Set(auditKey, ...)` 落结论，中间件在 `c.Next()` 之后读出并填入 `exec_*` 三列。改动是每处一行，判定顺序与逻辑不变。
- `api/security.go`：`logAccessRejection`（`:308`）同时 stash 一个 `role_denied` 结论，让 slog 与表同源。
- `api/handlers_admin.go`：新增 `GetAudit` handler 与路由。

### 9.6 装配（`cmd/server/main.go`）

`runtimeDeps` 新增闭包 `newObservabilityDB func(core.Config, *slog.Logger) (observabilityDB, error)`，返回的是 `cmd/server` 一侧定义的窄接口（`Path`/`JournalMode`/`Stats`/`Close`）而不是 `*sqlite.DB`：关闭顺序只有在句柄可替换时才断言得出来，且与本文件既有的 `schedulerAPI`/`serverAPI` 同一做法（接口定义在消费方）。实现列入既有的依赖完整性检查（`run` 开头那段 `== nil` 判断，参照 `newExecutorRegistry`/`newArtifactStore` 的处理：**闭包必须显式提供，缺了不让启动**）。`newServer` 的签名扩到传事件与审计依赖，测试里的替身跟着改。

`observability.enabled == false` 时：不调用该闭包、不建目录、三个注入全部传 nil，行为与本设计之前逐字节一致。

## 10. 安全模型

1. **不进表的内容**：任何请求体、`payload` 内容、参数取值、argv、env、密码字段。`route` 存模板而非原始 URL。`exec_reason_code` 是封闭枚举（D7），人类可读说明留在 slog。
2. **登录端点**：`/api/v1/auth/login` 与 `/refresh` 会进台账（POST），但只记 `route`/`status`/`remote_ip`，永不记体。登录失败次数另有 `loginLimiter` 在内存里限制，因此这张表不会被暴力尝试写满。
3. **凭据不进子进程这条既有约束不受影响**：`GODELAYQ_` 前键的排除逻辑在 `executor/profile.go:1010` 附近，本设计不触碰。
4. **文件权限**：库文件 `0640`、目录 `0750`，与产物文件同一取向（`executor/artifact.go` 的权限常量）。`data/` 若与 `jobs.json` 同级，则同一份备份策略覆盖两者。
5. **读取档位**：`/jobs/:id/artifacts` 取 viewer（它是元信息，不含输出正文，正文端点另有更严的档位判定：含 secret 参数的档位会把读取门槛升到提交档，`api/handlers_executors.go:72-79`）；`/admin/audit` 取 ops，因为行内含账号名与拒绝原因。

## 11. 兼容性与回滚

| 场景 | 行为 |
| --- | --- |
| `enabled: false`（默认） | 不建库文件、不订阅总线、不注入；两个事件端点与现在一字不差 |
| 库文件被手工删除 | 下次启动重新建表；历史事件与台账丢失，任务快照不受影响（这是"加速器而非账本"定位的直接后果） |
| 库打开失败（权限、磁盘满） | 启动失败并给出原因，与档位配置非法、产物目录建不起来同一口径：不带"记不住日志"的状态上线 |
| 单条写入失败 | 记 error 并计数，不冒泡到调度或请求路径（与 `core/scheduler.go:352` 的"尽力落盘"口径一致） |
| 回滚 | 三张表彼此独立，可以只关掉 `audit.enabled`；整体回滚是删 `observability` 一节 + revert `cmd/server` 的装配提交 |

## 12. 验收清单

1. `enabled: false` 时启动，`data/` 下不出现任何 `.sqlite*` 文件，`GET /jobs/:id/events` 响应与改动前逐字段一致。
2. 跑一个任务、看时间线、`Ctrl-C` 停服、重启、再查同一任务的时间线：事件仍在，且顺序与重启前一致（升序）。
3. 队列容量临时配成 2，提交 500 条批量任务：进程不崩、有 warn、`Stats()` 报告 `dropped > 0`、调度吞吐不受影响（这条是"不回压 EventBus"的证据）。
4. 执行一次 `exec.hello`：`artifact_index` 出现一行、`GET /jobs/:id/artifacts` 报出的 `out_bytes` 与产物文件实际大小一致；按 TTL 清理后该行消失；`GET /jobs/:id/result` 触发 purged 时索引状态同步。
5. 用 viewer 账号提交 `exec.*`（默认 `required_role: admin` 会拒）：`write_audit` 出现一行 `verdict=denied`、`exec_verdict=role_denied`，且表内任何列不含参数取值。
6. `POST /jobs/batch` 混合权限：一条 HTTP 请求一行审计，不是 N 行。
7. 优雅关闭：`stopObservability` 在 `store.Close` 之前完成；反复 `Stop` 不 panic、不重复 Flush；`-race` 干净。
8. 全仓：`go build ./...`、`go vet ./...`、`go test ./... -race`、`go test -run TestExampleConfigMatchesLocal ./core`、linux/darwin 交叉编译、`-tags dashboard` 构建。

## 13. 风险与后续演进

| # | 风险 | 应对 |
| --- | --- | --- |
| 1 | 二进制约 +8~10MB、`go.sum` 新增一条重依赖树（实测：S02 落地为 **+5.5 MiB / +17.7%**，`windows/amd64` 的 32,773,120 B → 38,583,808 B；驱动取 `modernc.org/sqlite v1.46.0`，因为 v1.47.0 起要求 `go 1.25`、v1.60.x 要求 `go 1.26`，抬版本会连带抬高整仓最低工具链与 `examples` 的 `go run` 门槛） | 记进 `docs/deployment.md`；驱动只在 `store/sqlite` 出现，删掉该包即可完全回到现状 |
| 2 | 备份口径变化：WAL 模式带 `-wal`/`-shm` 旁文件 | `docs/deployment.md` 明确"运行中备份用 `VACUUM INTO` 而不是拷单文件" |
| 3 | 表可能缺页（队满丢弃 / 事务二次失败） | `dropped` 计数公开在 `/admin/runtime`；读端点的 `Note` 说明写入延迟。**不做"绝不丢"承诺** |
| 4 | `write_audit` 随写请求量线性增长 | 默认 90 天 + 条数上限；部署方按吞吐调低。本期不做按列裁剪配置（未生效的选项不进配置） |
| 5 | 事件保留与快照留痕不同步：快照被淘汰后时间线还在 | 明确定为特性（历史事件比快照留得久对排障有利），写进 §6.1 注释与文档 |
| 6 | 后续做"快照换 SQLite"时可能与本包的迁移表撞名 | 迁移版本表命名 `observe_schema_migrations`，与未来的 `core` 后端无关 |

后续演进（都不在本期）：控制台审计页（S08）；把 `findSnapshot`/`ListJobs` 换成条件下推（依赖 §4.3 的选型）；`/jobs/:id/artifacts` 给前端做"按尝试对比输出"；把 `note` 从字符串升级成 `{source, oldest_seq, dropped}` 的观测块。

## 14. 实施计划

卡片见 `tasks/sqlite/README.md`。顺序：S01 配置 → S02 包与装配 → S03 事件写入 → S04 事件读取 → S05 产物索引 → S06 写审计 → S07 文档与全仓验证 →（独立排期）S08 控制台审计页。
