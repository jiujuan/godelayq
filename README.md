## 项目简介

**godelayq** 是一个生产级的 Go 语言延迟任务调度系统，采用四叉堆（4-ary Heap）作为核心数据结构，相比传统二叉堆具有更好的 CPU 缓存局部性。设计灵感来源于 Go 标准库 `time` 包的定时器实现。

### 为什么选择四叉堆？

| 特性 | 二叉堆 | 四叉堆 |
|------|--------|--------|
| 层数 | 高 | 低（减少约50%） |
| 缓存命中率 | 一般 | 更高 |
| 父子节点距离 | 远 | 近 |
| 适合场景 | 通用 | 高频调度 |

### 适用场景

- 🛒 **电商系统**：订单超时取消、延迟支付检查、库存回滚
- 📧 **消息推送**：定时邮件、短信、App 推送
- 📊 **数据处理**：定时报表生成、数据同步、日志清理
- 🔄 **工作流引擎**：状态机流转、审批超时提醒(* 暂时没实现)
- ⏰ **定时任务**：Cron 表达式支持的周期性任务
- 🛠️ **运维自动化**：定时清理、报表脚本、部署钩子——由配置声明的执行器档位执行，见核心特性第 3 条；
  执行过什么、输出多大、谁提交的，启用观测层后都可以查（见核心特性第 4 条）


## 架构设计

### 架构设计图

```shell
┌─────────────────────────────────────────────────────────────────┐
│                         API Gateway (Gin)                        │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────────────────┐  │
│  │  REST API   │  │  WebSocket  │  │  SSE (Server-Sent Events)│  │
│  │  /api/v1/*  │  │  /ws        │  │  /sse/events            │  │
│  └──────┬──────┘  └──────┬──────┘  └───────────┬─────────────┘  │
└─────────┼────────────────┼─────────────────────┼────────────────┘
│                │                     │
└────────────────┴─────────────────────┘
│
▼
┌─────────────────────────────────────────────────────────────────┐
│                      Scheduler Core (调度器核心)                  │
│  ┌─────────────────┐    ┌─────────────────┐    ┌─────────────┐  │
│  │  QuaternaryHeap │◄───│  Job Registry   │    │  EventBus   │  │
│  │  (四叉堆)        │    │  (任务注册表)    │    │  (事件总线)  │  │
│  │                 │    │                 │    │             │  │
│  │  • Push O(log n)│    │  • Handler管理   │    │  • Pub/Sub  │  │
│  │  • Pop  O(log n)│    │  • 动态注册      │    │  • 实时推送  │  │
│  │  • Remove O(log)│    │  • 类型安全      │    │  • 过滤订阅  │  │
│  └─────────────────┘    └─────────────────┘    └──────┬──────┘  │
└───────────────────────────────────────────────────────┼─────────┘
│
┌──────────────────┬──────────────────────────┤
│                  │                          │
▼                  ▼                          ▼
┌─────────────────┐  ┌─────────────────┐    ┌─────────────────────┐
│   Retry Policy  │  │   Cron Parser   │    │   Directory Loader  │
│  (重试策略)      │  │  (robfig/cron)  │    │   (文件任务加载器)   │
│                 │  │                 │    │                     │
│  • 指数退避      │  │  • 标准Cron     │    │  • 目录监控          │
│  • 随机抖动      │  │  • 秒级精度      │    │  • JSON任务文件      │
│  • 最大重试限制  │  │  • 下次执行计算  │    │  • 自动加载/归档     │
└─────────────────┘  └─────────────────┘    └─────────────────────┘
│                  │                          │
└──────────────────┴──────────────────────────┘
│
▼
┌─────────────────────────────────────────────────────────────────┐
│                Executor Layer (执行层，executors.enabled)        │
│  ┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐  │
│  │    Registry     │  │ Runner / HTTP   │  │ Result&Artifact │  │
│  │ (档位登记表)     │  │ (进程/HTTP 执行) │  │ (摘要与产物文件)  │  │
│  │                 │  │                 │  │                 │  │
│  │  • 配置声明      │  │  • 白名单命令行   │  │  • 退出码/状态码 │  │
│  │  • 可用性探测    │  │  • 超时与整树终止 │  │  • TTL/孤儿清理 │  │
│  │  • 参数校验      │  │  • 输出落盘      │  │  • 尾部预览      │  │
│  └─────────────────┘  └─────────────────┘  └─────────────────┘  │
│  独立执行池（与 scheduler.workers 分池），任务类别由调度器识别        │
└─────────────────────────────────────────────────────────────────┘
│
▼
┌─────────────────────────────────────────────────────────────────┐
│                      Persistence Layer (持久化层)                 │
│  ┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐  │
│  │  JSON File Store│  │  (*可扩展: Redis) │  │  (*可扩展: MySQL) │  │
│  │  • 原子写入      │  │  • 分布式锁      │  │  • 事务支持      │  │
│  │  • 崩溃恢复      │  │  • 集群支持      │  │  • 复杂查询      │  │
│  └─────────────────┘  └─────────────────┘  └─────────────────┘  │
└─────────────────────────────────────────────────────────────────┘
│
▼
┌─────────────────────────────────────────────────────────────────┐
│        Observation Layer (观测层，可选：observability.enabled)   │
│  ┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐  │
│  │   job_events    │  │ artifact_index  │  │   write_audit   │  │
│  │  (事件时间线)    │  │ (输出文件索引)   │  │  (写操作台账)    │  │
│  │  • 跨重启留存   │  │  • 尺寸/截断/状态 │  │  • 谁·做了什么·结论│  │
│  └─────────────────┘  └─────────────────┘  └─────────────────┘  │
│  一个 SQLite 文件（modernc 纯 Go 驱动，只出现在 store/sqlite）；  │
│  定位是加速器不是账本：不搬任务快照，写失败不改变执行结果与响应    │
└─────────────────────────────────────────────────────────────────┘
```

> * 暂时没有实现，持久化层的 可扩展: Redis 与 可扩展: MySQL 还没实现
>
> 观测层默认关闭（`observability.enabled: false`）：关着时不建文件、不订阅事件总线，
> 上面三个端点回到本图之前的行为。开启方式与代价见 [部署文档](./docs/deployment.md) 的"启用观测层"。

### 数据流图

```shell

[任务提交] ──► [API/文件/程序] ──► [四叉堆调度] ──► [时间到达] ──► [执行Handler]
│                │                  │              │
│                │                  ▼              ▼
│                │           [持久化存储]    [事件总线] ──► [WebSocket推送]
│                │                  │              │
│                │                  │              ├──────► [SSE 备选通道]
│                │                  │              └──────► [事件库 job_events]（可选，observability）
│                └──────────────────┘              ▼
│                                         [重试/Cron/归档]
▼
[立即返回任务ID]
```

> 事件总线的下游是并列的订阅者：实时推送与 SSE 之外，启用观测层时多一个把 `job.*` 写进 SQLite 的写入器。
> 每一路都是非阻塞投递，任一路取不到都不影响其它两路（见 [部署文档](./docs/deployment.md) 的"启用观测层"第 7 条）。

### 核心文件详解

| 文件 | 作用 | 关键设计 |
|------|------|----------|
| `heap.go` | 四叉堆实现 | 索引映射实现按 ID 的 O(1) 定位，四叉 sift 与 `Update`/`PopIfDue` |
| `job.go` | 任务模型 | 状态含 `paused`（追加在枚举末尾，兼容已落盘的 int）；`group` 只是标签，落盘省略空值；快照与重试副本四处搬运同一字段 |
| `scheduler.go` | 调度器引擎 | 堆顶定时器唤醒（非忙等待），有界 worker 池 + 队列背压，优雅关闭与崩溃恢复；`Pause`/`ForcePause`/`Resume` 暂停语义（收尾守卫保证不复活），`Suspend` 调度总开关，`SetGroup`/`RetagGroup` 连堆内条目一起改分组，`RuntimeStats` 供运维端点读占用 |
| `group_store.go` | 分组元数据 | 单 JSON 文件同步原子重写（低频实体不复制 jobs 的合并落盘协程）；组名规则、损坏文件报错而非当空集 |
| `executor_profile_store.go` | 档位文件（页面建的档位） | 与分组同一形态：单 JSON 数组、同步原子重写、损坏即报错而不是当空集合；`ExecutorProfileRecord` 是存储面形状，与配置面 `ExecutorCommand` 分两份，免得加一个存储字段就多一个"配置里能写但没意义"的键。`env` 的固定取值只落在这里，不出现在任何读口 |
| `auth.go` | 角色模型 | `viewer < operator < admin < ops` 单阶梯比较；`machine` 等同 operator 档，因而天然拿不到 admin 能力 |
| `event.go` | 事件驱动架构 | 发布-订阅；订阅缓冲满时丢弃事件而非阻塞调度主流程 |
| `websocket.go` | 实时通信 | 心跳保活、按事件类型/任务名过滤订阅；只依赖 `WSConn`/`WSUpgrader` 接口，协议库由上层注入（重连属客户端能力） |
| `load.go` | 文件任务加载 | fsnotify 监控 + 100ms 静默窗口合并写入事件，加载后删除/归档，非法文件可隔离到 `error_dir` |
| `config.go` | 运行配置 | viper 读 yaml + `GODELAYQ_*` 环境变量，未知键与非法取值启动即报错 |
| `logging.go` | 日志装配 | 标准库 `log/slog`，`NewLogger` 按级别/格式构造，组件经 `WithLogger` 注入 |
| `store/sqlite/` | 观测层存储（可选，独立于 `core.Store`） | 一个 SQLite 文件里三张表（`job_events` / `artifact_index` / `write_audit`）；全仓唯一 import 驱动（`modernc.org/sqlite`，纯 Go）的包，`core`/`api`/`executor` 都不认识 SQLite。三个写入器共用一个有界批量器：非阻塞入队、按 `flush_interval` 合并成一次事务、失败的一批重试一次后丢弃并计数，所以记不住观测数据永远不会反过来拖慢调度或请求 |


## 目录结构

```shell
godelayq/
├── README.md                 # 项目文档（本文件）
├── go.mod / go.sum           # 模块定义与依赖校验
├── .gitignore
│
├── cmd/                      # 可执行程序入口
│   ├── server/
│   │   ├── main.go           # 服务器主程序（配置 → 存储 → 调度器 → API → 信号）
│   │   ├── main_test.go
│   │   └── main_integration_test.go
│   └── hashpassword/         # 生成 server.auth.users 里要的 bcrypt 密码哈希
│
├── core/                     # 核心库（不依赖任何 web 框架）
│   ├── heap.go               # 四叉堆（索引映射、Update、PopIfDue）
│   ├── job.go                # 任务定义、状态（含 paused）、分组标签、快照与 CloneForRetry
│   ├── scheduler.go          # 调度器：堆 + worker 池 + 取消表 + 暂停/强制暂停 + 调度总开关 + 事件总线
│   ├── group_store.go        # 分组元数据存储（单 JSON 文件，同步原子落盘）
│   ├── executor_profile_store.go # 档位文件（executors.profiles_path）：页面建的档位存这里，形态照 group_store
│   ├── auth.go               # 控制台角色档位与权限比较
│   ├── store.go              # 存储接口与 JSON 实现（合并落盘、终态留痕）
│   ├── retry.go              # 指数退避重试策略（抖动 + 延迟上限）
│   ├── cron.go               # Cron 表达式解析（5/6 段，可选秒级）
│   ├── event.go              # 事件总线
│   ├── config.go             # 运行配置（viper + GODELAYQ_* 环境变量）
│   ├── logging.go            # slog 日志器构造与组件可选项
│   ├── load.go               # 目录任务加载器（fsnotify）
│   ├── websocket.go          # WebSocket 服务（只依赖连接/升级接口）
│   └── *_test.go             # 与上述文件一一对应的单元测试
│
├── api/                      # HTTP API 层（gin）
│   ├── server.go             # 路由、中间件、优雅关闭（可选依赖走 Option/WithGroupStore）
│   ├── handlers.go           # 任务 CRUD、列表过滤（含 group）、统计
│   ├── handlers_auth.go      # 登录、刷新、登出、身份、实时票据
│   ├── handlers_lifecycle.go # 暂停/强制暂停/恢复 + batch-ops 批量操作
│   ├── handlers_groups.go    # 分组注册表 CRUD（改名连带改写任务标签）
│   ├── handlers_events.go    # 任务时间线与全局最近事件（装配了事件库时改读库）
│   ├── handlers_artifacts.go # GET /jobs/:id/artifacts：按尝试列出输出尺寸与清理状态
│   ├── handlers_executors.go # 档位清单、执行输出读取、提交期判定与凭据掩码
│   ├── handlers_executor_profiles.go # 档位的在线管理：定义读口 + 增改删（ops 档，落盘先于生效）
│   ├── handlers_admin.go     # ops 档：运行时诊断、调度总开关、清缓冲、写操作台账查询
│   ├── audit.go              # 写操作台账：中间件、action/verdict 封闭词表、读写窄接口（观测层）
│   ├── history.go            # 事件内存环形缓冲（订阅事件总线，重启即清空；启用事件库后它是兜底）
│   ├── authenticator.go      # 账号校验与 JWT 签发/验签
│   ├── authstore.go          # refresh 表、登出拒绝表、一次性 ticket
│   ├── ratelimit.go          # 登录失败限流（IP+账号 与 IP 双维度）
│   ├── dto.go                # 请求/响应数据结构
│   ├── security.go           # 认证与角色中间件、跨域来源策略
│   ├── logging.go            # 访问日志与 panic 恢复中间件（slog）
│   ├── websocket.go          # gorilla/websocket 适配器
│   ├── sse.go                # Server-Sent Events
│   └── *_test.go             # 契约、鉴权、关闭与流式测试
│
├── executor/                 # 执行层（把档位变成可注册的执行任务；默认不启用）
│   ├── profile.go            # 档位加载与校验：两种路径模式（workspace 内 / 本机任意路径）、解释器白名单、参数与超时声明
│   ├── registry.go           # "档位 + 可用性 + 来源"的登记表（配置项 executors.* 的唯一读取方，运行期可整表替换）
│   ├── merge_profiles.go     # 把档位文件那份合进 store 侧（只管 store：config 侧仍由 NewRegistry 加载，同名以 config 为准）
│   ├── applier.go            # "校验+探测"与"整份文件生效"的入口（api 的写端点只经这一个口子改执行面）
│   ├── register.go           # 把档位注册进调度器（Registrar 由调用方提供，不依赖具体实现）
│   ├── probe.go              # 可用性探测：程序在不在 PATH、脚本在不在，不启动任何进程
│   ├── args.go               # payload 解码、参数校验与命令行/URL 渲染（没有 shell，也没有内联源码）
│   ├── env.go                # 子进程环境重建：固定注入 + env_allow 白名单，排除 GODELAYQ_ 前缀
│   ├── proc.go               # 进程档位（script / binary）的执行主体：起进程、收输出、判超时
│   ├── proc_unix.go          # Unix 侧进程组信号与终止
│   ├── proc_windows.go       # Windows 侧整树终止（taskkill /T /F）与差异说明
│   ├── exit.go               # 退出码 / 信号 / HTTP 状态到失败原因的归类（含"重试无意义"判定）
│   ├── http.go               # HTTP 档位：拨号层禁内网与元数据地址、请求渲染、响应与头的落盘
│   ├── result.go             # 一次执行的内存形态：结论摘要 + 两条输出流 + 预览裁剪
│   ├── artifact.go           # 产物文件存储（a<n>.out / a<n>.err / meta.json）与 TTL、孤儿清理
│   ├── artifact_index.go     # 产物索引的可选依赖接口（ArtifactIndexer）：登记、标清理、启动对账
│   └── *_test.go             # 与上述文件一一对应的单元测试（含真实进程与跨平台夹具）
│
├── store/sqlite/             # 观测层存储（可选，一个 SQLite 文件；全仓唯一 import SQLite 驱动的包）
│   ├── db.go                 # 打开与迁移、WAL/busy_timeout/synchronous 三个 PRAGMA、SetMaxOpenConns(1)
│   ├── schema.go             # version 1 迁移：三张表与八个索引，逐字对齐设计文档 §6
│   ├── batch.go              # 三个写入器共用的有界批量器（非阻塞入队、合并事务、重试一次后丢弃计数）
│   ├── events.go             # job_events：事件写入器，同时是两个事件端点的读取方
│   ├── artifacts.go          # artifact_index：产物索引实现（写失败只记日志，文件仍是权威）
│   ├── audit.go              # write_audit：写操作台账的写入与查询（含两条保留淘汰）
│   ├── doc.go                # 包说明与依赖边界
│   └── *_test.go             # 往返、过滤分页、保留淘汰、队满丢弃与关闭幂等
│
├── examples/                 # 独立可运行示例
│   ├── demo1/                # 编程式提交与崩溃恢复
│   └── demo2/                # 目录加载器
│
├── dashboard/
│   └── index.html            # 旧单文件监控页的跳转页（能力已并入 web/ 控制台的实时页）
│
├── web/                      # Vue 3 控制台前端（npm 工程；-tags dashboard 时产物内嵌进二进制）
│   ├── vite.config.ts        # dev 代理 /api、/sse、/ws → :8080
│   ├── embed_dashboard.go    # //go:build dashboard：//go:embed all:dist 暴露 web.Dist
│   ├── embed_stub.go         # //go:build !dashboard：web.Dist 恒为 nil，构建不依赖 npm
│   ├── package.json          # 版本钉在设计文档 §4.1：Vite 7 / TS 5 / Pinia 3 / Router 4
│   └── src/
│       ├── api/              # types、client（401→刷新→重放一次）、auth、jobs、groups、events、admin、keys、stats
│       ├── stores/           # auth（sessionStorage + 单飞刷新）、realtime（WS + 200 条事件缓冲 + 订阅过滤）、toast
│       ├── plugins/          # query 装配、realtime-effects（事件 → Query 失效，debounce 500ms）
│       ├── router/           # 路由表派生菜单，守卫按 meta.minimumRole 拦截
│       ├── composables/      # usePermission 能力表、useCountdown、useJobEvents、useEventFeed
│       ├── components/       # layout（侧栏/顶栏/页头）、ui、jobs（表格/表单/筛选/时间线）、groups、dashboard
│       ├── views/            # 登录、概览、任务、任务详情、分组、实时、运维、设置（八页均已实现）
│       ├── display.ts        # 短 ID（取 UUIDv7 尾段）与时间格式化
│       └── styles/           # tokens.css：蓝白灰主题变量
│
├── configs/
│   └── config.example.yaml   # 运行配置模板（入库的就是这一份；本机那份含凭据、已 gitignore）
│
├── job_queue/                # 目录加载器的示例任务文件（demo2 监控此目录）
├── data/                     # 默认 store.path 的数据文件（仓库内是空占位）
│
└── docs/
    ├── api.md                # API 详细文档
    ├── deployment.md         # 部署与安全配置指南（含"启用观测层"一节）
    ├── example.md            # 用法示例
    ├── core-scheduler-heap-event-load-analysis.md  # 核心模块设计分析
    └── design/
        ├── web-console-design.md            # Web 控制台设计与权限矩阵
        ├── executor-design.md               # 执行层设计（档位、结果与产物）
        └── sqlite-observability-design.md   # 观测层设计（三张表、批量写入与端点）
            └── tasks/            # 按卡实施：executor/ 19 张（E01…E19）、sqlite/ 8 张（S01…S08）
```

仓库不提供 Makefile、Dockerfile、docker-compose.yml 与运维脚本；构建直接用
`go build -o godelayq-server ./cmd/server`，部署方式见 [部署文档](./docs/deployment.md)。

---

## 核心特性

### 1. 高性能调度

- **四叉堆数据结构**：比二叉堆减少约 50% 的层级，提升缓存命中率
- **O(log n) 操作复杂度**：插入、删除、更新均为对数时间
- **并发控制**：堆、调度器与注册表各由 RWMutex 保护，读路径（列表/统计/查找）不互斥
- **有界并发执行**：默认 100 个执行 worker + 等容量队列；到期风暴时调度循环阻塞入队形成背压，不会无限起协程，未执行任务保留在堆与存储中

### 2. 可靠性保障

- **持久化存储**：JSON 文件原子写入，崩溃后自动恢复；写入按 200ms 周期合并，崩溃时最多丢失一个周期的状态变更
- **终态留痕**：成功/失败的快照按 `history_limit` / `history_ttl` 有界保留，`GET /jobs?status=success|failed` 与统计里的 completed/failed 因此可读；设为 -1 可关闭留痕回到"完成即删"
- **至少一次执行**：失败自动重试，支持指数退避和最大重试限制；被关停打断的执行不计入失败与重试，会保持待处理状态等下次启动恢复
- **执行超时**：任务可配置 `timeout`，到期后 Handler 收到 `DeadlineExceeded`（需自行检查 ctx）
- **优雅关闭**：SIGTERM 信号处理，停止投递新任务、取消在途任务上下文并等待执行协程退出

### 3. 灵活的任务定义

- **多种触发方式**：延迟执行（Duration）、定时执行（Time）、周期执行（Cron，5 或 6 段）
- **三种提交入口**：REST API、任务文件目录、代码内直接 `Schedule`
- **执行器档位（默认关闭）**：档位注册成 `exec.<档位名>` 任务类型，到点直接跑脚本、跑已编译产物
  或发一个 HTTP 请求，不需要改代码；能执行什么由两份来源决定——配置的 `executors.commands`
  （改完要重启）与档位文件 `executors.profiles_path`（`executors.web_enabled` 打开后由 ops 档在
  控制台"档位"页增删改，**立即生效、活过重启**），两份合并时同名以配置为准。无论来自哪份，
  **都不提供自由命令行，也不接受任务里内联源码**。参数声明（必填、正则、`secret`）、可用性探测、
  独立执行池与输出产物存储都在 `executor/` 包里；开关与部署前提见 [部署文档](./docs/deployment.md) 的"开启执行器"。
- **上下文传递**：Handler 收到带 cancellation 与 timeout 的 `context.Context`
- **标识**：任务 ID 为 UUIDv7（毫秒时间戳前缀 + 随机后缀，可按字典序粗略排序）

### 4. 实时可观测性

- **WebSocket 推送**：任务状态变更实时推送到前端，客户端可用 `subscribe` 设置过滤条件
- **SSE 备选方案**：兼容不支持 WebSocket 的客户端，`event_types` 走服务端类型订阅、`job_types` 按任务名过滤
- **REST API 查询**：完整的任务生命周期管理接口，支持 `POST /jobs/batch` 单请求最多 100 条的批量提交（逐条独立，混合结果以 207 返回）
- **暂停与分组**：`POST /jobs/:id/pause|resume|force-pause` 三个动作，`GET /jobs?group=` 按分组过滤（空值=未分组），
  分组注册表走 `/api/v1/groups` 增删改查；改名/detach 由调度器连堆内条目一起改写，任务跑完不会把旧组名写回去
- **事件时间线**：`GET /jobs/:id/events` 与 `GET /events`。默认读进程内的环形缓冲（每任务 100 条、全局 500 条，
  重启即清空）；启用观测层的事件库后改读持久化的 `job_events`，重启后时间线仍有历史、一次响应最多 1000 条。
  响应里的 `note` 字段会说清这次读的是哪一路，最新一条最多晚一个 `flush_interval`（默认 200ms）
- **观测层（默认关闭）**：`observability.enabled` 打开后多写一个 SQLite 文件，把运行事件、执行输出索引与
  写操作台账落盘——事件跨重启留存、`GET /jobs/:id/artifacts` 按尝试列出输出尺寸与清理状态、
  `GET /admin/audit` 查"谁在什么时候改了什么"。它是加速器不是账本：不搬任务快照，写失败只记日志，
  关掉开关即回到只有日志与内存缓冲的原状。容量、备份与丢弃计数怎么看见 [部署文档](./docs/deployment.md) 的"启用观测层"
- **运维端点**：`GET /admin/runtime` 读 worker/队列/堆/缓冲占用，`POST /admin/scheduler/suspend|unsuspend`
  是维护窗口的调度总开关（进程内状态，重启自动解除）
- **控制台**：`web/` 的 Vue 控制台共八个页面（概览、任务列表、任务详情、分组、实时、运维、设置、登录）；
  带 `-tags dashboard` 构建时与 API 同源提供，旧 `dashboard/index.html` 只剩一个跳转页
- **结构化日志**：全进程 `log/slog`，级别与格式可配，HTTP 访问日志与 panic 堆栈同流

### 5. 接入层安全

- **控制台账号**：`server.auth.users` 声明账号（只存 bcrypt 哈希，用 `go run ./cmd/hashpassword` 生成），
  登录换 JWT：access token 默认 15 分钟、refresh token 默认 12 小时且一次一用；
  登出会把当前 access token 立即拉黑，不等它自然过期。账号增删需重启进程。
- **静态 token**：`server.auth.token` 保留给脚本与 CI，身份是 `machine`——
  能读写任务，但不能强制暂停、不能删组、不能用运维端点。
- **角色**：`viewer < operator < admin < ops`，路由级中间件把关；
  只有 `batch-ops` 的 `action=force-pause` 需要在处理器里判档（档位取决于请求体）。
  被权限层拒掉的请求额外记一条 warn，比翻 403 状态码好定位。
  前端的按钮隐藏只是体验，服务端 403 才是边界。
- **实时通道凭据**：浏览器 WebSocket/EventSource 无法带请求头，改用一次一用、5 秒过期的
  `?ticket=`；JWT 不允许出现在 URL 里（访问日志会记下 query）。
- **跨域与握手来源**：`server.cors.allow_origins` 同时约束 HTTP 与 WebSocket 握手来源
- **登录限流**：同一来源对同一账号 1 分钟内失败 5 次、对任意账号失败 20 次即 429
  （bcrypt 单次约 60-100ms，不限流的登录端点是免费的 DoS 开关）
- **静态产物免鉴权，也只到产物为止**：带前端的二进制里 `GET`/`HEAD` 的 `/`、`/assets/*`
  与 SPA 深链不需要凭据（登录页本身就在产物里）；`/api`、`/ws`、`/sse` 三个名字空间与
  一切写方法照旧要凭据，未知路径也不会被兜底成页面
- **边界**：无 HTTPS、无在线账号管理，需前置反代。写操作台账分两条出口：未启用观测层时只有结构化日志
  （`msg="write operation audited"`），启用后同时落 `write_audit` 表并可用 `GET /admin/audit` 查；
  两种出口都不记请求体与参数取值

### 6. 扩展能力

- **存储插件化**：`Store` 接口化，Redis/MySQL 等后端可自行实现（仓库内目前只有 JSON）
- **任务文件化**：支持通过文件系统提交任务，便于 CI/CD 集成

---

## 快速开始

### 环境要求

- Go 1.24+（与 `go.mod` 的 `go 1.24.13` 一致）
- Linux/macOS/Windows
- Node 20.19+（只在开发 `web/` 控制台时需要；Vite 7 的版本下限）

### 安装

```bash
# 克隆项目
git clone https://github.com/jiujuan/godelayq.git
cd godelayq

# 下载依赖
go mod download

# 准备本机配置（含凭据的那份不入库，仓库里只有模板）
cp configs/config.example.yaml configs/config.yaml

# 编译
go build -o godelayq-server ./cmd/server

# 运行
./godelayq-server

# 使用配置文件（留空则自动查找 configs/config.yaml，找不到就用代码默认值）
./godelayq-server -config=configs/config.yaml
```

`configs/config.yaml` 由 `.gitignore` 排除：它装着静态 token、JWT 签名密钥与控制台账号，
提交就等于把凭据推到公开仓库。入库的是同结构的 `configs/config.example.yaml`，
两份的键由 `core.TestExampleConfigMatchesLocal` 比对，改任意一份都要同步另一份。
不复制模板也能跑——文件不存在时进程用代码默认值，只是鉴权处于关闭状态。

### 前端控制台的两种部署形态

**开发形态**（默认）：`web/` 是独立的 npm 工程，开发期与 Go 进程分开跑，靠 Vite 代理同源访问后端：

```bash
# 终端 1：后端（默认 :8080）
go build -o godelayq-server ./cmd/server && ./godelayq-server

# 终端 2：前端
cd web
npm install
npm run dev        # http://localhost:5173，端口被占用时 Vite 自动顺延到 5174
```

页面一律用相对路径请求 `/api`、`/ws`，所以顺延端口同样能用。校验命令：
`npx vue-tsc --noEmit`（类型）与 `npm run build`（产物在 `web/dist`，已 gitignore）。
**依赖版本钉在设计文档 §4.1**（Vite 7 / TS 5 / Pinia 3 / vue-router 4），
`npm install` 时不要随手升到最新大版本。

**单二进制**：前端产物内嵌进 Go 二进制，一个进程同时提供 API 与控制台，
没有跨域、也不需要上面那条代理。**顺序必须先是 npm 再是 go**：

```bash
cd web && npm run build && cd ..        # 产出 web/dist
go build -tags dashboard -o godelayq-console ./cmd/server
./godelayq-console -config=configs/config.yaml   # 浏览器打开 http://localhost:8080/
```

`-tags dashboard` 让 `web/embed_dashboard.go` 的 `//go:embed all:dist` 生效；
没跑过 `npm run build` 时这一步会直接编译失败（`pattern all:dist: no matching files found`），
而不是给出一个没有页面的二进制。不带这个 tag 的构建里 `web.Dist` 恒为 `nil`，
服务端只提供 API——`go build ./...` 与全部测试因此完全不依赖 Node。
启动日志的 `embedded console enabled mount=/` 用来确认这个二进制带没带前端。

同源托管下的路径划分（详见 `docs/api.md` 的"控制台与静态托管"）：`/` 给 `index.html`
（`no-cache`），`/assets/*` 给带哈希的产物（`immutable`），其余 `GET` 且浏览器声明
`Accept: text/html` 的路径回落 `index.html` 由前端路由接管；`/api`、`/ws`、`/sse`
三个名字空间的行为与不带前端时完全一致。启用鉴权时静态产物免凭据（登录页也在产物里），
写方法与三个名字空间照旧要凭据。

### 配置项

配置解析在 `core/config.go`，只收录当前真正生效的字段，写入未知键会直接报错。
每项都可被环境变量覆盖，前缀 `GODELAYQ_`、层级用下划线连接（如 `GODELAYQ_SERVER_PORT=9090`）。

```yaml
server:
  port: "8080"                # HTTP 监听端口
  auth:
    token: ""                 # 静态机器凭据；留空即不启用
    jwt:
      secret: ""              # HS256 密钥，≥32 字节；用 GODELAYQ_SERVER_AUTH_JWT_SECRET 注入
      access_ttl: 15m         # 访问令牌有效期；0 用默认值
      refresh_ttl: 12h        # 刷新令牌有效期；0 用默认值
    users: []                 # 控制台账号：name + password_bcrypt + role（viewer|operator|admin|ops）
  cors:
    allow_origins: ["*"]      # 跨域来源白名单，可写具体 origin 列表
    allow_credentials: false  # 与 "*" 互斥
scheduler:
  workers: 100                # 并发执行协程数；0 表示 core.DefaultConcurrency
  queue_capacity: 0           # 执行队列容量；0 表示与 workers 相等
  max_retry_delay: 30m        # 指数退避的单次重试延迟上限
  shutdown_timeout: 5s        # 优雅关闭等待时长
store:
  type: json                  # 目前仅支持 json
  path: ./data/jobs.json
  flush_interval: 200ms       # 合并落盘周期
  history_limit: 1000         # 终态快照留痕条数；-1 表示不留痕
  history_ttl: 0s             # 终态快照保留时长，如 24h；0 不按时间淘汰
  groups_path: ./data/groups.json  # 分组注册表文件（/api/v1/groups 读写它）
logging:
  level: info                 # debug|info|warn|error
  format: text                # text|json（输出固定为标准输出）
executors:
  enabled: false              # 总开关；打开等于把"能提交任务"扩成"能执行命令"，必须先配鉴权
  required_role: admin        # 提交 exec.* 任务与读取其含凭据档位的产物正文所需的最低角色
  workspace: ./exec-workspace # 脚本与产物的根目录，档位路径不得越出它
  runtime_allow: [bash, sh, cmd, pwsh, node, php, python, java]
  env_allow: [PATH, LANG, LC_ALL, TZ, HOME]  # GODELAYQ_ 前缀的键启动即拒（服务凭据在进程环境里）
  concurrency: 4              # 执行器专用池的协程数，与 scheduler.workers 分池
  queue_capacity: 0           # 执行器队列容量；0 表示与本节 concurrency 相等
  default_timeout: 5m         # 档位没写 timeout 时的单次执行超时
  max_timeout: 30m            # 超时上限：档位或任务请求超过它直接被拒
  restore_policy: pause       # 崩溃后正在跑的执行器任务：pause 等人确认 / replay 重跑
  loader_allow: false         # 目录加载器是否接受 exec.* 任务文件（服务端不启用加载器，详见部署文档）
  web_enabled: false          # 档位的在线管理：打开后 ops 档可在控制台增删改档位文件（必须同时 enabled: true）
  profiles_path: ./data/exec-profiles.json  # 页面建的档位存这里；与 executors.commands 是两份来源，同名以配置为准
  output:
    inline_preview: 2048      # 事件与任务详情里带的尾部预览字节数
    max_bytes: 262144         # 单条流的落盘上限，达到即标记截断
    dir: ./data/exec          # 产物目录，与 store.path 分开放，便于单独设权限与清理
    ttl: 168h                 # 产物保留时长；0 表示只做起动时的孤儿清理
  # 档位列表：脚本 / 已编译产物 / HTTP 三种，字段见模板注释与设计文档 §5。
  # 与 server.auth.users 同理，这里不写 []（解开示例即可启用；写 [] 再挂列表项会让 YAML 报错）。
  commands:
observability:
  enabled: false              # 总开关；false 时不建库文件、不订阅事件总线、三个注入全部为空，行为与本节之前一致
  path: ./data/observe.sqlite # 观测库文件；父目录不存在时由启动装配创建，WAL 会另生 -wal/-shm 旁文件
  flush_interval: 200ms       # 三个写入器共用的合并落盘周期；同时是事件端点看到最新一条的延迟上界
  queue_capacity: 4096        # 每个写入器的有界队列容量；队满丢弃新记录并计数，绝不阻塞调度或请求
  busy_timeout: 2s            # SQLite 锁等待上限
  synchronous: normal         # normal（默认，断电最多丢最后几个已提交事务）| full（每次提交等磁盘同步）
  events:
    enabled: true             # job_events 表：把 job.* 事件持久化，重启后时间线仍有历史
    retention_count: 200000   # 保留条数，超出按写入顺序淘汰最旧
    retention_age: 720h       # 保留时长；0 表示不按时间淘汰
  artifacts:
    enabled: true             # artifact_index 表：按任务与尝试记录输出尺寸、截断与清理状态
  audit:
    enabled: true             # write_audit 表：每个写请求一行台账（谁、什么身份、做了什么、成没成、多快）
    retention_count: 500000   # 保留条数；高频建任务的部署会先撞到条数上界而不是天数
    retention_age: 2160h      # 保留时长（默认 90 天）；0 表示不按时间淘汰
```

> 观测层的三个子开关只在总开关为真时生效，代价与备份口径见 [部署文档](./docs/deployment.md) 的"启用观测层"，
> 表结构与读端点见 [观测层设计文档](./docs/design/sqlite-observability-design.md)。

配置 `server.auth.token` 或 `server.auth.users` 任一后，除两个登录入口
（`POST /api/v1/auth/login`、`POST /api/v1/auth/refresh`）外全部端点都要凭据，
含 `/ws`、`/sse/events`、`/api/v1/health`；支持 `Authorization: Bearer <jwt|token>` 与 `X-Auth-Token`，
实时通道另可用一次一用的 `?ticket=`，静态 token 另可用 `?token=`。
`server.cors.allow_origins` 控制跨域来源，同时约束 WebSocket 握手 `Origin`。
默认不启用鉴权，公网或多团队环境必须显式配置，详见 [部署文档](./docs/deployment.md)。

日志统一走标准库 `log/slog`：调度器、存储、加载器、WebSocket 与 HTTP 访问日志
共享同一级别与格式，可按 `logging.*` 配置或注入自定义 `*slog.Logger`。

完整说明与取舍见 [部署文档](./docs/deployment.md)。

### 容器化

仓库不再提供 Dockerfile 与 docker-compose.yml。需要容器部署时，用
`go build -o godelayq-server ./cmd/server` 产出二进制自行打镜像，
把数据目录挂到 `-config` 里 `store.path` 指向的路径即可；
裸机/虚机的 systemd 部署、反向代理与安全加固见 [部署文档](./docs/deployment.md)。

## 其它文档

- [API文档](./docs/api.md)
- [用法示例](./docs/example.md)
- [部署文档](./docs/deployment.md)
- [核心模块设计分析](./docs/core-scheduler-heap-event-load-analysis.md)
- [Web 控制台设计文档](./docs/design/web-console-design.md)（`web/` 的技术选型、页面与后端改造方案，含里程碑进度）
- [执行器设计文档](./docs/design/executor-design.md)（shell/脚本/HTTP 执行层的白名单档位、结果通道与权限模型；**已实现（M0–M5），与实现的偏差在该文正文的 ⚠️ 标注里**）
  - 实施拆分：[执行器任务卡 TASK-E01 … E19](./docs/design/tasks/executor/README.md)
- [档位在线管理设计文档](./docs/design/web-profile-design.md)（把"改档位要重启"改成"控制台可增删改、立即生效、活过重启"：第二份档位来源、运行期可变的登记表、ops 档写端与控制台页面；**已实现（W01–W08）**，安全收口五条 S-1…S-5 只登记未实施）
  - 实施拆分：[档位在线管理任务卡 TASK-W01 … W09](./docs/design/tasks/web-profile/README.md)
- [观测层设计文档](./docs/design/sqlite-observability-design.md)（可选的 SQLite 观测层：运行事件、产物索引与写操作审计三张表；**已实现，与设计不同处逐条标在该文 §15**）
  - 实施拆分：[观测层任务卡 TASK-S01 … S08](./docs/design/tasks/sqlite/README.md)