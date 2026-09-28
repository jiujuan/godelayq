# Web 控制台：前端界面设计与后端实现方案

> 状态：设计稿（尚未实施）。
> 技术栈约定：Vue 3 + Vite + TypeScript + Pinia + TanStack Query + lucide 图标。
> 说明：用户要求中的 "typescript7" 按当前生态理解为 Vite 7 / TypeScript 5.x；
> "tank query" 按 TanStack Query（`@tanstack/vue-query` v5）理解。若需偏离请在评审时指出。

---

## 1. 背景与目标

当前仓库是一个 Go 延迟任务调度器（`core`）+ Gin HTTP API（`api`），`dashboard/index.html`
只是一个手写单页：连上 `/ws` 展示事件流，无任何操作能力。

目标是在 `web/` 下构建一个完整的 Vue 单页控制台，覆盖：

- 登录 / 退出：用户名 + 密码 + JWT，配套**角色权限体系**（决策 D1，见 §5.7）
- 概览 Dashboard：统计卡片 + 实时事件流（现有 dashboard 的全部能力）
- Job 全生命周期操作：创建、编辑、删除（取消）、**暂停 / 恢复**、
  **强制暂停（中止执行中的任务，限 admin/ops）**、手动重试
- 查看单个 job 的**运行情况**：状态、下次触发、重试链、最近执行事件历史（决策 D4）
- Job **分组管理**：组的增删改查 + 按组过滤与聚合统计（决策 D3）
- 运维页（ops 角色）：调度总开关 suspend/resume、事件缓冲清空、运行时诊断

非目标（本期不做）：

- 用户账号的在线增删改（账号在 `configs/config.yaml` 声明，改动需重启）
- 运行时调整 worker 数 / 队列容量（现有 `SetConcurrency`/`SetQueueCapacity`
  在 Start 后被忽略并告警，`core/scheduler.go:83-113`；需要 worker 池 resize 能力，列为二期）
- 运行历史的持久化审计（重启即清空，终态快照留痕仍由 `store.history_limit` 负责）
- 集群 / 多实例管理

---

## 2. 现状盘点（已核实的代码事实）

| 事实 | 位置 |
| --- | --- |
| REST 路由：jobs CRUD/cancel/retry/batch、stats、health、job-types | `api/server.go:102-124` |
| WS `/ws`、SSE `/sse/events` | `api/server.go:134-140`、`api/sse.go` |
| 鉴权：单一静态 token，`Authorization: Bearer` / `X-Auth-Token` / `?token=` 三通道 | `api/security.go:69-110` |
| 访问日志同时记录 `path` **与 `query`**（凭据写进 URL 会落日志的直接原因） | `api/logging.go:20-30` |
| SSE 已按 `event_types` 做**服务端类型级订阅**，未知类型返回 400 | `api/sse.go:13-33` |
| CORS：`server.cors.allow_origins`，预检在鉴权前短路（顺序：recovery→logger→CORS→auth） | `api/server.go:87-100`、`api/security.go:31-66` |
| 中间件顺序不可调换（预检必须先于鉴权答复） | `api/server.go:95-99` |
| Job 状态枚举 `pending(0)/running(1)/success(2)/failed(3)/cancelled(4)` | `core/job.go:12-18` |
| Job / JobSnapshot 字段（无 group、无 paused） | `core/job.go:54-86`、`core/job.go:132-149` |
| Store 接口（Save/Update/Delete/LoadAll/Flush/Close），仅 JSON 文件实现 | `core/store.go:25-37` |
| 调度循环：`PopIfDue` 到期即弹出投递 | `core/scheduler.go:426-475` |
| Cancel：堆内移除 + 取消执行中 + **删除存储记录** | `core/scheduler.go:216-244` |
| UpdatePending：原地更新，堆外返回 409 | `core/scheduler.go:253-296` |
| Restore：非终态快照全部重新入队并置为 pending | `core/scheduler.go:320-351` |
| Handler 注册表已收敛到 Scheduler（api 层 JobRegistry 已删除）：`LookupHandler`/`HandlerNames` | `core/scheduler.go:124-140`、`api/handlers.go:412-416` |
| Cron 重复任务执行成功后由 `handleSuccess` 重新排期 | `core/scheduler.go:661` 起 |
| 执行取消走 `handleInterrupted`，当前只记日志（存储清理由 Cancel 负责） | `core/scheduler.go:596-599`、`:631-639` |
| EventBus：`SubscribeAll` 返回 `<-chan Event` | `core/event.go:56-128` |
| WS 订阅过滤已支持 `job_types`/`event_types`/`job_ids`/**`status`**；发送缓冲 256 条 | `core/websocket.go:42-51`、`:158` |
| `POST /jobs/batch` 已实现：逐条独立、207 混合结果、单请求上限 100 | `api/handlers.go:419-474`、`:20-21` |
| ListJobs 只支持 `status/name/limit/offset` 过滤 | `api/handlers.go:108-175` |
| GET/重试/统计按 ID 或全量走 `store.LoadAll` 线性扫描 | `api/handlers.go:130,215,335,378` |
| 老 dashboard 是纯静态 HTML，靠 `?token=` 连 WS | `dashboard/index.html` |

---

## 3. 总体架构

```
┌────────────────────────────  浏览器  ────────────────────────────┐
│  Vue 3 SPA (web/)                                                │
│  ├─ Pinia: auth(access/refresh token, user+role) / realtime(ws)  │
│  ├─ TanStack Query: jobs / stats / groups / events 服务端缓存    │
│  └─ WS 客户端: /ws?ticket=…（一次性凭据）心跳 30s，事件→invalidate│
└──────────────┬───────────────────────────────┬───────────────────┘
               │ HTTP /api/v1/**               │ ws://…/ws
               ▼                               ▼
┌─────────────────────────────  godelayq-server  ─────────────────┐
│  api 层 (Gin)                                                    │
│  ├─ 既有: jobs/stats/health/job-types/ws/sse                     │
│  ├─ 新增: auth(login/refresh/logout/me/ws-ticket) + RBAC 中间件  │
│  ├─ 新增: pause/force-pause/resume、groups CRUD、jobs/:id/events │
│  ├─ 新增: admin(runtime 诊断 / scheduler suspend / 清缓冲)       │
│  ├─ 新增: EventHistory（SubscribeAll → 每 job 环形缓冲）         │
│  └─ 新增（可选）: go:embed web/dist 静态托管 + SPA fallback      │
│  core 层                                                         │
│  ├─ Job/JobSnapshot: +group 字段、+StatusPaused                  │
│  ├─ Scheduler: Pause/ForcePause/Resume、Suspend/Unsuspend        │
│  └─ GroupStore: 新 JSON 文件存储 data/groups.json                │
└──────────────────────────────────────────────────────────────────┘
```

数据流向：列表/统计以 REST 为准（TanStack Query 缓存），WS 事件只承担
"实时推送 + 触发缓存失效"，不做第二数据源，避免两套状态互相打架。

---

## 4. 前端设计（web/）

### 4.1 技术选型

| 关注点 | 选型 | 理由 |
| --- | --- | --- |
| 框架 | Vue 3（`<script setup>` + Composition API） | 用户指定 |
| 构建 | Vite 7 | 用户指定 |
| 语言 | TypeScript 5.x，`strict: true` | 用户指定 |
| 客户端状态 | Pinia 3（auth、ws 连接态、UI 偏好） | 用户指定 |
| 服务端状态 | `@tanstack/vue-query` v5 | 用户指定；天然覆盖列表分页、轮询、失效重取 |
| 路由 | vue-router 4，history 模式 | 需要登录守卫 |
| 图标 | `lucide-vue-next` | 用户指定；**禁止手写内联 SVG 或引入第二套图标库** |
| 样式 | Tailwind CSS 4 + 少量自定义 CSS 变量（主题 token） | 蓝白灰配色完全可控，不绑定重型组件库 |
| HTTP | 原生 fetch 封装（`web/src/api/client.ts`） | 只有 ~20 个端点，不需要 axios |
| Token 解析 | 不引库；角色/有效期由 `/auth/login` 响应直接给出 | 前端不验签，验签是服务端职责 |

### 4.2 目录结构

```
web/
├─ index.html
├─ vite.config.ts            # dev server 代理 /api、/ws、/sse → localhost:8080
├─ src/
│  ├─ main.ts
│  ├─ App.vue
│  ├─ router/index.ts        # 路由 + 登录守卫
│  ├─ api/
│  │  ├─ client.ts           # fetch 封装：注入 Bearer、401→刷新重试一次→跳登录、错误规范化
│  │  ├─ types.ts            # 与 api/dto.go 一一对齐的 TS 类型（手维护，评审时对照）
│  │  └─ auth.ts / jobs.ts / groups.ts / stats.ts / events.ts / admin.ts
│  ├─ stores/
│  │  ├─ auth.ts             # access/refresh token、user{name,role}、登录/刷新/登出、角色判断
│  │  └─ realtime.ts         # WS 生命周期（ticket 换连）、连接状态、事件环形缓冲(前端 200 条)
│  ├─ composables/
│  │  ├─ usePermission.ts    # can('job.force_pause') 等；档位比较逻辑与后端 Role.AtLeast 一致
│  │  ├─ useJobEvents.ts     # 订阅指定 job_id 的事件流
│  │  └─ useCountdown.ts     # next_run_in 的本地秒级倒数
│  ├─ components/
│  │  ├─ layout/ (Sidebar, Topbar, PageHeader)
│  │  ├─ ui/ (Button, Input, Select, Table, Modal, Drawer, Badge, Toast, EmptyState, Skeleton)
│  │  ├─ jobs/ (JobTable, JobStatusBadge, JobForm, JobFilterBar, CronPicker, PayloadEditor)
│  │  ├─ groups/ (GroupList, GroupForm)
│  │  └─ dashboard/ (StatCard, EventFeed)
│  ├─ views/
│  │  ├─ LoginView.vue
│  │  ├─ DashboardView.vue
│  │  ├─ JobsView.vue
│  │  ├─ JobDetailView.vue
│  │  ├─ GroupsView.vue
│  │  ├─ MonitorView.vue     # 全屏实时事件流（承接旧 dashboard）
│  │  ├─ AdminView.vue       # 运维：调度总开关/运行时诊断/清缓冲（仅 ops）
│  │  └─ SettingsView.vue    # 当前账号与角色、token 有效期、后端 health/version
│  └─ styles/tokens.css      # 颜色/圆角/间距 CSS 变量
```

### 4.3 布局与视觉规范

**双栏结构**：左栏固定导航（240px，可折叠到 64px 只留图标），右栏为内容区；
内容区顶部有 Topbar（面包屑 / 页面标题 / WS 连接状态徽标 / 用户菜单-退出）。

```
┌──────────┬────────────────────────────────────────┐
│ ▣ Logo   │  Topbar: 任务列表      ● 实时连接  张三▾ │
│ ──────── ├────────────────────────────────────────│
│ 📊 概览  │  筛选条: 状态 | 分组 | 名称 | +新建任务 │
│ 📋 任务  │ ┌────────────────────────────────────┐ │
│ 🗂 分组  │ │ 表格列表（分页 / 行内操作按钮）     │ │
│ 📡 实时  │ └────────────────────────────────────┘ │
│ 🛠 运维* │                                        │
│ ⚙ 设置  │  * 仅 ops 角色可见                      │
└──────────┴────────────────────────────────────────┘
```

配色 token（白为主、蓝为辅、灰打底）：

| Token | 值 | 用途 |
| --- | --- | --- |
| `--color-primary` | `#2563EB` | 主按钮、选中菜单、链接 |
| `--color-primary-hover` | `#1D4ED8` | hover |
| `--color-primary-soft` | `#EFF6FF` | 选中行/浅底 |
| `--color-bg` | `#FFFFFF` | 页面底色 |
| `--color-surface` | `#F9FAFB` | 表格斑马纹、卡片 |
| `--color-border` | `#E5E7EB` | 分隔线、输入框描边 |
| `--color-text` | `#111827` / `--color-text-muted` `#6B7280` | 正文 / 辅助文字 |

状态色（`JobStatusBadge` 唯一映射表，前后端一致）：

| status | 颜色 | lucide 图标 |
| --- | --- | --- |
| pending | 蓝 `#2563EB` | `Clock` |
| running | 琥珀 `#D97706` | `LoaderCircle`（spin） |
| success | 绿 `#16A34A` | `CheckCircle2` |
| failed | 红 `#DC2626` | `XCircle` |
| cancelled | 灰 `#9CA3AF` | `Ban` |
| paused（新增） | 靛 `#4F46E5` | `PauseCircle` |

### 4.4 认证与授权流（决策 D1：用户名+密码+JWT，见 §5.7）

1. **登录**：`/login` 输入账号+密码 → `POST /api/v1/auth/login` → 返回
   `{access_token, refresh_token, expires_at, user:{name, role}}`。
   `access_token`/`refresh_token` 存内存 Pinia + `sessionStorage`（不写 localStorage：
   减少 XSS 后的长期可用凭据；刷新页面丢失凭据时走 refresh 静默续期）。
   失败按后端返回区分提示：401 "账号或密码错误"（后端对"用户不存在"与"密码错"
   返回同一码同一文案，避免账号枚举）、423/429 "尝试过于频繁"。
2. **请求注入**：`client.ts` 统一加 `Authorization: Bearer <access_token>`；
   收到 401 且未过期过一次 → 用 refresh_token 调 `/api/v1/auth/refresh` 换新 access 并重放一次；
   refresh 也失败 → 清凭据跳 `/login`（"登录已过期，请重新登录"）。
3. **退出**：调 `POST /api/v1/auth/logout`（服务端吊销该 refresh token，并把当前
   access token 的 jti 加入拒绝表，§5.7.4），随后清本地态 + 断 WS + 跳 `/login`。
   所以退出是"真注销"，不是只清本地。
4. **路由守卫**：`router.beforeEach` 检查 auth store；受保护页未登录跳 `/login`；
   `meta.roles: ['admin','ops']` 的页面（运维页、强制暂停入口）角色不足跳首页并 toast。
   **前端隐藏只是体验，不是安全边界**——同一规则必须在服务端 RBAC 中间件上再判一次
   （§5.7.3），验收项覆盖越权请求。
5. **角色驱动 UI**：`usePermission()` composable 提供 `can('job.force_pause')` 之类判断，
   按钮/菜单据此渲染；`viewer` 下所有写操作按钮置灰 + 悬浮说明所需角色。
6. **实时通道凭据**：WS/SSE 需要带凭据但浏览器不能加请求头，为避免 JWT 进入访问日志，
   采用一次性 ticket（§5.7.5）：`POST /auth/ws-ticket` → `?ticket=`，ticket 5s 过期、用后即废。
   旧 `?token=` 通道仅对"机器凭据"生效，控制台一律走 ticket。

### 4.5 实时通道（realtime store）

- 单例 WS，登录后连接；连接前先 `POST /auth/ws-ticket` 取一次性 ticket（§5.7.5），
  避免 JWT 出现在 URL 与访问日志里；ticket 失效则重取，401 走刷新链路。
- 30s 发送 `{"action":"ping"}`（服务端 60s 空闲会断开，见 `docs/api.md` 协议说明）。
- 断线指数退避重连（1s→2s→4s→…上限 30s）；`onclose` 更新 Topbar 徽标。
- 收到的事件做两件事：
  1. 推入前端事件环形缓冲（最近 200 条，喂给 Dashboard/Monitor 的事件流）；
  2. 按 `job_id` 使相关 Query 失效：`jobs` 列表、`job/:id`、`stats`（debounce 500ms，
     避免高频事件把请求打爆）。
- Monitor 页沿用旧 dashboard 的订阅过滤（`job_types`/`event_types` 复选）。

### 4.6 页面详设

#### LoginView
账号 + 密码两个输入框，"登录控制台"按钮；错误内联提示（凭据错误 / 频率限制 /
服务不可达）。登录成功后把 `user{name, role}` 存 auth store，供菜单与按钮鉴权。
后端未配置任何账号且未配置 token 时显示"本服务未启用鉴权，直接进入"。

#### DashboardView（概览）
- 顶部 6 张 `StatCard`：pending / running / paused / completed / failed / heap_size+uptime。
  数据源 `GET /api/v1/stats`（新增 paused 后见 §5.4），Query `refetchInterval: 5s` + WS 事件即时失效。
- 下方两栏：左为分组健康度（每组 pending/failed 计数，来自 `GET /groups` 聚合字段）；
  右为实时事件流（前端缓冲，非持久）。

#### JobsView（任务列表，核心页）
- 筛选条：状态下拉（含 paused）、分组下拉、名称（=job type）下拉、关键字。
  前三个映射为 `?status=&group=&name=`（group 为新增参数）。
- 表格列：ID 短码、名称、分组、状态徽标、trigger_at + 本地倒数（`useCountdown` 显示
  `next_run_in`）、重试 `retry_count/max_retries`、cron、操作。
- 行操作（按状态 + 角色启用/禁用，与后端能力一致）：
  - pending：编辑（PUT）、暂停、取消
  - paused：恢复、取消
  - running：查看；**强制暂停**（`admin`/`ops`，需二次确认，见 §5.2 ForcePause；
    低角色看到的是置灰 + "需要 admin 角色"提示）
  - failed：重试（POST retry）、查看
  - success/cancelled：查看
- 行点击 → JobDetailView。多选列 + 批量条（批量取消/批量暂停/移入分组），
  依赖新端点 `POST /api/v1/jobs/batch-ops`（§5.5）。批量**创建**已有可用端点
  `POST /jobs/batch`（逐条独立、207 混合结果、单请求 ≤100，`api/handlers.go:419-474`），
  前端的"从模板批量导入"可直接复用。
- 新建任务 = 右侧 Drawer 中 `JobForm`：
  - name：下拉，数据来自 `GET /api/v1/job-types`；
  - 触发方式三选一 radio：相对延迟（`10m` 文本 + 快捷 chips）/ 绝对时间（datetime-local）/
    cron（表达式输入 + 6 字段说明气泡 + 校验，兼容 5 字段）；
  - payload：JSON 编辑器（语法校验、非法 JSON 禁止提交）；
  - 分组：下拉（可就地"新建分组"）；timeout / max_retries / retry_delay / is_repeat。

#### JobDetailView（查看 job 运行情况）
- 基本信息卡：全字段 + payload 只读 JSON 视图。
- **运行时间线**：`GET /api/v1/jobs/:id/events`（新增端点，§5.6）返回最近历史事件；
  同时把该 job 的 WS 实时事件 append 到同一时间线（按 timestamp 归并）。
  每条节点：图标 + 类型 + 时间 + metadata（duration_ms、attempt、error、timeout 标记）。
- 操作区：与列表行操作同一套规则。

#### GroupsView（分组管理）
- 左侧组列表（名称、颜色点、job 计数），右侧编辑区：名称、描述、颜色（从固定 8 色板选）、
  删除按钮（**仅 admin/ops 可见**，operator/viewer 置灰并提示所需角色）。
- 删除组时弹窗说明后果：**组内 job 不会被删除，只解除分组归入"未分组"**（决策 D5，§5.5）。
  admin/ops 请求不带 `strategy` 参数，后端默认按 detach 处理；弹窗只需勾选确认，
  无需输入 `?strategy=detach`。
- 未分组虚拟项：`group=` 空值过滤入口。

#### MonitorView（实时事件流）
即旧 dashboard 能力：WS 状态、事件类型复选过滤、事件卡片流（限 100 条 DOM）。

#### AdminView（运维，仅 ops 角色）
- **调度总开关**：suspend / resume 整个调度循环（维护窗口内不再弹出到期任务，
  执行中的任务不受影响），需要二次确认；状态在 Topbar 显示醒目横幅"scheduling suspended"。
  对应 §5.4 新端点与 §5.2 的 `Scheduler.Suspend/Resume`。
- **运行时诊断**（只读）：`GET /api/v1/admin/runtime` → worker 数、队列容量与占用、
  堆长度、in-flight、事件缓冲占用、启动时长。
- **清空事件缓冲**：`DELETE /api/v1/admin/events`（会让各详情页时间线从当前时刻重新开始）。
- 明确**不提供**运行时改 worker 数/队列容量与在线改账号（见 §1 非目标与 §5.9 装配说明）。

#### SettingsView
当前账号与角色展示、access token 剩余有效期、后端 health/version、
凭据与 CORS 的边界说明（引用 `docs/api.md`：WS/SSE 走 ticket 而非把凭据写进 URL）。

### 4.7 Query 键约定

| Key | Endpoint | 失效时机 |
| --- | --- | --- |
| `['stats']` | GET /stats | 任意 job.* 事件、5s 轮询兜底 |
| `['jobs', filters, page]` | GET /jobs | 变更类操作后、job.* 事件（debounce） |
| `['job', id]` | GET /jobs/:id | 同上按 id |
| `['job-events', id]` | GET /jobs/:id/events | 打开详情页拉一次；此后由 WS append，不轮询 |
| `['groups']` | GET /groups | 组 CRUD 后 |
| `['job-types']` | GET /job-types | 启动时拉一次，缓存 5min |

---

## 5. 后端修改方案

五项决策（已与用户确认）：

- **D1 登录**：用户名 + 密码 + JWT，`config.yaml` 声明账号与角色；四档角色
  `viewer < operator < admin < ops`。现有静态 token 降级为"机器凭据"保留（§5.7）。
- **D2 暂停**：core 真暂停——新增 `paused` 状态 + pause/resume API；
  **admin/ops 可强制暂停运行中的 job（中止当前 attempt，不计失败、不消耗重试）**（§5.2）。
- **D3 分组**：完整分组——Job 加 `group` 字段 + `/api/v1/groups` CRUD + 过滤 + 聚合。
- **D4 运行历史**：api 层内存环形缓冲 + `GET /jobs/:id/events`，不入库。
- **D5 删组策略**：operator 不可删组；admin/ops **删除组无需显式 `?strategy=detach`
  即默认解绑**，组内 job 归入未分组，**任何角色都不会级联删除 job**（§5.5）。

### 5.1 数据模型（core/job.go）

```go
const (
    StatusPending JobStatus = iota   // 0，保持现状
    StatusRunning                    // 1
    StatusSuccess                    // 2
    StatusFailed                     // 3
    StatusCancelled                  // 4
    StatusPaused                     // 5 ← 新增，追加在末尾
)
```

- **必须追加在末尾**：`JobSnapshot.Status` 以 int 持久化在既有 `data/jobs.json`
  （`core/job.go:132-149`），插入中间会错位历史数据。
- `String()`/`ParseJobStatus` 增加 `"paused"`（`core/job.go:21-46`）；`IsTerminal()` 不含 paused
  （`core/job.go:49-51`，paused 仍需可恢复，也不是终态留痕淘汰对象——见下方 trim 注意点）。
- `Job` 与 `JobSnapshot` 各加 `Group string`（json tag `group,omitempty`）；
  `ToSnapshot/FromSnapshot` 同步搬运。旧 jobs.json 无该字段，反序列化为空串 = 未分组，
  天然兼容。
- 事件类型补充：`EventJobPaused = "job.paused"`、`EventJobResumed = "job.resumed"`
  （常量表在 `core/event.go:13-24`）。
- **两个必须同步的白名单**（否则新事件在链路里被静默吞掉）：
  1. `api/sse.go:13-20` 的 `publishedEventTypes`——SSE 走类型级订阅，未列入的类型既不会被推送，
     `?event_types=job.paused` 还会被判 400。
  2. WS 侧无需改动：`filterMatches` 按 `event.Status.String()` 比对状态
     （`core/websocket.go:261-275`），`paused` 随 `JobStatus.String()` 自动可用；
     客户端因此可以直接用 `{"filter":{"status":["paused","failed"]}}` 订阅，前端 Monitor 页
     应暴露这个 status 过滤（旧 dashboard 只有事件类型过滤）。

### 5.2 调度器（core/scheduler.go）

新增四个方法。Pause/Resume 完全复用既有积木（堆移除 + 存储写回 + Schedule 重排），
但 ForcePause 必须动执行收尾路径（见下），Suspend 要动 `scheduleLoop` 顶部：

```go
// Pause 暂停：仅对堆内(pending)任务生效；从堆移除并以 paused 状态写回存储。
func (s *Scheduler) Pause(jobID string) (*Job, error)
// ForcePause 强制暂停：中止执行中的任务，不计失败、不消耗重试，停在 paused。
// 调用方需 admin/ops 角色（授权在 api 层，core 不感知角色）。
func (s *Scheduler) ForcePause(jobID string) error
// Resume 恢复：从存储读 paused 快照，重算触发时间后重新 Schedule（保留原 ID）。
func (s *Scheduler) Resume(jobID string) (*Job, error)
// Suspend/Unsuspend 进程内调度总开关：suspend 期间不弹出到期任务，堆与存储不动。
func (s *Scheduler) Suspend()
func (s *Scheduler) Unsuspend()
```

**Pause 语义**（与 Cancel/UpdatePending 对齐）：

1. `heap.Remove(jobID)`；拿不到 → 若在 `cancelMap`（执行中）返回 `ErrJobNotPending`（→409），
   否则 `ErrJobNotFound`（→404）。**运行中的 job 不允许暂停**——执行已被 worker 拿走，
   暂停它语义不明；v1 保持与 PUT 更新同样的边界（`core/scheduler.go:253-296` 先例）；
   `admin`/`ops` 走下面的 ForcePause。
2. 置 `Status=StatusPaused`、`UpdatedAt=now`，`store.Update(snapshot)`（不删除！
   这是与 `Cancel` 的本质差别，`core/scheduler.go:230-234` 是 Delete）。
3. 广播 `job.paused` 事件。
4. cron 重复任务：暂停即离开堆，`handleSuccess` 的重排路径（`core/scheduler.go:661` 起）
   不会碰到它——因为弹出发生在暂停前，此路径无需改动。

**Resume 语义**：

1. `store.LoadAll()` 找 ID（与 `hasStoredJob` 同款扫描，`core/scheduler.go:299-315`；
   当前规模可接受，见 §5.10 性能注记），非 paused 状态返回 409。
2. `FromSnapshot` 还原，绑定 Handler（`s.scheduler.LookupHandler(job.HandlerKey())`，同 RetryJob 做法，
   `api/handlers.go:359-361`；api 层已无 JobRegistry）。
3. 触发时间重算：cron 任务用 `cronParser.Next` 取未来时点；一次性任务若
   `TriggerAt` 已过期则"到期立即补跑"（与 Restore 的过期语义一致，`scheduler.go:
   313-315` 注释所述行为）。
4. `Schedule(job)`——ID 非空不会被重新生成（`core/scheduler.go:151-153`），堆、存储、
   事件广播全部复用现路径。

**ForcePause（强制暂停，限 admin/ops）**：

作用于**已出堆、正在执行**的任务，语义定为"中止当前 attempt，不计失败、不消耗重试，
停在 paused"：

1. `Scheduler.ForcePause(jobID)`：在 `s.mu` 下查 `cancelMap`（`core/scheduler.go:203-212`
   的 `CancelRunning` 已能拿到取消函数），命中则记录"该 ID 待强制暂停"到
   `forcedPause map[string]struct{}`（同 mutex 保护）并 `cancel()`；随后把快照以
   `StatusPaused` 写回存储并广播 `job.paused`（metadata `forced: true`）。
   堆里也有（不该发生，但竞态可能）→ 一并 `heap.Remove`。
2. Handler 收到 `ctx.Canceled` 后返回错误 → 走 `executeJob` 的取消分支
   （`core/scheduler.go:596-599`）→ `handleInterrupted`（`:631`）。
   **`handleInterrupted` 必须改为三分支**：当前实现是"running 时只记日志、交给 Cancel 清存储"
   （`:636-639`），那是取消语义；强制暂停要在此处识别 `forcedPause` 标记，
   把状态保持为 `StatusPaused` 落盘且不进 `handleFailure`。
3. **Handler 不检查 ctx 的情况**（`core/scheduler.go:555-556` 注释已承认这类处理器存在）：
   ctx 已取消但 Handler 返回 nil → 会走 `handleSuccess`（`:634` 起）并重排 cron 下一轮。
   因此 `handleSuccess`/`handleFailure` 开头都要先查 `forcedPause` 标记：命中则
   清标记、按 paused 落盘、跳过成功/失败/重试/重排逻辑，并额外广播一条
   `job.paused`（metadata `note: handler_ignored_cancellation`）。
   这条是强制暂停正确性的关键，必须有单测。
4. 副作用：被中止的 attempt 可能已产生部分副作用（本仓库整体是至少一次语义，
   `core/scheduler.go:628-630` 注释所述），文档与 UI 二次确认弹窗都要写明
   "当前执行会被中止，Handler 需保证幂等"。
5. UI：列表点强制暂停 → running 徽标变"暂停中"（本地态，等待 WS `job.paused`）；
   若 10s 内没收到 `job.paused`，提示"Handler 未响应取消，将在本轮执行结束后停在 paused"
   ——这正是第 3 点的可观测化。

**Restore 修正（关键正确性点）**：现有 Restore 把"所有非终态"快照重新入队并置为
pending（`core/scheduler.go:329-346`）。paused 也非终态，照旧逻辑会被**静默恢复**。必须加一行
`if status == StatusPaused { continue }`——重启不解除暂停。

**Cancel 修正**：paused 任务不在堆里也不在执行中，`Cancel` 会返回 `ErrJobNotFound`
（`core/scheduler.go:226-228`），导致暂停后的任务**删不掉**。需在 Cancel 中补一路：堆和执行
中都没有时，检查存储快照，若为 paused 则 `store.Delete` + 广播 `job.cancelled`。

**UpdatePending 修正**：语义保持"仅 pending 可改"；paused 走 409。前端表单以禁用态呈现，
不再发请求。

**调度总开关（ops 专用，供维护窗口）**：新增 `Scheduler.Suspend()` / `Scheduler.Unsuspend()`
+ `suspended atomic.Bool`。`scheduleLoop` 顶部（`core/scheduler.go:429-435` 的 select 之后）
判断 suspended 则等待唤醒信号，不弹出任何到期任务；已在 worker 里的任务照常跑完，
堆与存储都不动。注意这与 `Stop()` 完全不同（Stop 是优雅关停并落盘 pending），
不能复用；恢复时只需重新计算堆顶等待时长。suspend 期间 `POST /jobs` 仍可入队（
`Schedule` 不走 scheduleLoop），恢复后一并生效——文档需明示这一点，避免误以为
suspend 会屏蔽创建。

### 5.3 分组存储（core/group_store.go，新文件）

组是一等实体（可挂描述与颜色），不能从 job 列表反推，需要独立存储：

```go
type Group struct {
    Name        string    `json:"name"`        // 唯一键，同时就是 Job.Group 的取值
    Description string    `json:"description,omitempty"`
    Color       string    `json:"color,omitempty"` // "#2563EB" 等，前端色板八选一
    CreatedAt   time.Time `json:"created_at"`
    UpdatedAt   time.Time `json:"updated_at"`
}

type GroupStore interface {
    List() ([]Group, error)
    Get(name string) (Group, bool, error)
    Save(g Group) error
    Delete(name string) error
    Close() error
}
```

- 实现 `JSONFileGroupStore`：单文件 `data/groups.json`，复用 JSONFileStore 的
  合并写盘模式（`core/store.go:205-248` 的 flushLoop 可直接借用/抽取）。
- 配置：`StoreConfig` 增加 `GroupsPath string`（mapstructure `groups_path`，默认
  `./data/groups.json`），同步加入 `LoadConfig` 的环境变量绑定清单
  （`core/config.go:135-153`）与 `configs/config.yaml` 注释。**注意**：该仓库用
  `UnmarshalExact`，schema 不加键而 yaml 里写了会启动报错，需一起提交。
- 校验：组名 `[a-zA-Z0-9_-]{1,64}`（避免 URL/查询参数歧义），大小写不敏感唯一。
- Job 不强制属于已注册组：允许 `group=foo` 建任务而 foo 未建组（自动视同"未注册的
  临时组"，列表过滤仍可用）； GroupsView 提供"一键收编"（列出被 job 引用但未注册的组名）。

### 5.4 DTO 与 REST API（api 层）

**修改既有**：

| 端点 | 变化 |
| --- | --- |
| `POST /jobs` | `CreateJobRequest` 增加 `group` 字段（可选）；handler 赋给 `job.Group` |
| `GET /jobs` | 增加 `group` 查询参数；`status` 接受 `paused`（`ParseJobStatus` 自动支持）；空值参数：`group=`（精确取未分组）与省略（不过滤）要区分 |
| `PUT /jobs/:id` | `UpdateJobRequest` 增加 `group`（可把 job 移组） |
| `JobResponse` | 增加 `group`；`status` 枚举说明加 paused |
| `GET /stats` | `StatsResponse` 增加 `paused` 计数（扫描循环加一个 case，`api/handlers.go:386-394`） |

**新增**（"角色"列为最低可满足角色，服务端 RBAC 与前端 UI 同一张表，见 §5.7.3）：

```
POST   /api/v1/auth/login          公开         {username,password} → tokens + user
POST   /api/v1/auth/refresh        持有 RT      换新 access，RT 轮转
POST   /api/v1/auth/logout         任意已登录    吊销 RT + 拒绝当前 AT
GET    /api/v1/auth/me             任意已登录    {name, role, expires_at}
POST   /api/v1/auth/ws-ticket      任意已登录    一次性 ticket（供 WS/SSE，§5.7.5）

POST   /api/v1/jobs/:id/pause      operator     200 | 404 | 409(仅 pending 可暂停)
POST   /api/v1/jobs/:id/force-pause admin       200 | 404 | 409(不在执行中)  §5.2
POST   /api/v1/jobs/:id/resume     operator     200 | 404 | 409
GET    /api/v1/jobs/:id/events     viewer       {items:[Event]}              §5.6

GET    /api/v1/groups              viewer       [{name,description,color,job_count,paused_count}]
POST   /api/v1/groups              operator     201；重名 409
PUT    /api/v1/groups/:name        operator     重命名同步改写 job.Group
DELETE /api/v1/groups/:name        admin        204；组内有 job 时**默认 detach**（D5）；
                                                ?strategy=block 可要求先清空

POST   /api/v1/jobs/batch-ops      operator     {action: cancel|pause|resume|move, ids[], group?}
                                                逐条执行，返回 207 {succeeded, failed:[{id,reason}]}
                                                action=force-pause 需 admin

GET    /api/v1/admin/runtime       ops          worker/队列/堆/in-flight/缓冲占用
POST   /api/v1/admin/scheduler/suspend   ops    调度总开关（§5.2 Suspend）
POST   /api/v1/admin/scheduler/unsuspend ops
DELETE /api/v1/admin/events        ops          清空事件环形缓冲
# 写操作审计本期只输出结构化日志（§5.7.6）；/admin/audit 查询端点留二期
```

WS 侧无需协议改动：`job.paused`/`job.resumed` 会按类型透传，`filterMatches` 的状态过滤
按 `JobStatus.String()` 比对，paused 自动可用（`core/websocket.go:261-275`）。
**但 SSE 的 `publishedEventTypes` 白名单必须同步加这两类**（`api/sse.go:13-20`），
否则 SSE 客户端收不到暂停事件，见 §5.1。

**鉴权覆盖**：新端点全部挂在既有 `/api/v1` 组内。认证中间件本期由"静态 token 校验"升级为
"JWT 校验 + 角色注入"（§5.7.2-§5.7.3），两条顺序约束不能动：
CORS 预检必须早于认证（`api/server.go:95-99`）；
而 `POST /auth/login`、`POST /auth/refresh` 是**公开端点**，现有认证是 `engine.Use` 全局挂载
（`api/server.go:99`），因此要改成"公开路由组先注册 / 其余路由再挂认证"，
或在中间件里维护一份显式放行路径表——**不能用"路径前缀不是 /api 就放行"这类松散判断**，
否则漏保护一个写端点就是越权。验收项里包含"未认证调写端点 401"的逐端点用例（§7）。

### 5.5 分组的破坏性操作策略（写死在文档，避免实现时摇摆）

- **删除组**（决策 D5）：任何角色都**不会**级联删除或取消 job。
  `DELETE /groups/:name` 由 admin/ops 调用时**默认 detach**——组内 job 的 `Group` 置空后
  删除组记录，前端不需要传 `strategy`。要更保守可显式 `?strategy=block`，此时组非空返回 409。
  operator/viewer 调该端点直接 403（§5.4 角色列）。
- **重命名组**：`store.LoadAll()` 找出 `Group==old` 的快照逐个 `Update`。非原子，
  中途失败会在下次操作时呈现"部分 job 还挂在旧组"——UI 的"未注册临时组"入口能兜住，
  可接受（本仓库整体就是尽力落盘 + 崩溃靠 Restore 的哲学）。
- **batch-ops**：循环调用单个 Scheduler 方法即可，不做事务。含 `force-pause` 时逐条判角色
  （整批要求 admin 以上，简单一致）。批量**创建**已由 `POST /jobs/batch` 实现
  （逐条独立、207、≤100 条，`api/handlers.go:419-474`），本端点只做"对已有 job 的动作"，
  两者语义不要混用。

### 5.6 运行事件历史（api/history.go，新文件）

```go
// EventHistory 是 EventBus 的内存订阅者，按 job_id 保留最近 N 条事件。
type EventHistory struct {
    perJob  int            // 每 job 环形上限，常量 100
    maxJobs int            // LRU 上限，常量 2000 个 job_id
    global  []Event        // 全局最近 500 条，供 Dashboard 首屏回灌
    ... mu, map[string][]Event
}
func NewEventHistory(bus *core.EventBus) *EventHistory // 内部 bus.SubscribeAll() + drain goroutine
func (h *EventHistory) Events(jobID string, limit int) []Event
func (h *EventHistory) Recent(limit int) []Event
```

- 挂载点：`api.NewServer` 里创建（`api/server.go:40-78`），随 Server 一起 Stop。
  不新增配置项，上限先用常量，需要再开 knob（本仓库惯例是"未生效的选项不进配置"，
  见 `core/config.go:16-17` 注释）。
- 端点：`GET /api/v1/jobs/:id/events?limit=`；`GET /api/v1/events?limit=`（全局 recent，
  Dashboard 刷新后补历史用，可选实现）。
- **明确语义并写进前端 UI**：进程重启即清空；详情页时间线标题旁标注"内存缓冲，最近 100 条"。
  持久化审计不在本期范围。
- EventBus 的 `SubscribeAll` 通道容量/丢弃策略需在实现时核实（`core/event.go:105-122`）：
  若缓冲满丢消息，历史记录器丢的是"记录"而非调度事实，可接受，但要在 code review 时确认
  drain goroutine 不被阻塞反压整个总线。

### 5.7 认证与角色权限（决策 D1/D5 的后端部分）

现状：后端只有一个全局静态 token（`api/security.go:69-88`，恒定时间比较），
无身份概念。本期把它替换为"账号 + 密码 + JWT + 角色"的最小可用体系。

#### 5.7.1 凭据形态

- **JWT**（HS256，密钥来自配置 `server.auth.jwt.secret`，建议 32B 随机；
  `server.auth.jwt.access_ttl` 默认 15m、`refresh_ttl` 默认 12h）。
  选 HS256 而非 RS256：单进程自签自验，无需发布公钥；若将来多实例共享密钥即可。
  库用 `github.com/golang-jwt/jwt/v5`（新增依赖）。
- **Access Token** 只放 claims：`sub`(账号名)、`role`、`iat`、`exp`、`jti`。
  前端不解析角色，`GET /auth/me` 才是权威来源（避免 claim 与配置漂移）。
- **Refresh Token**：不透明随机串（`crypto/rand` 32B），**服务端存 map[rt]→{account, role, exp}**
  （内存 + 不落盘）。刷新时轮转（旧 rt 立即失效），防止被重放。
  服务重启后 RT 全失效，用户需重新登录——可接受，不额外持久化密钥材料。

#### 5.7.2 账号来源与哈希

账号定义在配置文件（本期不做在线增删改）：

```yaml
server:
  auth:
    # 静态 token 保留为机器凭据（兼容脚本/旧 dashboard），仅具备 machine 角色
    token: ""
    jwt:
      secret: ""          # 或用环境变量 GODELAYQ_SERVER_AUTH_JWT_SECRET 注入
      access_ttl: 15m
      refresh_ttl: 12h
    users:
      - name: ops01
        password_bcrypt: "$2a$10$..."   # 明文密码禁止入配置/入仓
        role: ops
      - name: dev01
        password_bcrypt: "$2a$10$..."
        role: operator
```

- 密码只存 **bcrypt** 哈希（cost 10；`golang.org/x/crypto/bcrypt` 已在 indirect 依赖中，
  会转为直接依赖）。提供 `cmd/hashpassword` 小工具生成哈希，避免用户手抄。
- `role` 取值 `viewer|operator|admin|ops`，`core.Config.Validate` 校验非法值启动即报错
  （延续"未生效/非法配置早失败"的既有惯例，`core/config.go:176-235`）。
- `users` 非空但 `jwt.secret` 为空 → 启动报错；`users` 为空且 `token` 为空 → 不启用鉴权
  （等价现状，登录页显示"未启用鉴权"）。
- 新键必须同步加入 `LoadConfig` 的环境变量绑定清单（`core/config.go:135-153`），
  但 **`users` 列表不绑定环境变量**（viper 的逗号分隔 hook 处理不了嵌套结构，
  明确写进注释避免踩坑）；secret 单独绑 `GODELAYQ_SERVER_AUTH_JWT_SECRET`。

#### 5.7.3 校验与授权分层

已实现的做法（`api/security.go`）：

- 认证顺序：`Authorization: Bearer` → `X-Auth-Token` → `?ticket=`（仅 `/ws`、`/sse/`）
  → `?token=`（仅静态机器凭据）。出现 `Authorization` 头就只认它，非 Bearer scheme 直接 401，
  不回退到 query（与历史行为一致，`api/security_test.go` 有对应用例）。
- JWT 与静态 token 在同一条头通道上共存：先按 JWT 验签（配了账号才有认证器），
  失败再对静态 token 做恒定时间比较，命中即 `RoleMachine`。
- **JWT 不接受走 query**：访问日志会把 `path` 与 `query` 原样记下（`api/logging.go:20-30`），
  长令牌进日志等于泄露；实时通道改用 §5.7.5 的 ticket。
- 认证器每次验签都会用**当前配置**复核 `sub` 与 `role`：账号被删除或降权后，
  旧令牌立即失效，不必等 TTL。
- 公开端点用**精确集合**放行（`publicAuthEndpoints`：`POST /api/v1/auth/login`、
  `POST /api/v1/auth/refresh`），不用前缀匹配——将来在 `/auth` 下新增任何端点
  都不会被静默免鉴权。`/api/v1/health` 保持"启用鉴权则需凭据"的历史契约
  （控制台登录页靠它的 401/200 判断鉴权是否开启）。
- 授权：`RequireRole(min)` 做档位比较（`viewer < operator < admin < ops`，`Role.AtLeast`）。
  machine 的等级刻意等同 operator，因此 `RequireRole(RoleAdmin)` 对它天然为否——
  强制暂停、删组、运维端点都要求 admin/ops，脚本凭据自动被排除，
  **不需要额外的能力谓词**（实现 M0 时先写了 `CanForcePause()` 之类，随即发现是阶梯比较的重复表达，删掉了）。
  只有当某个能力真的需要"跨档位"（例如给某账号单独开例外）时，再引入 `RequireCapability`。
- **未启用任何凭据时全部放行**（含写端点），这是历史语义的延续；`RequireRole` 在这一模式下
  直接 `c.Next()`，否则既有单用户部署升级后会直接失能。
- 认证器构造失败（哈希格式、cost、缺密钥）**必须挡住启动**：`NewServer` 记录 `authErr`，
  `Start()` 返回该错误（见 §5.9），避免出现"鉴权看似开启、实际所有登录都失败"的半死状态。
- **前端隐藏不是安全边界**：`usePermission()` 只决定渲染，真正的拒绝来自服务端 403。

权限矩阵（唯一事实来源，路由表 §5.4 的"角色"列与此同源，实现时以服务端为准）：

| 能力 | viewer | operator | admin | ops | machine(静态 token) |
| --- | :-: | :-: | :-: | :-: | :-: |
| 列表/详情/stats/events/job-types/groups 查询 | ✓ | ✓ | ✓ | ✓ | ✓ |
| WS / SSE 实时（含 ticket 申领） | ✓ | ✓ | ✓ | ✓ | ✓ |
| 创建 / 编辑 / 取消 / 重试 job | ✗ | ✓ | ✓ | ✓ | ✓ |
| 暂停 / 恢复（pending） | ✗ | ✓ | ✓ | ✓ | ✓ |
| **强制暂停（中止执行中）** | ✗ | ✗ | ✓ | ✓ | ✗ |
| 建组 / 改名 / 移组 | ✗ | ✓ | ✓ | ✓ | ✓ |
| **删除组（默认 detach）** | ✗ | ✗ | ✓ | ✓ | ✗ |
| 批量操作（cancel/pause/resume/move） | ✗ | ✓ | ✓ | ✓ | ✓ |
| **调度 suspend/unsuspend、清事件缓冲、运行时诊断** | ✗ | ✗ | ✗ | ✓ | ✗ |

`machine`（静态 token）能调 REST 写端点，但不可进 `/auth/*`、不可 force-pause、不可删组、不可运维。

#### 5.7.4 退出与吊销

- JWT 无状态，"退出后 AT 仍在 TTL 内有效"是常规弱点。用**jti 内存拒绝表**补齐：
  logout 把 `jti` 记入集合（TTL 取 AT 剩余寿命，到期自动清理），中间件命中即 401。
- 集合规模上界 = 已登录会话数，无风险；重启后清空（可接受，重启本就使 RT 失效）。

#### 5.7.5 WS / SSE 一次性 ticket

浏览器 `WebSocket`/`EventSource` 无法加请求头，把 JWT 塞 query 会进访问日志
（`api/logging.go:20-30` 会把 `path` 与 `query` 一起写进日志）。改法：

1. `POST /api/v1/auth/ws-ticket`（带 AT）→ 返回 16B 随机 ticket，内存登记 `{principal, exp=5s}`。
2. `GET /ws?ticket=...` / `GET /sse/events?ticket=...`：握手时校验并**立即消费**（一次性），
   随后按同一 principal 走角色检查（WS 订阅/运维事件推送要求 `viewer` 以上）。
3. 现有 `?token=` 静态 token 通道保持可用（脚本兼容），但只认机器凭据；控制台不再使用。
4. ticket 泄漏窗口 5s 且不可复用，比长 JWT 进日志安全得多。实现落在
   `api/authstore.go`（refresh 表 / jti 拒绝表 / ticket 表共用一个带 mutex 的结构），
   未单独建 `api/ticket.go`。
5. **ticket 只用于 `/ws` 与 `/sse/` 前缀**（`ticketPathPrefixes`）：它表达的是"一次连接"的授权，
   拿它当可复用的 REST 授权头用会绕开一次一用的设计意图，因此 REST 上一律 401。

#### 5.7.6 审计日志

`api` 层对**写操作**（POST/PUT/DELETE，含 pause/force-pause/groups/admin/*）记录一行
结构化日志：`who(name) role= action= method path status= latency=`。落现有 logger
（`core/scheduler.go` 已用 slog 风格），不引入新存储。持久化后可查询属二期。

---

#### 5.7.7 登录限流与防枚举

- bcrypt 校验是 CPU 密集的（cost 10 约 60-100ms），必须在登录端点限流。实现（`api/ratelimit.go`）
  用**两条阈值线**而不是单一组合键，1 分钟滑动窗口、超限返回 429 + `Retry-After`：
  - 同一来源 + 同一账号：5 次失败即锁定——精准打击"按字典爆破某个账号"；
  - 同一来源（任意账号）：20 次失败才锁——兜住"一个密码试一堆账号"的广撒网，
    又不至于让同一 NAT 后的同事互相拖累，也不给攻击者"把整个办公室锁在门外"的杠杆。
  登录成功只清"来源+账号"这一维，IP 维度保留：一次成功登录抹不掉该来源的爆破历史。
- 配置里的 bcrypt cost 必须落在 10-15，低于 10 启动即拒绝（`api/authenticator.go`）：
  便宜哈希离线爆破只要几秒，写成常态等于没防护。
- 用户不存在与密码错误返回**完全相同**的 401 与响应体，不透露账号是否存在。
- 中间件把 `c.ClientIP()` 作为限流键时注意 `docs/deployment.md` 里的反向代理场景
  （X-Forwarded-For 可信与否决定限流是否可被绕过）。

### 5.8 静态托管与部署形态

两种形态，建议都做、默认走 A：

**A. 开发形态（默认）**：`vite dev` 起 5173，`vite.config.ts` 代理
`/api → http://localhost:8080`、`/ws → ws://localhost:8080`（ws:true）、`/sse → 8080`。
后端 CORS 保持默认 `*` 即可本地联调；生产建议按 `docs/api.md` 收紧为显式白名单。

**B. 单二进制发布**：新建 `web/embed.go`：

```go
//go:embed all:dist
var dist embed.FS
```

`api` 层注册静态路由：`engine.NoRoute` 兜底——GET 且 Accept 含 text/html 时返回
`dist/index.html`（SPA history fallback），其余路径保持现有 JSON 404（`api/server.go:126-131`）。
资源走 `http.FS` + `/assets` 前缀。用构建 tag（`//go:build dashboard`）区分是否嵌入，
避免没跑前端构建时 dist 为空目录导致 `go build` 失败。

前端产物目录 `web/dist` 提交与否二选一，倾向 `.gitignore` 排除、CI/发布时构建。

### 5.9 装配点与依赖清单

- **认证不需要改构造签名**（M0 已实现）：`Security` 直接携带 `core.AuthConfig`，
  `NewServer` 内部构造认证器与凭据存储（`api/server.go:40-90`）。
  认证器构造失败（坏哈希、缺密钥）存进 `Server.authErr`，由 `Start()` 返回——
  既守住"配置坏掉就别上线"，又不用把 `error` 塞进已被十几处测试使用的构造函数。
- **groupStore 仍然要改签名**（M1）：`api.NewServer` 再加一个依赖时，
  按本仓库既有风格走可选参数 `opts ...Option`（对齐 `core.NewScheduler(..., opts ...Option)`，
  `core/scheduler.go:52`），用 `WithGroupStore` 承接，避免位置参数列表继续变长。
- **`Restore` 在 `Start()` 内部执行**（`core/scheduler.go:376`），不在 main 里；
  因此 §5.2 的 paused 跳过必须在 `Restore` 本体改，别指望调用方过滤。
- **`Start→Stop→Start` 是被支持的**（`core/scheduler.go:361-366` 显式复位 `stopCh`），
  所以 `Suspend` 标志要定义为"进程内、重启即解除"，并在 `Start()` 里清零；
  ops 的 suspend 不跨重启生效，UI 需说明。
- **Handler 注册只有一个入口**：`Server.RegisterJobHandler` 直接转发到
  `scheduler.RegisterHandler`（`api/server.go:81-85`、`cmd/server/main.go:161-166`），
  api 层已无 JobRegistry；控制台展示 job 类型走 `scheduler.HandlerNames()`
  （`api/handlers.go:412-416`，已按字典序）。
- **Go 依赖**（M0 已加）：`github.com/golang-jwt/jwt/v5 v5.3.1`（JWT）、
  `golang.org/x/crypto v0.41.0`（bcrypt，由 indirect 转为直接依赖）。
- **已落地文件（M0）**：`core/auth.go`（角色阶梯与档位比较）、`api/authenticator.go`（账号校验 + JWT 签发/验签）、
  `api/authstore.go`（refresh 表 + jti 拒绝表 + 一次性 ticket）、`api/ratelimit.go`（登录限流）、
  `api/handlers_auth.go`（五个 `/auth/*` 端点）、`cmd/hashpassword/main.go`（生成哈希）。
  未单独建 `api/rbac.go`/`api/ticket.go`：角色中间件与 ticket 分别归入
  `api/security.go` 与 `api/authstore.go`，避免为一个函数开一个文件。
- **待新增（M1/M2）**：`core/group_store.go`（分组存储）、`api/history.go`（事件环形缓冲）。

### 5.10 兼容性与性能注记

- **旧数据**：无 `group` 字段的 jobs.json、状态 int 0-4 的快照全部兼容（新状态取 5）。
- **LoadAll 线性扫描**：ListJobs/GetJob/RetryJob/分组重命名都在扫全量快照
  （`api/handlers.go:130,215,335,378`）。当前 history_limit=1000 量级无碍；Store 增加
  `Get(jobID)` 是合理的后续重构，**不阻塞本期**。
- **paused 与 trim**：`trimTerminalLocked` 只淘汰终态（`core/store.go:161-189`），
  paused 非终态，不会被历史淘汰策略误删。✅（实现时加回归测试）
- **前端轮询兜底**：WS 断线期间 stats 仍有 5s refetchInterval，页面不至于假死。

## 6. 实施计划

按可独立验收的里程碑切分（每个里程碑编译 + 测试绿再进下一个）：

| # | 里程碑 | 内容 | 涉及 |
| --- | --- | --- | --- |
| M0 | 认证与角色 **已交付** | JWT 签发/校验、bcrypt 账号配置、`RequireRole`（machine 由阶梯天然排除）、RT 表 + jti 拒绝表 + ticket、`/auth/*` 五个端点、登录限流、写操作进访问日志（who/role）；httptest 覆盖三档角色越权、轮转、登出即失效、ticket 一次一用 | `core/auth.go`、`core/config.go`、`api/auth*.go`、`api/ratelimit.go`、`api/security.go`、`cmd/hashpassword` |
| M1 | core：paused + group | 状态枚举/Job/Snapshot/事件常量；`Scheduler.Pause/ForcePause/Resume`、`Suspend/Unsuspend`；`handleInterrupted`/`handleSuccess`/`handleFailure` 的强制暂停分支；Restore/Cancel 修正；GroupStore 与配置键；单测（旧数据兼容、重启不解除暂停、暂停后可取消、Handler 忽略取消仍停在 paused） | `core/*.go`、`configs/config.yaml` |
| M2 | api：REST | pause/force-pause/resume/groups/batch-ops/events/admin 端点 + DTO 扩展 + stats.paused；EventHistory 记录器 | `api/*.go` |
| M3 | web 骨架 | Vite+TS+Tailwind+lucide 初始化、client(含刷新链路)/auth/permission/router/layout、登录流、WS store + Query 失效管线 | `web/` |
| M4 | web 页面 | Jobs 列表+表单+详情（时间线）、Groups、Dashboard、Monitor、Admin、Settings | `web/src/views/*` |
| M5 | 集成与发布 | vite proxy 联调、embed 单二进制、`docs/api.md` 与 README/deployment 更新（鉴权章节重写）、（可选）旧 dashboard/index.html 改为跳转页 | `docs/`、`api/server.go` |

后端 M0+M1+M2 约 6-8 天（M0 的 JWT/RBAC 比原 token 方案多约 2 天），
前端 M3+M4 约 1.5-2 周。M0 与 M1 **都要改 `core/config.go` 的环境变量键清单**（`core/config.go:135-153`），并行开发时在合并阶段协调这一处即可；M3 可在 M0 端点契约
（§5.4 + §5.7）冻结后启动。

## 7. 验收清单

- [ ] 未认证访问任意业务端点 → 401；`viewer` 调写端点 → 403；`operator` 调强制暂停/删组 → 403
- [ ] 错误密码与不存在的账号返回同一状态码与文案（不可枚举账号）；连续失败触发限流
- [ ] access token 过期后前端静默刷新并重放成功；logout 后旧 access token 立即 401（jti 拒绝表）、
      refresh token 立即失效
- [ ] WS/SSE 通过一次性 ticket 建连；ticket 复用第二次被拒；URL 与访问日志中不出现长期凭据
- [ ] pending job 暂停 → 列表显示 paused、不再触发、重启后仍 paused；恢复后 cron 任务按新周期排期，一次性过期任务立即补跑
- [ ] running job 强制暂停 → 当前 attempt 被中止、不计入 `retry_count`、状态停在 paused、
      不产生 `job.failed`；**Handler 不检查 ctx 时仍在成功/失败收尾后停在 paused**（§5.2 第 3 点）
- [ ] paused job 可取消（Cancel 修正生效）
- [ ] admin/ops `DELETE /groups/:name` 不带参数即删除，组内 job 归入未分组、无一被删除；
      `?strategy=block` 时组非空返回 409
- [ ] ops 可 suspend 调度：暂停期间到期任务不弹出，堆与存储不变；unsuspend 后按原时间补跑
- [ ] 详情页时间线展示最近事件（含 paused/force-paused/resumed/retrying/failed 与 timeout 标记）
- [ ] WS 事件触发列表/统计自动刷新；断线显示黄色徽标并自动重连
- [ ] 旧 `data/jobs.json`（无 group、状态 0-4）直接升级运行无报错；配置中写非法 role 或
      缺 jwt.secret 时启动即报错
- [ ] `go build ./... && go vet ./... && go test ./...` 全绿（含 -race）

## 8. 风险与后续演进

1. ~~运行中 job 不可暂停~~（**已确认并放宽**：operator 及以下仍不可暂停 running，
   `admin`/`ops` 通过 `force-pause` 中止当前 attempt；语义与竞态处理见 §5.2）。
   遗留：需要"跑完这一轮就不再排期"的温和 cron 暂停时，再补 `graceful` 参数。
2. ~~删除组的解绑策略~~（**已确认**：admin/ops 不带参数即默认 detach，任何角色都不级联删 job；
   更保守可显式 `?strategy=block`，见 §5.5）。
3. **强制暂停依赖 Handler 检查 ctx**：本仓库一贯承认"不检查 ctx 的 Handler 无法被中断"
   （`core/scheduler.go:555-556`）。方案用"执行收尾处兜底停在 paused"覆盖这一情形
   （§5.2 第 3 点），代价是中止要等到 Handler 自己返回——UI 必须把这段时间显示为
   "暂停中"而不是"已暂停"，否则用户会重复点击。
4. **JWT 的即时吊销是近似的**：access token 有效期内 logout 靠 jti 内存拒绝表兜住
   （§5.7.4），但进程重启会清空拒绝表、refresh token 表也一并丢失（所有人都要重新登录）。
   这是"无独立会话存储"的固有取舍，需在 `docs/deployment.md` 写清。
5. **账号写在配置文件里**：加/停用户要重启进程；无密码策略、无锁定、无自助改密。
   在线用户管理（`data/users.json` + bcrypt 重写）是明确的二期。
6. **密钥轮换**：`server.auth.jwt.secret` 一改，所有已发 token 立刻失效；生产必须走
   `GODELAYQ_SERVER_AUTH_JWT_SECRET`，禁止把明文密钥提交进 `configs/config.yaml`
   （该文件已入库，需在注释里显式警告）。
7. **EventHistory 内存上限**：2000 job × 100 条 Event 粗估 <10MB，可接受；如需上限
   可调再开配置。
8. **Query 失效风暴**：高频事件下 debounce 策略（500ms 合并）需在实现时压测一次。
9. 前端不引入重型 UI 库；若评审倾向 Element Plus，仅影响 §4.1/§4.3 落地方式，页面
   信息架构不变。
