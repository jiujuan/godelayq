## 示例 1：电商订单超时取消

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"godelayq/api"
	"godelayq/core"
)

func main() {
	logger, err := core.NewLogger("info", "text", os.Stdout)
	if err != nil {
		log.Fatal(err)
	}
	slog.SetDefault(logger)

	// 初始化存储
	store, err := core.NewJSONFileStore("./data/jobs.json")
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()

	// 创建调度器
	scheduler := core.NewScheduler(store, &core.ExponentialBackoffRetry{
		MaxDelay: 30 * time.Minute,
	}, nil, core.WithLogger(logger))

	// 创建 API 服务器：第 4 个参数是安全配置（零值即不鉴权、放开跨域），第 5 个是日志器
	server := api.NewServer(scheduler, store, "8080", api.Security{
		AllowOrigins: []string{"https://admin.example.com"},
	}, logger)

	// 注册订单超时处理器
	server.RegisterJobHandler("order_timeout_cancel", func(ctx context.Context, job *core.Job) error {
		var payload struct {
			OrderID string  `json:"order_id"`
			Amount  float64 `json:"amount"`
			UserID  string  `json:"user_id"`
		}

		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return fmt.Errorf("invalid payload: %w", err)
		}

		// Handler 必须检查 ctx，否则超时与取消都只能被记录、无法真正中止
		if err := ctx.Err(); err != nil {
			return err
		}

		if err := cancelOrderIfUnpaid(payload.OrderID); err != nil {
			return fmt.Errorf("cancel failed: %w", err)
		}

		slog.Info("order cancelled", "order_id", payload.OrderID, "job_id", job.ID)
		return nil
	})

	scheduler.Start()
	defer scheduler.Stop()

	if err := server.Start(); err != nil {
		log.Fatal(err)
	}

	// 等信号后优雅关闭：先收 HTTP/长连接，再停调度器
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Stop(ctx); err != nil {
		// 走到这里说明有连接没在超时内收尾，服务器已被强制关闭
		logger.Error("server forced to shutdown", "error", err)
	}
}

func cancelOrderIfUnpaid(orderID string) error {
	// 实现订单取消逻辑
	// 调用数据库或订单服务API
	return nil
}
```

`api.NewServer` 的第 4 个参数 `api.Security` 也可以是零值 `api.Security{}`，
含义是不启用鉴权、接受任意跨域来源；`AuthToken` 非空时全部端点（含 `/ws`、`/sse/events`）都要凭据。

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
- `max_retries`：不写取默认 3；**写 `0` 就是不重试**（历史版本会把 0 抬回 3，现在按字面值处理）；
  负数报错。注意与 `POST /jobs` 不同——HTTP 请求体里 `max_retries` 是普通整数，
  省略与写 0 无法区分，因此**省略等于不重试**，想要重试必须显式给值。
- `timeout` 可选，是单次执行超时（`time.ParseDuration` 格式）；省略则不限制。
- 文件里的 `tags`、`description` 一类的额外键不再被解析（此前也是解析后即丢弃），
  加载时会被忽略，不影响任务入队。

Handler 必须检查传入的 `ctx`，否则超时只能被记录为失败、无法真正中止执行。

配置目录加载器自动监控：

```go
loader, err := core.NewDirectoryLoader(scheduler, core.LoaderOptions{
    Dir:            "./jobs",
    Pattern:        "*.json",
    PostLoadAction: core.ArchiveAfterLoad,
    ArchiveDir:     "./jobs/archive",
    ErrorDir:       "./jobs/errors",   // 校验/解析失败的文件留档到这里
    AllowExecJobs:  false,             // 默认拒绝 exec.* 任务文件，见下一段
    EnableWatcher:  true,              // 实时监控新文件
    Logger:         logger,            // 可选，nil 用 slog.Default()
})
if err != nil {
    log.Fatal(err)
}

if err := loader.Start(); err != nil {
    log.Fatal(err)
}
defer loader.Stop()
```

任务文件里的 `name` 必须在调度器的注册表里存在（`scheduler.RegisterHandler(name, ...)`），
否则任务入堆后执行时会被判为失败（`no handler registered`）。

例外是执行器档位：`name` 以 `exec.` 开头的任务文件**默认被加载器拒绝**，即使档位已经注册成功。
这条路径上没有身份凭据，"能往这个目录写文件"如果不拒绝就等于"能在这台机器上执行档位声明的命令"。
档位来自配置里的 `executors.commands` 还是来自页面上写的档位文件（`executors.profiles_path`），这条拒绝都同样生效：
被拒的是任务文件进入的路径，与档位声明来自哪一份来源无关。
被拒绝的文件不会进堆、也不会调用任何 Handler，去向是一行 warn + 错误目录副本 + 按 `PostLoadAction` 处理：

```
level=WARN msg="executor job file rejected by the loader" path=job_queue\exec_try.json job_name=exec.echo reason="executor jobs are not accepted from the loader" hint="set LoaderOptions.AllowExecJobs only when writing into this directory is meant to grant execution"
```

要放开只能由程序显式设置 `LoaderOptions.AllowExecJobs`（配置项 `executors.loader_allow` 只由
`Registry.LoaderAllowed()` 读取，调用方把结果填给加载器；服务端二进制不启用目录加载器，
也就没有这条链）。前提与后果见 [部署文档](./deployment.md) 的"开启执行器"第 12 条。
`LoaderOptions.HandlerMap` 可以在加载时直接绑定 Handler，但执行侧回查用的仍是注册表，
两者键值都以 `name`（或任务 `type`）为准。

监控模式下同一文件的连续写入会合并成一次加载：事件到达后等 100ms 静默窗口，
窗口内的重复事件只排一次队，因此读到最后一次写入的内容，也不会读到半截 JSON。
`Stop()` 会取消尚未触发的排队加载。

## 示例 3：实时监控 Dashboard

前端 JavaScript 连接 WebSocket（启用鉴权时 URL 必须带 `?token=`，浏览器无法自定义握手头）：

```javascript
const token = localStorage.getItem('godelayq_token');
const ws = new WebSocket(`ws://localhost:8080/ws${token ? `?token=${token}` : ''}`);

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

    // 先挡掉控制帧：subscribed / unsubscribed / pong / stats 没有 type 字段
    if (!data.type) {
        return;
    }

    // status 在推送事件里是数字：0 pending / 1 running / 2 success / 3 failed / 4 cancelled
    switch (data.type) {
        case 'job.started':
            showNotification(`任务 ${data.job_name} 开始执行`, 'info');
            updateJobStatus(data.job_id, 'running');
            break;
        case 'job.completed':
            showNotification(`任务 ${data.job_name} 执行成功`, 'success');
            updateJobStatus(data.job_id, 'success');
            break;
        case 'job.failed':
            showNotification(`任务 ${data.job_name} 失败: ${data.data?.error}`, 'error');
            updateJobStatus(data.job_id, 'failed');
            break;
    }
};

// 心跳保活：服务端每 30 秒发一次 ping，也会主动读超时（60 秒无 pong 即断开）
setInterval(() => {
    ws.send(JSON.stringify({action: 'ping'}));
}, 30000);
```

仓库里的 `dashboard/index.html` 是这种单文件页面的**历史版本**：它已被 `web/` 的 Vue 控制台
取代，现在只会把人跳回同源根路径。想要一个自己托管的极简页面，照本节自己写一份即可；
跨域部署时需把服务端的 `server.cors.allow_origins` 指到面板所在来源。
