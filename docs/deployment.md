## 生产环境配置

```yaml
# config.yaml
server:
  port: 8080
  read_timeout: 10s
  write_timeout: 30s
  
scheduler:
  workers: 100              # 并发执行协程数
  max_retries: 5
  default_retry_delay: 1m
  
store:
  type: json               # json/redis/mysql
  path: /var/lib/godelayq/jobs.json
  # redis:
  #   addr: localhost:6379
  #   db: 0
  
loader:
  enabled: true
  dir: /var/spool/godelayq
  pattern: "*.json"
  action: archive          # delete/archive/keep
  archive_dir: /var/spool/godelayq/archive
  
websocket:
  max_connections: 10000
  buffer_size: 256
  
logging:
  level: info              # debug/info/warn/error
  format: json             # json/text
  output: /var/log/godelayq/app.log
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