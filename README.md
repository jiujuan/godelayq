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
│                      Persistence Layer (持久化层)                 │
│  ┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐  │
│  │  JSON File Store│  │  (*可扩展: Redis) │  │  (*可扩展: MySQL) │  │
│  │  • 原子写入      │  │  • 分布式锁      │  │  • 事务支持      │  │
│  │  • 崩溃恢复      │  │  • 集群支持      │  │  • 复杂查询      │  │
│  └─────────────────┘  └─────────────────┘  └─────────────────┘  │
└─────────────────────────────────────────────────────────────────┘
```

> * 暂时没有实现，持久化层的 可扩展: Redis 与 可扩展: MySQL 还没实现

### 数据流图

```shell

[任务提交] ──► [API/文件/程序] ──► [四叉堆调度] ──► [时间到达] ──► [执行Handler]
│                │                  │              │
│                │                  ▼              ▼
│                │           [持久化存储]    [事件总线] ──► [WebSocket推送]
│                │                  │              │
│                └──────────────────┘              ▼
│                                         [重试/Cron/归档]
▼
[立即返回任务ID]
```

### 核心文件详解

| 文件 | 作用 | 关键设计 |
|------|------|----------|
| `heap.go` | 四叉堆实现 | 索引映射实现按 ID 的 O(1) 定位，四叉 sift 与 `Update`/`PopIfDue` |
| `scheduler.go` | 调度器引擎 | 堆顶定时器唤醒（非忙等待），有界 worker 池 + 队列背压，优雅关闭与崩溃恢复 |
| `event.go` | 事件驱动架构 | 发布-订阅；订阅缓冲满时丢弃事件而非阻塞调度主流程 |
| `websocket.go` | 实时通信 | 心跳保活、按事件类型/任务名过滤订阅；只依赖 `WSConn`/`WSUpgrader` 接口，协议库由上层注入（重连属客户端能力） |
| `load.go` | 文件任务加载 | fsnotify 监控 + 100ms 静默窗口合并写入事件，加载后删除/归档，非法文件可隔离到 `error_dir` |
| `config.go` | 运行配置 | viper 读 yaml + `GODELAYQ_*` 环境变量，未知键与非法取值启动即报错 |
| `logging.go` | 日志装配 | 标准库 `log/slog`，`NewLogger` 按级别/格式构造，组件经 `WithLogger` 注入 |


## 目录结构

```shell
godelayq/
├── README.md                 # 项目文档（本文件）
├── go.mod / go.sum           # 模块定义与依赖校验
├── .gitignore
│
├── cmd/                      # 可执行程序入口
│   └── server/
│       ├── main.go           # 服务器主程序（配置 → 存储 → 调度器 → API → 信号）
│       ├── main_test.go
│       └── main_integration_test.go
│
├── core/                     # 核心库（不依赖任何 web 框架）
│   ├── heap.go               # 四叉堆（索引映射、Update、PopIfDue）
│   ├── job.go                # 任务定义、状态、快照与 CloneForRetry
│   ├── scheduler.go          # 调度器：堆 + worker 池 + 取消表 + 事件总线
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
│   ├── server.go             # 路由、中间件、优雅关闭
│   ├── handlers.go           # REST 处理器实现
│   ├── dto.go                # 请求/响应数据结构
│   ├── security.go           # Token 鉴权与跨域来源策略
│   ├── logging.go            # 访问日志与 panic 恢复中间件（slog）
│   ├── websocket.go          # gorilla/websocket 适配器
│   ├── sse.go                # Server-Sent Events
│   └── *_test.go             # 契约、鉴权、关闭与流式测试
│
├── examples/                 # 独立可运行示例
│   ├── demo1/                # 编程式提交与崩溃恢复
│   └── demo2/                # 目录加载器
│
├── dashboard/
│   └── index.html            # 单文件监控页（浏览器直接打开，可填 token）
│
├── configs/
│   └── config.yaml           # 运行配置样例
│
├── job_queue/                # 目录加载器的示例任务文件（demo2 监控此目录）
├── data/                     # 默认 store.path 的数据文件（仓库内是空占位）
│
└── docs/
    ├── api.md                # API 详细文档
    ├── deployment.md         # 部署与安全配置指南
    ├── example.md            # 用法示例
    └── core-scheduler-heap-event-load-analysis.md  # 核心模块设计分析
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
- **上下文传递**：Handler 收到带 cancellation 与 timeout 的 `context.Context`
- **标识**：任务 ID 为 UUIDv7（毫秒时间戳前缀 + 随机后缀，可按字典序粗略排序）

### 4. 实时可观测性

- **WebSocket 推送**：任务状态变更实时推送到前端，客户端可用 `subscribe` 设置过滤条件
- **SSE 备选方案**：兼容不支持 WebSocket 的客户端，`event_types` 走服务端类型订阅、`job_types` 按任务名过滤
- **REST API 查询**：完整的任务生命周期管理接口，支持 `POST /jobs/batch` 单请求最多 100 条的批量提交（逐条独立，混合结果以 207 返回）
- **监控页**：`dashboard/index.html` 是单文件页面，用浏览器直接打开即可连 `/ws` 与统计接口（不由服务端托管，需自行处理跨域或同源部署）
- **结构化日志**：全进程 `log/slog`，级别与格式可配，HTTP 访问日志与 panic 堆栈同流

### 5. 接入层安全

- **静态 token**：`server.auth.token` 覆盖全部端点（含 `/ws`、`/sse/events`、`/health`），支持 Bearer / `X-Auth-Token` / `?token=`
- **跨域与握手来源**：`server.cors.allow_origins` 同时约束 HTTP 与 WebSocket 握手来源
- **边界**：只有一个全局口令，无角色、无过期；不内置 HTTPS 与限流，需前置反代

### 6. 扩展能力

- **存储插件化**：`Store` 接口化，Redis/MySQL 等后端可自行实现（仓库内目前只有 JSON）
- **任务文件化**：支持通过文件系统提交任务，便于 CI/CD 集成

---

## 快速开始

### 环境要求

- Go 1.24+（与 `go.mod` 的 `go 1.24.13` 一致）
- Linux/macOS/Windows

### 安装

```bash
# 克隆项目
git clone https://github.com/jiujuan/godelayq.git
cd godelayq

# 下载依赖
go mod download

# 编译
go build -o godelayq-server ./cmd/server

# 运行
./godelayq-server

# 使用配置文件（留空则自动查找 configs/config.yaml，找不到就用代码默认值）
./godelayq-server -config=configs/config.yaml
```

### 配置项

配置解析在 `core/config.go`，只收录当前真正生效的字段，写入未知键会直接报错。
每项都可被环境变量覆盖，前缀 `GODELAYQ_`、层级用下划线连接（如 `GODELAYQ_SERVER_PORT=9090`）。

```yaml
server:
  port: "8080"                # HTTP 监听端口
  auth:
    token: ""                 # 留空即不启用鉴权（默认，便于本地开发）
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
logging:
  level: info                 # debug|info|warn|error
  format: text                # text|json（输出固定为标准输出）
```

设置 `server.auth.token` 后，全部端点（含 `/ws`、`/sse/events`、`/api/v1/health`）都要凭据，
支持 `Authorization: Bearer <token>`、`X-Auth-Token` 与浏览器专用的 `?token=`；
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