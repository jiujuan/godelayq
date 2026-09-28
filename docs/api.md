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

配置了任一凭据后，除 `POST /api/v1/auth/login`、`POST /api/v1/auth/refresh` 两个登录入口外，
全部端点（含 `/ws`、`/sse/events`、`/api/v1/health`）都要求有效凭据，缺失或无效返回 401：

```json
{ "code": 401, "message": "invalid or missing credentials" }
```

角色不足返回 403（`{"code":403,"message":"insufficient role"}`），登录失败过于频繁返回 429
并附 `Retry-After` 秒数。

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
| name         | string | ✅  | 任务类型名称，需提前注册 Handler                                |
| delay        | string | 条件 | 相对延迟，如 "10m", "1h30s"（与 trigger\_at/cron\_expr 三选一） |
| trigger\_at  | string | 条件 | 绝对时间，ISO 8601 格式                                    |
| cron\_expr   | string | 条件 | Cron 表达式，如 "0 \*/5 \* \* \* \*"                     |
| payload      | object | ❌  | 任务数据，JSON 对象，会透传给 Handler                           |
| is\_repeat   | bool   | ❌  | 是否重复执行（Cron 任务需设为 true）                             |
| timeout      | string | ❌  | 单次执行超时，如 "30s"；为空不限制。Handler 需检查 ctx 才能被按时中止      |
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
  "timeout": "30s"
}
```

只更新给出的字段，任务 ID 保持不变（未列出的字段沿用原值；`timeout` 传 `"0s"` 可取消限制，
格式非法返回 400）。更新在堆内原地完成（重排位置并写回快照），
不再走"先取消再重排"，因此不存在两步之间失败导致任务丢失的窗口。
任务已被弹出执行或已结束时返回 409（`job cannot be modified`），ID 从未存在返回 404。

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

### 8. 暂停状态（`paused`）目前只能观测

任务模型已支持 `paused`：调度器提供 `Pause` / `ForcePause` / `Resume`，
暂停中的任务离开待触发堆但**保留快照**（与 `Cancel` 连记录一起删不同），
重启后仍是暂停态——`Restore` 不会替用户解除这个决定。

本仓库当前只在库层暴露这些能力（`core.Scheduler`，用法见 `examples/`），
**HTTP 侧还没有 pause / resume / force-pause 端点**，它们随 Web 控制台的
后端里程碑一起提供（见 `docs/design/web-console-design.md` §5.4）。

因此这个状态现在"看得到、改不了"：

- `GET /jobs?status=paused` 可正常过滤，`JobResponse.status` 会返回 `"paused"`；
- WS 的 `status` 过滤与 SSE 的 `event_types=job.paused|job.resumed` 均已支持；
- 暂停中的任务既不在堆里也不在执行中，`PUT /jobs/:id` 返回 409（仅 pending 可改），
  但 `DELETE /jobs/:id` 能删掉它。

`group` 字段同理：任务模型与分组存储（`core.GroupStore` → `data/groups.json`）已就位，
但按分组过滤、分组增删改查的端点尚未开放。

这条路径依赖终态留痕：若已关闭留痕（`store.history_limit: -1`）或该记录已被保留策略淘汰，则 404。

### 8. 批量创建任务

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
  "completed": 1542,
  "failed": 8,
  "heap_size": 15,
  "uptime": "72h15m30s"
}
```

字段口径：

| 字段 | 来源 | 说明 |
| --- | --- | --- |
| pending / completed / failed | 存储快照按状态计数 | completed/failed 依赖终态留痕，关闭留痕后恒 0 |
| running | 进程内实时执行数 | 已进入 Handler、尚未返回的任务数 |
| heap_size | 调度堆长度 | 仍在堆里等待的任务数 |
| uptime | 进程启动至今 | 计数器不跨重启，completed/failed 随存储恢复而继续累计 |

`pending` 与 `heap_size` 通常相等，差值来自"已出堆、还在执行队列里排队"的那一小段：
它的快照仍是 `pending`，但已不在堆里，也未进入 Handler。`running` 只统计已进入 Handler 的任务。

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