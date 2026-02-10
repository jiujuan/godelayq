package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"core"
)

func main() {
	// 初始化组件
	store, _ := goschedjob.NewJSONFileStore("./jobs.json")
	scheduler := goschedjob.NewScheduler(store, &goschedjob.ExponentialBackoffRetry{
		MaxDelay: 30 * time.Minute,
	})

	// 注册处理器
	handlers := map[string]goschedjob.Handler{
		"payment_check": func(ctx context.Context, job *goschedjob.Job) error {
			fmt.Printf("检查支付: %s\n", string(job.Payload))
			return nil
		},
		"email_notify": func(ctx context.Context, job *goschedjob.Job) error {
			fmt.Printf("发送邮件: %s\n", string(job.Payload))
			return nil
		},
		"data_backup": func(ctx context.Context, job *goschedjob.Job) error {
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
	loader, err := goschedjob.NewDirectoryLoader(scheduler, goschedjob.LoaderOptions{
		Dir:            "./job_queue",               // 任务文件存放目录
		Pattern:        "*.json",                    // 匹配所有json文件
		PostLoadAction: goschedjob.ArchiveAfterLoad, // 加载后归档
		ArchiveDir:     "./job_archive",             // 归档目录
		ErrorDir:       "./job_errors",              // 错误文件目录
		Recursive:      true,                        // 递归子目录
		EnableWatcher:  true,                        // 实时监控新文件
		HandlerMap:     handlers,                    // 自动绑定handler
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
  "retry_delay": "5m",
  "description": "10分钟后检查订单支付状态"
}`)
}
