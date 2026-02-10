API 文档

## 基础信息

Base URL: http://localhost:8080/api/v1
Content-Type: application/json
字符编码: UTF-8

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
  "retry_delay": "5m"
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
| max\_retries | int    | ❌  | 最大重试次数，默认 3                                         |
| retry\_delay | string | ❌  | 基础重试间隔，默认 "1m"                                      |


**响应**：

```json
{
  "id": "job_1704182400_a1b2c3d4",
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
| status | string | 过滤状态：pending/running/success/failed/cancelled |
| name   | string | 按任务类型过滤                                       |
| limit  | int    | 分页大小，默认 50，最大 100                             |
| offset | int    | 分页偏移                                          |


响应：

```json
{
  "total": 156,
  "items": [
    {
      "id": "job_1704182400_a1b2c3d4",
      "name": "payment_check",
      "status": "pending",
      "trigger_at": "2024-01-02T15:30:00+08:00",
      "next_run_in": "5m30s",
      "retry_count": 0,
      "max_retries": 3,
      "is_repeat": false,
      "created_at": "2024-01-02T15:20:00+08:00"
    }
  ]
}
```

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
  "max_retries": 5
}
```

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

## WebSocket 实时通信

连接地址: ws://localhost:8080/ws

**协议说明**

客户端发送 JSON 消息进行订阅控制：

```json
// 订阅特定事件
{
  "action": "subscribe",
  "filter": {
    "job_types": ["payment_check", "email_send"],
    "event_types": ["job.scheduled", "job.started", "job.completed", "job.failed"],
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
```

服务端推送事件


```json
// 任务已调度
{
  "type": "job.scheduled",
  "job_id": "job_1704182400_a1b2c3d4",
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
  "job_id": "job_1704182400_a1b2c3d4",
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
  "job_id": "job_1704182400_a1b2c3d4",
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
  "job_id": "job_1704182400_a1b2c3d4",
  "job_name": "payment_check",
  "status": 3,
  "timestamp": "2024-01-02T15:30:01+08:00",
  "data": {
    "error": "connection timeout"
  },
  "metadata": {
    "retry_count": 1,
    "max_retries": 3
  }
}

// 任务重试中
{
  "type": "job.retrying",
  "job_id": "job_1704182400_a1b2c3d4",
  "job_name": "payment_check",
  "status": 0,
  "timestamp": "2024-01-02T15:30:01+08:00",
  "metadata": {
    "next_retry_at": "2024-01-02T15:35:00+08:00",
    "retry_count": 1
  }
}

```