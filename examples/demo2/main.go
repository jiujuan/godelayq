package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"godelayq/core"
)

func main() {
	// 初始化组件
	store, _ := core.NewJSONFileStore("./jobs.json")
	scheduler := core.NewScheduler(store, &core.ExponentialBackoffRetry{
		MaxDelay: 30 * time.Minute,
	}, nil)

	// 注册处理器
	handlers := map[string]core.Handler{
		"payment_check": func(ctx context.Context, job *core.Job) error {
			fmt.Printf("检查支付: %s\n", string(job.Payload))
			return nil
		},
		"email_notify": func(ctx context.Context, job *core.Job) error {
			fmt.Printf("发送邮件: %s\n", string(job.Payload))
			return nil
		},
		"data_backup": func(ctx context.Context, job *core.Job) error {
			fmt.Printf("数据备份: %s\n", string(job.Payload))
			return nil
		},
	}

	// 注册到调度器
	for name, h := range handlers {
		scheduler.RegisterHandler(name, h)
	}

	// 启动调度器
	scheduler.Start()
	defer scheduler.Stop()

	// 配置并启动目录加载器
	loader, err := core.NewDirectoryLoader(scheduler, core.LoaderOptions{
		Dir:            "./job_queue",         // 任务文件存放目录
		Pattern:        "*.json",              // 匹配所有json文件
		PostLoadAction: core.ArchiveAfterLoad, // 加载后归档
		ArchiveDir:     "./job_archive",       // 归档目录
		ErrorDir:       "./job_errors",        // 错误文件目录
		Recursive:      true,                  // 递归子目录
		EnableWatcher:  true,                  // 实时监控新文件
		HandlerMap:     handlers,              // 自动绑定handler
		// AllowExecJobs 这里显式留 false（TASK-E17）：任务文件这条路上没有任何凭据，
		// 打开它等于把"能往 ./job_queue 写文件"变成"能执行 exec.<档位名> 声明的命令"。
		// 真要这么用，就把它改成 true 并把这个目录收成只有服务账号可写；
		// 如果程序同时装配了执行器登记表，配置项 executors.loader_allow 的取值可以从
		// executor.Registry.LoaderAllowed() 读出来接到这里。
		AllowExecJobs: false,
	})
	if err != nil {
		log.Fatal(err)
	}

	if err := loader.Start(); err != nil {
		log.Fatal(err)
	}
	defer loader.Stop()

	fmt.Println("系统运行中，请将任务JSON文件放入 ./job_queue 目录...")
	fmt.Println("支持的JSON格式示例：")
	fmt.Println(string(exampleJob()))
	fmt.Println("提示：name 以 exec. 开头的任务文件默认不被接受，会被移入 ./job_errors 并留一行 warn")

	select {} // 保持运行
}

func exampleJob() []byte {
	return []byte(`{
  "id": "pay_001",
  "name": "payment_check",
  "delay": "10m",
  "payload": {
    "order_id": "ORD-20240115-001",
    "user_id": "U12345",
    "amount": 199.99,
    "currency": "CNY"
  },
  "max_retries": 3,
  "retry_delay": "5m"
}`)
}
