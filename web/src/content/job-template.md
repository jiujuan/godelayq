# 新建任务模板与说明

> 控制台"任务 → 新建任务"抽屉里每一项的含义、写法与可直接改用的完整示例。
> 页面上的每个约束都对应后端的一处校验，标注了报错时的原话，方便对照排查。

## 1. 一个任务由三件事构成

| 成分 | 说明 | 填错的后果 |
|---|---|---|
| **任务名称** | 就是**任务类型**，必须是后端已注册的 handler 名。控制台下拉的候选来自 `GET /api/v1/job-types` | `400 unknown job type` / `job type 'xxx' not registered` |
| **触发时间** | 相对延迟 / 绝对时间 / Cron 周期，三选一。都不填＝立即执行 | `400 invalid time format` |
| **payload** | 自由 JSON，字段含义完全由该任务类型的 handler 决定 | 控制台只校验"是不是合法 JSON" |

可选项：分组标签、单次执行超时、最大重试次数、重试间隔。

## 2. 触发时间

优先级是 **Cron > 绝对时间 > 延迟 > 立即执行**（后端 `api/handlers.go:530`）。

### 2.1 相对延迟

Go duration 写法，见下一节"§4 时间写法"。示例：`30s`、`10m`、`1h30m`、`24h`。

### 2.2 绝对时间

控制台里是一个日期时间选择器，提交时转成 ISO 8601（UTC）。REST 直接写：

```json
{ "name": "payment_check", "trigger_at": "2026-10-01T09:30:00+08:00" }
```

时间必须在将来，否则 `400 invalid time format：trigger time must be in the future`。

### 2.3 Cron 周期

6 段式，**秒在最前**；也接受 5 段（此时秒按 0 处理）。

| 表达式 | 含义 |
|---|---|
| `0 */10 * * * *` | 每 10 分钟 |
| `0 0 9 * * *` | 每天 09:00:00 |
| `*/30 * * * * *` | 每 30 秒 |
| `0 0 3 * * 1-5` | 工作日凌晨 3 点 |
| `0 9 * * *` | 5 段写法：每天 09:00 |

**重复执行**这个勾必须同时选上，否则 Cron 只用来算出"下一次"，任务跑一次就结束。
表达式非法时后端报 `invalid cron expression`。

## 3. payload 能写什么

**格式：一个合法的 JSON 值**，留空表示不带 payload。控制台不做 schema 校验，只做
`JSON.parse`——对象、数组、字符串、数字、`null` 都提交得出去，但只有**对象**是合适的载体：
handler 拿到的是原始字节，通常按结构体去解，标量与数组多半对不上它的字段。

字段名由任务类型自己定义，后端不解释、只透传。以自带的四个演示类型为例，
它们只把 payload 原样写进日志，不解析任何字段：

```json
{
  "order_id": "ORD-2024-001",
  "amount": 199.99,
  "user_id": "U123456",
  "items": [
    { "sku": "A-1", "qty": 2 },
    { "sku": "B-7", "qty": 1 }
  ],
  "notify": { "email": "ops@example.com", "sms": false }
}
```

写 payload 的四条经验：

1. **键名对齐 handler**。接一个任务类型前先问它读哪些键，或看它的源码/文档；
   拼错的键不会报错，只会被静默忽略——这是最难查的一类问题。
2. **金额与时间用字符串**。`"amount": "199.99"`、`"trigger_note": "2026-10-01T09:30:00+08:00"`，
   避免浮点与本地时区在 JSON 里漂移。
3. **别放凭据**。payload 会原样落进 `store.path` 的数据文件、出现在任务列表与详情接口里，
   也能被任何有读权限的账号看到。
4. **控制体积**。它随每次调度、每条事件广播一起流动，几百字节量级最合适。

## 4. 时间写法（Go duration）

**延迟时长、单次执行超时、重试间隔**这三项都是 Go duration（后端 `time.ParseDuration`）。

- 单位只有：`ns`、`us`、`ms`、`s`、`m`、`h`
- **必须带单位**，`30` 无效（`missing unit in duration "30"`）
- **没有 `d`、没有"周"**，`1d` 无效（`unknown unit "d" in duration "1d"`）；要表达一天就写 `24h`
- 可组合、可小数：`1h30m`、`90s`、`1.5h`、`500ms`

| 想表达 | 写 |
|---|---|
| 90 秒 | `90s` |
| 2 分钟 | `2m` |
| 1 小时 30 分 | `1h30m` |
| 半天 | `12h` |
| 一天 | `24h`（不是 `1d`） |
| 半秒 | `500ms` |

## 5. 超时与重试

| 项 | 含义 | 默认 |
|---|---|---|
| 单次执行超时 | 一次尝试最多跑多久，留空不限制 | 不限制 |
| 最大重试次数 | 失败后再试几次，`0` 表示不重试 | REST 省略即 `0` |
| 重试间隔 | 指数退避的**基准**值，仅当最大重试次数 > 0 时有意义 | `1m` |

三条容易踩的语义：

- **超时按一次失败处理**，会消耗重试次数并进入退避重排；事件里带 `timeout: true` 便于区分。
- **只有主动检查 `ctx` 的 handler 才可能被超时中止**。写死的 `time.Sleep` 或忽略 ctx 的循环
  会一直跑到自己结束，页面上看起来"超时没生效"。
- **用户取消与进程关停不算失败**，不消耗重试次数；取消会把任务记录整个删除。

退避是**每次翻倍**：基准 `1m` 的序列为 1m → 2m → 4m → 8m…，单次间隔由
`scheduler.max_retry_delay` 封顶（默认 `30m`）。

## 6. 分组

标签性质，不要求先在 `/groups` 注册。规则是 `[A-Za-z0-9_-]{1,64}`（字母、数字、下划线、短横线），
中文与空格会被拒。留空表示"未分组"。

## 7. 可直接改用的完整示例

对应 REST `POST /api/v1/jobs`；控制台表单里能找到同名字段。

**7.1 十分钟后执行一次，失败重试三次**

```json
{
  "name": "payment_check",
  "delay": "10m",
  "timeout": "30s",
  "max_retries": 3,
  "retry_delay": "1m",
  "group": "nightly",
  "payload": { "order_id": "ORD-2024-001", "amount": "199.99" }
}
```

**7.2 指定时刻执行**

```json
{
  "name": "email_send",
  "trigger_at": "2026-10-01T09:30:00+08:00",
  "payload": { "to": "user@example.com", "subject": "月度对账" }
}
```

**7.3 每天 09:00 反复执行**

```json
{
  "name": "report_generate",
  "cron_expr": "0 0 9 * * *",
  "is_repeat": true,
  "timeout": "5m",
  "payload": { "report_type": "daily_sales", "recipients": ["admin@example.com"] }
}
```

**7.4 最简：立即执行、不带 payload**

```json
{ "name": "data_sync" }
```

## 8. 批量创建

`POST /api/v1/jobs/batch`，请求体是上面的对象组成的数组。返回 **207**，
混合结果逐条看 `errors[].index`（即使全部失败也是 207，不是 4xx）：

```json
[
  { "name": "payment_check", "delay": "5m", "payload": { "order_id": "A-1" } },
  { "name": "email_send", "delay": "1h", "timeout": "30s" },
  { "name": "not_registered", "delay": "5m" }
]
```

```json
{
  "succeeded": 2,
  "failed": 1,
  "items": [ { "id": "…", "name": "payment_check", "status": "pending" } ],
  "errors": [ { "index": 2, "code": 400, "message": "unknown job type",
                "details": "job type 'not_registered' not registered" } ]
}
```

## 9. 目录加载器（另一种建任务的方式）

把 JSON 文件丢进被监控的目录即可建任务（示例见仓库的 `job_queue/`，只匹配 `*.json`）：

```json
{
  "id": "task_001",
  "name": "payment_check",
  "delay": "10m",
  "payload": { "order_id": "ORD-12345", "amount": 99.99 },
  "max_retries": 3,
  "retry_delay": "1m"
}
```

与 REST 有一处**必须记住的差别**：文件里 `max_retries` **不写＝重试 3 次**，
显式写 `0` 才是不重试；而 REST 请求体里省略 `max_retries` 就是 `0`。
`id` 可省略，省略时随机生成一个 UUID——想要"这份文件对应那个任务"的可追踪性，就自己写死一个。

## 10. 报错对照

| 后端原话 | 原因 | 怎么改 |
|---|---|---|
| `unknown job type` | 名称没注册 | 看 `GET /api/v1/job-types` 的候选 |
| `invalid timeout format：time: missing unit in duration "30"` | 时长没带单位 | 写 `30s` |
| `invalid timeout format：time: unknown unit "d"` | 用了 `1d` | 写 `24h` |
| `invalid time format：trigger time must be in the future` | 绝对时间已过 | 选将来的时刻 |
| `invalid cron expression` | 段数或取值不对 | 6 段式，秒在最前 |
| 分组名被拒 | 含中文/空格/超 64 字符 | 用 `[A-Za-z0-9_-]{1,64}` |
| `job type '…' not registered`（批量里逐条出现） | 数组中某一条名称无效 | 按 `errors[].index` 定位那一条 |

## 11. 页面上没有但常用的端点

| 用途 | 请求 |
|---|---|
| 可建的任务类型 | `GET /api/v1/job-types` |
| 单个任务详情 | `GET /api/v1/jobs/{id}` |
| 运行时间线 | `GET /api/v1/jobs/{id}/events`（内存缓冲，进程重启即清空） |
| 暂停 / 恢复 / 重试 | `POST /api/v1/jobs/{id}/pause`、`/resume`、`/retry` |
| 强制暂停（中止正在执行的这一次） | `POST /api/v1/jobs/{id}/force-pause`，需 admin |
| 取消（**删除记录**，不是暂停） | `POST /api/v1/jobs/{id}/cancel` |
| 批量操作 | `POST /api/v1/jobs/batch-ops` |
