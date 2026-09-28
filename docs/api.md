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

服务端默认**不启用鉴权**（`server.auth.token` 为空）。配置 token 后，全部端点都要求凭据，
包括 `/ws`、`/sse/events` 与 `/api/v1/health`；缺少或错误凭据返回 401。

```json
{ "code": 401, "message": "invalid or missing token" }
```

三种等价的传法，按此优先级取其一：

| 通道 | 示例 | 适用场景 |
| --- | --- | --- |
| `Authorization` 请求头 | `Authorization: Bearer <token>` | 推荐，REST 客户端默认用法 |
| `X-Auth-Token` 请求头 | `X-Auth-Token: <token>` | 不便设置标准授权头时 |
| `token` 查询参数 | `GET /ws?token=<token>` | 浏览器 WebSocket / EventSource 无法自定义请求头 |

注意两点：

- 出现 `Authorization` 头时只认它（非 `Bearer` scheme 直接判失败），不会再回退到查询参数。
- 查询参数会进入访问日志与浏览器历史，除 WS/SSE 外建议一律用请求头。

跨域由 `server.cors.allow_origins` 控制，默认 `["*"]`（任意来源）。配置为具体白名单后，
只回显命中的 `Origin` 并附 `Vary: Origin`，未命中的响应不带 `Access-Control-Allow-Origin`，
由浏览器拦截。`allow_credentials: true` 与 `*` 互斥（启动即报错）。预检 `OPTIONS` 请求
不校验 token，由 CORS 中间件直接返回 204。

```bash
# 启用鉴权后的调用示例
curl -H "Authorization: Bearer $GODELAYQ_TOKEN" http://localhost:8080/api/v1/jobs
curl -H "X-Auth-Token: $GODELAYQ_TOKEN"      http://localhost:8080/api/v1/stats
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
| max\_retries | int    | ❌  | 最大重试次数，默认 3                                         |
| retry\_delay | string | ❌  | 基础重试间隔，默认 "1m"                                      |

`timeout` 格式非法会直接返回 400（`invalid timeout format`），不会被静默忽略。


**响应**：

```json
{
  "id": "0198a2e3-7d4f-7abc-9def-0123456789ab",
  "name": "payment_check",
  "status": "pending",
  "trigger_at": "2024-01-02T15:30:00+08:00",
  "next_run_in": "10m0s",
  "retry_count": 0,
  "max_retries": 3,
  "created_at": "2024-01-02T15:20:00+08:00",
  "updated_at": "2024-01-02T15:20:00+08:00"
}
```

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
| status | string | 过滤状态：pending/running/success/failed/cancelled（大小写不敏感） |
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
`timeout` 在执行超时时才出现，未设置时字段省略。

### 4. 获取任务详情

```json
GET /jobs/:id
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

### 7. 手动重试失败任务

```json
POST /jobs/:id/retry
```

在存储中查找该 ID 且状态为 `failed` 的记录，重置 `retry_count` 并在 1 秒后重新入队，
返回新的任务视图（状态 `pending`）。这条路径依赖终态留痕：若已关闭留痕
（`store.history_limit: -1`）或该记录已被保留策略淘汰，则返回 404。

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
  "time": "2024-01-02T15:25:30+08:00",
  "version": "v1.0.0"
}
```

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

启用鉴权后浏览器只能写 `ws://localhost:8080/ws?token=<token>`（握手前由 HTTP 中间件校验，
凭据不对直接返回 401，不进入升级流程）；`server.cors.allow_origins` 白名单同时约束握手的 `Origin`。

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
（`pending`/`running`/`success`/`failed`/`cancelled`），`job_types` 比较事件里的任务名。

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
GET /sse/events?token=<token>   # 启用鉴权后：EventSource 无法带请求头，只能走查询参数
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
| `event_types` | 可重复，也可逗号分隔（`?event_types=job.failed,job.completed`） | 按事件类型做**服务端订阅**（`EventBus.Subscribe(types...)`），不是收全量再丢弃；取值必须是已发布的类型：`job.scheduled`、`job.started`、`job.completed`、`job.failed`、`job.cancelled`、`job.retrying`，未知取值在建流前返回 400 |
| `job_types` | 可重复 | 按事件的 `job_name` 过滤（事件总线不感知任务属性，这一层在 SSE 处理器内完成） |

同一类型重复出现只注册一次，事件不会重复投递。过滤参数只在建连时生效，变更需重连。