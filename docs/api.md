API 文档

## 基础信息

Base URL: http://localhost:8080/api/v1
Content-Type: application/json
字符编码: UTF-8

任务 ID 是 UUIDv7 的 36 位小写文本（如 `0198a2e3-7d4f-7abc-9def-0123456789ab`）：
前 48 位是毫秒时间戳，因此字符串序即创建序；随机位来自 `crypto/rand`。
创建任务时可以不带 `id`（由服务端生成）；历史遗留的 `20260928173933-XXXXXXXX` 形式 ID
仍能被读取与取消，只是新任务不再使用该格式。

## 鉴权与跨域

服务端支持两类凭据，都不配置时**不启用鉴权**（全部端点匿名放行，等价单机自托管）：

| 凭据 | 来源 | 身份 | 适用 |
| --- | --- | --- | --- |
| 用户名 + 密码 → JWT | `server.auth.users`（bcrypt 哈希）+ `server.auth.jwt.secret` | 账号自身角色 | Web 控制台 |
| 静态 token | `server.auth.token` | `machine` | 脚本、CI、旧集成 |

配置了任一凭据后，除下面列出的免鉴权路径外，全部端点（含 `/ws`、`/sse/events`、
`/api/v1/health`）都要求有效凭据，缺失或无效返回 401：

```json
{ "code": 401, "message": "invalid or missing credentials" }
```

角色不足返回 403（`{"code":403,"message":"insufficient role"}`），登录失败过于频繁返回 429
并附 `Retry-After` 秒数。

### 免鉴权路径

只有三类请求不需要凭据就能通过鉴权中间件：

| 路径 | 为什么放行 |
| --- | --- |
| `POST /api/v1/auth/login`、`POST /api/v1/auth/refresh` | 它们本身就是领取凭据的入口 |
| 带前端产物的二进制里，`GET`/`HEAD` 的 `/`、`/assets/*` 与 SPA 深链（如 `/jobs/<id>`） | 登录页也是产物的一部分；拦住它等于启用鉴权后连登录框都打不开 |

产物那一条的判定与路由分派共用一个函数（`api/console.go` 的 `consoleRequest`），
口径是"**GET 或 HEAD**，且路径不在 `/api/`、`/ws`、`/sse/` 名字空间里，且（除 `/` 与
`/assets/` 之外）请求头声明接受 `text/html`"。任何写方法都不在放行范围内；
未注入产物的部署（`go build` 不带 `-tags dashboard`，或没调 `WithConsole`）里这条
豁免根本不存在，`/` 依旧返回 JSON 404 或 401。

### 认证端点

```json
POST /api/v1/auth/login
{ "username": "admin01", "password": "..." }
```

响应：

```json
{
  "access_token": "eyJhbGciOiJIUzI1NiIs...",
  "refresh_token": "6f1c...32字节随机串",
  "token_type": "Bearer",
  "expires_at": "2024-01-02T15:35:00+08:00",
  "user": { "name": "admin01", "role": "admin" }
}
```

- `POST /api/v1/auth/refresh`：`{"refresh_token":"..."}` → 同一形状的响应。
  refresh token **一次一用**，每次刷新都轮转，旧的立即作废。
- `POST /api/v1/auth/logout`：`{"refresh_token":"..."}`（可省略）→ 204。
  同时吊销 refresh token，并把当前 access token 记入拒绝表，
  因此退出后旧 access token 立刻失效，不必等它自然过期。
- `GET /api/v1/auth/me` → `{"name":"admin01","role":"admin","expires_at":"..."}`。
  前端以这里为权威身份，不去解析 JWT 的 claims。
- `POST /api/v1/auth/ws-ticket` → `{"ticket":"...","expires_in_seconds":5}`。
  见下文实时通道。

未配置 `server.auth.users` 时登录端点返回 400（`console accounts are not configured`），
静态 token 照常可用。

### 角色

四档有序角色，低档不能做高档的事：

| 角色 | 能做什么 |
| --- | --- |
| `viewer` | 只读：任务列表/详情、`/stats`、`/job-types`、实时事件流 |
| `operator` | + 创建、编辑、取消、重试任务，建组与改名 |
| `admin` | + 强制暂停执行中的任务、删除分组 |
| `ops` | + 调度总开关、清空事件缓冲、运行时诊断 |
| `machine` | 静态 token 的身份：等同于 `operator` 的读写，但**没有** admin/ops 的任何能力 |

完整矩阵见 `docs/design/web-console-design.md` §5.7.3。

开启执行器后还有两条由配置决定的门槛，不放在上面那张表里（理由：它们可以按部署改，写死进代码就会与配置各说各话）：

| 能力 | 门槛 | 说明 |
| --- | --- | --- |
| 提交执行器任务（`exec.*`） | `executors.required_role`，默认 `admin` | `viewer` 先被 `POST /jobs` 的 operator 门槛挡下；`machine` 与 operator 同档，默认配置下不能提交；`ops` 高于 `admin`，始终可提交 |
| 读取含 `secret` 参数档位的产物正文 | 同一个 `executors.required_role` | `GET /jobs/:id/result` 的路由门槛是 `reader`，档位声明了 `secret` 时在处理器里升到这档 |

15 组组合（三种 `required_role` × 五种身份）的逐条断言在 `api/executors_submission_test.go` 的
`TestCreateJob_ExecutorRequiredRole`，结论与现场记录见 `docs/design/tasks/executor/task-e16-submission-role-and-mask.md` §5.1。

### 凭据通道

四种等价传法，**按此优先级取其一**：

| 通道 | 示例 | 适用场景 |
| --- | --- | --- |
| `Authorization` 请求头 | `Authorization: Bearer <jwt>` | 推荐，REST 客户端默认用法 |
| `X-Auth-Token` 请求头 | `X-Auth-Token: <jwt>` | 不便设置标准授权头时 |
| `ticket` 查询参数 | `GET /ws?ticket=<ticket>` | **仅限 `/ws` 与 `/sse/events`**，一次性、5 秒过期 |
| `token` 查询参数 | `GET /api/v1/jobs?token=<静态token>` | 仅接受静态机器凭据，且同样会进访问日志 |

注意三点：

- 出现 `Authorization` 头时只认它（非 `Bearer` scheme 直接判失败），不会再回退到其他通道。
- 查询参数会进入访问日志与浏览器历史。JWT 是长期凭据，**不接受**它走 `?token=`；
  浏览器 WebSocket / EventSource 无法自定义请求头，请改用 `ticket`：
  先带 access token 调 `POST /api/v1/auth/ws-ticket`，再用返回的票据建连，票据用一次即废。
- 静态 token 走 `?token=` 是历史兼容行为，仅供脚本使用；生产环境建议注入到
  `GODELAYQ_SERVER_AUTH_TOKEN` 并收紧 CORS 白名单。

令牌与账号的一致性：access token 里写着角色，但服务端每次都会用**当前配置**复核——
账号被删除或降权后，旧令牌立即不再可用。

### 跨域

跨域由 `server.cors.allow_origins` 控制，默认 `["*"]`（任意来源）。配置为具体白名单后，
只回显命中的 `Origin` 并附 `Vary: Origin`，未命中的响应不带 `Access-Control-Allow-Origin`，
由浏览器拦截。`allow_credentials: true` 与 `*` 互斥（启动即报错）。预检 `OPTIONS` 请求
不校验凭据，由 CORS 中间件直接返回 204。

```bash
# 启用静态 token 后的调用示例
curl -H "Authorization: Bearer $GODELAYQ_TOKEN" http://localhost:8080/api/v1/jobs
curl -H "X-Auth-Token: $GODELAYQ_TOKEN"         http://localhost:8080/api/v1/stats

# 控制台账号：先登录，再用 access token
curl -X POST -d '{"username":"admin01","password":"..."}' http://localhost:8080/api/v1/auth/login
curl -H "Authorization: Bearer <access_token>" http://localhost:8080/api/v1/jobs
```

## 控制台与静态托管

Web 控制台有两种部署形态，API 契约完全一致，区别只在前端从哪里来（设计文档 §5.8）：

**开发形态**：`cd web && npm run dev` 起 Vite（默认 :5173），`vite.config.ts` 把
`/api`、`/ws`（`ws: true`）、`/sse` 代理到 :8080。此时后端进程里没有产物，
访问 `http://localhost:8080/` 得到 JSON 404 是正常的。

**单二进制**：先 `cd web && npm run build` 产出 `web/dist`，再
`go build -tags dashboard ./cmd/server`。`-tags dashboard` 会让
`web/embed_dashboard.go` 的 `//go:embed all:dist` 生效；不带这个 tag 的构建里
`web.Dist` 恒为 `nil`，服务端只提供 API——所以先 build 前端、后 build 二进制，
顺序反了会直接编译失败（`pattern all:dist: no matching files found`）。

产物挂在根路径上，与 API 同源，因此不存在跨域、也不需要那条代理：

| 请求 | 响应 |
| --- | --- |
| `GET /` | `web/dist/index.html`，`Cache-Control: no-cache` |
| `GET /assets/<name>` | 对应产物文件，`Cache-Control: public, max-age=31536000, immutable`（文件名带内容哈希） |
| `GET /jobs/<id>` 等 SPA 深链，且 `Accept` 含 `text/html` | 回落 `index.html`，由前端路由决定显示什么 |
| `GET /assets/<不存在>` | 404（不拿 index.html 兜，否则得到一个 200 的白屏 JS） |
| `/api/v1/*`、`/ws`、`/sse/*` 下的未知路径 | 维持原样：启用鉴权先 401，带凭据后 JSON 404 |

`index.html` 必须 `no-cache`：它引用的是哈希文件名，缓存住它，升级后旧页面就会
去取已经被换掉的资源。启动日志里的 `embedded console enabled mount=/` 用来确认
这个二进制到底带没带前端。

## 任务管理 API

### 1. 创建延迟任务

```json
POST /jobs
Content-Type: application/json

{
  "name": "payment_check",
  "delay": "10m",
  "payload": {
    "order_id": "ORD-2024-001",
    "amount": 199.99,
    "user_id": "U123456"
  },
  "max_retries": 3,
  "retry_delay": "5m",
  "timeout": "30s"
}
```

参数说明：

| 字段           | 类型     | 必填 | 说明                                                  |
| ------------ | ------ | -- | --------------------------------------------------- |
| name         | string | ✅  | 任务类型名称：代码里注册的 Handler，或 `executors.commands` 声明的执行器档位（`exec.<档位名>`）                                |
| delay        | string | 条件 | 相对延迟，如 "10m", "1h30s"（与 trigger\_at/cron\_expr 三选一） |
| trigger\_at  | string | 条件 | 绝对时间，ISO 8601 格式                                    |
| cron\_expr   | string | 条件 | Cron 表达式，如 "0 \*/5 \* \* \* \*"                     |
| payload      | object | ❌  | 任务数据，JSON 对象，会透传给 Handler；执行器任务的顶层键是固定的，见[执行器 API](#执行器-api)                           |
| group        | string | ❌  | 分组标签，`[A-Za-z0-9_-]{1,64}`；不要求该分组已在 `/groups` 注册   |
| is\_repeat   | bool   | ❌  | 是否重复执行（Cron 任务需设为 true）                             |
| timeout      | string | ❌  | 单次执行超时，如 "30s"；为空不限制。Handler 需检查 ctx 才能被按时中止；执行器任务以 `payload.timeout` 为准      |
| max\_retries | int    | ❌  | 最大重试次数。**省略即 0（不重试）**，想要重试必须显式给值                |
| retry\_delay | string | ❌  | 基础重试间隔，默认 "1m"                                      |

`timeout` 格式非法会直接返回 400（`invalid timeout format`），不会被静默忽略。

> 与任务文件的差别：文件里的 `max_retries` 是可选整数，**不写取默认 3**、写 `0` 才是 0
> （见 [示例文档](./example.md) 的字段约定）；HTTP 请求体无法区分"未写"与"写 0"，所以一律按 0 处理。


**响应**（`201 Created`）：

```json
{
  "id": "0198a2e3-7d4f-7abc-9def-0123456789ab",
  "name": "payment_check",
  "status": "pending",
  "trigger_at": "2024-01-02T15:30:00+08:00",
  "payload": {
    "order_id": "ORD-2024-001",
    "amount": 199.99,
    "user_id": "U123456"
  },
  "next_run_in": "10m0s",
  "retry_count": 0,
  "max_retries": 3,
  "is_repeat": false,
  "created_at": "2024-01-02T15:20:00+08:00",
  "updated_at": "2024-01-02T15:20:00+08:00"
}
```

`next_run_in` 只对 `pending` 任务计算：距触发时间还有多久，已过期则写作 `imminent`，
其它状态下该字段省略。`timeout` 只在任务设置了执行超时时出现（与是否真的超时无关），
`payload` 为空时省略。

### 2. 创建 Cron 重复任务

```json
POST /jobs
Content-Type: application/json

{
  "name": "daily_report",
  "cron_expr": "0 0 9 * * *",
  "is_repeat": true,
  "payload": {
    "report_type": "daily_sales",
    "recipients": ["admin@example.com"]
  }
}

```

**Cron 表达式格式（秒级）**：

```json
┌───────────── 秒 (0-59)
│ ┌───────────── 分 (0-59)
│ │ ┌───────────── 时 (0-23)
│ │ │ ┌───────────── 日 (1-31)
│ │ │ │ ┌───────────── 月 (1-12)
│ │ │ │ │ ┌───────────── 周 (0-6, 0=周日)
│ │ │ │ │ │
* * * * * *

```

同时兼容 5 字段写法（`分 时 日 月 周`，秒按 0 处理），例如 `0 9 * * *` 与 `0 0 9 * * *` 等价。

常用示例：

- 0 */5 * * * *：每 5 分钟
- 0 0 9 * * *：每天上午 9 点
- 0 0 0 * * 1：每周一零点

### 3. 查询任务列表

```json
GET /jobs?status=pending&name=payment_check&limit=20&offset=0
```

查询参数：

| 参数     | 类型     | 说明                                            |
| ------ | ------ | --------------------------------------------- |
| status | string | 过滤状态：pending/running/success/failed/cancelled/paused（大小写不敏感） |
| name   | string | 按任务类型过滤                                       |
| group  | string | 按分组过滤，忽略大小写。**省略=不筛**，`group=`（空值）=只看未分组 |
| limit  | int    | 分页大小，默认 50，最大 100（超过按 100 截断）  |
| offset | int    | 分页偏移，默认 0；非数字或负数按 0 处理            |


`status` 只接受状态名；旧版按数字（`status=1`）过滤的写法现在返回 400。
返回按 `updated_at` 倒序（同刻度按 ID 升序）排列，`total` 是匹配总数、不受分页影响。

响应：

```json
{
  "total": 156,
  "items": [
    {
      "id": "0198a2e3-7d4f-7abc-9def-0123456789ab",
      "name": "payment_check",
      "status": "pending",
      "group": "nightly",
      "trigger_at": "2024-01-02T15:30:00+08:00",
      "next_run_in": "5m30s",
      "retry_count": 0,
      "max_retries": 3,
      "timeout": "30s",
      "is_repeat": false,
      "created_at": "2024-01-02T15:20:00+08:00"
    }
  ]
}
```

任务列表来自存储：未完成任务（pending/running）加上按保留策略留痕的终态记录，
因此 `status=success|failed` 能查到历史（见部署文档的 `store.history_*`）。
条目字段与单个任务视图一致（含 `updated_at`）。

### 4. 获取任务详情

```json
GET /jobs/:id
```

返回单个任务视图（字段同上）。ID 不存在时返回 404：

```json
{ "code": 404, "message": "job not found" }
```

### 5. 更新任务（仅 pending 状态）

```json
PUT /jobs/:id
Content-Type: application/json

{
  "trigger_at": "2024-01-02T16:00:00+08:00",
  "payload": {"order_id": "ORD-NEW-001"},
  "max_retries": 5,
  "timeout": "30s",
  "group": "nightly"
}
```

只更新给出的字段，任务 ID 保持不变（未列出的字段沿用原值；`timeout` 传 `"0s"` 可取消限制，
格式非法返回 400）。`group` 用**是否出现**来区分意图：省略=不改分组，`"group": ""`=取消分组。
更新在堆内原地完成（重排位置并写回快照），
不再走"先取消再重排"，因此不存在两步之间失败导致任务丢失的窗口。
任务已被弹出执行或已结束时返回 409（`job cannot be modified`），ID 从未存在返回 404。

暂停中的任务同样不能用它改分组（不在堆里 → 409）；给暂停或已结束的任务移组，
用下面的批量操作端点。

执行器任务在这里多四条约束。判定顺序是"任务不存在 404 → 改了名字 400 → 档位不够 403 → 不是 pending 409"，
前两条在状态检查之前判，所以档位不够的身份不会从 409 里读出这条任务是否已经跑完：

- `name` 只允许传回任务原本的名字，不同值即 400（`job name cannot be changed`，`details` 里写明当前名字
  与要改成的那个）；这条对所有任务生效，把普通任务改成 `exec.` 前缀同样被拒。
- 改 `payload` 会重跑提交期那一整套判定，因此执行器章节里那四种拒绝都可能在这里出现。
- 档位声明为 `secret` 的参数在读取接口是 `***`，PUT 不会把它回填成原值；照原样保存会把 `***` 当成新的
  取值写进任务，要改参数就把它改成真实取值再提交。
- 改顶层 `timeout` 对执行器任务不起作用：生效超时每次从 payload 与档位重新求出，请求字段会被覆盖。

### 6. 取消任务

```json
DELETE /jobs/:id

# 或
POST /jobs/:id/cancel
```

成功返回 `204 No Content`（无响应体）。任务不在堆里（已弹出执行、已结束或从未存在）返回 404：

```json
{ "code": 404, "message": "job not found or already executed" }
```

正在执行的任务会连同其 `context` 一起被取消，Handler 收到 `context.Canceled`，
这类中断不计入失败与重试。

### 7. 手动重试失败任务

```json
POST /jobs/:id/retry
```

在存储中查找该 ID 且状态为 `failed` 的记录，重置 `retry_count` 并在 1 秒后重新入队，
返回新的任务视图（`200`，状态 `pending`）。找不到符合条件的记录返回 404：

```json
{ "code": 404, "message": "failed job not found" }
```

这条路径依赖终态留痕：若已关闭留痕（`store.history_limit: -1`）或该记录已被保留策略淘汰，则 404。

### 8. 暂停、恢复与强制暂停

```json
POST /jobs/:id/pause        # operator 及以上
POST /jobs/:id/resume       # operator 及以上
POST /jobs/:id/force-pause  # admin、ops
```

三者都返回操作后的任务视图（`200`），失败返回 404（ID 不存在）或 409（状态不允许）。

**pause**：把任务从待触发堆里取出，状态落成 `paused`，**快照与历史都保留**
（这是它与 `DELETE` 的本质差别）。重启后仍是暂停态——`Restore` 不会替用户解除这个决定。
对已暂停的任务重复调用是幂等的（仍返回 200），控制器的双击或重试不会变成 409。

```json
// 对正在执行的任务调 pause：409
{
  "code": 409,
  "message": "job is running; force-pause interrupts the current attempt"
}
```

**force-pause**：中止正在执行的这一次尝试，不计失败、不消耗重试次数，停在 `paused`。
返回 200 表示"中止已发起"：Handler 收到 `context.Canceled` 后自己退场，
状态由执行收尾钉回 `paused`；不检查 ctx 的 Handler 会跑完这一次，
但收尾仍把它停在 `paused`（不会记成成功，也不会重新排期）。
对还没出堆的任务调用时行为等同 pause，并且更强：保证不再执行一次。

**resume**：按原 ID 重新排期。Cron 重复任务按表达式取下一个未来时点；
一次性任务的 `trigger_at` 若已过期则立刻补跑（与崩溃恢复同一口径）。
强制暂停后紧跟着恢复可能被拒（`409`，任务还在收尾），稍后重试即可。

暂停中的任务既不在堆里也不在执行中：`PUT /jobs/:id` 返回 409，但 `DELETE /jobs/:id` 能删掉它，
`/stats` 里的 `paused` 计数与 `GET /jobs?status=paused` 都能查到它。

### 9. 批量操作已有任务

```json
POST /jobs/batch-ops        # operator 及以上；action=force-pause 整批要求 admin 以上
Content-Type: application/json

{ "action": "pause", "ids": ["0198a2e3-...", "0198a2e4-..."] }
```

| 字段 | 说明 |
| --- | --- |
| `action` | `cancel` / `pause` / `force-pause` / `resume` / `move` |
| `ids` | 任务 ID 数组，1-100 条；空数组或超限返回 400 |
| `group` | 仅 `action=move` 使用，**必填**；`""` 表示取消分组 |

逐条独立执行，响应固定 `207`，失败原因按 ID 返回：

```json
{
  "action": "pause",
  "succeeded": 2,
  "failed": 1,
  "items": [ { "id": "0198a2e3-...", "status": "paused" } ],
  "errors": [ { "id": "0198a2e9-...", "code": 404, "message": "job not found" } ]
}
```

`cancel` 之后没有任务现状可返回，所以 `items` 里不含这些条目。
`move` 对暂停中与已结束留痕的任务同样有效（它改的是分组标签，不重排任务）。
`action=force-pause` 时整批要求 admin 以上：档位判断要看请求体，
所以这一条在处理器里完成，越权请求整批 403 而不是"批里几条偷偷执行"。

### 10. 批量创建任务

```json
POST /jobs/batch
Content-Type: application/json

[
  { "name": "payment_check", "delay": "5m", "payload": {"order_id": "A-1"} },
  { "name": "email_send", "delay": "1h", "timeout": "30s" },
  { "name": "not_registered", "delay": "5m" }
]
```

请求体是任务数组（不是对象），单请求最多 100 条，空数组或超限返回 400。

响应固定 `207 Multi-Status`，条目**逐条独立处理**：某条失败不会回退其他条，也不会中断后续解析。

```json
{
  "succeeded": 2,
  "failed": 1,
  "items": [
    { "id": "0198a2e3-7d4f-7abc-9def-0123456789ab", "name": "payment_check", "status": "pending", "next_run_in": "5m0s" }
  ],
  "errors": [
    { "index": 2, "code": 400, "message": "unknown job type", "details": "job type 'not_registered' not registered" }
  ]
}
```

`errors[].index` 指回请求数组的下标；全部成功时 `errors` 为空数组，全部失败时 `items` 为空数组
（此时仍是 207，调用方按 `failed` 判断结果，而不是靠 HTTP 状态码）。
校验规则与单条 `POST /jobs` 完全一致（同一套解析逻辑），包括 `delay`/`trigger_at`/`cron_expr`
优先级、`timeout` 格式非法即拒绝、以及未注册的 `name` 视为错误。

## 执行器 API

执行器把"任务类型"从代码里注册的 Handler 扩展到配置文件里声明的**档位**（profile）：一条档位写清楚
用什么程序、跑哪个脚本、发哪个 HTTP 请求、允许哪些参数。注册之后它的任务类型名就是 `exec.<档位名>`。

能执行什么完全由 `executors.commands` 决定：**没有自由命令行**，任务里也不能内联源码、不能要求现场编译。
档位改动要重启进程。开关、白名单与部署前提见 [部署文档](./deployment.md) 的"开启执行器"一节。

### 与 `/job-types` 的关系

执行器打开（`executors.enabled: true`）并注册成功时，档位名与内置 Handler 名一起出现在
`GET /api/v1/job-types` 里（按字典序）：

```json
{
  "types": ["data_sync", "email_send", "exec.fail3", "exec.long60", "exec.nap",
            "exec.not_deployed", "exec.show_token", "payment_check", "report_generate"]
}
```

关闭时这个列表只剩代码里注册的那几个。探测不通过的档位（脚本没部署、程序不在 PATH）
**仍然注册**，因此也会出现在这里；提交它会被拒，见下面的"提交期被拒的四种响应"。

### 档位清单：`GET /api/v1/executors`

```json
GET /api/v1/executors
```

档位声明是公开信息（不含脚本内容与参数取值），`viewer` 及以上可读；执行器关闭时这不是错误，
返回 `enabled: false` 与空列表。真实响应（本机六个档位里挑四个，含一个不可用的与一个 HTTP 的）：

```json
{
  "enabled": true,
  "required_role": "admin",
  "max_timeout": "5m0s",
  "profiles": [
    {
      "key": "exec.fail3",
      "name": "fail3",
      "kind": "script",
      "runtime_ok": true,
      "reason": "",
      "timeout": "30s",
      "max_parallel": 1,
      "args": [
        { "name": "day", "required": true, "default": "", "pattern": "^[a-z0-9-]{1,32}$", "secret": false }
      ],
      "env_allow": [],
      "has_secret_args": false,
      "preferred_result_direction": "tail"
    },
    {
      "key": "exec.not_deployed",
      "name": "not_deployed",
      "kind": "script",
      "runtime_ok": false,
      "reason": "script file \"scripts/not-deployed.sh\" does not exist",
      "timeout": "30s",
      "max_parallel": 1,
      "args": [],
      "env_allow": [],
      "has_secret_args": false,
      "preferred_result_direction": "tail"
    },
    {
      "key": "exec.show_token",
      "name": "show_token",
      "kind": "script",
      "runtime_ok": true,
      "reason": "",
      "timeout": "30s",
      "max_parallel": 1,
      "args": [
        { "name": "token", "required": true, "default": "", "pattern": "^[A-Za-z0-9._:/=,-]{1,256}$", "secret": true }
      ],
      "env_allow": [],
      "has_secret_args": true,
      "preferred_result_direction": "tail"
    },
    {
      "key": "exec.local_health",
      "name": "local_health",
      "kind": "http",
      "runtime_ok": true,
      "reason": "",
      "timeout": "10s",
      "max_parallel": 1,
      "args": [],
      "env_allow": [],
      "has_secret_args": false,
      "preferred_result_direction": "head",
      "method": "GET",
      "body_mode": "none",
      "url": "http://127.0.0.1:18090/api/v1/health"
    }
  ]
}
```

字段口径：

| 字段 | 说明 |
| --- | --- |
| `enabled` | `executors.enabled` 的当前值 |
| `required_role` | 提交 `exec.*` 任务所需的最低角色；**执行器关闭时是 `null`**（那时"要什么档位"这个问题不成立） |
| `max_timeout` | `executors.max_timeout`，payload 里 `timeout` 的上限；只在 `enabled: true` 时出现，关闭时整个键省略 |
| `profiles[].key` | 提交任务时 `name` 要写的值（`exec.<档位名>`） |
| `kind` | `script` / `binary` / `http` |
| `runtime_ok` | **这台机器现在能不能跑**：解释器或程序在不在 PATH、脚本文件在不在。false 时提交被拒 |
| `reason` | `runtime_ok: false` 的原因原文；可用时是空串 |
| `timeout` | 档位声明的单次执行超时 |
| `max_parallel` | 同一档位最多同时跑几个（配置里写 0 已在加载时归一为 1） |
| `args[]` | 具名参数声明：`required`、`default`、`pattern`（配置留空时给的是实际生效的那份默认安全字符集）、`secret` |
| `positional` | 对象 `{max, pattern}`；档位没声明位置参数时**整个键不出现** |
| `env_allow` | payload 可以注入的环境变量**键名**白名单（取值不外露）；空表给 `[]` |
| `has_secret_args` | 档位是否声明了至少一个 `secret` 参数。true 时 payload 与输出预览会掩码，读取产物正文的门槛也升到 `required_role` |
| `preferred_result_direction` | 读这个档位输出时的建议起点：`head` 或 `tail`（`http` 给 `head`，进程档位给 `tail`），与下面 `/result` 的 `from` 参数同一套词 |
| `method` / `body_mode` / `url` / `header_allow` | 只出现在 `http` 档位上。`body_mode` 是 `json` / `raw` / `none` 之一（配置里没写 `body` 的档位在这里归一成 `none`）；`url` 给的是模板原文（含 `{占位符}`），不是渲染后的地址；`header_allow` 是 payload 可覆盖的请求头名，**空表时整个键省略**（与 `env_allow` 的口径不同，别当成"没返回"） |

### 提交执行器任务：`POST /jobs`

任务对象与普通任务同一个端点、同一批字段，只有 `payload` 换成固定结构。顶层键**只能用下面这几个**，
多写一个键整条请求被拒：

| 键 | 适用档位 | 说明 |
| --- | --- | --- |
| `args` | 脚本 / 产物 | 具名参数的取值，键名必须是档位 `args[].name` 声明过的那些 |
| `args._positional` | 脚本 / 产物 | 位置参数，字符串数组；数量上限与字符集见响应的 `positional` |
| `env` | 脚本 / 产物 | 追加的环境变量，键名必须在 `env_allow` 里 |
| `params` | HTTP | URL 模板里 `{占位符}` 的取值（占位符同样声明在档位的 `args` 里） |
| `headers` | HTTP | 覆盖的请求头，名字必须在 `header_allow` 里；`Host`、`Content-Length`、`Transfer-Encoding` 由执行器决定，出现在这里就是错误 |
| `body` | HTTP | 请求体，只有档位 `body_mode` 是 `json` 或 `raw` 才能给 |
| `timeout` | 全部 | 本次执行的超时；超过 `max_timeout` 的值被夹到上限，超过档位 `timeout` 的值直接被拒 |

**脚本档位**（`kind: script`，请求与 201 响应都是本机实测）：

```json
POST /api/v1/jobs
{
  "name": "exec.fail3",
  "delay": "3s",
  "payload": { "args": { "day": "mon" }, "timeout": "30s" }
}
```

```json
201 Created
{
  "id": "01a0f1f7-77bc-7209-bee8-daf1506f288e",
  "name": "exec.fail3",
  "status": "pending",
  "trigger_at": "2026-09-30T18:58:55.7876014+08:00",
  "payload": { "args": { "day": "mon" }, "timeout": "30s" },
  "retry_count": 0,
  "max_retries": 0,
  "is_repeat": false,
  "attempts": 0,
  "created_at": "2026-09-30T18:58:52.7876973+08:00",
  "updated_at": "2026-09-30T18:58:52.7876973+08:00",
  "next_run_in": "3s"
}
```

**产物档位带位置参数**（`kind: binary`）：

```json
{
  "name": "exec.etl_full",
  "delay": "10m",
  "payload": { "args": { "window": "20261001", "_positional": ["part-01", "part-02"] } }
}
```

**HTTP 档位**（`kind: http`）：

```json
{
  "name": "exec.rebuild_index",
  "delay": "1m",
  "payload": {
    "params": { "tenant": "acme" },
    "headers": { "X-Trace-Id": "t-1" },
    "body": { "force": true }
  }
}
```

两条容易写错的地方：

- 请求体顶层的 `timeout`（任务字段）与 `payload.timeout` 不是一回事。执行器任务以 `payload.timeout` 为准：
  它被求成生效超时并写进任务；顶层 `timeout` 只做上限检查（超过档位 `timeout` 直接 400），检查完就被覆盖。
- 脚本与产物档位的命令行是 `[程序, 脚本路径, 固定参数, 渲染出的参数]` 拼出来的，
  **中间没有 shell**。参数值里的空格与 `;`、`&&`、`|` 都只是普通字符，不会被当成命令语法解释。
  Windows 上 `cmd`、`pwsh` 要带 `/c`、`-File` 之类开关才会去执行文件，而脚本档位没有放开关的位置，
  可用写法见 [部署文档](./deployment.md) 的 Windows 限制一节。

### 提交期被拒的四种响应

四条文案各不相同，`details` 给的是判定依据原文（不含参数取值，除非取值本身违规）。都是本机实测响应：

| 情况 | HTTP | 响应原文 |
| --- | --- | --- |
| 身份低于 `required_role` | 403 | `{"code":403,"message":"insufficient role","details":"job type \"exec.fail3\" is an executor profile; submitting it requires role admin (executors.required_role)"}` |
| 参数格式不符 | 400 | `{"code":400,"message":"invalid executor payload","details":"args.day: value \"BAD DAY!\" does not match pattern ^[a-z0-9-]{1,32}$"}` |
| payload 有档位不接受的键 | 400 | `{"code":400,"message":"invalid executor payload","details":"payload key \"cmd\" is not accepted by profile \"fail3\" (allowed keys: args, env, timeout)"}` |
| 档位在这台机器跑不了 | 400 | `{"code":400,"message":"executor profile is not available on this server","details":"script file \"scripts/not-deployed.sh\" does not exist"}` |
| 顶层 `timeout` 超档位上限 | 400 | `{"code":400,"message":"invalid timeout","details":"timeout 9m0s exceeds the 30s allowed by profile \"fail3\""}` |

`viewer` 连建任务这一步都进不去（`POST /jobs` 路由本身要 operator），拿到的是不带 `details` 的
`{"code":403,"message":"insufficient role"}`。静态 token 的身份是 `machine`，与 operator 同档，
所以默认配置（`required_role: admin`）下**脚本凭据不能提交执行器任务**。

批量创建逐条独立：档位不够的那一条在 `errors[]` 里给 403，整批仍是 207。

```json
207 Multi-Status
{
  "succeeded": 1,
  "failed": 1,
  "items": [
    { "id": "01a0f206-5465-7a1a-9931-ac08706909b7", "name": "payment_check", "status": "pending" }
  ],
  "errors": [
    {
      "index": 1,
      "code": 403,
      "message": "insufficient role",
      "details": "job type \"exec.fail3\" is an executor profile; submitting it requires role admin (executors.required_role)"
    }
  ]
}
```

### 读取执行输出：`GET /jobs/:id/result`

摘要在任务对象里就有，**正文要单独读**：它来自磁盘上的产物文件，响应带 `Cache-Control: no-store`，
不随详情页一起取。

查询参数：

| 参数 | 取值 | 默认 | 说明 |
| --- | --- | --- | --- |
| `stream` | `out` / `err` | `out` | 读哪一路。`http` 档位的 `out` 是响应体，`err` 是请求与响应的头（**不是 stderr**） |
| `from` | `head` / `tail` | `tail` | 从文件头还是尾读。建议起点看 `GET /executors` 的 `preferred_result_direction` |
| `attempt` | 整数 | 最近一次 | 允许范围是 `1..attempts`；越界 400 |
| `max_bytes` | 正整数 | 预览上限的 4 倍（`executors.output.inline_preview × 4`） | 单次请求的硬上限是 8 MiB，超过直接 400；再大的请求值也只会夹到 `executors.output.max_bytes`（文件不可能比它更大） |

```json
GET /api/v1/jobs/01a0f1f7-77bc-7209-bee8-daf1506f288e/result?stream=err&from=tail
```

```json
200 OK
{
  "job_id": "01a0f1f7-77bc-7209-bee8-daf1506f288e",
  "attempt": 1,
  "stream": "err",
  "found": true,
  "size_bytes": 51,
  "returned_bytes": 51,
  "truncated": false,
  "meta": { "kind": "script", "profile": "fail3", "exit_code": 3, "duration_ms": 34,
            "out_bytes": 22, "err_bytes": 51, "permanent": true,
            "preview": "stderr: missing input file\nstderr: cannot continue\n", "artifact": "available" },
  "content": "stderr: missing input file\nstderr: cannot continue\n"
}
```

`meta` 来自任务快照，`content` 来自产物文件，两者来源不同。产物文件被保留策略清掉之后（本机实测：
同一个任务在 TTL 清理之后再读一次），摘要照旧给出，只是 `found: false`、`content` 为空，
同时把快照里的 `meta.artifact` 回写成 `purged`（每个任务每次尝试最多回写一次；之后 `GET /jobs/:id`
里的 `exec.artifact` 同样是 `purged`）：

```json
200 OK
{
  "job_id": "01a0f1f7-77bc-7209-bee8-daf1506f288e",
  "attempt": 1,
  "stream": "err",
  "found": false,
  "size_bytes": 0,
  "returned_bytes": 0,
  "truncated": false,
  "meta": { "kind": "script", "profile": "fail3", "exit_code": 3, "duration_ms": 34,
            "out_bytes": 22, "err_bytes": 51, "permanent": true,
            "preview": "stderr: missing input file\nstderr: cannot continue\n", "artifact": "purged" },
  "content": ""
}
```

"文件不在"不等于"输出是空的"，也不等于"任务没跑过"：这三件事由状态码、`meta` 是否为 `null` 与 `found` 共同区分。

失败返回（都是本机实测原文）：

| HTTP | 触发条件 | 响应原文 |
| --- | --- | --- |
| 400 | 参数取值非法 | `{"code":400,"message":"invalid attempt","details":"got \"9\": attempt 9 is out of range, this job has 1 attempt(s)"}`；`stream`、`from`、`max_bytes` 同形（`invalid stream` / `invalid from` / `invalid max_bytes`） |
| 403 | 档位声明了 `secret` 参数，而身份低于 `required_role` | `{"code":403,"message":"insufficient role","details":"profile \"show_token\" declares secret arguments; reading its execution output requires role admin"}` |
| 404 | 任务不存在 | `{"code":404,"message":"job not found"}` |
| 404 | 任务存在但没有执行结论（普通任务，或还没执行完） | `{"code":404,"message":"no execution result for this job"}` |
| 503 | 部署没装配产物存储 | `{"code":503,"message":"execution output storage is not configured","details":"start the server with api.WithArtifacts to enable /api/v1/jobs/:id/result"}` |

档位没声明 `secret` 参数时，这个端点的门槛就是 `reader`（`viewer` 及以上）。

### 任务对象里的 `exec` 与 `attempts`

执行器任务进入终态后带一个 `exec` 对象（普通任务没有这个键，读取方不要假设它一定存在）：

| 字段 | 说明 |
| --- | --- |
| `kind` | `script` / `binary` / `http` |
| `profile` | 档位名（不含 `exec.` 前缀） |
| `exit_code` | 进程档位的退出码。与 `http_status` **互斥**：`http` 档位只写后者，状态码永远不会进 `exit_code` |
| `http_status` | HTTP 档位响应的状态码 |
| `signal` | 被信号中止时的信号名（Windows 上不产生） |
| `duration_ms` | 本次执行的墙钟耗时 |
| `out_bytes` / `err_bytes` | **落盘字节数**，不是输出的全长：超过 `executors.output.max_bytes` 的部分不会写进文件，此时 `truncated: true`。`capture_response: false` 的 HTTP 档位 `out_bytes` 为 0 |
| `permanent` | 只在"重试无意义"时为 `true`（参数不合法、命令不存在、4xx、地址被策略拒掉等）；缺省即假 |
| `preview` | 尾部预览，长度上限 `executors.output.inline_preview`；含 `secret` 参数的档位里命中的值被替换成 `***` |
| `artifact` | `available` / `purged`：产物文件在不在。`purged` 由读取端点在发现文件已缺失时回写 |

`attempts` 是所有任务都有的字段：已经启动过的执行次数，同时决定 `/result` 里 `attempt` 的允许范围。
**已知现状**：重试副本不携带这个计数，所以重试过的任务这里仍然是 `1`，产物文件名也还是 `a1.out`，
后一次执行覆盖前一次。要按尝试分别读输出，得先修那条计数（登记在 TASK-E15 卡的遗留事项里）。

### 事件里的执行结论

`job.completed` 与 `job.failed` 的 `data` 在有执行结论时是一个对象，`result` 就是上面那份 `exec` 摘要
（失败时另有 `error` 一句人话）：

```json
{
  "type": "job.failed",
  "job_id": "01a0f1f7-77bc-7209-bee8-daf1506f288e",
  "job_name": "exec.fail3",
  "status": "failed",
  "data": {
    "error": "profile \"fail3\": exit status 3",
    "result": { "kind": "script", "profile": "fail3", "exit_code": 3, "duration_ms": 34,
                "out_bytes": 22, "err_bytes": 51, "permanent": true,
                "preview": "stderr: missing input file\nstderr: cannot continue\n", "artifact": "available" }
  },
  "metadata": { "retry_count": 0, "max_retries": 0, "timeout": false, "permanent": true }
}
```

`job.failed` 的 `metadata` 里 `retry_count`、`max_retries`、`timeout`（这次是否因超时中止）是既有字段；
`permanent` 只在"重试无意义"时出现，取值恒为 `true`（不写 `false`，为了让既有事件的 JSON 形状一字不变）。
`job.completed` 的 `metadata` 只有 `duration_ms`；`job.started` 带 `attempt`（见上文 `attempts` 的现状）。

被崩溃恢复置为 `paused` 的执行器任务，`job.paused` 事件带来源标记：

```json
{
  "type": "job.paused",
  "metadata": { "reason": "restore_after_crash", "forced": true,
                "attempts": 1, "trigger_at": "2026-09-30T19:04:34.4905316+08:00" }
}
```

`reason: "restore_after_crash"` 表示这条暂停来自重启时的崩溃恢复，不是人在控制台点的；没有 `reason`
的才是用户操作。这条标记必须写在事件里：事件历史在进程内存里，重启之后崩溃前那几轮 `job.started`
已经不在，时间线上只剩这一条 `job.paused`。

### 敏感参数（`secret`）出现在哪些地方

档位把某个参数声明成 `secret` 之后，读取接口按值掩码，命中的位置是这些：

- 任务对象的六处出口（创建、列表、详情、更新、重试、批量创建）里的 `payload`：掩的是 `args`、`params`、
  `headers`、`env` 四个字段里命中参数名的**取值**；`body` 不掩（档位没有声明"body 里哪个键是凭据"的能力，
  要传凭据就走请求头：把它声明成 `secret` 参数并放进 `header_allow`）。
- 同一批出口与事件里的 `exec.preview`：按值替换，例如 `orders/***`，不是整段抹掉。
- `/result` 响应里的 `meta.preview`。

实测一对（同一个任务，输入框里的原值与读回来的样子）：

```json
{ "payload": { "args": { "token": "***" } },
  "exec": { "preview": "received: --token=***\n" } }
```

两点要说清：

- **掩码只发生在读取接口这一层。** `jobs.json` 里的 `payload`、产物文件正文与 `meta.json` 仍是提交时的原文，
  所以这两个位置的权限要按凭据文件的等级设（见部署文档）。
- 正文不掩，因为输出是脚本或对端打印出来的，框架层管不住它把值写回正文。因此含 `secret` 参数的档位被放行
  读取时，响应多一个固定说明字段：

```json
{
  "redaction_note": "output is produced by the script or the remote endpoint and may contain the values of secret arguments"
}
```

这句话是提醒读的人"正文里可能有凭据"，不是"内容已防护"的声明。

## 分组管理 API

分组的元数据存在 `store.groups_path`（默认 `./data/groups.json`）。
任务上的 `group` 只是一个标签字符串，**不要求分组先注册**：`POST /jobs` 带任何合法组名都能写进去。
用 `WithGroupStore` 之外的方式启动（没有装配注册表）时，本章端点统一返回 503。

### 列出分组

```json
GET /groups      # viewer 及以上
```

响应是数组，按名称字典序（忽略大小写），每项是注册表条目加上实时统计的挂载数：

```json
[
  {
    "name": "nightly",
    "description": "夜间批处理",
    "color": "#2563eb",
    "created_at": "2024-01-02T15:20:00+08:00",
    "updated_at": "2024-01-02T15:20:00+08:00",
    "job_count": 12,
    "paused_count": 1,
    "registered": true
  },
  {
    "name": "adhoc",
    "created_at": "0001-01-01T00:00:00Z",
    "updated_at": "0001-01-01T00:00:00Z",
    "job_count": 3,
    "paused_count": 0,
    "registered": false
  }
]
```

`job_count` / `paused_count` 来自扫描任务快照（含终态留痕），大小写不同的同组名算同一组。
`registered: false` 表示这个组名只出现在任务标签上、注册表里没有对应条目——
包括手工建的临时组，也包括改名改到一半失败留下的"半个旧组"，UI 需要看得见它才能兜住。

### 新建分组

```json
POST /groups      # operator 及以上
Content-Type: application/json

{ "name": "nightly", "description": "夜间批处理", "color": "#2563eb" }
```

`name` 必填且需匹配 `[A-Za-z0-9_-]{1,64}`；`color` 取 `#rgb` 或 `#rrggbb`。
非法取值 400，重名（忽略大小写）409，成功返回 201 与上面的单项结构。

### 改名 / 改描述 / 改颜色

```json
PUT /groups/:name      # operator 及以上
Content-Type: application/json

{ "name": "nightly-batch", "description": "", "color": "#0ea5e9" }
```

字段全部可选，**出现了才改**（`description: ""` 是明确的清空）。改名会连带改写
挂着这个组的任务的 `group` 标签：堆里的条目与存储快照一起更新，因此正在等待与已暂停、
已结束留痕的任务都会跟着改名，任务执行完也不会把旧组名写回去。
改名撞上已有分组返回 409，组不存在返回 404。

改写任务标签是逐条落盘、非原子的：中途失败会留下部分任务仍挂在旧组名上，
重发一次 PUT 即可（幂等）。

### 删除分组

```json
DELETE /groups/:name            # admin、ops；默认 detach
DELETE /groups/:name?strategy=block
```

**任何角色都不会通过这两个调用删掉任务**（决策 D5）：

- 默认 `detach`：组内任务的 `group` 置空（归入"未分组"），然后删除分组记录，返回 204；
- `strategy=block`：组内还有任务时返回 409，空组才允许删；
- `strategy` 的其他取值返回 400，不会被静默当成 detach 处理；
- 组不存在返回 404。

## 运行事件 API

事件历史是**进程内的环形缓冲**（每个任务最近 100 条、全局最近 500 条、
最多记录 2000 个任务后按 LRU 整体淘汰），不是审计日志：**重启即清空**。
需要长期留痕请接外部日志/存储。

```json
GET /jobs/:id/events?limit=50   # viewer 及以上
GET /events?limit=100           # viewer 及以上，跨任务的全局最近事件
```

两者返回同一结构，条目**按时间升序**（详情页时间线可直接铺）：

```json
{
  "job_id": "0198a2e3-7d4f-7abc-9def-0123456789ab",
  "count": 2,
  "items": [
    { "type": "job.scheduled", "job_id": "0198a2e3-...", "job_name": "payment_check", "status": 0, "timestamp": "2024-01-02T15:20:00+08:00" },
    { "type": "job.paused", "job_id": "0198a2e3-...", "job_name": "payment_check", "status": 5, "timestamp": "2024-01-02T15:21:00+08:00", "metadata": { "forced": false, "trigger_at": "2024-01-02T17:20:00+08:00" } }
  ],
  "note": "in-memory buffer, cleared on restart"
}
```

`limit` 缺省或非法表示"给我全部已留存的"，上限就是窗口容量本身。
没有记录时返回空列表而不是 404：详情页时间线本来就可能在等第一个事件。
事件与实时通道的关系：这里补的是"打开页面之前"的历史，之后的增量仍由 WS/SSE 推送。

执行器任务的 `job.completed` / `job.failed` 在 `data` 里多带一份执行结论、`job.failed` 的 `metadata`
多一个 `permanent`，字段口径见[执行器 API](#执行器-api)的"事件里的执行结论"。

## 运维 API（ops）

只有 `ops` 档可用（`admin` 也不行）。未启用鉴权时所有请求都是匿名的 ops 档，
这些端点同样可调用（`docs/api.md` 的鉴权章节）。

### 运行时诊断

```json
GET /admin/runtime
```

```json
{
  "uptime": "72h15m30s",
  "started_at": "2024-01-01T09:00:00+08:00",
  "scheduler": {
    "started": true,
    "workers": 100,
    "queue_capacity": 100,
    "queue_length": 0,
    "running": 3,
    "heap_size": 15,
    "suspended": false,
    "force_pause_pending": 0,
    "exec_workers": 2,
    "exec_running": 2,
    "exec_queue_length": 2,
    "exec_queue_capacity": 2
  },
  "event_history": {
    "jobs": 42,
    "events": 118,
    "global_capacity": 500,
    "per_job_capacity": 100,
    "job_capacity": 2000
  }
}
```

只读，不含任务内容与凭据。`queue_length`/`running` 是瞬时值，用来看趋势不是用来审计的。

执行器打开时 `scheduler` 里多四个 `exec_` 前缀的字段：执行器专用池的 worker 数、正在跑几个、队列长度与容量，
与同一对象里的 `workers`/`running`/`queue_length`/`queue_capacity`（普通池）分账。档位任务走独立队列，
队列满时任务留在堆与存储里等空位，普通任务的准时性不受它影响。`exec_running` 只算执行器池，
因此它不等于 `GET /stats` 的 `running`（后者是两池之和）。执行器关闭时这四个字段是零值。

### 调度总开关

```json
POST /admin/scheduler/suspend     # 挂起：不再弹出任何到期任务
POST /admin/scheduler/unsuspend   # 恢复
```

两者都返回 `{"suspended": true|false}`，重复调用幂等。
挂起只停"取任务"：已在执行的任务照常跑完，堆与存储都不改动，
挂起期间 `POST /jobs` 仍然可用（任务进堆，只是暂不触发），恢复后一并生效。

**状态是进程内的，重启即解除**——维护窗口不该跨一次重启悄悄生效。

### 清空事件缓冲

```json
DELETE /admin/events
```

返回 `{"cleared": 118}`（被清掉的条数）。之后各详情页时间线从当前时刻重新开始，
任务本身与存储记录不受影响。

## 统计与监控 API

### 获取统计信息

```json
GET /stats
```

响应：

```json
{
  "pending": 12,
  "running": 3,
  "paused": 1,
  "completed": 1542,
  "failed": 8,
  "heap_size": 15,
  "uptime": "72h15m30s",
  "scheduling_suspended": false
}
```

字段口径：

| 字段 | 来源 | 说明 |
| --- | --- | --- |
| pending / paused / completed / failed | 存储快照按状态计数 | completed/failed 依赖终态留痕，关闭留痕后恒 0 |
| running | 进程内实时执行数 | 已进入 Handler、尚未返回的任务数；**执行器任务与普通任务共用这一个计数（两池之和）**，分池的数字在 `GET /admin/runtime` 里读 |
| heap_size | 调度堆长度 | 仍在堆里等待的任务数 |
| uptime | 进程启动至今 | 计数器不跨重启，completed/failed 随存储恢复而继续累计 |
| scheduling_suspended | 调度总开关的当前值 | 与 `GET /admin/runtime` 的 `scheduler.suspended` 同源 |

`scheduling_suspended` 放在 `/stats` 而不是只留在 ops 专属的 `/admin/runtime`：
调度被挂起时，"任务为什么不出"是每一个看列表的人的疑问，不是只有改得动它的人才需要知道。
该字段不带 `omitempty`——缺席会让前端只能猜，而猜错的方向是"以为调度器卡死了"。

`pending` 与 `heap_size` 通常相等，差值来自"已出堆、还在执行队列里排队"的那一小段：
它的快照仍是 `pending`，但已不在堆里，也未进入 Handler。`running` 只统计已进入 Handler 的任务。
`paused` 既不在 `pending` 里也不在 `heap_size` 里（暂停的任务已被取出堆），
所以概览页要单独显示它，不然"还剩多少要跑"会算多。

### 健康检查

```json
GET /health
```

响应：

```json
{
  "status": "healthy",
  "time": "2024-01-02T15:25:30+08:00"
}
```

只有这两个字段：探针可用 `status` 判活，版本号未对外暴露。

## 获取支持的 Job 类型

```json
GET /job-types
```

响应：

```json
{
  "types": ["data_sync", "email_send", "payment_check", "report_generate"]
}
```

`types` 来自调度器里唯一的一份 Handler 注册表（`core.Scheduler.LookupHandler`），
按字典序返回；`POST /jobs` 与 `POST /jobs/batch` 只接受其中出现过的 `name`，
否则返回 400 `unknown job type`。注册表由进程启动时代码注册（见 `cmd/server`），
不能通过 HTTP 动态增删。

开启执行器时，配置里 `executors.commands` 声明的档位也在这一份注册表里，因此同样出现在 `types` 里；
探测不通过的档位也在（提交它会被拒）。上面的响应是执行器关闭时的样子，打开时形如：

```json
{
  "types": ["data_sync", "email_send", "exec.fail3", "exec.nap", "payment_check", "report_generate"]
}
```

档位随进程启动从配置注册，改档位要重启；不存在"通过接口新增档位"这条路（见[执行器 API](#执行器-api)）。

## WebSocket 实时通信

连接地址: ws://localhost:8080/ws

启用鉴权后浏览器只能把凭据写在 URL 上，两种写法：

- `ws://localhost:8080/ws?ticket=<ticket>`（**推荐**）：先带 access token 调
  `POST /api/v1/auth/ws-ticket` 申领，票据 5 秒过期、一次一用，重放直接 401。
- `ws://localhost:8080/ws?token=<静态token>`：只接受 `server.auth.token`（机器凭据）。
  JWT **不能**走这条通道——访问日志会原样记下 query，长令牌进日志等于泄露。

握手前由 HTTP 中间件校验凭据，凭据不对直接返回 401，不进入升级流程；
`server.cors.allow_origins` 白名单同时约束握手的 `Origin`，
来源不在白名单内返回 `403 Forbidden`（响应体是纯文本 `websocket origin not allowed`）；
不带 `Origin` 头的客户端（Go/curl）不受白名单限制。
实时通道要求 `viewer` 档及以上：ticket 由已登录账号申领，身份随票据一起带过来。

**协议说明**

客户端发送 JSON 消息进行订阅控制：

```json
// 订阅特定事件
{
  "action": "subscribe",
  "filter": {
    "job_types": ["payment_check", "email_send"],
    "event_types": ["job.scheduled", "job.started", "job.completed", "job.failed"],
    "status": ["failed", "cancelled"],  // 可选：按任务状态名过滤
    "job_ids": ["job_xxx"]  // 可选：关注特定任务
  }
}

// 心跳保活
{
  "action": "ping"
}

// 获取实时统计
{
  "action": "get_stats"
}

// 取消全部过滤条件（恢复接收所有事件）
{
  "action": "unsubscribe"
}
```

过滤条件之间的关系：同一列表内是“命中任一即可”，不同列表之间是“同时满足”；
某一项留空（或省略）表示该项不参与过滤。`status` 比较的是状态名
（`pending`/`running`/`success`/`failed`/`cancelled`/`paused`），`job_types` 比较事件里的任务名。

服务端除事件外还会回送**控制帧**，它们没有 `type` / `job_id` 字段，客户端需先判断再解析：

```json
{"action": "subscribed",   "timestamp": "..."}
{"action": "unsubscribed", "timestamp": "..."}
{"action": "pong",         "timestamp": "..."}
{"action": "stats",        "data": {"clients": 2, "timestamp": "..."}}
```

事件推送缓冲区为 256 条；写队列满时该条消息被丢弃（不阻塞调度主循环）。
连接空闲 60 秒未收到客户端 pong 即被关闭，服务端每 30 秒发一次 ping。

服务端推送事件

事件里的 `status` 是**整数**（`JobStatus` 未实现自定义 JSON 编码），取值对照：

| 数值 | 状态名 | 含义 |
| --- | --- | --- |
| 0 | `pending` | 待执行（重试排队中也是 0） |
| 1 | `running` | 已进入 Handler 执行 |
| 2 | `success` | 执行成功 |
| 3 | `failed` | 执行失败 |
| 4 | `cancelled` | 已取消 |
| 5 | `paused` | 已暂停（见下方"暂停状态"） |

注意两套写法不要混用：REST 响应里的 `status` 是状态**名**（`"pending"`），
WS/SSE 推送事件里的 `status` 是**数字**；而 WS 订阅过滤的 `status` 列表按状态**名**匹配
（`core/websocket.go` 用 `event.Status.String()` 比较）。


```json
// 任务已调度
{
  "type": "job.scheduled",
  "job_id": "0198a2e3-7d4f-7abc-9def-0123456789ab",
  "job_name": "payment_check",
  "status": 0,
  "timestamp": "2024-01-02T15:20:00+08:00",
  "metadata": {
    "trigger_at": "2024-01-02T15:30:00+08:00",
    "is_repeat": false
  }
}

// 任务开始执行
{
  "type": "job.started",
  "job_id": "0198a2e3-7d4f-7abc-9def-0123456789ab",
  "job_name": "payment_check",
  "status": 1,
  "timestamp": "2024-01-02T15:30:00+08:00",
  "metadata": {
    "attempt": 1
  }
}

// 任务执行成功
{
  "type": "job.completed",
  "job_id": "0198a2e3-7d4f-7abc-9def-0123456789ab",
  "job_name": "payment_check",
  "status": 2,
  "timestamp": "2024-01-02T15:30:02+08:00",
  "metadata": {
    "duration_ms": 2050
  }
}

// 任务执行失败
{
  "type": "job.failed",
  "job_id": "0198a2e3-7d4f-7abc-9def-0123456789ab",
  "job_name": "payment_check",
  "status": 3,
  "timestamp": "2024-01-02T15:30:01+08:00",
  "data": {
    "error": "connection timeout"
  },
  "metadata": {
    "retry_count": 1,
    "max_retries": 3,
    "timeout": false
  }
}

// 任务重试中
{
  "type": "job.retrying",
  "job_id": "0198a2e3-7d4f-7abc-9def-0123456789ab",
  "job_name": "payment_check",
  "status": 0,
  "timestamp": "2024-01-02T15:30:01+08:00",
  "metadata": {
    "next_retry_at": "2024-01-02T15:35:00+08:00",
    "retry_count": 1
  }
}

```

`job.failed` 的 `metadata.timeout` 为 `true` 表示本次失败由执行超时引起（Handler 收到的 ctx 已
`DeadlineExceeded`），它仍按普通失败计入重试次数。任务被**关停打断**时不会发出 `job.failed`：
调度器会把它保持为 `pending` 落盘，等下次启动恢复，并广播一条
`{"type":"job.cancelled","metadata":{"reason":"shutdown"}}`。

## Server-Sent Events（WebSocket 备选）

```
GET /sse/events
GET /sse/events?ticket=<ticket>   # 推荐：先调 POST /api/v1/auth/ws-ticket 申领，一次一用
GET /sse/events?token=<静态token>  # 仅机器凭据可用；JWT 不接受走 query
```

响应头为 `text/event-stream`。建连后服务端**先下发一个注释帧** `: connected`，
用于立刻把响应头交给客户端（否则订阅方要等到第一个事件才能拿到 header），
标准 `EventSource` 会自动忽略以 `:` 开头的行。之后的每一帧是 `data: <Event JSON>`：

```
: connected

data: {"type":"job.scheduled","job_id":"...","job_name":"...","status":0,"timestamp":"..."}

```

查询参数用于服务端过滤，两者都省略时等价于订阅全部事件：

| 参数 | 写法 | 语义 |
| --- | --- | --- |
| `event_types` | 可重复，也可逗号分隔（`?event_types=job.failed,job.completed`） | 按事件类型做**服务端订阅**（`EventBus.Subscribe(types...)`），不是收全量再丢弃；取值必须是已发布的类型：`job.scheduled`、`job.started`、`job.completed`、`job.failed`、`job.cancelled`、`job.retrying`、`job.paused`、`job.resumed`，未知取值在建流前返回 400 |
| `job_types` | 可重复 | 按事件的 `job_name` 过滤（事件总线不感知任务属性，这一层在 SSE 处理器内完成） |

同一类型重复出现只注册一次，事件不会重复投递。过滤参数只在建连时生效，变更需重连。