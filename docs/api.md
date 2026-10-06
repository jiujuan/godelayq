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
| `viewer` | 只读：任务列表/详情、`/stats`、`/job-types`、实时事件流、任务的时间线端点与输出列表（`GET /jobs/:id/artifacts`） |
| `operator` | + 创建、编辑、取消、重试任务，建组与改名 |
| `admin` | + 强制暂停执行中的任务、删除分组 |
| `ops` | + 调度总开关、清空事件缓冲、运行时诊断、写操作台账查询（`GET /admin/audit`） |
| `machine` | 静态 token 的身份：等同于 `operator` 的读写，但**没有** admin/ops 的任何能力（因此也读不到台账） |

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
| name         | string | ✅  | 任务名称。带 `type` 时它是给人看的标签（汉字/字母/数字，1-64 个字符）；不带 `type` 时它兼作任务类型，见下面的[名称与类型两种写法](#名称与类型两种写法)                                |
| type         | string | ❌  | 任务类型：代码里注册的 Handler，或 `executors.commands` 声明的执行器档位（`exec.<档位名>`）。决定这条任务跑什么                                |
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

#### 名称与类型两种写法

创建任务有两种写法，判据只有一条：请求体带不带 `type`。

| 写法 | 请求体 | `name` 的规则 | 查注册表用的键 |
| --- | --- | --- | --- |
| 新写法 | `{"name":"每晚对账","type":"payment_check"}` | 标签：汉字、字母、数字，1-64 个字符，其它字符（含空格、`_`、`-`、`.`）一律 400 | `type` |
| 旧写法 | `{"name":"payment_check"}` | 兼作类型，因此**不套**标签规则（`payment_check` 带下划线是合法的） | `name` |

旧写法是既有调用方的兼容路径，行为与名称/类型解耦之前一致；新写的界面与脚本建议一律带 `type`。
`{"name":"   ","type":"   "}` 这类两边都是空白的请求算不出注册键，返回 400 `job type is required`。

响应里的 `type` 说的是同一件事：旧写法建的任务 `type` 省略（此时名称就是类型）。
读回来的对象改几个字段再 `PUT` 回去是安全的——`type` 传相同值按没传处理。

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
GET /jobs?status=pending&type=payment_check&name=每晚对账&limit=20&offset=0
```

查询参数：

| 参数     | 类型     | 说明                                            |
| ------ | ------ | --------------------------------------------- |
| status | string | 过滤状态：pending/running/success/failed/cancelled/paused（大小写不敏感） |
| type   | string | 按任务类型（注册键）过滤。旧写法建的任务类型在名称里，因此也会被 `type=` 命中 |
| name   | string | 按任务名称精确过滤（名称是给人看的标签，见[名称与类型两种写法](#名称与类型两种写法)） |
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

执行器任务在这里多四条约束。判定顺序是"任务不存在 404 → 改了名字或类型 400 → 档位不够 403 → 不是 pending 409"，
前两条在状态检查之前判，所以档位不够的身份不会从 409 里读出这条任务是否已经跑完：

- `name` 只允许传回任务原本的名字，不同值即 400（`job name cannot be changed`，`details` 里写明当前名字
  与要改成的那个）；这条对所有任务生效，把普通任务改成 `exec.` 前缀同样被拒。
- `type` 只允许传回这条任务当前的注册键（旧写法建的任务，注册键就是它的名称），不同值即 400
  （`job type cannot be changed`）。换类型等于换执行体，与档位的"改类型请删除后重建"是同一条口径；
  传相同值按没传处理，方便客户端把读到的对象改几个字段再 PUT 回来。
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

两种写法可以在同一个批次里混用，判据仍然是逐条看那一项请求体带不带 `type`：
带 `type` 的条目按标签规则判 `name`（中文标签合法、注册键取 `type`），不带的条目沿用旧规则
（`name` 兼作注册键，因此它必须是注册表里出现过的名字）。逐条独立处理意味着一条的 400
只影响那一条自己：同批里名称带空格的条目单独拿到 `invalid job name`（`details` 指出第几个字符），
类型未注册的条目单独拿到 `unknown job type`，其余条目照常 201。
`items[]` 是完整的任务对象，所以新写法建的条目带 `type`、旧写法的不带——读回来原样 `PUT` 回去仍然安全。

```json
[
  { "name": "每晚对账", "type": "payment_check", "delay": "30m" },
  { "name": "旧写法", "delay": "30m" },
  { "name": "bad name", "type": "email_send", "delay": "30m" },
  { "name": "未注册", "type": "no_such_type", "delay": "30m" }
]
```

这样一批的实测结果是 `{"succeeded":1,"failed":3}`：第 0 条 201（`items[0]` 里带
`"type": "payment_check"`），第 1 条 400 `unknown job type`（旧写法下"旧写法"这个标签就是注册键，
而表里没有它），第 2 条 400 `invalid job name`（`character " " at position 3`），
第 3 条 400 `unknown job type`。四条的结论互不影响，`succeeded` 只数成功的那几条。

## 执行器 API

执行器把"任务类型"从代码里注册的 Handler 扩展到配置文件里声明的**档位**（profile）：一条档位写清楚
用什么程序、跑哪个脚本、发哪个 HTTP 请求、允许哪些参数。注册之后它的任务类型名就是 `exec.<档位名>`。

能执行什么完全由 `executors.commands` 决定：**没有自由命令行**，任务里也不能内联源码、不能要求现场编译。
档位有两份来源：配置里的 `executors.commands`（`reload.enabled: true` 时改条目自身的可改字段
——超时、参数声明、条目增删——下一个防抖窗口就生效，改动条目内的执行许可字段会让整次重载被拒绝；
关闭热重载时改它仍要重启进程），`executors.web_enabled: true` 时
还可以在下面"档位的在线管理"一节那三个写端点上增删改，写完立即生效。开关、白名单与部署前提见 [部署文档](./deployment.md) 的"开启执行器"一节。

### 与 `/job-types` 的关系

执行器打开（`executors.enabled: true`）并注册成功时，档位名与内置 Handler 名一起出现在
`GET /api/v1/job-types` 里（按字典序）：

```json
{
  "types": ["data_sync", "email_send", "exec.fail3", "exec.long60", "exec.nap",
            "exec.not_deployed", "exec.show_token", "payment_check", "report_generate"]
}
```

打开 `executors.adhoc.enabled` 时，四条内置自由执行档位（`exec.php`、`exec.python`、`exec.shell`、`exec.http`）也在这里：**它们的名字是类型，而跑哪个文件、打到哪条地址由任务的 payload 给**（顶层的 `script` 或 `url`，见下面[自由执行档位](#自由执行档位executorsadhoc)）。

关闭时这个列表只剩代码里注册的那几个。探测不通过的档位（脚本没部署、程序不在 PATH）
**仍然注册**，因此也会出现在这里；提交它会被拒，见下面的"提交期被拒的四种响应"。

### 档位清单：`GET /api/v1/executors`

```json
GET /api/v1/executors
```

档位声明是公开信息（不含脚本内容与参数取值），`viewer` 及以上可读；执行器关闭时这不是错误，
返回 `enabled: false` 与空列表。真实响应（2026-10-01 本机冒烟：配置侧两个档位，档位文件三条，
其中一条与配置的 `py_hello` 撞名，另一条指向 workspace 之外的脚本；为篇幅省掉一条同形的）：

```json
{
  "enabled": true,
  "profiles": [
    {
      "key": "exec.cfg_health",
      "name": "cfg_health",
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
      "url": "https://api.example.com/health",
      "source": "config",
      "editable": false,
      "degraded": false
    },
    {
      "key": "exec.py_hello",
      "name": "py_hello",
      "kind": "script",
      "runtime_ok": true,
      "reason": "",
      "timeout": "1m0s",
      "max_parallel": 1,
      "args": [
        { "name": "day", "required": true, "default": "", "pattern": "^(yesterday|today)$", "secret": false }
      ],
      "env_allow": [],
      "has_secret_args": false,
      "preferred_result_direction": "tail",
      "source": "config",
      "editable": false,
      "degraded": false,
      "path_display": "scripts/py_hello.py"
    },
    {
      "key": "exec.py_hello",
      "name": "py_hello",
      "kind": "script",
      "runtime_ok": false,
      "reason": "profile \"py_hello\" is already declared in executors.commands, the stored one is not registered",
      "timeout": "1m0s",
      "max_parallel": 1,
      "args": [],
      "env_allow": [],
      "has_secret_args": false,
      "preferred_result_direction": "tail",
      "source": "store",
      "editable": false,
      "degraded": true,
      "path_display": "scripts/py_hello.py"
    },
    {
      "key": "exec.store_outside",
      "name": "store_outside",
      "kind": "script",
      "runtime_ok": true,
      "reason": "",
      "timeout": "1m0s",
      "max_parallel": 1,
      "args": [],
      "env_allow": [],
      "has_secret_args": false,
      "preferred_result_direction": "tail",
      "source": "store",
      "editable": true,
      "degraded": false,
      "path_display": "C:\\Users\\xing\\AppData\\Local\\Temp\\w07smoke\\outside\\outside_report.py"
    }
  ],
  "required_role": "admin",
  "max_timeout": "30m0s",
  "web_enabled": true,
  "runtime_allow": ["python", "node"]
}
```

上面那份响应里的四行是同一次读取里连着的：第三条与第二条 `key` 相同（撞名的本义），
它指向的脚本其实存在，但作为没注册的那一条仍然报 `runtime_ok: false`。
第四条那种越界写法只有页面那一侧能建出来，绝对路径对 `viewer` 也可见（待做项 S-3）。

字段口径：

| 字段 | 说明 |
| --- | --- |
| `enabled` | `executors.enabled` 的当前值 |
| `required_role` | 提交 `exec.*` 任务所需的最低角色；**执行器关闭时是 `null`**（那时"要什么档位"这个问题不成立） |
| `max_timeout` | `executors.max_timeout`，payload 里 `timeout` 的上限；只在 `enabled: true` 时出现，关闭时整个键省略 |
| `web_enabled` | `executors.web_enabled`，也就是档位的在线管理开没开。**关闭时这个键照样给出并回 `false`**（TASK-W07）：它是前端判断"能不能改档位"的唯一判据，缺键等于让人猜后端认不认识这套管理 |
| `runtime_allow` | `executors.runtime_allow` 实际生效的那份名单（配置留空时给的是代码补齐的默认名单），给档位表单的"解释器"下拉用。与上面两项故意不同：**`enabled: false` 时也给出**——那两项说的是"现在能不能提交执行任务"，这一项是一份配置事实 |
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
| `source` | `config`（来自配置的 `executors.commands`，在这里只读）或 `store`（来自档位文件 `executors.profiles_path`，由上面那三个写端点管） |
| `editable` | 能不能在页面上改它：`web_enabled && source == "store" && !degraded`，**由后端一次算好**，前端只读这个布尔决定按钮显不显示。写请求的边界仍是 ops 档判定——隐藏按钮从来不是安全边界 |
| `degraded` | 为真表示这条档位与 `executors.commands` 里的同名档位撞上了：**看得见但没生效**（见[档位的在线管理](#档位的在线管理)）。此时 `runtime_ok` 恒 `false`、`reason` 给的是那句撞名说明 |
| `path_display` | 这个档位指向的本机文件写法：`executors.workspace` 之内给相对写法、之外给**绝对路径**（决策 D5）。给的是解析后的结果而不是请求体里的原文：分隔符按本机归一、多余的段会被收掉（Windows 上写 `a//b` 会读回 `a\\b`）。`http` 档位与"`program` 写成 `runtime_allow` 里的程序名"的 binary 档位没有文件可指，**整个键省略**。⚠️  reader 档也读得到这个键，目录结构的遮蔽方案登记为待做项 S-3，不在本节 |
| `adhoc` | 这一条是不是**自由执行档位**（`executors.adhoc.enabled: true` 时那四条内置档位）。普通档位是 `false`，而这个键一律出现：省略会让前端分不出普通档位与"后端还不认识自由执行档位"（TASK-N06） |
| `location` | 只有 `adhoc: true` 的条目带，对象 `{key, kind, label, required, hint}`——payload 顶层的键名（`script` / `url`）、位置的形态（`path` / `url`）、输入框标题、是否必填（内置四条恒 `true`）、以及这份部署对取值范围的说法（见[自由执行档位](#自由执行档位executorsadhoc)）。前端以"有没有 `location`"为唯一判据，不必自己认 `exec.php` 这类键名 |
| `method` / `body_mode` / `url` / `header_allow` | 只出现在 `http` 档位上。`body_mode` 是 `json` / `raw` / `none` 之一（配置里没写 `body` 的档位在这里归一成 `none`）；`url` 给的是模板原文（含 `{占位符}`），不是渲染后的地址；`header_allow` 是 payload 可覆盖的请求头名，**空表时整个键省略**（与 `env_allow` 的口径不同，别当成"没返回"） |

一个注册键在 `profiles` 里最多出现两行（TASK-W07）：`executors.commands` 与档位文件写了同一个名字时，
生效那条与降级那条都在这份列表里，`key` 相同、靠 `degraded` 区分，降级那条排在它后面。
静默丢掉那一条会让"页面上明明建过、重启后却不见了"变成无解之谜，所以宁可多给一行说明。

`runtime_ok` 只代表**当前这台机器**：同一份档位在另一台机器上可能因为解释器没装而不可用，
而探测既不访问网络也不启动进程。降级那条的 `runtime_ok` 一律是 `false`（它确实跑不了），
即便它指向的脚本其实存在——两条事实同时成立时，"为什么这条没生效"是更要紧的那一件。

这份响应**不是请求体**：它多出 `key`、`runtime_ok`、`source` 这些只给展示看的键，
而写端点把多余字段一律按未知键拒掉（见下面那一节的解码口径）。
因此编辑一条档位时要把手上的**定义**发回去，不能拿 `GET` 的结果直接 `PUT` 回来——
响应里本来也没有 `runtime` / `script` / `program` / `args_render` / `env` 这些定义字段。
定义另有读口：[`GET /api/v1/executors/profiles/:name`](#读取档位定义get-apiv1executorsprofilesname)。

### 自由执行档位（`executors.adhoc`）

`executors.adhoc.enabled: true`（默认 `false`，打开还要求 `executors.enabled: true`）时，
登记表里多出四条不指向任何具体脚本或地址的档位。它们与 `executors.commands` 里的档位是同一个东西
——同一套参数判据、同一个执行池、同一份超时与产物规则——唯一区别是**执行位置写在任务里**：

```json
{"name": "昨晚的对账", "type": "exec.php",   "payload": {"script": "D:\\work\\reconcile.php"}}
{"name": "订单回调",   "type": "exec.http",  "payload": {"url": "https://hook.example.com/orders/7"}}
```

`payload` 顶层因此多了两个键：`script`（`exec.php` / `exec.python` / `exec.shell` 收）与
`url`（`exec.http` 收）。两条内置档位各只认自己那一个键再加 `timeout`，
`args` / `params` / `env` / `headers` / `body` 一律被拒——普通档位不受影响，
它们带上这两个键同样会被拒（错误文案会列出这条档位实际接受哪些键）。

判据在提交期就跑完（`POST /jobs` 直接 400，任务不入队），执行期还会各重跑一遍：

| 项 | 判据 |
| --- | --- |
| `script` | 非空 → 无控制字符 → 无 shell 元字符（分号、竖线、与号、反引号、美元符、尖括号；**反斜杠除外**，Windows 绝对路径必带）→ 能算出绝对路径 → 落在 `executors.adhoc.path_prefixes` 之内（空列表为不限目录）→ 扩展名匹配（`require_extension` 打开时）→ 文件存在且是普通文件 |
| `url` | 非空 → 无空白与控制字符 → 能解析 → scheme 是 `http` / `https` → 不带 `user:pass` → 主机非空 → 命中 `executors.adhoc.url_hosts`（空列表为不限主机） |

**没放宽的那一条**：地址范围守卫照旧，在建连之前拒回环、私网、链路本地（含 `169.254.169.254`）、
组播与 `100.64.0.0/10`，只有 `executors.adhoc.url_allow_private: true` 才整条关掉。
argv 也仍然是数组直传，不经过 shell。

响应里的一行长这样（`GET /api/v1/executors`，为篇幅只留必要字段；`hint` 的取值随配置变）：

```json
{
  "key": "exec.php", "name": "php", "kind": "script",
  "runtime_ok": true,
  "reason": "runtime \"php\" is available; the script path comes from each job's payload, so this check cannot tell whether a given file exists",
  "timeout": "5m0s", "max_parallel": 1, "args": [], "env_allow": [],
  "has_secret_args": false, "preferred_result_direction": "tail",
  "source": "adhoc", "editable": false, "degraded": false,
  "adhoc": true,
  "location": {
    "key": "script", "kind": "path", "label": "脚本路径（.php）", "required": true,
    "hint": "可以填本机上任意位置的脚本文件；这个部署没有做目录范围限制。"
  }
}
```

`location.hint` 与启动日志里的那几条 `WARN` 说的是同一批事实，只是一个给界面、一个给运维：
打开整节时 `executor handlers registered` 之后会有一条
`free-form execution profiles are enabled`，带上当前生效的 `path_prefixes`、`url_hosts`、
`url_allow_private` 与 `required_role` 四个取值；限定目录留空或放开回环与私网时各再多一条。
整节关闭时这四条键不存在，`GET /executors` 与 `GET /job-types` 的形状与之前逐字一致。

**提交这四条的身份门槛与其余档位同一条**：`executors.required_role`（默认 `admin`），
不够时 403 的 `details` 会点名这个配置项（见下面"提交期被拒的四种响应"）。
风险面因此比配置侧大一档：够这个档位的身份可以让这台机器执行它提交的路径、请求它提交的地址，
而配置文件里从没写过那两个位置——这也是启动日志要把当前范围说出来的原因。

### 档位的在线管理

档位有两份来源：配置里的 `executors.commands`（这一组端点读得到、改不了），以及
`executors.profiles_path` 指向的 JSON 文件（默认 `./data/exec-profiles.json`，这一组端点写它）。
启动时两份合并，同名以配置为准、文件里那一条标记为未生效；页面写入**立即生效、不必重启**，重启之后仍在。

这一组端点改的是"这台机器能执行什么"，门槛比删组更高：

- **只接受 `ops` 档 JWT**（决策 D10），读定义那一个也一样——记录里有脚本路径、固定参数与请求头。
  静态 token 的身份是 `machine`（档位等同 operator），一样 403。
  它与"谁能提交 `exec.*` 任务"（`executors.required_role`，全局一份）是两条互不替代的判定。
- **前提是** `executors.enabled: true` 且 `executors.web_enabled: true`（决策 D2，默认 false）。
  关闭时这一组一律 503 且 `message` 含 `not enabled`；打开了却没装配依赖时也是 503，
  `message` 含 `not configured` 并在 `details` 里点名 `api.WithExecutorProfileStore`。
  两条文案刻意可区分：一个要改配置重启，一个是部署漏了接线。
- 每次写请求都进[写操作台账](#写操作台账get-apiv1adminaudit)一行，动作词是
  `executor.profile_create` / `executor.profile_update` / `executor.profile_delete`。
  台账里只有身份、方法、路由、状态码与结论，**没有请求体，也没有 `env` 的固定取值**。

请求体的键名与档位文件里的记录一字不差，也就是 `executors.commands` 那批键再加两个时间戳
（`created_at` / `updated_at` 由服务端写，传进来也会被覆盖）：

| 键 | 适用 `kind` | 说明 |
| --- | --- | --- |
| `name` | 全部 | 必填，`[A-Za-z0-9_-]{1,64}`；注册后的任务类型名是 `exec.<name>`，**主键忽略大小写** |
| `kind` | 全部 | 必填，`script` / `binary` / `http` |
| `runtime` / `script` | script | 解释器名（必须在 `executors.runtime_allow` 里）与脚本路径 |
| `program` / `fixed_args` | binary | 程序名与固定参数 |
| `args` / `args_render` / `positional` | script、binary | 参数声明与命令行模板，规则与配置侧同一份 |
| `cwd` | script、binary | 工作目录 |
| `env` / `env_allow` | script、binary | 固定注入的环境变量、payload 可注入的键名白名单 |
| `timeout` / `max_parallel` / `retry_on_exit` | 全部 | 单次超时、并发上限、可重试的退出码 |
| `method` / `url_template` / `allowed_hosts` / `headers` / `header_allow` / `body` / `expect_status` / `capture_response` / `max_body_bytes` / `max_redirects` / `deny_private_ranges` | http | HTTP 档位的字段，含义与配置侧一致 |

字段组合与取值的校验走的是启动时那一份规则（同一个函数），所以"页面上存得下的"与
"配置文件里写得的"不会长出第三种判定。未知键一律 400（与配置侧解码同一口径），
`details` 给校验原文——里面只有字段名与路径写法，不含参数取值。

**路径写法与配置侧不同**（决策 D5）：这一组端点接受绝对路径与 `executors.workspace` 之外的相对路径，
配置文件里那一条仍然越界即拒。这是本期有意留的口子，理由与配套的安全加固待做项见
[设计文档](./design/web-profile-design.md) §7.2。

三个端点共同的固定顺序：**校验 → 探测 → 冲突判定 → 写文件 → 生效 → 响应**。
写文件一定在生效之前（不变量 I2）；生效失败会立刻把文件退回请求之前的状态，
所以"文件里有这条、进程里跑不了"不会留在系统里。回滚本身也失败时记 error 日志，
响应里直说"文件与进程不一致，重启可对齐"。

一个实测到的客户端现象（TASK-W09，Windows 本机循环口，带请求体的 POST 各 30 次 × 若干轮）：
**被门禁挡下的请求**（上面那两条 503 与角色判定那条 403）里约 5% 让调用方看到"连接被重置"而不是那个 JSON 错误体
（三轮分别是 2、3、0 次）。服务端两边的日志与台账里状态码都在，说明响应已经写出，只是客户端那一次读失败了。
对照是"处理器读完请求体再拒"的那条路径（校验 400、撞名 409）：90 次一次都没出现。
判据因此指向"门禁在读取请求体之前就中止"，与这一组端点无关——任何被门禁挡下的带体写请求都一样。
调用方按"写请求遇到连接重置就重试一次"处理即可；修法（门禁里先把请求体读完再回错误）登记为 D-0904，不在本系列。

### 读取档位定义：`GET /api/v1/executors/profiles/:name`

`GET /api/v1/executors` 的每一行说的是**处境**（来源、能不能改、这台机器跑不跑得动），
档位的定义字段一个都不在里面（上一节末尾那句就是这件事）。页面上要"编辑一条已有档位"，
就得有一个把定义给回来的口——这一条就是它，也是 PUT 请求体的合法回填来源。

```json
GET /api/v1/executors/profiles/doc_py        # ops

{"name":"doc_py","kind":"script","runtime":"python","script":"scripts/py_hello.py",
 "args":[{"name":"day","required":true,"pattern":"^(yesterday|today)$"}],
 "args_render":["--day={day}"],"env_allow":["TRACE_ID"],
 "env_keys":["LANG_PACK","REPORT_HOME"],
 "timeout":"2m","max_parallel":2,"retry_on_exit":[75],
 "created_at":"2026-10-02T00:18:07.2119685+08:00","updated_at":"2026-10-02T00:18:07.2119685+08:00"}
```

（2026-10-02 本机冒烟原文，键序按响应原文。）返回的是档位文件里那条记录本身，
键名与 PUT 的请求体一字不差，因此把这份响应去掉 `env_keys` 再发回 PUT 就是一次有效的修改。

- **`env` 只有键名**：`env_keys` 按字典序列出这条档位固定的环境变量名，取值一律不外露
  （与 `GET /executors` 只给 `env_allow` 键名同一条口径，`details` 与台账里也不会有）。
- 配置侧的档位没有存储记录 → 409，`details` 点名 `executors.commands`（与 PUT/DELETE 同一条判据与文案）。
- 文件里没有这个名字 → 404；名字非法 → 400；`web_enabled=false` 或没装配依赖 → 503，两条文案可区分。
- **读请求不进写操作台账**（`api/audit.go` 的中间件只记 POST/PUT/DELETE），所以这里没有新的动作词。

### 新建档位：`POST /api/v1/executors/profiles`

```json
POST /api/v1/executors/profiles        # ops
Content-Type: application/json

{
  "name": "smoke_py",
  "kind": "script",
  "runtime": "python",
  "script": "scripts/py_hello.py",
  "args_render": ["--day=today"],
  "env": {"SMOKE_FIXED": "只写进档位文件，不出现在任何响应里"},
  "env_allow": ["LANG"],
  "timeout": "2m"
}
```

201 的响应就是这个档位在登记表里的当前形状，与 `GET /api/v1/executors` 的单项**完全相同**
（字段口径见上一节），因此带着探测结论。真实响应（2026-10-01 本机冒烟）：

```json
{"key":"exec.smoke_py","name":"smoke_py","kind":"script","runtime_ok":true,"reason":"",
 "timeout":"2m0s","max_parallel":1,"args":[],"env_allow":["LANG"],"has_secret_args":false,
 "preferred_result_direction":"tail"}
```

**探测失败不拒绝保存**：`runtime_ok: false` 也照样 201、照样进登记表。一条指向还没部署的脚本的档位
是运维要留着的东西，把它拒在门外只会逼人回去改 yaml、绕过这套审计。它会以不可用的样子出现在
`GET /executors` 里，提交它仍然被拒（400 `executor profile is not available on this server`）。

| 码 | 条件 |
| --- | --- |
| 201 | 已落盘并已生效 |
| 400 | 名字非法 / 出现未知键 / 字段组合不合法（`details` 给校验原文） |
| 403 | 身份不足 ops |
| 409 | 文件里已有同名档位（`details` 指出该用 PUT）；或与 `executors.commands` 里的名字撞了（`details` 点名配置来源，配置文件那份不会被顶掉） |
| 500 | 写文件失败（登记表与调度器一行都没动）；或写成功却没能生效（已回滚，`details` 说明回滚结果） |
| 503 | `web_enabled=false`；或打开了却没装配依赖 |

### 修改档位：`PUT /api/v1/executors/profiles/:name`

请求体与 POST 同形状。`name` 省略时取路径上那个；写了就必须与路径一致（忽略大小写），否则 400——
改名等于换一条档位，本端点不做改名，要换名就删了重建。

`kind` / `script` / `program` **不许改**（决策 D7）：这三项决定"这条档位是什么"，换内核不是改参数：

```json
{"code":400,"message":"field cannot be changed",
 "details":"script cannot be changed on an existing profile (\"smoke_py\"); delete it and create a new one"}
```

其余判定、五步顺序与状态码与 POST 相同，两处差别：路径上的名字在文件里没有 → 404
（它属于配置侧档位时是 409，`details` 说明它是只读的）；生效失败时文件退回到**改之前的值**。

**`env` 这个键不带就是"不改"**：请求体里没有 `env` 时，文件里原有的那一组原样留着；
显式写 `"env": {}` 才是清空它。这条规则存在的前提是定义读口不回显取值——
一次只想改超时的编辑如果把 `env` 整组抹掉，界面上看不见、响应里也不可见。
带非空 `env` 时是**整组替换**（不是逐键合并），页面上因此把这件事写在输入框下面。
本机冒烟（2026-10-02）：不带 `env` 的 PUT 之后文件里仍是
`{"LANG_PACK":"zh","REPORT_HOME":"/srv/report"}`，`"env": {}` 的 PUT 之后这个键整个消失。

PUT 是整条覆盖，不是补丁：请求体没写的其它字段都会落成零值，所以编辑表单要先把
[档位定义](#读取档位定义get-apiv1executorsprofilesname)读回来再改。

改动只影响之后的执行：已经在跑的那一次用的还是它启动时拿到的那份定义。

### 删除档位：`DELETE /api/v1/executors/profiles/:name`

```json
DELETE /api/v1/executors/profiles/smoke_py             # 默认 jobs=pause
DELETE /api/v1/executors/profiles/smoke_py?jobs=block
```

- `pause`（默认）：把该类型**待执行**的任务逐条置 `paused` 等人工确认；
  **一条正在执行的都不动**（决策 D6：中止正在跑的执行属于 admin 档的 `force-pause`）。
- `block`：该类型还有未终态任务（待执行 / 正在执行 / 已暂停）就 409，什么都不改。
- `jobs` 的其它取值 400（`details` 写 `expected pause or block`），不会被静默当成默认处理。
- 文件里没有这条档位 → 404；它属于配置侧 → 409。

三步顺序：先钉住任务 → 再删文件里的记录 → 最后让整张表重新生效（摘掉处理函数）。
生效失败会把记录写回去，于是任务只是被钉住、档位仍在，重试删除即可（幂等）。

200 响应把影响面报清楚（2026-10-01 本机冒烟）：

```json
{"key":"exec.smoke_py","name":"smoke_py","paused_jobs":1,"running_jobs":0,"already_paused_jobs":0}
```

`paused_jobs` 是**本次新钉住的待执行任务数**，不含正在执行的那条，也不删之前就已经暂停的——
后一项在 `already_paused_jobs` 里单独给。

删除之后，被钉住的那条恢复时会走到"没有处理函数"的既有判定，不会悄悄跑起来：

```
level=ERROR msg="no handler registered for job" handler_key=exec.smoke_py
```

任务因此判为 `failed` 且没有执行产物可读。要保住结果就别恢复它，让它停在 `paused` 上。

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

一个任务有哪些尝试、各自的输出多大、产物还在不在，不必逐个 `attempt` 试读：见下一节
[列出各次尝试的输出](#列出各次尝试的输出-get-jobsidartifacts)。

### 列出各次尝试的输出：`GET /jobs/:id/artifacts`

回答"这个任务跑过哪几次、每次输出多大、被清理了没有"，**不含输出正文**。
数据来自观测库的 `artifact_index` 表（SQLite 观测层，设计依据见
`docs/design/sqlite-observability-design.md` 的 D8 与 §6.2），
正文仍然只存在于磁盘上的产物文件里，读它是上面那个 `/result` 端点的事。

```json
GET /api/v1/jobs/01a0f3eb-7670-78bf-bdf4-605434827d4b/artifacts
```

```json
200 OK
{
  "job_id": "01a0f3eb-7670-78bf-bdf4-605434827d4b",
  "count": 1,
  "items": [
    { "attempt": 1, "kind": "script", "profile": "fail3",
      "out_bytes": 34, "err_bytes": 41, "truncated": false,
      "state": "available", "created_at": "2026-10-01T04:04:51+08:00" }
  ]
}
```

| 字段 | 说明 |
| --- | --- |
| `attempt` | 第几次尝试，**按升序返回**（重试链的自然阅读顺序）。同一 `(任务, 尝试)` 只有一行：重复跑到同一个 `attempt` 是覆盖，`created_at` 跟着走 |
| `kind` / `profile` | 与 `exec.kind` / `exec.profile` 同一取值，来自执行侧而不是快照：这一行登记的时刻就是产物写完的时刻 |
| `out_bytes` / `err_bytes` | 落盘字节数，与 `exec` 里那两个数**同源**（都来自写盘收尾的同一份结论），也就是产物文件的实际大小 |
| `truncated` | 该次输出撞到 `executors.output.max_bytes` 上限，后面的部分没写进文件 |
| `state` | `available` / `purged`。见下面两条 |
| `created_at` | 这一行写入的时刻，不是任务的执行时刻（两者通常只差几毫秒） |

`state` 的两条来历：`purged` 要么由**启动对账**标的（这一次启动时发现目录已经不在了），
要么由**读取端点**标的（`/result` 去读文件时读到了"不存在"，顺手把快照与索引一起纠正）。
一张表只登记一次执行的输出属性，所以**删掉某一路文件会让整行变成 `purged`**，
而另一路文件可能还在、`/result?stream=err` 照样读得到（本机实测：删掉 `a1.out` 之后
`state` 已是 `purged`，同一个 attempt 的 `a1.err` 仍以 41 字节返回 `found: true`）。

档位是 `reader`（`viewer` 及以上）：这里出去的全是元信息。含 `secret` 参数的档位
会把**读正文**的门槛抬到提交档，那道判定不在这个端点上，别把它当成读输出的捷径。

失败返回：

| HTTP | 触发条件 | 响应原文 |
| --- | --- | --- |
| 400 | `id` 不是合法的任务 ID（含路径写法、点号、非 ASCII） | `{"code":400,"message":"invalid id","details":"got \"..escape\": artifact: job id \"..escape\" contains an unusable character '.'"}` |
| 500 | 索引读不出东西（观测库坏了、表被外部改坏）。不静默退回"扫目录"，也不假装是空列表 | `{"code":500,"message":"failed to load artifact records","details":"..."}` |
| 503 | 这次部署没挂索引（`observability.enabled`、`observability.artifacts.enabled`、`executors.enabled` 任一为 `false`） | `{"code":503,"message":"artifact index is not configured","details":"start the server with executors.enabled and observability.artifacts.enabled to enable /api/v1/jobs/:id/artifacts"}` |

任务存在但一行记录都没有 → `200` + `"items": []`（不是 404，与事件端点同一口径）；
这个端点也不查任务是否存在，所以对一个不存在的 ID 同样回空列表。

上表里 400 与 503 是本机实测原文；500 那条由用例取证（`TestListJobArtifacts_ReadErrorIs500`），
手工环境里造不出"库在但读不动"的状态。

**索引从启用它的那个版本开始记**：在此之前就落在产物目录里的输出没有索引行，
它们不出现在这里，但 `/result` 照旧读得到（那个端点直接读文件）。产物被 TTL 清理之后
这一行的处理是**整行删掉**（先删目录、再删行），所以过期产物的列表会变短而不是变成一堆 `purged`。

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
的才是用户操作。这条标记必须写在事件里：未启用观测层时事件历史只在进程内存里，重启之后崩溃前那几轮
`job.started` 已经不在，时间线上只剩这一条 `job.paused`（启用 `observability.events.enabled` 后
重启前的轮次仍查得到，但这一条标记本身仍然只有事件能表达——它说的是"当初为什么暂停"）。

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

两个端点的数据来源有两个，由这次部署的观测层配置决定，响应里的 `note` 字段会说清是哪一个：

| 装配情况 | 读的是 | `note` |
| --- | --- | --- |
| `observability.enabled` 与 `observability.events.enabled` 都为真 | 持久化事件库（`store/sqlite` 的 `job_events`） | `persisted event store; newest entry may lag by the write flush interval` |
| 其余情况（默认配置即此） | 进程内的环形缓冲：每任务最近 100 条、全局最近 500 条、最多 2000 个任务后按 LRU 整体淘汰 | `in-memory buffer, cleared on restart` |

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

`count` 是**本次返回的条数**，不是库里/缓冲里的总数。
`limit` 缺省或非法表示"给我全部已留存的"，上限按来源分岔：

| 来源 | `limit` 上界 | 说明 |
| --- | --- | --- |
| 库 | **1000** | 一次响应的体积界限，不是策略旋钮（不可配置）。任务时间线的常规用法不需要超过 100，1000 是给排障导出用的 |
| 内存缓冲 | 每任务 100 / 全局 500 | 上限就是窗口容量本身 |

读库时的两条精度口径，前端若拿时间戳做精确比对必须知道：

- `timestamp` **精确到微秒**。库里存的是微秒整数，发布时的纳秒部分在写入时截断。
- 读回的 `timestamp` 用**进程本地时区**表示同一时刻（`time.UnixMicro` 的口径），
  所以 JSON 里的时区偏移不一定等于事件当初产生时的偏移。比较时刻，不要比较文本。

同毫秒连发的两条（例如 `job.scheduled` 与 `job.started`）按写入顺序 `seq` 定序，与时间戳无关。

库路径有一条与直觉不同的地方：批量写入按 `observability.flush_interval`（默认 200ms）合并落盘，
所以刚发生的事件可能还没进库；`note` 的第二句说的就是这件事。实时增量本来就由 WS/SSE 负责，
不要把"WS 已经推过来的事件在 REST 里查不到"当故障处理。

没有记录时返回空列表（`"items": []`）而不是 404：详情页时间线本来就可能在等第一个事件。
读库失败返回 500 + `ErrorResponse`，**不会**静默退回内存缓冲——两条路径的数据范围不同，
给一份缺历史的列表比给一个错误更容易被误用。

`DELETE /api/v1/admin/events`（ops 档）清的是**那份内存缓冲**，返回被清掉的条数。
装配了事件库时详情页时间线读的是库，因此清空不会让历史消失；库里的事件由
`observability.events.retention_count` / `retention_age` 淘汰，没有手工清空的端点。

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
  },
  "reload": {
    "enabled": true,
    "watched_path": "D:/app/configs/config.yaml",
    "last_attempt_at": "2024-01-04T10:12:03.481+08:00",
    "last_applied_at": "2024-01-04T10:12:03.483+08:00",
    "result": "ok",
    "error": "",
    "applied_keys": ["logging.level", "scheduler.workers"],
    "ignored_keys": ["scheduler.queue_capacity"],
    "rejected_keys": [],
    "watcher_error": ""
  }
}
```

只读，不含任务内容与凭据。`queue_length`/`running` 是瞬时值，用来看趋势不是用来审计的。

`reload` 这一段是配置热重载最近一次的结论，**这台部署没打开 `reload.enabled` 时整个键缺省**
（不是给一个空对象——空对象会被读成"启用过但从没重载过"）。十个字段的口径：

| 字段 | 含义 |
| --- | --- |
| `enabled` | 这台进程启动时 `reload.enabled` 的取值。它是重启档，所以运行期改了文件里的这一项，这里也不变 |
| `watched_path` | 监听器盯的那个文件的绝对路径；没建监听器时缺省 |
| `last_attempt_at` | 最近一次重载尝试的时刻（`unchanged` 与 `rejected` 也算一次尝试） |
| `last_applied_at` | **这一次**尝试里应用成功的时刻，只在 `result=ok` 且真换了东西时出现。它不是历史台账：随后再来一次失败的重载（`rejected`/`failed`），这个键就不再出现，因为整份读数是按次替换的快照。要看历史请翻日志里那条 `config reload applied` |
| `result` | `ok`（改了并应用）/ `unchanged`（取值等价，什么都没动）/ `rejected`（整次作废：坏文件、未知键、触碰拒绝档）/ `failed`（应用中途失败，已逆序回滚）/ `degraded`（回滚自己又失败，现网可能是混合态）。空串表示启用过但从没尝试过 |
| `error` | 这一次为什么没生效，一句话 + 底层原因原文；成功时缺省 |
| `applied_keys` | 本次处理过的键路径。执行器档位按条目摊开，所以看到的是 `executors.commands.<档位名>.<字段>` 一整套而不是 `executors.commands`；执行器整节没打开时档位改动也出现在这里，另有一条 warn 说明"未生效" |
| `ignored_keys` | 重启档的键：改了、接受了、没应用，重启才变 |
| `rejected_keys` | 拒绝档的键。注意它列的是"当前与生效那份不一致的全部凭据/执行许可键"，不是"这次只改了哪一个" |
| `watcher_error` | 监听器自身的故障（事件循环没在限期内退出、`Close` 报错等）；与重载结论分开记 |

两个时间字段是 RFC3339Nano 字符串，零值不给键。三个键清单为空时序列化出 `null`（`omitempty`
对 nil 切片的效果），消费方要把 `null` 与"缺键"当成同一件事——"没有内容"。

本系列**没有新增任何端点**：`/admin/runtime` 仍是 ops 档专用（`ops` 角色或运维凭据），
读端点仍不进写操作台账；配置里那三份凭据的取值不会出现在这段读数里。

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

### 写操作台账：`GET /api/v1/admin/audit`

```json
GET /admin/audit?limit=20&actor=admin01&verdict=denied
```

读的是 SQLite 观测库的 `write_audit` 表：**每个写请求（POST/PUT/DELETE）一行**，
记录谁、以什么身份、对哪个端点做了什么、成没成、耗时多久。
读端点（GET）与 WebSocket/SSE 握手不进台账，否则会被前端轮询写满。

前置条件是 `observability.enabled` 与 `observability.audit.enabled` 同时为真
（设计文档 §6.3、任务卡 TASK-S06）。任一为假时本端点返回 503：

```json
{
  "code": 503,
  "message": "write audit log is not configured",
  "details": "start the server with observability.enabled and observability.audit.enabled to enable /api/v1/admin/audit"
}
```

503 而不是 404：开关没打开的部署里，路由存在这一点本身就该说清楚。
台账写入器未装配时写请求仍会落一行结构化日志（`msg="write operation audited"`），
所以关掉开关不是丢掉审计，是退回只查日志。

查询参数（全部可选，组合时按 AND 处理）：

| 参数 | 取值 | 说明 |
| --- | --- | --- |
| `limit` | 非负整数，默认 50，上限 500 | 一页行数；`0` 按默认 50 处理，超过 500 是 400 而不是被截断 |
| `offset` | 非负整数，默认 0 | 跳过行数，配合 `total` 翻页 |
| `actor` | 账号名，≤128 字符 | 精确匹配；匿名请求这列是空串，`actor=` 匹配不到它们 |
| `action` | 下方动作词表之一 | 拼错或不在表内一律 400 |
| `verdict` | 下方结论词表之一 | 同上 |
| `since` / `until` | RFC3339 时间 | 两端都含；按写入时间比较 |

`limit=abc`、`limit=-5`、`?verdict=nonsense` 这类都是 400，`details` 会给出该参数的合法取值：

```json
{
  "code": 400,
  "message": "invalid verdict",
  "details": "got \"nonsense\": expected one of ok, bad_request, denied, not_found, conflict, partial, throttled, error, other"
}
```

读库失败是 500（`failed to load audit records`），不会静默返回一份看起来完整的空结果。

响应体：

```json
{
  "count": 2,
  "total": 41,
  "limit": 2,
  "offset": 0,
  "items": [
    {
      "time": "2026-10-01T05:34:06.888916+08:00",
      "actor": "admin01",
      "actor_kind": "user",
      "role": "admin",
      "action": "job.create",
      "method": "POST",
      "route": "/api/v1/jobs",
      "status": 201,
      "latency_us": 0,
      "verdict": "ok",
      "exec_verdict": "accepted",
      "handler_key": "exec.hello",
      "profile": "hello",
      "job_id": "01a0f43d-3428-7dfa-8fc0-4a93a626436f",
      "remote_ip": "127.0.0.1",
      "user_agent": "curl/8.17.0"
    },
    {
      "time": "2026-10-01T05:34:06.825442+08:00",
      "actor": "operator01",
      "actor_kind": "user",
      "role": "operator",
      "action": "job.create",
      "method": "POST",
      "route": "/api/v1/jobs",
      "status": 403,
      "latency_us": 0,
      "verdict": "denied",
      "exec_verdict": "role_denied",
      "exec_reason_code": "admin",
      "handler_key": "exec.hello",
      "profile": "hello",
      "remote_ip": "127.0.0.1",
      "user_agent": "curl/8.17.0"
    }
  ]
}
```

- **顺序是最新在前**（表内 `seq` 降序），与事件端点的升序相反：台账的用法是"刚发生了什么"。
  空结果是 `items: []` 而不是 `null`。`total` 是匹配过滤条件的总行数，`count` 是本次返回的行数。
- `route` 存的是 gin 的**路由模板**（`/api/v1/jobs/:id`），不是原始 URL——任务 ID 属于请求内容。
- `actor_kind` 是 `user`（控制台账号）、`machine`（静态 token）、`anonymous`（免鉴权部署）。
  静态 token 折算 operator 档，因此它读不到本端点（要 ops）。`actor` 对 `machine` 与匿名请求都是空串，
  区分它们看 `actor_kind`。
- `verdict` 由状态码派生：`ok`（2xx）、`bad_request`（400）、`denied`（401/403）、`not_found`（404）、
  `conflict`（409）、`partial`（207）、`throttled`（429）、`error`（其余 5xx）、`other`（兜底）。
  401 算 `denied`：一次失败的登录尝试正是要看的行。
  两个批量端点（`job.batch_create`/`job.batch_op`）的 `action` 行一律是 `partial`——
  批量端点即使全部成功也返回 207，逐条结果在响应体里，台账一行不表达"哪几条失败"。
- `action` 是封闭动作词，与"方法 + 路由模板"一一对应：`auth.login`、`auth.refresh`、`auth.logout`、
  `auth.ws_ticket`、`job.create`、`job.update`、`job.cancel`、`job.retry`、`job.pause`、`job.resume`、
  `job.force_pause`、`job.batch_create`、`job.batch_op`、`group.create`、`group.update`、`group.delete`、
  `admin.scheduler_suspend`、`admin.scheduler_unsuspend`、`admin.events_clear`。
  未匹配到路由的请求记 `unmatched`（404 的乱撞路径），匹配到路由但表里没配动作记 `other`——
  加了写路由忘了配表时台账不会静默少行。
- `exec_verdict`/`exec_reason_code`/`handler_key`/`profile` 只在执行器任务的提交期出现
  （`POST /jobs` 与 `PUT /jobs/:id`）。`exec_verdict` 是 `accepted`|`role_denied`|`profile_unavailable`|
  `payload_rejected`|`timeout_rejected`；`exec_reason_code` 只存结论码，其中 `role_denied` 那行存的是
  要求达到的档位名（如 `admin`）。**参数取值、请求体、校验错误原文都不进表**（设计文档 D7）：
  人类可读的说明继续走 slog 与响应体。
- `job_id` 只在提交动作里填（新建任务时由服务端生成），批量请求那一行的 `job_id` 留空。
- `latency_us` 是微秒整数。开发机上的单调时钟粒度可能粗到让快请求显示 0——它是量级参考，不是精确计时。

保留策略在配置里（`observability.audit.retention_count` 默认 50 万条、`retention_age` 默认 2160 小时），
本端点只读不删：给它一个清空按钮会让"谁删了台账"这件事没有出处。

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

档位来自配置的 `executors.commands` 与 `executors.profiles_path` 那份文件，两份都在进程启动时注册。
`executors.adhoc.enabled: true` 时，那四条内置档位（`exec.http`、`exec.php`、`exec.python`、`exec.shell`）
同样出现在这里；整节关闭时一条都不在，这个数组与名称/类型解耦之前逐字一致。
`executors.commands` 什么时候要重启取决于 `reload.enabled`：关闭时（默认）改完要重启进程；打开时改条目
自身的可改字段（超时、参数声明、条目增删）下一个防抖窗口就生效并在这里立刻出现，而改动条目内的执行许可
字段（`kind`/`runtime`/`script` 等十五项）会让整次重载被拒绝，那种改动连同 `executors.workspace`/
`runtime_allow`/`env_allow` 仍须重启（三档清单见[部署文档](./deployment.md) 的"配置热重载"）。
`executors.profiles_path` 那份在 `executors.web_enabled: true` 时可以用接口改，立即生效并在这里立刻出现
（见[档位的在线管理](#档位的在线管理)）。

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