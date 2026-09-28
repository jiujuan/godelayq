## 生产环境配置

可配置项定义在 `core/config.go`，通过 `-config` 指定文件路径；留空时按 `configs/config.yaml`
自动查找，文件不存在则使用代码默认值。任何键都可以用环境变量覆盖，前缀 `GODELAYQ_`、
层级用下划线连接（如 `GODELAYQ_SERVER_PORT=9090`、`GODELAYQ_SCHEDULER_WORKERS=32`）。

```bash
./godelayq-server -config=/etc/godelayq/config.yaml
```

```yaml
# config.yaml
server:
  port: "8080"                # HTTP 监听端口
  auth:
    token: ""                 # 留空即不启用鉴权；生产必须设置（建议用环境变量注入）
  cors:
    allow_origins: ["*"]      # 跨域来源白名单；["*"] 或留空表示任意来源
    allow_credentials: false  # 与 "*" 互斥，开启前先写具体 origin

scheduler:
  workers: 100                # 并发执行协程数；0 表示 core.DefaultConcurrency
  queue_capacity: 0           # 执行队列容量；0 表示与 workers 相等
  max_retry_delay: 30m        # 指数退避的单次重试延迟上限
  shutdown_timeout: 5s        # 优雅关闭等待时长

store:
  type: json                  # 目前仅支持 json
  path: /var/lib/godelayq/jobs.json
  flush_interval: 200ms       # 合并落盘周期；崩溃时最多丢失一个周期的状态
  history_limit: 1000         # 终态快照留痕条数；0 用默认值，-1 不留痕
  history_ttl: 0s             # 终态快照保留时长，如 24h；0 不按时间淘汰
  groups_path: /var/lib/godelayq/groups.json  # 分组注册表（改动即同步落盘）

logging:
  level: info                 # debug|info|warn|error
  format: text                # text|json
```

任务状态与分组是**两个文件**：前者按 `flush_interval` 合并写盘（崩溃最多丢一个周期），
后者每次改动同步原子重写。备份或迁移时两个都要带走——只带 `jobs.json` 的话，
任务的 `group` 标签还在，但分组的描述与颜色没了。

两点设计取舍，配置校验会直接拒绝未知键，因此不要照抄旧文档里的其它字段：

- **没有 `read_timeout` / `write_timeout`**：`http.Server` 的这两个超时是按连接生效的，
  而 `/ws`（hijack 后长连接）与 `/sse/events`（持续写）会被它们掐断。
  服务器只固定设置 `ReadHeaderTimeout: 10s`。
- **`loader` / `websocket.max_connections` 尚未实现为配置项**：
  目录加载器需在代码中显式创建（见 `core/load.go`），WebSocket 发送缓冲固定 256 条。

### 日志

全进程统一使用标准库 `log/slog`，输出到**标准输出**（systemd/docker 可直接采集），
由 `logging.level` 与 `logging.format` 控制：

- `level`：低于该级别的记录不写出。排障时临时设 `debug` 可见目录加载器等细粒度日志。
- `format`：`text` 适合人读；`json` 适合日志采集端建索引（每行一个 JSON 对象）。
- 非法取值在启动时就报错，不会静默退回默认。

级别约定：任务生命周期与访问请求为 `info`，4xx 请求、重试排队、发送缓冲丢弃等
可恢复异常为 `warn`，落盘失败、执行失败、panic 为 `error`。
HTTP 访问日志与 panic 恢复由 `api/logging.go` 的中间件产出，替代了 gin 自带日志，
因此 `GIN_MODE` 的 debug 启动横幅仍会打印，但每条请求只走 slog 一份。

各组件（调度器、存储、加载器、WebSocket、API）都可接收注入的 `*slog.Logger`，
未注入时回退到 `slog.Default()`；`cmd/server` 启动时会把按配置构建的日志器设为进程默认，
因此自定义 Handler 里直接用 `slog.Info(...)` 即可与主日志同格式。

### 终态留痕与容量

任务执行成功后，快照不再被直接删除，而是与最终失败记录一起作为终态留痕保存，
`GET /jobs?status=success|failed`、`POST /jobs/:id/retry` 与统计里的 completed/failed 都依赖它。
淘汰只作用于终态记录（`pending`/`running` 永不回收），在每次写入终态快照时顺带完成：

- `history_limit`：保留最近 N 条（按 `updated_at` 排序）。默认 1000，写 `0` 等价默认，
  写 `-1` 表示完全不留痕——回到"完成即删"，此时 completed/failed 计数与历史查询都为空。
- `history_ttl`：超过该时长的终态记录被清掉，`0` 表示不按时间淘汰。

存储是一个 JSON 对象文件，整文件重写，因此留痕条数直接决定文件大小与每次刷盘的开销：
1000 条约几百 KB 到 1 MB 量级。任务量大又只需观测当前状态时，把 `history_limit` 调小、
或配合 `history_ttl` 限定窗口；不需要历史时设 `-1`。

注意两点边界：Cron 重复任务沿用同一 ID，每轮成功都会被下一轮的 `pending` 覆盖，
所以看不到逐轮历史；进程重启后 `uptime` 归零，但 completed/failed 会随留痕记录一并恢复。

### 接入层安全（账号、token 与跨域）

**默认是敞开的**：不配任何凭据时，任何能连上端口的主机都能创建/取消任务并订阅全部事件，
`allow_origins` 默认 `*`，任意网页也能跨域调用。公网或多人环境至少做四件事：

1. 启用鉴权。两种凭据可以并存：
   - **控制台账号** `server.auth.users`：只存 bcrypt 哈希（`go run ./cmd/hashpassword` 生成，
     cost 至少 10），配 `server.auth.jwt.secret` 签发 JWT。哈希写进配置文件是可以的（它不可逆），
     **签名密钥不要落盘**：用 `GODELAYQ_SERVER_AUTH_JWT_SECRET` 注入，systemd 下放进
     `EnvironmentFile=/etc/godelayq/auth.env`（权限 0600）。轮换密钥会让全部已发令牌立即失效，
     所有人都要重新登录。账号本身不支持环境变量覆盖，增删账号需重启进程。
   - **静态 token** `server.auth.token`：给脚本与 CI 用的全局口令，身份是 `machine`
     （能读写任务，没有 admin/ops 能力）。同样优先用 `GODELAYQ_SERVER_AUTH_TOKEN` 注入。
2. 按人分配角色（`viewer`/`operator`/`admin`/`ops`）。最小可用的一组通常是：一个 `ops` 给值班、
   若干 `operator` 给业务、`viewer` 给只看监控的人。机器凭据不给 admin 能力，
   需要自动化做强制暂停/删组时，请为脚本单建一个 `admin` 账号并单独保管其口令。
3. 收紧跨域。把 `allow_origins` 写成实际前端地址列表；需要 Cookie 时配
   `allow_credentials: true`（此时不允许 `*`，否则启动报错）。
4. 前置 TLS。本服务不内置 HTTPS，用 Nginx/负载均衡终结证书后回环转发（见下文反向代理）。
   登录限流按 `c.ClientIP()` 计数，反代必须正确传 `X-Forwarded-For`，
   否则所有请求会被算成同一个来源 IP（详见 gin 的代理信任配置）。

已知边界：

- 令牌与登录态都在内存里。**进程重启 = 所有人重新登录**，登出拒绝表也随之清空
  （重启后旧 access token 依然过不了验签，因为 refresh 表没了、且部署本身已被认为可信边界内）。
- 浏览器页面的 WS/SSE 无法自定义请求头。控制台走一次一用、5 秒过期的 `?ticket=`；
  机器凭据仍可用 `?token=`。后者会出现在访问日志与浏览器历史里，
  暴露给不可信网络前请在反向代理层关掉 `/ws`、`/sse/events` 的 query 日志，或只在内网开放。
- 健康检查 `/api/v1/health` 在保护范围内，探针需要带凭据。
- 写操作只有结构化日志，没有可查询的审计存储；要留证据链请收集 stdout 日志（见下文日志与观测）。

数据目录需提前创建并保证进程可写：

```bash
mkdir -p /var/lib/godelayq && chown godelayq:godelayq /var/lib/godelayq
```

## Systemd 服务配置

```ini
# /etc/systemd/system/godelayq.service
[Unit]
Description=godelayq Delayed Task Scheduler
After=network.target

[Service]
Type=simple
User=godelayq
Group=godelayq
WorkingDirectory=/opt/godelayq
ExecStart=/opt/godelayq/godelayq-server -config=/etc/godelayq/config.yaml
# token 不落盘进 config.yaml，单独放 0600 权限的环境文件
EnvironmentFile=-/etc/godelayq/token.env
Restart=always
RestartSec=5

# 资源限制
LimitNOFILE=65535
MemoryLimit=2G

# 优雅关闭
TimeoutStopSec=30
KillSignal=SIGTERM

[Install]
WantedBy=multi-user.target
```

收到 `SIGTERM` 后进程按序关停：停止接受新连接 → 取消所有请求上下文（SSE 长连接随即返回）→
关闭全部 WebSocket 客户端并等待读写协程退出 → 停调度器（取消在途任务的 `context`，等待 worker 返回）→
最后一次落盘。

其中**只有 HTTP/长连接这一段受 `scheduler.shutdown_timeout`（默认 5s）约束**：超时后强制关闭残留连接并继续收尾。
`Scheduler.Stop()` 没有自己的超时——它依赖 Handler 检查传入的 `context`；
一个无视取消的处理器会把关停无限期挂住，此时由 systemd 的 `TimeoutStopSec` 发 `SIGKILL` 兜底，
因此 `TimeoutStopSec` 要留得比 `shutdown_timeout` 宽裕，并按最坏的 Handler 收尾时间设定。

启用服务：

```shell
sudo systemctl daemon-reload
sudo systemctl enable godelayq
sudo systemctl start godelayq
sudo systemctl status godelayq
```

## Nginx 反向代理（SSL）

```shell
upstream godelayq {
    server 127.0.0.1:8080;
    keepalive 32;
}

server {
    listen 443 ssl;
    http2 on;                       # nginx 1.25+；旧版仍写 listen 443 ssl http2
    server_name scheduler.example.com;

    ssl_certificate /path/to/cert.pem;
    ssl_certificate_key /path/to/key.pem;

    # SSE：必须关掉代理缓冲，否则事件会攒在 nginx 里不往外发
    location /sse/ {
        proxy_pass http://godelayq;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header Connection "";
        proxy_buffering off;
        proxy_cache off;
        chunked_transfer_encoding on;
        read_timeout 0;             # 长连接不主动掐断
        access_log off;             # 避免 ?token= 进访问日志
    }

    location / {
        proxy_pass http://godelayq;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_read_timeout 86400;   # WebSocket 长连接
        access_log off;             # /ws 同理：握手带 ?token= 时不要落日志
    }
}
```

服务端只看 `Origin` 头做握手来源校验，因此 `allow_origins` 要写**面板自己的 origin**
（如 `https://scheduler.example.com`），不要写成上游地址。