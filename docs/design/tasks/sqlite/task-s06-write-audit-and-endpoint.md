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

- [ ] `observability.enabled=false` 或 `audit.enabled=false` 时不注入 recorder：表不写、端点 503，但 §5 第 9 条的"补上单行审计日志"这条收益仍然生效（它就是现状缺的那一半）。
- [ ] 一行对应一个 HTTP 请求，批量请求不是 N 行（§5 第 6 条）。
- [ ] 表里任何列都不含请求体、参数取值、密码、原始查询串。§5 第 4、8 两条是这条的证据，缺一不可。
- [ ] `action` 映射表覆盖 `setupRoutes` 里的全部写路由；未覆盖时落到 `other` 并记 debug，用例里造一条新写路由证明这个兜底真的会触发。
- [ ] `gateExecutorSubmission` 的五个 return 点逻辑一字未改，只多了 stash 一行（对照：本卡前后 `api/handlers_executors_test.go` 全部通过）。
- [ ] 队满丢弃可观测（`Dropped()`），且丢弃不阻塞请求（`Append` 非阻塞）。
- [ ] `docs/api.md` 的"运维 API（ops）"一节补该端点与全部查询参数；`web-console-design.md:607` 与 §5.7.6 的"留二期"标注改为指向本卡。

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
