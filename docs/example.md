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
  "payload": {
    "order_id": "ORD-20240115-002",
    "amount": 199.99,
    "user_id": "U789012"
  },
  "max_retries": 2,
  "retry_delay": "5m",
  "description": "批量导入的订单超时任务"
}
```

配置目录加载器自动监控：

```go
loader, err := godelayq.NewDirectoryLoader(scheduler, godelayq.LoaderOptions{
    Dir:            "./jobs",
    Pattern:        "*.json",
    PostLoadAction: godelayq.ArchiveAfterLoad,
    ArchiveDir:     "./jobs/archive",
    EnableWatcher:  true,  // 实时监控新文件
})
if err != nil {
    log.Fatal(err)
}

if err := loader.Start(); err != nil {
    log.Fatal(err)
}
```
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
