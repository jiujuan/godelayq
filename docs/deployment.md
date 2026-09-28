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

scheduler:
  workers: 100                # 并发执行协程数；0 表示 core.DefaultConcurrency
  queue_capacity: 0           # 执行队列容量；0 表示与 workers 相等
  max_retry_delay: 30m        # 指数退避的单次重试延迟上限
  shutdown_timeout: 5s        # 优雅关闭等待时长

store:
  type: json                  # 目前仅支持 json
  path: /var/lib/godelayq/jobs.json
  flush_interval: 200ms       # 合并落盘周期；崩溃时最多丢失一个周期的状态
```

两点设计取舍，配置校验会直接拒绝未知键，因此不要照抄旧文档里的其它字段：

- **没有 `read_timeout` / `write_timeout`**：`http.Server` 的这两个超时是按连接生效的，
  而 `/ws`（hijack 后长连接）与 `/sse/events`（持续写）会被它们掐断。
  服务器只固定设置 `ReadHeaderTimeout: 10s`。
- **`loader` / `websocket.max_connections` / `logging` 尚未实现为配置项**：
  目录加载器需在代码中显式创建（见 `core/load.go`），WebSocket 缓冲区固定 256 条，
  日志仍是标准库 `log` 输出到 stderr。

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
关闭全部 WebSocket 客户端并等待读写协程退出。整个流程由 `cmd/server` 的 5 秒超时兜底，
超时后强制关闭残留连接，因此 `TimeoutStopSec` 无需大于该值。

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
    listen 443 ssl http2;
    server_name scheduler.example.com;
    
    ssl_certificate /path/to/cert.pem;
    ssl_certificate_key /path/to/key.pem;
    
    location / {
        proxy_pass http://godelayq;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_read_timeout 86400;  # WebSocket 长连接
    }
}
```