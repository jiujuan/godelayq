## 示例 1：电商订单超时取消

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"godelayq/core"
	"godelayq/api"
)

func main() {
	// 初始化组件
	store, err := godelayq.NewJSONFileStore("./data/jobs.json")
	if err != nil {
		log.Fatal(err)
	}
	
	// 创建调度器
	scheduler := godelayq.NewScheduler(store, &godelayq.ExponentialBackoffRetry{
		MaxDelay: 30 * time.Minute,
	}, nil)
	
	// 创建API服务器
	server := api.NewServer(scheduler, store, "8080")
	
	// 注册订单超时处理器
	server.RegisterJobHandler("order_timeout_cancel", func(ctx context.Context, job *godelayq.Job) error {
		var payload struct {
			OrderID string  `json:"order_id"`
			Amount  float64 `json:"amount"`
			UserID  string  `json:"user_id"`
		}
		
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return fmt.Errorf("invalid payload: %w", err)
		}
		
		// 调用订单服务检查状态
		if err := cancelOrderIfUnpaid(payload.OrderID); err != nil {
			return fmt.Errorf("cancel failed: %w", err)
		}
		
		log.Printf("Order %s cancelled successfully", payload.OrderID)
		return nil
	})
	
	// 启动服务
	scheduler.Start()
	defer scheduler.Stop()
	
	if err := server.Start(); err != nil {
		log.Fatal(err)
	}
	
	// 保持运行
	select {}
}

func cancelOrderIfUnpaid(orderID string) error {
	// 实现订单取消逻辑
	// 调用数据库或订单服务API
	return nil
}
```

通过 API 提交任务：


```shell
curl -X POST http://localhost:8080/api/v1/jobs \
  -H "Content-Type: application/json" \
  -d '{
    "name": "order_timeout_cancel",
    "delay": "30m",
    "payload": {
      "order_id": "ORD-20240115-001",
      "amount": 299.99,
      "user_id": "U123456"
    },
    "max_retries": 3
  }'

```

## 示例 2：文件任务批量提交

创建任务文件 ./jobs/bulk_orders.json：

```json
{
  "id": "bulk_cancel_001",
  "name": "order_timeout_cancel",
  "delay": "15m",
  "timeout": "30s",
  "payload": {
    "order_id": "ORD-20240115-002",
    "amount": 199.99,
    "user_id": "U789012"
  },
  "max_retries": 2,
  "retry_delay": "5m"
}
```

字段约定：

- `name` **必填**：既是任务名，也是 Handler 查找键（`type` 为空时回退到它）。
  留空（或只有空白）的文件在加载即判为格式错误，会记 `error` 日志并在配置了
  `error_dir` 时归档到 `<文件名>.error`，不会进入调度队列。
- `id` 可省略，省略时由调度器生成 UUIDv7。
- `trigger_at`（RFC3339 绝对时间）与 `delay`（`time.ParseDuration`）二选一，
  前者优先；两者都不写则 1 秒后立即执行。
- `max_retries`：不写取默认 3；**写 `0` 就是不重试**（历史版本会把 0 抬回 3，
  现在按字面值处理，与 `POST /jobs` 一致）；负数报错。
- `timeout` 可选，是单次执行超时（`time.ParseDuration` 格式）；省略则不限制。
- 文件里的 `tags`、`description` 一类的额外键不再被解析（此前也是解析后即丢弃），
  加载时会被忽略，不影响任务入队。

Handler 必须检查传入的 `ctx`，否则超时只能被记录为失败、无法真正中止执行。

配置目录加载器自动监控：

```go
loader, err := godelayq.NewDirectoryLoader(scheduler, godelayq.LoaderOptions{
    Dir:            "./jobs",
    Pattern:        "*.json",
    PostLoadAction: godelayq.ArchiveAfterLoad,
    ArchiveDir:     "./jobs/archive",
    EnableWatcher:  true,  // 实时监控新文件
    Logger:         logger, // 可选，nil 用 slog.Default()
})
if err != nil {
    log.Fatal(err)
}

if err := loader.Start(); err != nil {
    log.Fatal(err)
}
defer loader.Stop()
```

监控模式下同一文件的连续写入会合并成一次加载：事件到达后等 100ms 静默窗口，
窗口内的重复事件只排一次队，因此读到最后一次写入的内容，也不会读到半截 JSON。
`Stop()` 会取消尚未触发的排队加载。
## 示例 3：实时监控 Dashboard

前端 JavaScript 连接 WebSocket：

```javascript
const ws = new WebSocket('ws://localhost:8080/ws');

ws.onopen = () => {
    console.log('Connected to godelayq');
    
    // 订阅支付相关任务
    ws.send(JSON.stringify({
        action: 'subscribe',
        filter: {
            job_types: ['order_timeout_cancel', 'payment_check'],
            event_types: ['job.started', 'job.completed', 'job.failed']
        }
    }));
};

ws.onmessage = (event) => {
    const data = JSON.parse(event.data);
    
    // 更新UI
    switch(data.type) {
        case 'job.started':
            showNotification(`任务 ${data.job_name} 开始执行`, 'info');
            updateJobStatus(data.job_id, 'running');
            break;
        case 'job.completed':
            showNotification(`任务 ${data.job_name} 执行成功`, 'success');
            updateJobStatus(data.job_id, 'completed');
            break;
        case 'job.failed':
            showNotification(`任务 ${data.job_name} 失败: ${data.data?.error}`, 'error');
            updateJobStatus(data.job_id, 'failed');
            break;
    }
};

// 心跳保活
setInterval(() => {
    ws.send(JSON.stringify({action: 'ping'}));
}, 30000);
```
