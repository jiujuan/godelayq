# Web 控制台：前端界面设计与后端实现方案

> 状态：设计定稿。**M0（认证与角色）、M1（core 的暂停/分组模型与调度器改动）已实施**，
> 实现与本文件的偏离已在对应小节就地标注；M2 起（HTTP 端点、运维端点、前端）尚未开工。
> 引用约定：本文件按**符号**锚定代码（如 `core/scheduler.go` 的 `Restore`），不写行号——
> 行号每加一个里程碑就会漂移，符号不会。
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
  在 Start 后被忽略并告警，`core/scheduler.go 的 SetConcurrency/SetQueueCapacity`；需要 worker 池 resize 能力，列为二期）
- 运行历史的持久化审计：**本文写作时不做，现已由观测层落地**——事件持久化在 `TASK-S03`/`TASK-S04`
  （`job_events`，重启后时间线仍有历史），写操作台账在 `TASK-S06`（`write_audit` + `GET /admin/audit`）。
  两者都是 `observability.enabled` 打开后可选，默认仍只有内存缓冲与结构化日志；
  终态快照留痕仍由 `store.history_limit` 负责（三处保留的是不同东西，见 `sqlite-observability-design.md` D9）
- 集群 / 多实例管理

---

## 2. 现状盘点（改动前的基线，已核实）

下表记录规划时的代码事实。**M0/M1/M2 已把其中若干条改掉**，被改动的行以 ⚠️ 标注并写明去向，避免读者把基线当成现状。

| 事实 | 位置 |
| --- | --- |
| REST 路由：jobs CRUD/cancel/retry/batch、stats、health、job-types | `api/server.go 的 setupRoutes` |
| WS `/ws`、SSE `/sse/events` | `api/server.go 的 setupRoutes（ws/sse 注册）`、`api/sse.go` |
| ⚠️ 鉴权原本只有单一静态 token → **M0 已改为账号 + JWT + 四档角色**，token 降级为 `machine` 机器凭据；query 通道新增一次一用的 `?ticket=` | `api/security.go` 的 `authMiddleware`，见 §5.7 |
| 访问日志同时记录 `path` **与 `query`**（凭据写进 URL 会落日志的直接原因） | `api/logging.go 的 requestLogger` |
| SSE 按 `event_types` 做**服务端类型级订阅**，未知类型返回 400（因此新事件类型必须同步白名单，**M1 已补 paused/resumed**） | `api/sse.go` 的 `publishedEventTypes`，见 §5.1 |
| CORS：`server.cors.allow_origins`，预检在鉴权前短路（顺序：recovery→logger→CORS→auth） | `api/server.go 的 setupMiddleware`、`api/security.go 的 corsMiddleware` |
| 中间件顺序不可调换（预检必须先于鉴权答复） | `api/server.go 的 setupMiddleware（顺序不可调换）` |
| ⚠️ Job 状态枚举原为 `pending(0)…cancelled(4)` → **M1 追加 `paused(5)`**（追加在末尾，避免与已落盘的 int 错位） | `core/job.go` 的 `JobStatus` 常量块，见 §5.1 |
| ⚠️ Job / JobSnapshot 原本无分组 → **M1 各加 `Group`**，并在 `ToSnapshot`/`FromSnapshot`/`CloneForRetry`/Cron 重排四处搬运 | `core/job.go` 的 `Job`、`JobSnapshot`，见 §5.1 |
| Store 接口（Save/Update/Delete/LoadAll/Flush/Close），仅 JSON 文件实现 | `core/store.go 的 Store 接口` |
| 调度循环：`PopIfDue` 到期即弹出投递 | `core/scheduler.go 的 scheduleLoop` |
| Cancel：堆内移除 + 取消执行中 + **删除存储记录** | `core/scheduler.go 的 Cancel` |
| UpdatePending：原地更新，堆外返回 409 | `core/scheduler.go 的 UpdatePending` |
| ⚠️ Restore 原本把非终态快照全部入队 → **M1 起跳过 paused**（重启不解除暂停） | `core/scheduler.go` 的 `Restore`，见 §5.2 |
| Handler 注册表已收敛到 Scheduler（api 层 JobRegistry 已删除）：`LookupHandler`/`HandlerNames` | `core/scheduler.go 的 LookupHandler/HandlerNames`、`api/handlers.go 的 ListJobTypes` |
| Cron 重复任务执行成功后由 `handleSuccess` 重新排期 | `core/scheduler.go 的 handleSuccess` 起 |
| ⚠️ 执行取消走 `handleInterrupted`，基线只记日志（存储清理由 Cancel 负责）→ **M1 起三条收尾路径都先认领强制暂停标记**，把任务钉在 paused | `core/scheduler.go` 的 `executeJob`（取消分支）与 `handleInterrupted`，见 §5.2 |
| EventBus：`SubscribeAll` 返回 `<-chan Event` | `core/event.go 的 EventBus` |
| WS 订阅过滤已支持 `job_types`/`event_types`/`job_ids`/**`status`**；发送缓冲 256 条 | `core/websocket.go` 的 `WSFilter` 与发送缓冲 |
| `POST /jobs/batch` 已实现：逐条独立、207 混合结果、单请求上限 100 | `api/handlers.go` 的 `BatchCreateJobs` 与 `maxBatchCreateSize` |
| ⚠️ ListJobs 基线只支持 `status/name/limit/offset` → **M2 已加 `group` 过滤**（省略=不筛，`group=`=只看未分组） | `api/handlers.go` 的 `ListJobs`、`parseGroupFilter` |
| GET/重试/统计按 ID 或全量走 `store.LoadAll` 线性扫描 | `api/handlers.go 里多处 store.LoadAll` |
| 老 dashboard 是纯静态 HTML，靠 `?token=` 连 WS | `dashboard/index.html`（M5 已改为跳转页，能力落在控制台实时页） |

M0/M1/M2 之后新增的事实（不属于基线）：`core/auth.go` 的角色阶梯与 `machine` 例外档、
`core/group_store.go` 的 `GroupStore`、`api/authenticator.go`/`api/authstore.go`/`api/ratelimit.go` 三件套、
`core/scheduler.go` 的 `Pause`/`ForcePause`/`Resume`/`Suspend`/`Unsuspend`/`RuntimeStats`/`SetGroup`/`RetagGroup`、
`api/history.go` 的事件内存缓冲、`api/server.go` 的 `Option`/`WithGroupStore`，
以及 §5.4 列出的全部 M2 端点（生命周期、分组、事件、运维）。
下表 §2 的"基线"仍是规划时的事实，读它时注意 ⚠️ 行已被改掉。

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

M3 交的是这张表的骨架层，M4 把页面层补齐：`api/` 现在有 types/client/auth/keys/stats
与 jobs、groups、events、admin 四个调用模块，composables 有 usePermission、useCountdown、
useJobEvents、useEventFeed，`ui/` 补上 Select/Drawer/Confirm，
并按页面长出 `jobs/`（状态徽标、筛选条、表格、表单、Cron、Payload、时间线）与 `groups/`（列表、表单）。
另有 `api/keys.ts`（Query 键集中一处，失效管线与页面共用）、`display.ts`（短 ID 与时间格式化）与 `plugins/`
（`query.ts` 装 QueryClient、`realtime-effects.ts` 把事件翻译成失效请求），
目的是让 realtime store 不认识 QueryClient。
Select 之外的 Table/Modal/Drawer 类通用组件没有预先抽出来：表格与抽屉的形状由各页自己长，
在没有第三个使用者之前抽出来只会变成一层需要维护的间接。

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
4. **路由守卫**：`router.beforeEach` 检查 auth store；受保护页未登录跳 `/login?redirect=<原路径>`；
   受控页面用 `meta.minimumRole: 'ops'`（运维页）这类**单一档位**声明，角色不足跳首页并 toast。
   实现与本文原稿的 `meta.roles: ['admin','ops']` 列表写法不同：后端本就是一条阶梯
   （`core.Role.AtLeast`），列档位集合只是在重复阶梯已经说过的事。
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
  **未启用鉴权的部署同样走 ticket**：那时中间件给请求挂上匿名 ops 主体，
  `/auth/ws-ticket` 返回 200 + 匿名票据（实测），因此前端没有"匿名直连"分支，
  建连只有三种结局——拿到票据就连、401 判定离线、其余错误按退避重连。
- 30s 发送 `{"action":"ping"}`（服务端 60s 空闲会断开，见 `docs/api.md` 协议说明）。
- 断线指数退避重连（1s→2s→4s→…上限 30s）；`onclose` 更新 Topbar 徽标。
- 收到的事件做两件事：
  1. 推入前端事件环形缓冲（最近 200 条，喂给 Dashboard/Monitor 的事件流）；
  2. 按 `job_id` 使相关 Query 失效：`jobs` 列表、`job/:id`、`stats`（debounce 500ms，
     避免高频事件把请求打爆）。
- Monitor 页沿用旧 dashboard 的订阅过滤（`job_types`/`event_types` 复选）。
  过滤是**服务端**的：store 发 `{"action":"subscribe","filter":{...}}`（`core.WSFilter`），
  不匹配的事件根本不会被推过来。由此带出两条必须一起做的规则，实测都踩过：
  1. 视图还要对"已回填的历史 + 已到达的缓冲"再判一次同一个谓词，否则一次筛选会出现两套结果；
  2. 过滤条件一变就要作废 `['events']` 的回填重取一次——过滤期间服务端没推的事件
     浏览器从来没拿到过，不清掉这份缓存，"清除筛选"会显示成一段凭空消失的时间。
  离开 Monitor 页时恢复全量订阅：这条连接是全应用共用的，不能让一个页面把别的页面饿着。

### 4.6 页面详设

#### LoginView
账号 + 密码两个输入框，"登录控制台"按钮；错误内联提示（凭据错误 / 频率限制 /
服务不可达）。登录成功后把 `user{name, role}` 存 auth store，供菜单与按钮鉴权。
后端未配置任何账号且未配置 token 时显示"本服务未启用鉴权，直接进入"。

#### DashboardView（概览）
- 顶部 6 张 `StatCard`：pending / running / paused / completed / failed / heap_size+uptime。
  数据源 `GET /api/v1/stats`（新增 paused 后见 §5.4），Query `refetchInterval: 5s` + WS 事件即时失效。
- 下方两栏：左为分组健康度（每组的 `job_count` 与 `paused_count`，来自 `GET /groups`），
  右为实时事件流（最近几条，完整流与筛选在 Monitor 页）。
  两栏与统计卡共用同一套 Query 键，所以概览上的数字与列表页必然同源。
  `/stats` 的 5s 轮询挂在 **Topbar**（每个页面都渲染它）而不是本页：
  两处都声明 `refetchInterval` 会让同一把钥匙被两个观察者各敲一遍，请求量翻倍却买不到新鲜度。

#### JobsView（任务列表，核心页）
- 筛选条：状态下拉（含 paused）、分组下拉、名称（=job type）下拉，映射为 `?status=&group=&name=`
  （group 为新增参数）。**原稿里的"关键字"输入框不做**：列表端点只认这三个参数，
  硬加一个"只过滤当前这一页"的输入框会让翻页与总数自相矛盾，比没有更糟。
  想按 ID 定位就复制链接——筛选条件同时写进 URL（用 replace，不压历史记录），
  于是 `/jobs?group=nightly` 既是从分组页跳过来的入口，也是可以贴给别人的地址。
  `group=`（空串）与不带 `group` 在后端分别是"只看未分组"与"不过滤"，这一区分在筛选条
  用两个独立选项（全部分组 / 未分组）表达，不能合并。
- 表格列：ID 短码、名称、分组、状态徽标、trigger_at + 本地倒数（`useCountdown` 显示
  `next_run_in`）、重试 `retry_count/max_retries`、cron、操作。
- 行操作（按状态 + 角色启用/禁用，与后端能力一致）：
  - pending：编辑（PUT）、暂停、取消
  - paused：恢复、取消
  - running：查看；**强制暂停**（`admin`/`ops`，需二次确认，见 §5.2 ForcePause；
    低角色看到的是置灰 + "需要 admin 角色"提示）
  - failed：重试（POST retry）、查看
  - success/cancelled：查看
- 行点击 → JobDetailView。多选列 + 批量条（批量取消/暂停/恢复/移入分组），
  依赖端点 `POST /api/v1/jobs/batch-ops`（§5.5）。批量按钮与行内按钮**同一档位判断**：
  只置灰行内而放过批量，等于给低角色留一条"点了才知道 403"的旁路。
  batch-ops 的 207 混合结果要逐条给原因（最常见的是"这条不在待执行队列"），
  并把失败条目留在选中状态里以便重试；回报中的 ID 用**尾段**而不是前 8 位——
  任务 ID 是 UUIDv7，高 48 位是毫秒时间戳，一起提交的任务前缀完全相同
  （实测同一秒内创建的 6 条任务前 8 位一模一样）。
  批量**创建**已有可用端点
  `POST /jobs/batch`（逐条独立、207 混合结果、单请求 ≤100，`api/handlers.go 的 BatchCreateJobs`），
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
  实时部分读的是 realtime store 那 200 条全局环形缓冲，而不是给它再开一个监听位：
  监听位只有一个且已被"事件 → Query 失效"管线占用，多处注册会互相覆盖。
  代价是高频任务的旧事件可能被缓冲挤出，但首屏那 100 条已在手上，不会因此丢历史。
  时间线的说明文案直接引用响应里的 `note`（"in-memory buffer, cleared on restart"），
  两处各写一句迟早不同步。
  另一个实现约束：`/jobs/:id → /jobs/:id` 复用同一个组件实例，
  所以 job id 必须是响应式的——捕获一次就会永远停在第一条任务上（实测踩过）。
- 操作区：与列表行操作同一套规则。

#### GroupsView（分组管理）
- 左侧组列表（名称、颜色点、job 计数），右侧编辑区：名称、描述、颜色（从固定 8 色板选）、
  删除按钮（**仅 admin/ops 可见**，operator/viewer 置灰并提示所需角色）。
  计数用 `GET /groups` 实际给出的 `job_count`/`paused_count`（原稿写的"每组 pending/failed
  计数"后端并不提供：分组维度只统计挂了多少条任务、其中多少条处于暂停）。
- 颜色是 8 色板而不是自由取色器：后端只校验 `#rgb`/`#rrggbb`，放开选择器只会产出
  成百上千种"近似蓝"，分组色就失去了"一眼认出"的意义。没配色渲染成描边空圈，
  不挑一个默认色假装它有身份。
- `registered=false` 的组（只挂在任务标签上、注册表里没有条目）是一等公民：
  列表要显示"未注册"，编辑区退化为只读 + **注册这个组**（走 POST），
  不给改名与删除（那两个端点对它返回 404）。改名改到一半失败留下的"半个旧组"
  全靠这一格看得见，否则无处下手。
- 删除组时弹窗说明后果：**组内 job 不会被删除，只解除分组归入"未分组"**（决策 D5，§5.5）。
  admin/ops 请求不带 `strategy` 参数，后端默认按 detach 处理；弹窗只需勾选确认，
  无需输入 `?strategy=detach`。
- 未分组虚拟项：`group=` 空值过滤入口。

#### MonitorView（实时事件流）
即旧 dashboard 能力：WS 状态、事件类型复选过滤、事件卡片流（限 100 条 DOM）。
过滤是服务端订阅级的，机制与两条配套规则（视图二次判定、筛选变化作废回填）见 §4.5。
首屏由 `GET /events` 回灌一次：刷新页面不该让刚发生的事凭空消失——
M3 那一版只渲染前端缓冲，重载后是空的，页头却写着"由后端缓冲重新供给"，
属于文案承诺了代码没做的事，本轮把代码补上而不是把话删掉。

#### AdminView（运维，仅 ops 角色）
- **调度总开关**：suspend / resume 整个调度循环（维护窗口内不再弹出到期任务，
  执行中的任务不受影响），需要二次确认；状态在 Topbar 显示醒目横幅"scheduling suspended"。
  对应 §5.4 新端点与 §5.2 的 `Scheduler.Suspend/Resume`。
  横幅的数据源是 `/stats` 的 `scheduling_suspended` 而不是 `/admin/runtime`：
  后者只有 ops 能读，那样"任务为什么不出"这句话就说给了改得动它的人，
  而真正需要被告知的是在维护窗口里等结果的所有人。
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

实现落在 `api/keys.ts`（唯一出处，失效管线与页面共用一套键，否则 invalidate 打不中缓存）。
与上表的差异只有两处：分页参数用 `offset`（后端列表就是 limit/offset，见 `docs/api.md`），
另加一把 `['jobs']` 前缀键给事件的整体失效。

---

## 5. 后端修改方案

五项决策（已与用户确认）：

- **D1 登录**：用户名 + 密码 + JWT，`config.yaml` 声明账号与角色；四档角色
  `viewer < operator < admin < ops`。现有静态 token 降级为"机器凭据"保留（§5.7）。
- **D2 暂停**：core 真暂停——新增 `paused` 状态 + pause/resume API；
  **admin/ops 可强制暂停运行中的 job（中止当前 attempt，不计失败、不消耗重试）**（§5.2）。
- **D3 分组**：完整分组——Job 加 `group` 字段 + `/api/v1/groups` CRUD + 过滤 + 聚合。
- **D4 运行历史**：api 层内存环形缓冲 + `GET /jobs/:id/events`，不入库。
  ⚠️ 实现如此：默认仍是不入库（`observability.enabled: false`）；打开总开关与 `events.enabled` 后
  事件写入 SQLite 观测库的 `job_events`，两个事件端点改读库、内存缓冲退成兜底（`TASK-S03`/`TASK-S04`，
  设计文档 `sqlite-observability-design.md` §6.1 与 §8.1）。
- **D5 删组策略**：operator 不可删组；admin/ops **删除组无需显式 `?strategy=detach`
  即默认解绑**，组内 job 归入未分组，**任何角色都不会级联删除 job**（§5.5）。

### 5.1 数据模型（core/job.go）**已实现**

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
  （`core/job.go 的 JobSnapshot`），插入中间会错位历史数据。
- `String()`/`ParseJobStatus` 增加 `"paused"`（`core/job.go 的 String/ParseJobStatus`）；`IsTerminal()` 不含 paused
  （`core/job.go 的 IsTerminal`，paused 仍需可恢复，也不是终态留痕淘汰对象——见下方 trim 注意点）。
- `Job` 与 `JobSnapshot` 各加 `Group string`（json tag `group,omitempty`）；
  `ToSnapshot/FromSnapshot` 同步搬运。旧 jobs.json 无该字段，反序列化为空串 = 未分组，
  天然兼容。
- 事件类型补充：`EventJobPaused = "job.paused"`、`EventJobResumed = "job.resumed"`
  （常量表在 `core/event.go 的 EventType 常量`）。
- **两个必须同步的白名单**（否则新事件在链路里被静默吞掉）：
  1. `api/sse.go 的 publishedEventTypes` 的 `publishedEventTypes`——SSE 走类型级订阅，未列入的类型既不会被推送，
     `?event_types=job.paused` 还会被判 400。
  2. WS 侧无需改动：`filterMatches` 按 `event.Status.String()` 比对状态
     （`core/websocket.go 的 filterMatches`），`paused` 随 `JobStatus.String()` 自动可用；
     客户端因此可以直接用 `{"filter":{"status":["paused","failed"]}}` 订阅，前端 Monitor 页
     应暴露这个 status 过滤（旧 dashboard 只有事件类型过滤）。

### 5.2 调度器（core/scheduler.go）**已实现**

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
   暂停它语义不明；v1 保持与 PUT 更新同样的边界（`core/scheduler.go 的 UpdatePending` 先例）；
   `admin`/`ops` 走下面的 ForcePause。
2. 置 `Status=StatusPaused`、`UpdatedAt=now`，`store.Update(snapshot)`（不删除！
   这是与 `Cancel` 的本质差别，`core/scheduler.go 的 Cancel（store.Delete 分支）` 是 Delete）。
3. 广播 `job.paused` 事件。
4. cron 重复任务：暂停即离开堆，`handleSuccess` 的重排路径（`core/scheduler.go 的 handleSuccess` 起）
   不会碰到它——因为弹出发生在暂停前，此路径无需改动。

**Resume 语义**：

1. `store.LoadAll()` 找 ID（与 `hasStoredJob` 同款扫描，`core/scheduler.go 的 findSnapshot`；
   当前规模可接受，见 §5.10 性能注记），非 paused 状态返回 409。
2. `FromSnapshot` 还原，绑定 Handler（`s.scheduler.LookupHandler(job.HandlerKey())`，同 RetryJob 做法，
   `api/handlers.go 的 RetryJob（按 HandlerKey 绑定）`；api 层已无 JobRegistry）。
3. 触发时间重算：cron 任务用 `cronParser.Next` 取未来时点；一次性任务若
   `TriggerAt` 已过期则"到期立即补跑"（与 Restore 的过期语义一致，`scheduler.go:
   313-315` 注释所述行为）。
4. `Schedule(job)`——ID 非空不会被重新生成（`core/scheduler.go 的 Schedule（ID 非空不会被重新生成）`），堆、存储、
   事件广播全部复用现路径。
5. **实现补充**：强制暂停的快照落盘发生在取消上下文之前，因此"已暂停但 Handler 还在收尾"
   是一个真实存在的中间态。`Resume` 此时拒绝（任务仍在 `cancelMap` 里 → `ErrJobNotPending`），
   否则会与"即将把状态钉回 paused"的收尾守卫打架，表现为恢复了却又自己停住。
   另外 `Suspend` 标志在 `Start()` 里清零：`Start→Stop→Start` 与重启都不继承挂起状态，
   一个遗留的挂起会让调度器"看起来在跑却永不出任务"，是最难排查的故障形状。

**ForcePause（强制暂停，限 admin/ops）**：

作用于**已出堆、正在执行**的任务，语义定为"中止当前 attempt，不计失败、不消耗重试，
停在 paused"：

1. `Scheduler.ForcePause(jobID)`：在 `s.mu` 下查 `cancelMap`（`core/scheduler.go` 的
   `CancelRunning` 已能拿到取消函数），命中则记录"该 ID 待强制暂停"到
   `forcedPause map[string]struct{}`（同 mutex 保护）并 `cancel()`；快照以 `StatusPaused`
   写回存储。任务若还在堆里（竞态可能）→ 一并 `heap.Remove`，退化成一次更强的普通暂停。
   **实现偏离原方案的一点**：`job.paused` 事件不在这里发出，而是由认领标记的那条收尾路径
   发出（见第 2、3 点）。这样"一条 job.paused = 确实停住了"，前端就能诚实区分
   "暂停中"（已请求、Handler 还没返回）与"已暂停"；否则事件先于事实到达，
   UI 会在 Handler 仍在跑的时候显示已暂停，用户以为按钮坏了。
2. Handler 收到 `ctx.Canceled` 后返回错误 → 走 `executeJob` 的取消分支 →
   `handleInterrupted`。**该函数现在先认领强制暂停标记**：命中就保持 `StatusPaused`
   落盘并返回，绝不落到"关停打断→复位 pending"那条分支，否则暂停请求会当场失效。
   未命中时行为不变（running 时只记日志，存储清理由 Cancel 负责）。
3. **Handler 不检查 ctx 的情况**（`executeJob` 的超时注释一直承认这类处理器存在）：
   ctx 已取消但 Handler 返回 nil → 会走 `handleSuccess` 并重排 Cron 的下一轮；
   返回 error → 会走 `handleFailure` 消耗重试。因此这两个函数开头都先试
   `parkForcedPause`：命中则清标记、按 paused 落盘、跳过成功/失败/重试/重排，
   并广播 `job.paused`，metadata 用 `note` 区分是哪条收尾路径兜住的：
   `interrupted`（守规矩）、`handler_completed_despite_cancel`、
   `handler_failed_despite_cancel`。后两个是强制暂停正确性的关键，都有单测。
4. 副作用：被中止的 attempt 可能已产生部分副作用（`handleInterrupted` 的注释所述，
   本仓库整体是至少一次语义），文档与 UI 二次确认弹窗都要写明
   "当前执行会被中止，Handler 需保证幂等"。
5. UI：列表点强制暂停 → running 徽标变"暂停中"（本地态，等待 WS `job.paused`）；
   若 10s 内没收到 `job.paused`，提示"Handler 未响应取消，将在本轮执行结束后停在 paused"
   ——这正是第 3 点的可观测化。

**Restore 修正（关键正确性点）**：现有 Restore 把"所有非终态"快照重新入队并置为
pending（`core/scheduler.go 的 Restore`）。paused 也非终态，照旧逻辑会被**静默恢复**。必须加一行
`if status == StatusPaused { continue }`——重启不解除暂停。

**Cancel 修正**：paused 任务不在堆里也不在执行中，`Cancel` 会返回 `ErrJobNotFound`
（`core/scheduler.go 的 Cancel（未命中即 ErrJobNotFound）`），导致暂停后的任务**删不掉**。需在 Cancel 中补一路：堆和执行
中都没有时，检查存储快照，若为 paused 则 `store.Delete` + 广播 `job.cancelled`。

**UpdatePending 修正**：语义保持"仅 pending 可改"；paused 走 409。前端表单以禁用态呈现，
不再发请求。

**调度总开关（ops 专用，供维护窗口）**：新增 `Scheduler.Suspend()` / `Scheduler.Unsuspend()`
+ `suspended atomic.Bool`。`scheduleLoop` 顶部（`core/scheduler.go 的 scheduleLoop 顶部` 的 select 之后）
判断 suspended 则等待唤醒信号，不弹出任何到期任务；已在 worker 里的任务照常跑完，
堆与存储都不动。注意这与 `Stop()` 完全不同（Stop 是优雅关停并落盘 pending），
不能复用；恢复时只需重新计算堆顶等待时长。suspend 期间 `POST /jobs` 仍可入队（
`Schedule` 不走 scheduleLoop），恢复后一并生效——文档需明示这一点，避免误以为
suspend 会屏蔽创建。

### 5.3 分组存储（core/group_store.go）**已实现**

组是一等实体（可挂描述与颜色），不能从 job 列表反推，需要独立存储。
实现与原方案有三处刻意的偏离，都记录在这里以免后来者以为文档写错了：

```go
type Group struct {
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Color       string    `json:"color,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type GroupStore interface {
	List() ([]Group, error)
	Get(name string) (Group, bool, error)   // 忽略大小写
	Save(group Group) error                // 已存在时保留 CreatedAt
	Delete(name string) error              // 不存在返回 ErrGroupNotFound
}
```

1. **接口不含 Close/Flush**。分组是低频实体，每次改动同步全量原子重写
   （临时文件 + rename，与 jobs 同源）即可；照搬 `core/store.go` 的 `flushLoop` 那套合并写盘协程
   只会多出一个"崩溃丢掉最后一次改动"的窗口，也没有后台协程需要收尾。
2. **配置键 `store.groups_path` 随 M2 的消费者一起加**（已落地：`core/config.go 的 StoreConfig`
   与 `configs/config.yaml`，默认 `./data/groups.json`，可用 `GODELAYQ_STORE_GROUPS_PATH` 覆盖）。
   本仓库的惯例是配置只收录真正生效的字段，所以 M1 只让构造函数显式收路径；
   M2 分组端点接线时才加键，与 schema、配置文件同批提交
   （`UnmarshalExact` 对未知键直接报错）。
3. **损坏的分组文件报错而不是当成空集合**。静默视为空意味着下一次 Save
   就把用户已有的分组全部覆盖掉，这类数据丢失不可逆。

其余按原方案：组名 `[A-Za-z0-9_-]{1,64}`（要当 URL 段与查询参数用），
大小写不敏感唯一但保留用户书写的原名；颜色接受 `#rgb` 与 `#rrggbb` 并在写入时校验。

Job 不强制属于已注册组：允许 `group=foo` 建任务而 foo 未建组（视同"未注册的临时组"，
列表过滤仍可用）；GroupsView 提供"一键收编"（列出被 job 引用但未注册的组名）。
`List()` 返回按名称字典序的稳定顺序，UI 不必再排一次，也不会每次刷新就换位置。

### 5.4 DTO 与 REST API（api 层）**已实现**

> 落地后的口径以 `docs/api.md` 为准（含每个端点的角色与状态码）；本节保留的是设计意图，
> 与实现的差异都在下面标注出来。

**修改既有**：

| 端点 | 变化 |
| --- | --- |
| `POST /jobs` | `CreateJobRequest` 增加 `group` 字段（可选）；handler 赋给 `job.Group`，非法组名 400 |
| `GET /jobs` | 增加 `group` 查询参数（忽略大小写匹配）；`status` 接受 `paused`（`ParseJobStatus` 自动支持）；空值参数：`group=`（精确取未分组）与省略（不过滤）要区分 |
| `PUT /jobs/:id` | `UpdateJobRequest` 增加 `group`（**指针**，区分"没传"与"传空串=取消分组"）。只对 pending 生效：暂停/已结束的任务移组走 `batch-ops` 的 `move` |
| `JobResponse` | 增加 `group`；`status` 枚举说明加 paused |
| `GET /stats` | `StatsResponse` 增加 `paused` 计数（扫描循环加一个 case，`api/handlers.go 的 GetStats`）；M4 再加 `scheduling_suspended`（布尔、不带 omitempty，取 `Scheduler.RuntimeStats().Suspended`），给 Topbar 的挂起横幅做全角色可读的数据源。顺手把 `running`/`heap_size` 改成同一次 `RuntimeStats()` 里取，免得三个字段来自三个瞬间而自相矛盾 |

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
GET    /api/v1/events              viewer       跨任务全局最近事件（Dashboard 首屏回灌）

GET    /api/v1/groups              viewer       [{name,description,color,job_count,paused_count,registered}]
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
DELETE /api/v1/admin/events        ops          清空事件环形缓冲，返回 {cleared:n}
# 写操作台账：结构化日志（§5.7.6）+ SQLite write_audit 表与查询端点，见
# docs/design/sqlite-observability-design.md §6.3 与任务卡 TASK-S06
GET    /api/v1/admin/audit         ops          写操作台账查询（§5.7.6），未装配观测库时 503
```

实现落点：生命周期与批量在 `api/handlers_lifecycle.go`，分组在 `api/handlers_groups.go`，
事件在 `api/handlers_events.go`，运维在 `api/handlers_admin.go`；
调度器侧新增 `RuntimeStats`（诊断）与 `SetGroup`/`RetagGroup`（改组名单原语）。

WS 侧无需协议改动：`job.paused`/`job.resumed` 会按类型透传，`filterMatches` 的状态过滤
按 `JobStatus.String()` 比对，paused 自动可用（`core/websocket.go 的 filterMatches`）。
**但 SSE 的 `publishedEventTypes` 白名单必须同步加这两类**（`api/sse.go 的 publishedEventTypes`），
否则 SSE 客户端收不到暂停事件，见 §5.1。

**鉴权覆盖**：新端点全部挂在既有 `/api/v1` 组内。认证中间件本期由"静态 token 校验"升级为
"JWT 校验 + 角色注入"（§5.7.2-§5.7.3），两条顺序约束不能动：
CORS 预检必须早于认证（`api/server.go 的 setupMiddleware（顺序不可调换）`）；
而 `POST /auth/login`、`POST /auth/refresh` 是**公开端点**，现有认证是 `engine.Use` 全局挂载
（`api/server.go 的 setupMiddleware`），因此要改成"公开路由组先注册 / 其余路由再挂认证"，
或在中间件里维护一份显式放行路径表——**不能用"路径前缀不是 /api 就放行"这类松散判断**，
否则漏保护一个写端点就是越权。验收项里包含"未认证调写端点 401"的逐端点用例（§7）。

### 5.5 分组的破坏性操作策略（写死在文档，避免实现时摇摆）

- **删除组**（决策 D5）：任何角色都**不会**级联删除或取消 job。
  `DELETE /groups/:name` 由 admin/ops 调用时**默认 detach**——组内 job 的 `Group` 置空后
  删除组记录，前端不需要传 `strategy`。要更保守可显式 `?strategy=block`，此时组非空返回 409；
  其它策略名一律 400，绝不静默降级成 detach。
  operator/viewer 调该端点直接 403（§5.4 角色列）。
- **重命名组**：走 `core.Scheduler.RetagGroup(from, to)`，**不是**只改存储快照。
  原因是执行收尾会用内存里的那份 `Job` 写回快照：只改 `store` 的话，任务跑完一轮
  组名就被旧值覆盖回去（`core/scheduler.go 的 applyGroupToSnapshot`）。
  堆里的条目与快照一起改，暂停中与终态留痕的走快照路径。
  非原子，中途失败会在下次操作时呈现"部分 job 还挂在旧组"——UI 的"未注册临时组"入口
  （`GET /groups` 里 `registered:false` 的条目）能兜住，重发一次 PUT 即可（幂等），
  可接受（本仓库整体就是尽力落盘 + 崩溃靠 Restore 的哲学）。
- **batch-ops**：循环调用单个 Scheduler 方法即可，不做事务。含 `force-pause` 时整批要求 admin 以上
  （档位要看请求体，中间件做不到，落在处理器里判，见 `api/security.go 的 allowRole`）。批量**创建**
  已由 `POST /jobs/batch` 实现（逐条独立、207、≤100 条，`api/handlers.go 的 BatchCreateJobs`），
  本端点只做"对已有 job 的动作"，两者语义不要混用。

### 5.6 运行事件历史（api/history.go）**已实现**

实现即下面的形状（常量名 `historyPerJobLimit`/`historyMaxJobs`/`historyGlobalLimit`），
外加 `Stats()`（占用观测，`/admin/runtime` 用它）与 `Clear()`（`DELETE /admin/events` 用它）。

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

- 挂载点：`api.NewServer` 里创建（`api/server.go 的 NewServer`），随 Server 一起 Stop
  （`Stop` 走 `bus.Unsubscribe`，drain 协程随通道关闭退出，重复调用安全）。
  不新增配置项，上限先用常量，需要再开 knob（本仓库惯例是"未生效的选项不进配置"，
  见 `core/config.go 顶部的"只收录生效字段"注释` 注释）。
- 端点：`GET /api/v1/jobs/:id/events?limit=`；`GET /api/v1/events?limit=`（全局 recent，
  Dashboard 刷新后补历史用）。两者**按时间升序**返回，空结果是空列表而不是 404——
  详情页时间线本来就可能还没事件。响应带 `note` 字段说明这批数据从哪儿来。
- **本节描述的是内存缓冲这一条路径，它现在只是兜底**：`observability.enabled` 且
  `observability.events.enabled` 时，两个端点改读持久化事件库（`job_events` 表），
  `note` 换成持久化那句。写入侧见 `TASK-S03`（`docs/design/tasks/sqlite/task-s03-job-events-writer.md`），
  读取侧与 `note`/`limit` 的分岔见 `TASK-S04`（`task-s04-events-endpoints-db-first.md`），
  口径落在 `docs/api.md` 的"运行事件 API"一节。
  本卡当年登记的两处"二期"里，**运行历史的持久化与写操作审计都已落地**：前者是 `TASK-S03`/`TASK-S04`，
  后者是 `TASK-S06`（`write_audit` 表 + `GET /api/v1/admin/audit`，口径在 §5.7.6 与 `docs/api.md` 的运维一节）。
- 反压问题已在实现里核实（`core/event.go 的 Publish`）：总线的发送是
  `select { case ch <- event: default: }`，缓冲区满就丢，所以 drain 协程不可能阻塞总线；
  丢掉的是记录而不是调度事实。`api/history_test.go` 的窗口裁剪用例因此直接调 `record`，
  而不是靠总线发上千条（那种断言会退化成竞态掷硬币）。
- 不带 `JobID` 的事件（如 `heap.updated`）被忽略：它没有归属，塞进任何任务的时间线都是噪音。

### 5.7 认证与角色权限（决策 D1/D5 的后端部分）

现状：后端只有一个全局静态 token（`api/security.go 的 authMiddleware`，恒定时间比较），
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
  （延续"未生效/非法配置早失败"的既有惯例，`core/config.go 的 Validate`）。
- `users` 非空但 `jwt.secret` 为空 → 启动报错；`users` 为空且 `token` 为空 → 不启用鉴权
  （等价现状，登录页显示"未启用鉴权"）。
- 新键必须同步加入 `LoadConfig` 的环境变量绑定清单（`core/config.go 的 LoadConfig 环境变量清单`），
  但 **`users` 列表不绑定环境变量**（viper 的逗号分隔 hook 处理不了嵌套结构，
  明确写进注释避免踩坑）；secret 单独绑 `GODELAYQ_SERVER_AUTH_JWT_SECRET`。

#### 5.7.3 校验与授权分层

已实现的做法（`api/security.go`）：

- 认证顺序：`Authorization: Bearer` → `X-Auth-Token` → `?ticket=`（仅 `/ws`、`/sse/`）
  → `?token=`（仅静态机器凭据）。出现 `Authorization` 头就只认它，非 Bearer scheme 直接 401，
  不回退到 query（与历史行为一致，`api/security_test.go` 有对应用例）。
- JWT 与静态 token 在同一条头通道上共存：先按 JWT 验签（配了账号才有认证器），
  失败再对静态 token 做恒定时间比较，命中即 `RoleMachine`。
- **JWT 不接受走 query**：访问日志会把 `path` 与 `query` 原样记下（`api/logging.go 的 requestLogger`），
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
（`api/logging.go 的 requestLogger` 会把 `path` 与 `query` 一起写进日志）。改法：

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

`api` 层对**写操作**（POST/PUT/DELETE，含 pause/force-pause/groups/admin/*）记一笔台账，
两条出口：

- **结构化日志**（本节原始方案，仍然是关闭观测层时的唯一出口）：一行
  `msg="write operation audited"`，字段与表列同名（`who`/`role`/`action`/`method`/`route`/
  `status`/`latency_us`/`verdict`，执行器任务再多 `exec_verdict`/`exec_reason`/`profile`/`handler_key`）。
  由 `api/audit.go` 的中间件产出，未装配写入器时走这条。
- **持久化与查询**（`docs/design/sqlite-observability-design.md` §6.3 的 `write_audit` 表，
  由 `docs/design/tasks/sqlite/task-s06-write-audit-and-endpoint.md` 落地）：同一份内容写进
  SQLite 观测库，`GET /api/v1/admin/audit` 按账号/动作/结论/时间区间查询，两端都取 ops 档。
  开关是 `observability.enabled` 与 `observability.audit.enabled`，任一为假时端点返回 503、
  台账退回只落日志。
- **界面**（`docs/design/tasks/sqlite/task-s08-console-audit-page.md` 落地）：侧边栏 `审计` 页
  （`web/src/views/AuditView.vue`，路由 `meta.minimumRole: 'ops'`）读的就是上面那个端点——
  按身份/动作/结论/时间窗筛选、上一页下一页翻页、点开一行看全部列（含 `route` 模板、`remote_ip`、
  `user_agent`）。503 呈现为"这次部署没有启用写操作台账"的说明态而不是错误弹窗；
  这一页不轮询、不接 WebSocket 失效，只有"重新查询"按钮。

两条都不记请求体、参数取值与校验错误原文（设计文档 D7）；`route` 记路由模板而非原始 URL。
访问日志 `requestLogger` 那行（`msg="http request"`）继续存在且覆盖读请求，
它说的是"这个请求被怎么处理"，台账说的是"谁改动了什么"，两行的字段与用途不同。

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
`dist/index.html`（SPA history fallback），其余路径保持现有 JSON 404（`api/server.go 的 NoRoute`）。
资源走 `http.FS` + `/assets` 前缀。用构建 tag（`//go:build dashboard`）区分是否嵌入，
避免没跑前端构建时 dist 为空目录导致 `go build` 失败。

前端产物目录 `web/dist` 提交与否二选一，倾向 `.gitignore` 排除、CI/发布时构建。

**落地实况（M5）**：按方案 B 实现，与上面的草图有三处不同，都是当时没预见到的耦合：

1. **不给 `/assets` 单独注册路由**，静态资源、`/` 与 SPA 深链全部走 `NoRoute` 一个分派点。
   理由是这个进程里还有一个全局鉴权中间件：豁免判断与分派判断必须是同一个函数
   （`api/console.go` 的 `consoleRequest`），分成两处就会出现"中间件放行了却没有处理器
   接管"（未认证请求拿到 JSON 404，前端当成页面不存在）或反向的"页面能取到但先被 401"。
2. **豁免范围**：`GET`/`HEAD`，路径为 `/` 或 `/assets/*`，或（不在 `/api/`、`/ws`、`/sse/`
   名字空间里且）`Accept` 含 `text/html`。写方法一律不免；未注入产物时这条豁免根本不存在
   （`TestNoConsoleKeepsPlainAPIBehaviour` 钉住）。
3. **缓存分档**：`index.html` 是 `no-cache`，`/assets/*` 是 `max-age=31536000, immutable`。
   原稿没提这一点，但它是升级事故的直接来源——缓存住 index.html 就会去取已被替换的哈希文件。

`web/dist` 取"不提交"这一支（`web/.gitignore` 已排除），构建顺序写进 README 与
`docs/deployment.md`。构建 tag 就叫 `dashboard`，与草图一致：不带它时 `web.Dist` 为 nil，
`go build ./...` 与全部测试因此完全不依赖 Node。

### 5.9 装配点与依赖清单

- **认证不需要改构造签名**（M0 已实现）：`Security` 直接携带 `core.AuthConfig`，
  `NewServer` 内部构造认证器与凭据存储（`api/server.go 的 NewServer`）。
  认证器构造失败（坏哈希、缺密钥）存进 `Server.authErr`，由 `Start()` 返回——
  既守住"配置坏掉就别上线"，又不用把 `error` 塞进已被十几处测试使用的构造函数。
- **groupStore 走可选参数**（M1/M2 已实现）：`api.NewServer(..., opts ...Option)`，
  由 `api.WithGroupStore(core.GroupStore)` 承接（`api/server.go 的 Option`），
  形态对齐 `core.NewScheduler(..., opts ...Option)`（`core/scheduler.go 的 NewScheduler`），
  位置参数列表不再变长、既有测试的构造调用一行都不用改。
  没注入注册表的部署里 `/api/v1/groups` 统一返回 503（`api/handlers_groups.go 的 requireGroupStore`），
  任务的 `group` 标签照常读写——那是两份数据。
  生产装配点在 `cmd/server/main.go 的 defaultRuntimeDeps`：分组文件读不出来就挡住启动，
  而不是让每个分组请求都 500。
- **`Restore` 在 `Start()` 内部执行**（`core/scheduler.go 的 Start（内部调用 Restore）`），不在 main 里；
  因此 §5.2 的 paused 跳过必须在 `Restore` 本体改，别指望调用方过滤。
- **`Start→Stop→Start` 是被支持的**（`core/scheduler.go 的 Start（stopCh 复位）` 显式复位 `stopCh`），
  所以 `Suspend` 标志要定义为"进程内、重启即解除"，并在 `Start()` 里清零；
  ops 的 suspend 不跨重启生效，UI 需说明。
- **Handler 注册只有一个入口**：`Server.RegisterJobHandler` 直接转发到
  `scheduler.RegisterHandler`（`api/server.go 的 RegisterJobHandler`、`cmd/server/main.go 的 registerHandlers`），
  api 层已无 JobRegistry；控制台展示 job 类型走 `scheduler.HandlerNames()`
  （`api/handlers.go 的 ListJobTypes`，已按字典序）。
- **Go 依赖**（M0 已加）：`github.com/golang-jwt/jwt/v5 v5.3.1`（JWT）、
  `golang.org/x/crypto v0.41.0`（bcrypt，由 indirect 转为直接依赖）。
- **已落地文件（M0）**：`core/auth.go`（角色阶梯与档位比较）、`api/authenticator.go`（账号校验 + JWT 签发/验签）、
  `api/authstore.go`（refresh 表 + jti 拒绝表 + 一次性 ticket）、`api/ratelimit.go`（登录限流）、
  `api/handlers_auth.go`（五个 `/auth/*` 端点）、`cmd/hashpassword/main.go`（生成哈希）。
  未单独建 `api/rbac.go`/`api/ticket.go`：角色中间件与 ticket 分别归入
  `api/security.go` 与 `api/authstore.go`，避免为一个函数开一个文件。
- **M1/M2 新增文件**：`core/group_store.go`（分组存储）、`api/history.go`（事件环形缓冲）、
  `api/handlers_lifecycle.go`（暂停/强制暂停/恢复/批量操作）、`api/handlers_groups.go`（分组 CRUD）、
  `api/handlers_events.go`（时间线与全局事件流）、`api/handlers_admin.go`（ops 端点）。
  没有为单个函数再拆文件：`allowRole` 归 `api/security.go`；
  任务与认证相关的 DTO 仍在 `api/dto.go`，分组/事件/运维的响应体与它的端点同文件（只有一处用它们）。

### 5.10 兼容性与性能注记

- **旧数据**：无 `group` 字段的 jobs.json、状态 int 0-4 的快照全部兼容（新状态取 5）。
- **LoadAll 线性扫描**：ListJobs/GetJob/RetryJob/分组重命名都在扫全量快照
  （`api/handlers.go 里多处 store.LoadAll`）。当前 history_limit=1000 量级无碍；Store 增加
  `Get(jobID)` 是合理的后续重构，**不阻塞本期**。
- **paused 与 trim**：`trimTerminalLocked` 只淘汰终态（`core/store.go 的 trimTerminalLocked`），
  paused 非终态，不会被历史淘汰策略误删。✅（实现时加回归测试）
- **前端轮询兜底**：WS 断线期间 stats 仍有 5s refetchInterval，页面不至于假死。

## 6. 实施计划

按可独立验收的里程碑切分（每个里程碑编译 + 测试绿再进下一个）：

| # | 里程碑 | 内容 | 涉及 |
| --- | --- | --- | --- |
| M0 | 认证与角色 **已交付** | JWT 签发/校验、bcrypt 账号配置、`RequireRole`（machine 由阶梯天然排除）、RT 表 + jti 拒绝表 + ticket、`/auth/*` 五个端点、登录限流、写操作进访问日志（who/role）；httptest 覆盖三档角色越权、轮转、登出即失效、ticket 一次一用 | `core/auth.go`、`core/config.go`、`api/auth*.go`、`api/ratelimit.go`、`api/security.go`、`cmd/hashpassword` |
| M1 | core：paused + group **已交付** | 状态枚举/Job/Snapshot/事件常量；`Scheduler.Pause/ForcePause/Resume`、`Suspend/Unsuspend`；`handleInterrupted`/`handleSuccess`/`handleFailure` 的强制暂停守卫；Restore/Cancel 修正；`GroupStore`（路径由构造传入，`store.groups_path` 配置键随 M2 消费者一起加）；SSE 类型白名单补 paused/resumed；单测（旧数据兼容、重启不解除暂停、暂停后可取消、Handler 忽略取消仍停在 paused、并发落盘完整性） | `core/*.go`、`api/sse.go` |
| M2 | api：REST **已交付** | pause/force-pause/resume/groups/batch-ops/events/admin 端点 + DTO 扩展 + stats.paused；EventHistory 记录器；`store.groups_path` 配置键与生产装配；403 越权尝试进访问日志 | `api/*.go`、`core/scheduler.go`（RuntimeStats/SetGroup/RetagGroup）、`core/config.go`、`cmd/server/main.go` |
| M3 | web 骨架 **已交付** | Vite 7+TS 5+Tailwind 4+lucide 工程、token 样式层、`api/`（types/client 401→refresh→重放/auth/keys/stats）、auth store（sessionStorage + 单飞刷新）与 `usePermission` 能力表、路由守卫与角色过滤菜单、两栏 layout + 基础 UI 组件、登录流（含"未启用鉴权 直接进入"）、realtime store（ticket 建连/退避重连/200 条缓冲）与事件→Query 失效管线。**为把整条链路跑通验证，Dashboard/Monitor/Settings 三页按真页面实现**；Jobs/Groups/Admin/JobDetail 为 M4 占位 | `web/` |
| M4 | web 页面 **已交付** | Jobs 列表（筛选/分页/URL 驱动）、JobForm（新建与编辑两种字段集）、批量条（batch-ops 207 逐条回报）、JobDetail（信息卡 + payload + 运行时间线）、Groups（左列表右编辑、8 色板、未注册组的注册路径、删除 detach 确认）、Admin（占用/缓冲诊断 + 挂起与清缓冲二次确认）、Topbar 挂起横幅、Monitor 服务端订阅过滤 + `/events` 回灌、Dashboard 两面板。附带一处后端契约追加：`/stats.scheduling_suspended`（含 Go 测试与 api.md 同步） | `web/src/views/*`、`web/src/components/{jobs,groups}/*`、`web/src/composables/*`、`api/{dto,handlers}.go` |
| M5 | 集成与发布 **已交付** | 单二进制形态落地：`web/embed_dashboard.go`（`//go:build dashboard` + `//go:embed all:dist`）与 `web/embed_stub.go`（无 tag 时 `Dist` 恒为 nil），`api.WithConsole(fs.FS)` 可选依赖，`/`、`/assets/*` 与 SPA 深链由 NoRoute 统一分派（鉴权豁免与分派共用 `consoleRequest`），`index.html` no-cache / `/assets` immutable；不带 tag 的构建与全部测试不依赖 Node。vite 联调按开发形态复验。旧 `dashboard/index.html` 改为跳转页（http 下自动跳同源根路径，`file://` 下只给指引）。文档：api.md 免鉴权路径与静态托管两节、deployment.md 控制台与产物一节 + 已知边界、README 两种形态与构建顺序、example.md 旧页描述更正 | `web/embed_*.go`、`api/console.go`、`cmd/server/main.go`、`docs/`、`dashboard/` |

后端 M0+M1+M2 约 6-8 天（M0 的 JWT/RBAC 比原 token 方案多约 2 天），
前端 M3+M4 约 1.5-2 周。M0 与 M1 **都要改 `core/config.go` 的环境变量键清单**（`core/config.go 的 LoadConfig 环境变量清单`），并行开发时在合并阶段协调这一处即可；M3 可在 M0 端点契约
（§5.4 + §5.7）冻结后启动。

## 7. 验收清单

后端三项里程碑（M0/M1/M2）能自证的部分已勾选，括号里是覆盖它的测试；
依赖浏览器的条目在 M3/M4 两轮真浏览器冒烟中逐条复核，实测证据写在条目后面，
观测不到的（例如被隐藏标签页节流掉的定时动画）明确标"未观测"而不是勾掉。

- [x] 未认证访问任意业务端点 → 401；`viewer` 调写端点 → 403；`operator` 调强制暂停/删组 → 403
      （`api/auth_test.go`、`api/handlers_lifecycle_test.go`、`api/handlers_groups_test.go`、`api/handlers_admin_test.go`）
- [x] 前端把同一张档位表用在 UI 上：`viewer` 下**行内与批量**的写按钮一律置灰并写出
      "需要 operator 及以上角色"，运维菜单入口不出现、直连 `/admin` 被守卫弹回首页
      （浏览器实测：以 viewer 登录后逐项读 disabled 与 title；两处曾不一致——
      批量按钮漏了判档、以及冷启动时应用外壳先挂载引发的一次 401，均已修）
- [x] 挂起调度时所有角色都能在 Topbar 看到"调度已挂起"横幅（数据源 `/stats.scheduling_suspended`，
      `TestStatsReportsSchedulingSuspended`；浏览器实测挂起→横幅出现、
      恢复→横幅消失，且挂起期间到点的任务停在 pending 不出堆，恢复后立即补跑）
- [x] 错误密码与不存在的账号返回同一状态码与文案（不可枚举账号）；连续失败触发限流（`TestLoginFailureIsIndistinguishable`、`TestLoginRateLimitedAfterFailures`）
- [x] access token 过期后前端静默刷新并重放成功；logout 后旧 access token 立即 401（jti 拒绝表）、
      refresh token 立即失效（后端轮转/吊销已覆盖：`TestRefreshRotatesRefreshToken`、
      `TestLogoutRevokesAccessTokenAndRefreshToken`；前端侧在浏览器实测：把 `access_ttl` 调到 20s
      后不刷新页面跑完一轮任务，服务端访问日志出现 4 次 `/auth/refresh` 且其间无 401 冒泡；
      退出后 sessionStorage 清空并回到 `/login`（按钮是显式跳转，凭据彻底失效时由
      sessionLost 带 `?redirect=` 送回）。M3 时因标签页隐藏、定时器被节流而没观测到的
      toast，M4 在浏览器里读到了实际文案（"批量暂停：3 条成功、1 条未执行。…"、
      "已保存"、"已注册"），这条保留意见撤销）
- [x] WS/SSE 通过一次性 ticket 建连；ticket 复用第二次被拒；URL 与访问日志中不出现长期凭据
      （`TestWSTicketIsSingleUseAndScopedToRealtimeChannels`、`TestAccessTokenIsRejectedInQueryString`；
      浏览器复核：启用鉴权与未启用鉴权两种部署下 Topbar 徽标均进入"实时连接"，
      未启用鉴权时 `/auth/ws-ticket` 由匿名 ops 主体签发票据，前端因此不设匿名直连分支）
- [x] pending job 暂停 → 列表显示 paused、不再触发、重启后仍 paused；恢复后 cron 任务按新周期排期，
      一次性过期任务立即补跑（`core/pause_test.go`、`TestPauseAndResumeOverHTTP`）
- [x] running job 强制暂停 → 当前 attempt 被中止、不计入 `retry_count`、状态停在 paused、
      不产生 `job.failed`；**Handler 不检查 ctx 时仍在成功/失败收尾后停在 paused**（§5.2 第 3 点）
      （`core/pause_test.go` 的忽略取消用例、`TestPauseRunningJobPointsAtForcePause`）
- [x] paused job 可取消（Cancel 修正生效，`core/pause_test.go`）
- [x] admin/ops `DELETE /groups/:name` 不带参数即删除，组内 job 归入未分组、无一被删除；
      `?strategy=block` 时组非空返回 409（`TestGroupDeleteDefaultsToDetach`、
      `TestGroupDeleteBlockStrategyRefusesNonEmptyGroup`）
- [x] ops 可 suspend 调度：暂停期间到期任务不弹出，堆与存储不变；unsuspend 后按原时间补跑
      （`core/suspend_test.go`、`TestAdminRuntimeReportsOccupancy`）
- [x] 分组改名连带改写任务标签，且不会被下一次执行写回旧值（`TestGroupRenameRetagsJobs`、
      `TestScheduler_SetGroup_KeepsNewGroupAfterTheJobRuns`）
- [x] 详情页时间线展示最近事件（含 paused/force-paused/resumed/retrying/failed 与 timeout 标记）
      （端点：`TestJobEventsEndpointServesTimeline`；浏览器实测：一条完成任务渲染出
      scheduled/started/completed 三节点并显示"第 1 次尝试"，一条暂停任务显示
      scheduled/paused；`/jobs/a → /jobs/b` 切换后正文与时间线都跟着换）
- [x] WS 事件触发列表/统计自动刷新；断线显示黄色徽标并自动重连。**统计与列表都已实测**：
      不刷新页面建任务、暂停任务，概览计数与列表行状态同步变化（事件→debounce→失效→重取）。
      断线重连在 M5 的单二进制联调里补齐：杀掉服务端进程后顶栏徽标变为"重连中"，
      实测计算样式 `color`/`dot` 均为 `rgb(217, 119, 6)`（warning 档），tooltip 显示
      "实时票据申领失败"；重启进程后徽标回到"实时连接"，无需刷新页面
      （`web/src/components/layout/AppTopbar.vue` 的 tone 映射 + 浏览器 computed style 取样）
- [x] 旧 `data/jobs.json`（无 group、状态 0-4）直接升级运行无报错；配置中写非法 role 或
      缺 jwt.secret 时启动即报错（`core/job_status_test.go`、`core/config_test.go`、
      `TestStartFailsWhenAuthMisconfigured`）
- [x] `go build ./... && go vet ./... && go test ./...` 全绿（含 -race）

## 8. 风险与后续演进

1. ~~运行中 job 不可暂停~~（**已确认并放宽**：operator 及以下仍不可暂停 running，
   `admin`/`ops` 通过 `force-pause` 中止当前 attempt；语义与竞态处理见 §5.2）。
   遗留：需要"跑完这一轮就不再排期"的温和 cron 暂停时，再补 `graceful` 参数。
2. ~~删除组的解绑策略~~（**已确认**：admin/ops 不带参数即默认 detach，任何角色都不级联删 job；
   更保守可显式 `?strategy=block`，见 §5.5）。
3. **强制暂停依赖 Handler 检查 ctx**：本仓库一贯承认"不检查 ctx 的 Handler 无法被中断"
   （`core/scheduler.go 的 executeJob（超时注释）`）。方案用"执行收尾处兜底停在 paused"覆盖这一情形
   （§5.2 第 3 点），代价是中止要等到 Handler 自己返回——UI 必须把这段时间显示为
   "暂停中"而不是"已暂停"，否则用户会重复点击。
4. **JWT 的即时吊销是近似的**：access token 有效期内 logout 靠 jti 内存拒绝表兜住
   （§5.7.4），但进程重启会清空拒绝表、refresh token 表也一并丢失（所有人都要重新登录）。
   这是"无独立会话存储"的固有取舍，需在 `docs/deployment.md` 写清。
5. **账号写在配置文件里**：加/停用户要重启进程；无密码策略、无锁定、无自助改密。
   在线用户管理（`data/users.json` + bcrypt 重写）是明确的二期。
6. **密钥轮换**：`server.auth.jwt.secret` 一改，所有已发 token 立刻失效；生产必须走
   `GODELAYQ_SERVER_AUTH_JWT_SECRET`（密钥用 `go run ./cmd/gensecret` 生成），禁止把明文密钥
   写进配置文件。落地时把 `configs/config.yaml` 整个移出版本库（`.gitignore`），
   入库的只有同结构的 `configs/config.example.yaml`，两份键由
   `core.TestExampleConfigMatchesLocal` 比对——原来"文件入库、靠注释警告"的写法，
   只要有人图省事在本机填一次真凭据，`git add -A` 就会把它推出去。
7. **EventHistory 内存上限**：2000 job × 100 条 Event 粗估 <10MB，可接受；如需上限
   可调再开配置。
8. **Query 失效风暴**：高频事件下 debounce 策略（500ms 合并）需在实现时压测一次。
9. 前端不引入重型 UI 库；若评审倾向 Element Plus，仅影响 §4.1/§4.3 落地方式，页面
   信息架构不变。
