# TASK-S06　`write_audit` 表、审计中间件与查询端点

- 所属阶段：M3 审计
- 依赖任务：TASK-S01、S02
- 涉及文件：新增 `api/audit.go`、`api/audit_test.go`、`store/sqlite/audit.go`、`store/sqlite/audit_test.go`；改 `api/server.go`、`api/security.go`、`api/handlers_executors.go`、`api/handlers_admin.go`、`cmd/server/main.go`
- 预计规模：中

## 1. 任务目标

用一块 gin 中间件把所有写操作（`POST`/`PUT`/`DELETE`）登记成一行台账，执行器任务的拒绝原因作为补充列写进同一行；并提供 `GET /api/v1/admin/audit` 只读查询端点（ops 档）。

## 2. 背景与当前问题

`docs/design/web-console-design.md:607` 已经登记了这件事的两半：

```
# 写操作审计本期只输出结构化日志（§5.7.6）；/admin/audit 查询端点留二期
```

§5.7.6（同文件 `:793`）把字段形状也定好了：`who(name) role= action= method path status= latency=`，并明确"落现有 logger，不引入新存储。持久化后可查询属二期"。本卡就是那个二期，路由名沿用预留的 `/admin/audit`，字段沿用已定的形状。

**这里要修正设计阶段的一个先前结论。** 设计讨论时曾提出"只审计 `exec.*` 档位、不审计普通任务提交"的窄表方案。它不成立：`§5.7.6` 已把审计范围定为全部写操作，若另起一张只记执行器的表，同一个 `POST /jobs` 请求会同时落在两处、且两张表对"谁做了什么"的回答不一致。因此本卡做**一张宽表 + 执行器补充列**（设计文档 D6）。

现状的具体缺口：

- 越权拒绝的唯一出口是 `logAccessRejection`（`api/security.go:308`），它只 `logger.Warn`；`gateExecutorSubmissionRole`（`api/handlers_executors.go:509`）拒绝时带一句 `Details`，进 HTTP 响应后就丢了。事后无法回答"这台机器上周被谁尝试提交过执行任务、为什么被拒"。
- `Principal` 的注释已经预留了审计语义：`machinePrincipalName` 那行写着"仅用于日志与审计显示"（`api/authenticator.go:24`）。

## 3. 要实现的功能

1. `api/audit.go`——消费方接口与记录结构定义在 `api` 侧：

   ```go
   // AuditEntry 是一行写操作台账。字段与 store/sqlite 的表列一一对应。
   type AuditEntry struct {
       Time time.Time; Actor, ActorKind, Role string
       Action, Method, Route string
       Status int; Latency time.Duration
       Verdict string
       ExecVerdict, ExecReasonCode, HandlerKey, Profile, JobID string
       RemoteIP, UserAgent string
   }
   // AuditRecorder 是台账的写入能力；nil 表示未装配，中间件退回"只记结构化日志"（即现状）。
   type AuditRecorder interface { Append(AuditEntry) error }
   // AuditReader 是台账的查询能力，供 /admin/audit 用。
   type AuditReader interface { Query(f AuditFilter) ([]AuditEntry, int, error) }
   func WithAuditLog(w AuditRecorder, r AuditReader) Option
   ```

   `AuditFilter{Actor, Action, Verdict string; Since, Until *time.Time; Limit, Offset int}`。
   接口分开成写与读两个，理由与 S04 的 `eventReader` 一致：装配方可以只给写、不给读（测试替身常用）。
2. 中间件 `auditMiddleware(s *Server) gin.HandlerFunc`，注册在 `setupMiddleware` 的**鉴权之后**（`api/server.go:184` 之后），因为 `Principal` 由 `authMiddleware` 用 `c.Set(principalKey, p)` 放进去（`api/security.go:52`），早于它就取不到身份。
   行为：
   - 只处理 `POST`/`PUT`/`DELETE`，其它方法直接 `c.Next()`。
   - **所有字段都在 `c.Next()` 之后读**：`c.Writer.Status()` 与 `c.FullPath()` 要到处理器执行完才是终值。开始时间用 `c.Next()` 之前的一次 `time.Now()`。
   - 未注入 recorder 时：仍然构造 `AuditEntry` 并 `logger.Info("write operation audited", ...)`（把 §5.7.6 那句结构化日志补齐——现在它只覆盖被拒的请求，成功写操作没有单行审计日志）。**这条是本卡的顺带收益，别漏。**
3. `route` 存路由模板而不是原始 URL，`action` 由模板映射到封闭集（映射表是一个 `map[string]string`，与 `setupRoutes` 一处对照维护）：

   | 路由模板 | `action` |
   | --- | --- |
   | `/api/v1/jobs` POST | `job.create` |
   | `/api/v1/jobs/:id` PUT | `job.update` |
   | `/api/v1/jobs/:id` DELETE、`/jobs/:id/cancel` POST | `job.cancel` |
   | `/jobs/:id/pause` / `resume` / `retry` / `force-pause` POST | `job.pause` / `job.resume` / `job.retry` / `job.force_pause` |
   | `/api/v1/jobs/batch` / `batch-ops` POST | `job.batch_create` / `job.batch_op` |
   | `/api/v1/groups` POST、`/groups/:name` PUT、DELETE | `group.create` / `group.update` / `group.delete` |
   | `/api/v1/admin/scheduler/suspend` / `unsuspend` POST、`/admin/events` DELETE | `admin.scheduler_suspend` / `admin.scheduler_unsuspend` / `admin.events_clear` |
   | `/api/v1/auth/login` / `refresh` / `logout` / `ws-ticket` POST | `auth.login` / `auth.refresh` / `auth.logout` / `auth.ws_ticket` |
   | `c.FullPath()` 为空（路由未匹配） | `unmatched` |

   表里没有的写路由映射到 `other`，并记一条 debug：说明映射表与 `setupRoutes` 不同步了。**不要**为未匹配的路径回退到 `c.Request.URL.Path`——那会把任务 ID 与查询串带进列里（设计文档 §6.3）。
4. `verdict` 由 HTTP 状态码派生的封闭集：`2xx→ok`、`400→bad_request`、`401/403→denied`、`404→not_found`、`409→conflict`、`207→partial`（批量端点用 207 表达混合结果，见 `api/handlers.go` 的 `BatchCreateJobs`）、`5xx→error`。
5. 执行器补充列（`exec_verdict`、`exec_reason_code`、`handler_key`、`profile`）：
   - 在 `api/handlers_executors.go` 的 `gateExecutorSubmission`（`:537`）各 return 点用 `c.Set(auditExecutorKey, auditExecutorInfo{...})` 落结论，中间件在 `c.Next()` 之后读出填入。改动是每个 return 点一行，判定顺序与逻辑一字不改。
   - 取值集合是封闭的：`accepted`、`role_denied`、`profile_unavailable`、`payload_rejected`、`timeout_rejected`。
   - **`exec_reason_code` 不存 error 原文**（设计文档 D7）：参数校验失败的错误文本里可能含参数取值，而执行侧既有规范是"档位名可以外泄，参数值不可以"（`executor/proc.go:318-321` 的 `logRun` 注释）。人类可读的说明继续走 slog，表里只有枚举码。
   - `handler_key`/`profile` 来自 `Profile.HandlerKey()` 与 `Profile.Name`，两者都是配置里声明的名字，不含用户输入。
   - `logAccessRejection`（`api/security.go:308`）也 stash 一个 `role_denied`，让 slog 与表同源；它的签名不变（额外信息从 `c` 拿不到，所以用 gin 上下文而不是参数传递——实现时若发现拿不到 `c`，允许改为在调用点各自 stash，把取舍记进第 10 节）。
6. `job_id` 列：只在能从响应里便宜地拿到的路径上填（`CreateJob` 成功后 `c.Set` 一次），批量请求留空。理由与设计文档 §6.3："一行对应一个 HTTP 请求"，批量里的逐条结果继续走响应体与 slog。
7. `store/sqlite/audit.go`：`NewAuditLog(db *DB, opts AuditLogOptions, logger *slog.Logger) (*AuditLog, error)`，内部复用 S02 的 `batcher`；`Append` 不阻塞（队满丢弃并计数，`Dropped()` 可查）；`Query` 按 `seq DESC`（最新在前）+ `limit/offset` 分页，并返回匹配总数（`COUNT(*) OVER ()` 或两次查询皆可，记进第 10 节）。
   保留淘汰：与 S03 同一形状（每批一次，按 `seq` 上界与 `ts_us` 下界两条语句）。
8. 端点 `GET /api/v1/admin/audit`：注册在既有 `admin` 组里（`api/server.go:256` 已要求 ops 档），参数 `?limit=&offset=&actor=&action=&verdict=&since=&until=`（时间用 RFC3339）；未注入 reader → 503，口径与 `requireArtifacts()` / `requireIndex()` 一致；响应 `{count, total, limit, offset, items:[...]}`。
   **`items` 永不含参数字段**：这张表从构造上就没有 payload 列，注释里写死这条，防止后来人"顺手加一个 `body` 列"。

## 4. 实现步骤

1. 先写 `api/audit.go` 的接口、`action` 映射表与 `verdict` 派生函数（纯函数，配单测）。
2. 写中间件（先不接表，只走"未注入 → 记 slog"分支），配 gin 测试断言字段。
3. 写 `store/sqlite/audit.go` 的 `Append`/`Query`/保留淘汰。
4. 接执行器补充列（`gateExecutorSubmission` 的五个 return 点 + 一处成功）。
5. 加查询端点与 503 守卫，最后接 `cmd/server` 装配。

## 5. 测试要求

`api/audit_test.go`（用假 recorder）：

1. `TestAuditMiddleware_OnlyWrites`：`GET /api/v1/jobs`、`/health`、`/ws` 不产生任何台账行。
2. `TestAuditMiddleware_StatusFieldsAreFinal`：一次 `POST /jobs` 成功 → 断言 `status=201`、`action=job.create`、`route=/api/v1/jobs`、`verdict=ok`、`latency > 0`。故意让处理器 `c.JSON(201, ...)` 之后再检查，证明读的是终值。
3. `TestAuditMiddleware_RoleFields`：三种身份各一次——账号登录（`actor_kind=user`）、静态 token（`machine` + `role=operator` 的折算值）、未配鉴权（`anonymous`、`actor` 为空）。
4. `TestAuditMiddleware_RouteTemplateNotRawURL`：请求 `/api/v1/jobs/0198.../pause?token=secret-value`，断言 `route` 是模板、**整行任何列都不含 `secret-value`**。这条守住查询串不外泄。
5. `TestAuditMiddleware_UnmatchedPath`：`POST /api/v1/nope` → `action=unmatched`、`route` 为空或固定哨兵，且不含请求路径。
6. `TestAuditMiddleware_BatchIsOneRow`：`POST /jobs/batch` 提交 5 条 → 只有 1 行，`verdict=ok`（全成功）或 `partial`（混合）。
7. `TestAuditExecutorInfo_ReasonCodes`：表驱动，五个拒绝分支各一次（角色不足、档位不可用、payload 非法、超时越界、成功），断言 `exec_verdict` 与封闭集一一对应。
8. `TestAuditExecutorInfo_NoArgValues`：用 `exec.hello` 提交一个含 secret 参数、且参数值故意写成可搜索字符串的 payload，被拒之后断言整行不含该字符串。**这条是本卡最重要的负向断言。**
9. `TestAuditMiddleware_WithoutRecorderLogsLine`：未注入 recorder 时产生一行 `write operation audited` 日志、字段齐全、不 panic。
10. `TestAuditMiddleware_RecorderErrorIgnored`：recorder 返回错误 → 请求响应不受影响（`Append` 本来也不返回错误给调用方）。

`store/sqlite/audit_test.go`：

11. `TestAuditLog_RoundTrip`：写入若干行后 `Query` 回来，除时间精度外字段全等；`Query` 返回的顺序是 `seq DESC`。
12. `TestAuditLog_FilterAndPagination`：按 `actor`/`action`/`verdict`/时间窗过滤，`limit`/`offset` 翻页不重不漏，`total` 是不带 `limit` 的匹配数。
13. `TestAuditLog_DroppedWhenFull` / `TestAuditLog_Retention`：与 S03 §5 第 6、7 条同形。
14. `TestGetAudit_NoReader503`、`TestGetAudit_FilterValidation`：非法 `action=../x`、非法时间格式 → 400，且错误信息不泄露 SQL 片段。

## 6. 完成标准（DoD）

- [x] `observability.enabled=false` 或 `audit.enabled=false` 时不注入 recorder：表不写、端点 503，但 §5 第 9 条的"补上单行审计日志"这条收益仍然生效（它就是现状缺的那一半）。（冒烟第二轮 A/B 两轮实测：写请求 201 照常、`GET /admin/audit` 回 503 原文、`write_audit` 表 0 行、日志里 3 行 `write operation audited`；用例 `TestAuditMiddleware_WithoutRecorderLogsLine`、`TestRun_AuditWriterOffByEachSwitch`、`TestGetAudit_NoReader503`）
- [x] 一行对应一个 HTTP 请求，批量请求不是 N 行（§5 第 6 条）。（`TestAuditMiddleware_BatchIsOneRow`：5 条目的批量 → `capture.only(t)` 即恰好一行；该行的 `job_id` 与 `exec_verdict` 都留空）
- [x] 表里任何列都不含请求体、参数取值、密码、原始查询串。§5 第 4、8 两条是这条的证据，缺一不可。（`TestAuditMiddleware_RouteTemplateNotRawURL` 守住查询串、`TestAuditExecutorInfo_ReasonCodes` 与 `TestAuditExecutorInfo_NoArgValues` 守住参数取值；手工第四轮把 canary 写进档位参数跑通一次执行，`write_audit` 整表 0 命中）
- [x] `action` 映射表覆盖 `setupRoutes` 里的全部写路由；未覆盖时落到 `other` 并记 debug，用例里造一条新写路由证明这个兜底真的会触发。（`TestAuditMiddleware_MappedActionCoversWriteRoutes` 用 `engine.Routes()` 双向对照，`TestAuditMiddleware_UnmappedRouteFallsBackToOther` 现造一条写路由注册到同一台引擎上证明兜底会触发）
- [x] `gateExecutorSubmission` 的五个 return 点逻辑一字未改，只多了 stash 一行（对照：本卡前后 `api/handlers_executors_test.go` 全部通过）。（`go test ./api -run Executor` 9 条 PASS；五处各是 `api/handlers_executors.go:529`、`:563`、`:572`、`:584`、`:595` 单行）
- [x] 队满丢弃可观测（`Dropped()`），且丢弃不阻塞请求（`Append` 非阻塞）。（`TestAuditLog_DroppedWhenFull`：容量 4 塞 10 条 → 队列 4、`Dropped()` 6；`Append` 走 S02 的 `batcher.append`，队满只加计数）
- [x] `docs/api.md` 的"运维 API（ops）"一节补该端点与全部查询参数；`web-console-design.md:607` 与 §5.7.6 的"留二期"标注改为指向本卡。（api.md 新增"写操作台账：`GET /api/v1/admin/audit`"小节，含 7 个参数、两套封闭词表、503/400/500 与实测响应；web-console-design.md 的端点表那一行与 §5.7.6 全段改写，见 §10.5 的 D-0603）

## 7. 验收方式

```bash
go build ./... && go vet ./... && go test ./... -race
go test ./api -run Audit -v
go test ./store/sqlite -run Audit -v
```

手工（临时目录配置 + 独立账号，跑完删除）：
配两个账号（`operator01/operator`、`admin01/admin`）与一个静态 token；依次做
「operator 建一条普通任务成功」「operator 提交 `exec.hello` 被拒」「admin 提交 `exec.hello` 成功」
「viewer 试图 `DELETE /groups/x` 被拒」四件事，然后 `GET /api/v1/admin/audit?limit=10`：
断言四行、`verdict` 分别是 `ok` / `denied` / `ok` / `denied`，第二行 `exec_verdict=role_denied`、
第三行 `accepted`；再用 `SELECT * FROM write_audit` 确认**整表搜不到 payload 里的字符串**。
最后用静态 token 调 `/admin/audit` → 403（machine 折算 operator，低于 ops）。

> 这一段的前提交错两处，实测时按 §10.5 的 D-0604 补了一个 ops 账号与一个 viewer 账号才跑通；
> 逐轮记录在 §10.4。

## 8. 不在本任务范围

- 不做读操作审计（`GET` 量大且没有权限变更含义）。
- 不做告警、导出（CSV/JSON）、按小时聚合统计。
- 不给审计页做前端（S08）。
- 不改 `loginLimiter` 的内存实现，也不把登录失败计数搬进表（跨重启累计会改变锁定行为，设计文档 §4.2）。
- 不做防篡改（追加校验、哈希链）：这张表的用途是运维追溯，不是法律证据。
- 不记录 `slog` 已有的详细内容（表里只有枚举码 + 计数 + 身份），两者互补而不是互相替换。

## 9. 风险与回滚

- 风险：**表随写请求量线性增长**。默认 90 天 / 50 万行上界；CI 里高频建任务的部署会先撞到条数上界而不是天数，`?action=job.create` 的历史会比预期短。部署方需要长历史时应调 `audit.retention_count`，这一点写进 `docs/deployment.md`（S07）。
- 风险：`action` 映射表与 `setupRoutes` 是两处，新增写路由容易漏更。应对有两层：兜底 `other` + debug 日志（不会因为漏配而静默丢行），以及 §6 第 4 条要求用一条用例证明兜底可触发。中期收口办法（本期不做）是在 `setupRoutes` 里注册时顺手登记映射，代价是路由声明与审计声明耦合，需要单独判断是否值得。
- 风险：中间件顺序错（注册在鉴权之前）会导致全部行的 `actor` 为空，而单测若不覆盖"带凭据的请求"就发现不了。§5 第 3 条三种身份各一次就是为这条服务的。
- 风险：`c.Writer.Status()` 在 SSE/WS 这类长连接上语义不明确——但本卡只审计写方法，`GET /ws` 与 `GET /sse/events` 不进这条路径，因此不需要处理。这个判断要写在中间件注释里，避免后来人为 WS 加特例。
- 回滚：`audit.enabled: false` 即停止写入（端点转 503）；代码层是 `api/audit.go` + `store/sqlite/audit.go` 两个新文件加若干处单行 stash，可按文件粒度回滚。

## 10. 实现记录（执行时补写）

### 10.1 落点

| 能力 | 位置 |
| --- | --- |
| `AuditEntry`（17 字段，与表列同名） | `api/audit.go:16` |
| 三套封闭词表 | `api/audit.go`：`verdict` 常量 47-57、`exec_verdict` 60-66、`exec_reason` 74-79；`action` 映射表 `auditActions`（97）、`AuditVerdicts`（122）、`AuditActions()`（129） |
| 兜底动作词与 User-Agent 长度上界 | `auditActionUnmatched`/`auditActionOther`（83-86）、`auditUserAgentLimit = 256`（90） |
| 消费方接口与注入 | `AuditRecorder`（197）、`AuditReader`（203）、`AuditFilter`（209）、`WithAuditLog`（223） |
| 中间件 | `api/audit.go:239` `auditMiddleware`（只处理 POST/PUT/DELETE，全部字段在 `c.Next()` 之后读）；构造器 `buildAuditEntry`（258） |
| 状态码 → verdict | `auditVerdict`（335），207 先于 2xx 判、429 归 `throttled`、其余 5xx 归 `error` |
| 未装配时的日志行 | `recordAudit`（363）+ `auditLogAttrs`（376）；`User-Agent` 截断在 `truncateRunes`（397） |
| 执行器结论的挂载与读取 | `auditExecutorInfo`（144）、`stashAuditExecutor`（170）、`stashAuditJobID`（186）、`markAuditBatch`（162）；五个写入点在 `api/handlers_executors.go:529/563/572/584/595` |
| 注册顺序 | `api/server.go:218`（`authMiddleware` 之后一行）；台账字段 `api/server.go:65-70` |
| 查询端点 | 路由 `api/server.go:301`；`requireAudit`（`api/handlers_admin.go:86`）、`AuditItem`/`AuditResponse`（102/127）、`GetAudit`（139）、`parseAuditFilter`（174）、`parseAuditInt`（249）、`parseAuditTime`（260） |
| 真库实现 | `store/sqlite/audit.go`：`AuditLogOptions`（25）、`NewAuditLog`（85）、`Append`（128）、`mapEntry`（140）、`writeBatch`（177）、`prune`（213）、`Query`（245）、`auditWhere`（287）、`scanAudit`（319）、`Flush`/`Dropped`/`Count`/`Close`（367/372/377/389） |
| 装配 | `cmd/server/main.go`：`auditLogAPI`（93）、`observabilityAPI`（107）、`newAuditLog` 字段（140）与默认闭包（206）、依赖完整性检查（312）、构造与收尾（459 起）、启动日志 `audit_writer`（483）、交给 `newServer` 的 `obs`（504-506） |

依赖方向沿用 S05 那条：`store/sqlite` import `api`（接口定义在消费方，实现方去引它），
所以 `go list -deps ./api | grep -i sqlite` 与 `./executor` 同一命令都是空，驱动仍只在 `store/sqlite` 里。

### 10.2 偏离了卡片的地方

1. **`newServer` 的第 6 个参数换了类型，而不是新增第 7 个**（S05 §10.2 第 7 条说过"S06 不必动这个签名"——那句不成立）。
   事件写入器与台账写入器都来自同一个观测库、同一对开关、同一段装配，参数改成
   `obs *observabilityAPI{events, audit}`（`cmd/server/main.go:107`）。这是执行中问过的一个分叉，选的是"合并成一个观测层参数"。
   代价是既有装配字面量与替身要一起改：`cmd/server/main_integration_test.go` 里 21 处观测层装配字面量中
   7 处要显式补 `newAuditLog` 闭包（其余走 `observabilityCase` 的共用构造），新增 `auditLogStub` 替身
   （14 处引用）与 `observabilityCase` 的 `newAudit`/`gotAudit` 两个字段，
   以及关停顺序断言从 `[event_log, observability, store]` 变成 `[audit_log, event_log, observability, store]`。
2. **中间件是 `(*Server)` 的方法而不是卡片 §3.2 写的自由函数** `auditMiddleware(s *Server)`。
   它要读 `s.auditLog`、`s.logger`、`s.principal`（`PrincipalFrom` 走 `c`，但 recorder 与 logger 挂在 Server 上），
   写成方法就无需把 `s` 传来传去，注册处也就是一行 `s.engine.Use(s.auditMiddleware())`。
3. **`logAccessRejection` 一字未改**，`role_denied` 的 stash 落在它的调用点
   （`api/handlers_executors.go:529`，紧挨既有那句 `s.logAccessRejection`）。
   卡片 §3.5 末尾预先允许了这一种落法。实际原因是那个函数没有 `c` 参数（签名是
   `(p Principal, why string, required core.Role)`），而它同时被路由档位与结果端点共用——
   在函数内部 stash 会让"读 `/result` 被挡"这类请求也带上执行器列。
   代价是另外两处档位拒绝（`/jobs/:id/result` 的 secret 门槛、`RequireRole`）不会写 `exec_verdict`，
   它们的行仍靠 `verdict=denied` 与 `action` 说明。
4. **批量请求额外压掉执行器列与 `handler_key`/`profile`**（卡片只说 `job_id` 留空）。
   `markAuditBatch`（`api/audit.go:162`）在 `BatchCreateJobs` 进门处声明一次（`api/handlers.go:538`），
   之后三个 stash 助手都直接返回。理由是一行盖 5 条任务时，"这一行的执行器结论是什么"没有答案，
   填最后一条或第一条都会读成"这条任务被拒过"。
5. **`verdict` 词表比卡片 §3.4 多两项**：`throttled`（429）与 `other`（兜底）。
   429 不是理论取值——`loginLimiter` 真的会给（`api/ratelimit.go`），归到 `error` 会把"被限流"记成"服务器坏了"。
   `other` 与 `action` 的兜底同一取向，`TestAuditVerdict_CoversStatuses` 用 418 守住它。
6. **`Append` 的签名带 error**（卡片 §3.1 的接口是 `Append(entry) error`，实现照用），
   但真实现只在"已关闭"时返回错误，队满不返回错误而是计数——与 S03 的事件写入器同一口径。
   `AuditReader.Query` 返回 `([]AuditEntry, int, error)`，`int` 是匹配总数：用了两次查询（先 `COUNT(*)` 再取页），
   没用 `COUNT(*) OVER ()`。表在当前构建里只有这一处窗口函数用法，而窗口函数会把总数与排序绑在同一个语句里，
   `ORDER BY seq DESC` 一改总数口径也跟着改，所以分开写。
7. **保留淘汰的"每批一次"落在 `writeBatch` 里**（`store/sqlite/audit.go:177` 调 `prune`，213），
   与 S03 的 `EventLog` 同形；按条数与按时长两条语句在同一事务里，`retention_age` 为 0 时跳过时长那条。
8. **端点多了一条卡片没写的校验**：`?actor=` 超过 128 字符直接 400（`auditActorLimit`）。
   `actor` 来自配置里的账号名，本来到不了这个长度，但它是过滤条件里唯一原样进语句的客户端字符串，
   上界比"相信配置"便宜。
9. **`docs/api.md` 的运维一节顺带补了两处口径**（不是本卡的端点）：批量行的 `verdict` 恒为 `partial`、
   `latency_us` 在开发机上可能是 0。见 §10.5 的 D-0602/D-0606。
10. **测试用例比卡片 §5 列的 14 条多**：`api/audit_test.go` 21 条、`store/sqlite/audit_test.go` 8 条、
    `cmd/server/main_integration_test.go` 新增 4 条。多出来的是本卡自己踩到的分支：
    `buildAuditEntry` 的确定性耗时、`MappedActionCoversWriteRoutes` 的双向对照、
    `ConflictAndDeniedRows`、`GetAudit_RecordsAndPaging`、`EmptyIsListNotNull`、`ReadErrorIs500`、`RoleOps`、
    `ListIsNeverNil`、`FallbackLimit`、`AppendAfterClose`、`NewAuditLog_RejectsNilDB`、`QueryErrorIsReturned`，
    以及装配侧的"未装配时传 nil"与"打开闭包是必需的"。

### 10.3 验证证据

```
go build ./...                → 通过
go vet ./...                  → 通过
go test ./... -race           → 全绿
go test ./api -run Audit      → 21 条 PASS（含子例共 59 个 RUN）
go test ./store/sqlite -run Audit → 8 条 PASS
go test ./cmd/server -run 'Audit|Observability' → 6 条 PASS
go test ./api -run Executor   → 9 条 PASS（§6 第 5 条的"前后都过"对照）
go list -deps ./api | grep -i sqlite      → 空
go list -deps ./executor | grep -i sqlite → 空
go list -deps ./store/sqlite | grep godelayq/api → 1 条（实现方引消费方，与 S05 同方向）
gofmt -l -s（剥 CRLF 的临时副本，10 个文件）→ 无输出（首轮列出 cmd/server/main.go，是 newServer 参数换类型后那一段字段对齐没跟上，已 gofmt -w 修好并重跑）
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./cmd/server → 通过
CGO_ENABLED=0 GOOS=windows GOARCH=386 go build ./cmd/server → 通过
go vet -tags dashboard ./...  → 通过（无输出）
```

### 10.4 手工验收（本卡 §7）

临时目录 `%LOCALAPPDATA%\Temp\s06smoke`：独立二进制 + 自己写的 42 行配置，路径全在临时目录内，
仓库的 `configs/config.yaml` 与 `data/` 未写。端口 8143，静态 token 作 `machine`，
`executors.required_role: admin`，档位 `hello` 是 `kind: script` + `runtime: node` + 一个具名参数 `who`
（`args_render: ["{who}"]`），`observability.enabled: true`、`flush_interval: 50ms`。

卡片说"配两个账号"，实际配了四个：`operator01`/`admin01`/`ops01`/`viewer01`。
读端点要 ops 档（设计文档 §10.5），两个账号读不到自己造成的台账；`viewer01` 是 §7 第四条要用的
（见 §10.5 的 D-0604）。

**第一轮**（§7 的四件写操作 + 读台账 + 静态 token）：

```
1. operator POST /jobs (data_sync)            → 201
2. operator POST /jobs exec.hello             → 403 insufficient role
3. admin    POST /jobs exec.hello             → 201（attempt 0，job_id 01a0f43d-3428-…436f）
4. viewer   DELETE /groups/nope               → 403
   admin    DELETE /groups/nope               → 404（顺手取一行 not_found）
6. machine（静态 token）GET /admin/audit      → 403
9. viewer   GET /admin/audit                  → 403
```

`GET /api/v1/admin/audit?limit=12` 回 `count=9 total=9`，最新在前，§7 要断言的四行是：

```
{"action":"job.create","actor":"admin01","role":"admin","verdict":"ok","status":201,
 "exec_verdict":"accepted","handler_key":"exec.hello","profile":"hello",
 "job_id":"01a0f43d-3428-7dfa-8fc0-4a93a626436f","route":"/api/v1/jobs"}
{"action":"job.create","actor":"operator01","role":"operator","verdict":"denied","status":403,
 "exec_verdict":"role_denied","exec_reason_code":"admin","handler_key":"exec.hello","profile":"hello"}
{"action":"job.create","actor":"operator01","role":"operator","verdict":"ok","status":201,
 "job_id":"01a0f43d-339f-74f3-b1b9-0ec0671e7455"}
{"action":"group.delete","actor":"viewer01","role":"viewer","verdict":"denied","status":403,
 "route":"/api/v1/groups/:name"}
```

四行的 `verdict` 与 `exec_verdict` 与 §7 的断言一致；`route` 是模板、没有任务 ID 出现在任何列里。

**过滤与错误响应**（同一轮取到原文）：

```
GET /admin/audit?action=job.create&limit=5           → count=3 total=3
GET /admin/audit?actor=admin01&action=group.delete   → count=1
GET /admin/audit?since=2099-01-01T00:00:00Z          → count=0 total=0（items 是 []）
GET /admin/audit?limit=-5    → 400 invalid limit  "expected a non-negative integer"
GET /admin/audit?offset=abc  → 400 invalid offset "expected a non-negative integer"
GET /admin/audit?verdict=nonsense → 400 invalid verdict
     "expected one of ok, bad_request, denied, not_found, conflict, partial, throttled, error, other"
```

（`?offset=abc` 与 `?limit=-5` 首轮跑出的是 **200 空体**，那是缺陷 D-0601，修好后才是上面的 400。）

**D7 反查**：canary 分两处放——普通任务的 `payload.note`（`plain-job-canary-3d91`）与
档位参数 `args.who`（`exec-arg-canary-8f2c`）。整库（含 `-wal`）用 `grep -a` 扫：

```
grep -a 'plain-job-canary-3d91' observe.sqlite* → 0 处
grep -a 'exec-arg-canary-8f2c'   observe.sqlite* → 1 处
grep -a 'canary'                 write_audit 全部文本列 → 0 处（SQL 侧 LIKE 复查）
```

那 1 处在 `job_events` 第 6 行的 `data` 里，是执行结论事件的输出预览：

```
type=job.completed data={"result":{...,"preview":"hello exec-arg-canary-8f2c\n\n",...}}
```

脚本把参数原样打印出来，预览就把值带回了表——那是 S04/S05 的事件表与 `preview` 字段既有设计，
不是本卡的台账（`write_audit` 侧 0 命中）。登记为 D-0605。

**第二轮**（回滚开关，A/B 各一次，每轮先把 `data/` 清空）：

| 配置 | 启动日志 | 写请求 | `GET /admin/audit` | `write_audit` 行数 | `write operation audited` 日志 |
| --- | --- | --- | --- | --- | --- |
| `observability.enabled=true` + `audit.enabled=false` | `events_writer=true artifact_index=true audit_writer=false` | 201 | 503 | 0（`job_events` 1 行，说明只关了审计那一路） | 3 行 |
| `observability.enabled=false` | 无 `observability enabled` 那行，也不建库文件 | 201 | 503 | 观测库文件不存在 | 3 行 |

503 原文（两轮相同，`details` 把两个开关都点名了）：

```
{"code":503,"message":"write audit log is not configured","details":"start the server with observability.enabled and observability.audit.enabled to enable /api/v1/admin/audit"}
```

进程一律 `taskkill //F` 结束（Windows 上外部发不出优雅 SIGTERM，README 第 4 条口径）；
两轮跑完删掉临时目录。

### 10.5 缺陷处置

| 编号 | 严重度 | 事实 | 处置 |
| --- | --- | --- | --- |
| D-0601 | 中 | 首轮手工里 `?limit=-5`、`?offset=abc` 返回 **200 空体**而不是 400：`parseAuditInt` 只把结果报给调用方，四个数值/时间参数里只有 `limit`/`offset` 这一支漏了写响应，gin 于是给出没有 body 的 200 | **已修**：`api/handlers_admin.go:249` 把 `parseAuditInt`/`parseAuditTime` 改成 `(*Server)` 方法、错误响应由它们自己写（少补一处就是"返回 false 但没人写过响应"，注释里写明）。证据：`TestGetAudit_FilterValidation` 的 `limit=0`/`offset=abc`/`since=not-a-time` 三个子例都断言 400，第二轮手工取到上面那三条原文 |
| D-0602 | 低 | §5 第 2 条要求断言 `latency > 0`，本机做不到稳定成立：Windows 的单调时钟粒度粗到 `time.Since` 在一个 tick 内返回 0（写死一个忙循环探针实测：函数内部读到 `elapsed=0s`，函数外部同时读到 625.9µs）。真二进制里也有同样现象——手工四行里三行 `latency_us: 0`，只有 bcrypt 登录那几行是 90-196 ms | **已按现状调整断言 + 文档化**：`TestAuditMiddleware_StatusFieldsAreFinal` 改判 `GreaterOrEqual(0)`，另加 `TestBuildAuditEntry_LatencyFromStart`（把 `started` 往前推 5ms 再构造，确定性地证明读的是 `c.Next()` 之前那一个时间点）。`docs/api.md` 的 `latency_us` 一段明写"量级参考，不是精确计时" |
| D-0603 | 低 | 卡片 §3.2 说"成功写操作没有单行审计日志，本卡的日志行是补上缺的那一半"——不准确：`api/logging.go` 的 `requestLogger` 已经为**每个**请求（含读）记一行 `msg="http request"`，带 `who`/`role`/`status`/`latency_ms`/`client_ip`。手工第二轮的日志里同时有这两种行 | **已改文档**：`docs/design/web-console-design.md` §5.7.6 重写为"两条出口 + 与访问日志的分工"（访问日志说"这个请求被怎么处理"、台账行说"谁改动了什么"，字段集不同），`recordAudit` 的注释同口径。日志行仍然要补：它是台账形状的（有 `action`/`verdict`/执行器列），访问日志没有 |
| D-0604 | 低 | 卡片 §7 的手工配方跑不通：`/admin/audit` 要 ops 档，而配方只给了 `operator01`、`admin01` 两个账号（`admin` 也不行，设计文档 §10.5）；第四条动作要的 `viewer` 账号同样没给 | **已按现状执行**：临时配置配了四个账号（补 `ops01`/`viewer01`），卡片 §7 段末加了一句指向本条的说明。端点档位本身按设计文档不变 |
| D-0605 | 中 | `job_events` 的 `data` 列会带出参数取值：档位脚本把入参打印到 stdout，`job.completed` 事件的 `result.preview`（输出的前 512 B）就把 `exec-arg-canary-8f2c` 原样写进了观测库（§10.4 的 D7 反查实测 1 处命中）。设计文档 §10.1 那句"不进表的内容：……参数取值"字面上覆盖所有表 | **登记不修**：不在本卡范围内——`write_audit` 侧 0 命中，D7 对本卡的约束成立；预览进事件是 S03/S04 已落地的既有行为（`docs/api.md` 的"事件里的执行结论"一节已写明 `preview` 字段与它的截断上界）。要收口要么截掉预览、要么在事件里只留字节数，两者都会改变 `/jobs/:id/result` 与控制台时间线的现有表现。已在 §10.4 记下现场，交给 S07 判断是否写进部署文档的告警段 |
| D-0606 | 低 | 两个词表在台账里各有"恒等于另一个值"的情形，读的人容易以为数据缺了：① `job.batch_create`/`job.batch_op` 的 `verdict` **永远是** `partial`（批量端点全成功也回 207，`api/handlers.go:585`），② `role_denied` 那行的 `exec_reason_code` 存的是**要求达到的档位名**（`admin`），与另外三个取值（`payload_invalid` 等）不是一套词 | **已文档化**：`docs/api.md` 的 verdict 段落明写批量恒为 `partial`；`exec_reason_code` 那一段写明"其中 `role_denied` 那行存的是要求达到的档位名"，`api/audit.go:68-73` 的常量注释同口径（两个集合都是封闭的、都不含用户输入，所以不违反 D7）。合并词表要改列语义，留给 S08 之前统一 |
| D-0607 | 低 | `observability.audit.enabled` 默认 `true`（S01 定的），于是本卡的装配改动一落地，既有观测层用例全部以 `runtime dependencies are incomplete` 失败——`defaultRuntimeDeps` 少了闭包会启动期报错，而装配测试自己构造 `runtimeDeps` | **已修**：`cmd/server/main_integration_test.go` 的观测层装配字面量补上 `newAuditLog` 替身（7 处显式闭包 + `observabilityCase` 共用路径），`observabilityCase` 加 `newAudit`/`gotAudit` 两个字段，关停顺序断言随参数合并一起更新。这条不是"既有测试写坏了"，而是默认开 + 闭包必需的组合本来就该在启动期报错，见卡片 §3.5 的依赖完整性口径 |
| D-0608 | 低 | 测试里"登录一次再断言台账只有一行"会拿到两行：`POST /auth/login` 本身就是写操作、正在被审计的范围内 | **已修（测试侧）**：`api/audit_test.go` 的 `loginHeader` 在登录后清一次 capture。这是设计意图（登录要进台账，设计文档 §10.2），不是产品缺陷，记在这里免得后来人再踩 |

### 10.6 未覆盖与已知边界

- `unmatched`（`c.FullPath()` 为空）只在"路由未匹配"这一种现场被测到（`TestAuditMiddleware_UnmatchedPath`）。
  404 页面前端路由与反代改写过路径的现场没有覆盖。
- `throttled` 只有 `auditVerdict` 的表驱动用例，没有真实现场：要触发得连打 5 次错密码把 `loginLimiter` 打满，
  而那会连带影响同一进程里后续测试用例的登录。手工也没做（临时目录那两轮每次只登录 4 个账号一次）。
- 保留淘汰（`retention_count` / `retention_age`）只有单元测试（`TestAuditLog_Retention` 两个子例，
  用固定时间源造出跨 cutoff 的行）。手工环境跑 50 ms 一轮，既到不了 50 万条也到不了 90 天。
- 队列满丢弃（`Dropped()`）在真二进制里没有现场：手工两轮各造成不到 15 个写请求，默认容量 4096。
  丢弃数进关停日志这条只由 `TestRun_AuditLogIsHandedToTheServer`（塞 `dropped=3` 的替身）证明。
- 台账写入失败（库被外部改坏、表被删）没有真库用例：`store/sqlite` 侧只测到"查询失败原样返回错误"
  （`TestAuditLog_QueryErrorIsReturned`），"写失败只丢一行、不影响响应"这条靠的是 S02 的 `batcher` 语义与
  `TestAuditMiddleware_RecorderErrorIgnored` 的替身。
- 中间件顺序的风险（§9 第三条）由 `TestAuditMiddleware_RoleFields` 三种身份各一次守住，
  但"把 `Use` 挪到鉴权之前"这种改动在编译期与现有用例里都会立刻失败——这是好事，只是它意味着
  真正的静默风险（比如将来加一个在鉴权之后、审计之前的中间件把 `Principal` 改掉）本卡没有对应用例。
- `action` 映射表与 `setupRoutes` 仍是两处（§9 第二条的中期收口本期没做）。双向对照用例只在 `api` 包内成立，
  `-tags dashboard` 下多出来的静态托管路由不是写路由，因此不参与对照。
- 前端不消费这个端点（控制台审计页在 S08），`web/` 与 `vue-tsc` 本卡未动。
- Linux 上的表现未验证（与执行器系列同一限制）：本卡在 Windows 上跑绿，`-race` 与两条交叉编译都过，
  但没有 Linux 实跑；台账里 `remote_ip` 在 IPv6 反代环境下的写法依赖 `TrustedProxies` 配置，本期未碰。
